// Package signaling hosts the WebSocket endpoint the iPhone connects to and
// brokers the WebRTC handshake. Because the agent both serves signaling and is
// the peer, there is no third-party server — the phone talks directly to the
// Mac.
//
// Single viewer at a time: a new "hello" replaces any existing session. Capture
// (ffmpeg) is started per viewer and torn down on disconnect so we never burn
// CPU encoding when nobody is watching.
package signaling

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/salimarizi/mac-agent/internal/capture"
	"github.com/salimarizi/mac-agent/internal/input"
	"github.com/salimarizi/mac-agent/internal/session"
)

type message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// iceServerJSON is the browser-compatible ICE server format.
type iceServerJSON struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// Server holds shared config; one is created in main.
type Server struct {
	ICEServers []webrtc.ICEServer
	Capture    capture.Config
	Input      *input.Controller
	WebRoot    string // optional: directory of static client files to serve at /
	Password   string // required: checked with constant-time compare
}

// conn wraps a websocket with a write mutex (gorilla writes aren't concurrent-safe).
type conn struct {
	ws   *websocket.Conn
	wmu  sync.Mutex
	sess *session.Session
	// capCancel stops the ffmpeg pipeline for this connection.
	capCancel context.CancelFunc
}

func (c *conn) send(kind string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		log.Printf("signaling: marshal %s: %v", kind, err)
		return
	}
	env, _ := json.Marshal(message{Type: kind, Payload: b})
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.ws.WriteMessage(websocket.TextMessage, env); err != nil {
		log.Printf("signaling: write %s: %v", kind, err)
	}
}

// --- Rate limiter for auth attempts ---

const (
	maxAuthFails  = 5
	blockDuration = 30 * time.Second
)

type rlEntry struct {
	fails      int
	blockUntil time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	entries map[string]*rlEntry
}

var rl = &rateLimiter{entries: make(map[string]*rlEntry)}

func (r *rateLimiter) allowed(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[ip]
	if e == nil {
		return true
	}
	if time.Now().Before(e.blockUntil) {
		return false
	}
	if e.fails >= maxAuthFails {
		delete(r.entries, ip)
	}
	return true
}

func (r *rateLimiter) fail(ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[ip]
	if e == nil {
		e = &rlEntry{}
		r.entries[ip] = e
	}
	e.fails++
	if e.fails >= maxAuthFails {
		e.blockUntil = time.Now().Add(blockDuration)
		log.Printf("signaling: blocking %s for %v after %d failed auth attempts", ip, blockDuration, e.fails)
	}
}

func (r *rateLimiter) reset(ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, ip)
}

// --- WebSocket upgrader with origin check ---

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // non-browser clients
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return u.Host == r.Host
	},
}

// --- Security middleware ---

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// --- IP extraction ---

func remoteIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.SplitN(xff, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Handler returns the HTTP mux (WebSocket at /ws, optional static files at /).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	if s.WebRoot != "" {
		mux.Handle("/", http.FileServer(http.Dir(s.WebRoot)))
	}
	return securityHeaders(mux)
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)

	if !rl.allowed(ip) {
		log.Printf("signaling: rate-limited %s", ip)
		http.Error(w, "too many attempts, try again later", http.StatusTooManyRequests)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("signaling: upgrade: %v", err)
		return
	}
	c := &conn{ws: ws}
	log.Printf("signaling: viewer connected from %s", ip)
	defer s.teardown(c)

	// Auth required as first message.
	if s.Password != "" {
		if !s.authenticate(c, ip) {
			return
		}
	}

	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			log.Printf("signaling: read: %v", err)
			return
		}
		var msg message
		if err := json.Unmarshal(raw, &msg); err != nil {
			log.Printf("signaling: bad message: %v", err)
			continue
		}
		s.route(c, msg)
	}
}

// authenticate reads the first WS message, expects {"type":"auth","payload":"<pw>"}.
func (s *Server) authenticate(c *conn, ip string) bool {
	_ = c.ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := c.ws.ReadMessage()
	_ = c.ws.SetReadDeadline(time.Time{})
	if err != nil {
		log.Printf("signaling: auth read timeout from %s", ip)
		return false
	}

	var msg message
	if err := json.Unmarshal(raw, &msg); err != nil || msg.Type != "auth" {
		c.send("auth_fail", map[string]string{"message": "auth required"})
		rl.fail(ip)
		return false
	}

	var password string
	if err := json.Unmarshal(msg.Payload, &password); err != nil {
		c.send("auth_fail", map[string]string{"message": "invalid auth payload"})
		rl.fail(ip)
		return false
	}

	if subtle.ConstantTimeCompare([]byte(password), []byte(s.Password)) != 1 {
		log.Printf("signaling: auth failed from %s", ip)
		c.send("auth_fail", map[string]string{"message": "incorrect password"})
		rl.fail(ip)
		return false
	}

	rl.reset(ip)
	log.Printf("signaling: authenticated from %s", ip)

	// Send ICE servers config so TURN credentials stay server-side only.
	ice := make([]iceServerJSON, 0, len(s.ICEServers))
	for _, is := range s.ICEServers {
		entry := iceServerJSON{URLs: is.URLs, Username: is.Username}
		if cred, ok := is.Credential.(string); ok {
			entry.Credential = cred
		}
		ice = append(ice, entry)
	}
	c.send("auth_ok", map[string]any{"iceServers": ice})
	return true
}

func (s *Server) route(c *conn, msg message) {
	switch msg.Type {
	case "hello":
		s.startSession(c)

	case "answer":
		if c.sess == nil {
			return
		}
		sd, err := session.DecodeAnswer(msg.Payload)
		if err != nil {
			log.Printf("signaling: decode answer: %v", err)
			return
		}
		if err := c.sess.SetAnswer(sd); err != nil {
			log.Printf("signaling: set answer: %v", err)
		}

	case "candidate":
		if c.sess == nil {
			return
		}
		cand, err := session.DecodeCandidate(msg.Payload)
		if err != nil {
			log.Printf("signaling: decode candidate: %v", err)
			return
		}
		if err := c.sess.AddCandidate(cand); err != nil {
			log.Printf("signaling: add candidate: %v", err)
		}

	default:
		log.Printf("signaling: unknown message type %q", msg.Type)
	}
}

func (s *Server) startSession(c *conn) {
	// Replace any existing session on this connection.
	s.teardownSession(c)

	ctx, cancel := context.WithCancel(context.Background())
	frames, err := capture.Start(ctx, s.Capture)
	if err != nil {
		cancel()
		log.Printf("signaling: capture start: %v", err)
		c.send("error", map[string]string{"message": "capture failed: " + err.Error()})
		return
	}

	sess, err := session.New(s.ICEServers, frames, s.Capture.FPS, s.Input, c.send)
	if err != nil {
		cancel()
		log.Printf("signaling: session: %v", err)
		c.send("error", map[string]string{"message": "session failed"})
		return
	}
	c.sess = sess
	c.capCancel = cancel

	if err := sess.Offer(); err != nil {
		log.Printf("signaling: offer: %v", err)
		s.teardownSession(c)
	}
}

func (s *Server) teardownSession(c *conn) {
	if c.sess != nil {
		c.sess.Close()
		c.sess = nil
	}
	if c.capCancel != nil {
		c.capCancel()
		c.capCancel = nil
	}
}

func (s *Server) teardown(c *conn) {
	s.teardownSession(c)
	_ = c.ws.Close()
	log.Print("signaling: viewer disconnected")
}
