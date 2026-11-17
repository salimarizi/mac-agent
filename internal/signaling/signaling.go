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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	viewers   atomic.Int64
	startedAt time.Time
	startOnce sync.Once

	// active is the single viewer allowed to hold a session at a time.
	mu     sync.Mutex
	active *conn
}

// WebSocket liveness. Mobile networks and the nginx hop in front of us both drop
// idle sockets silently; pinging keeps them open and surfaces a dead peer within
// pongWait instead of leaving a zombie session holding the capture pipeline.
const (
	pongWait   = 30 * time.Second
	pingPeriod = 10 * time.Second
	writeWait  = 10 * time.Second
	// evictWait bounds how long a new viewer waits for the old one's capture to
	// stop before starting its own.
	evictWait = 3 * time.Second
)

// conn wraps a websocket with a write mutex (gorilla writes aren't concurrent-safe).
type conn struct {
	ws  *websocket.Conn
	wmu sync.Mutex

	// sessMu guards sess/capCancel. A session can die on a pion callback goroutine
	// at the same moment the reader goroutine is replacing it.
	sessMu sync.Mutex
	sess   *session.Session
	// capCancel stops the ffmpeg pipeline for this connection.
	capCancel context.CancelFunc
	// capMu guards capCfg, this viewer's capture override (nil = server default).
	capMu  sync.Mutex
	capCfg *capture.Config
	// done closes when teardown begins, stopping the ping loop.
	done     chan struct{}
	doneOnce sync.Once
	// gone closes once teardown has finished and the capture pipeline is released.
	gone     chan struct{}
	goneOnce sync.Once
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
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	if err := c.ws.WriteMessage(websocket.TextMessage, env); err != nil {
		log.Printf("signaling: write %s: %v", kind, err)
	}
}

// keepalive pings the viewer until the connection is torn down. A failed write
// closes the socket, which unblocks the reader in handleWS.
func (c *conn) keepalive() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.wmu.Lock()
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			err := c.ws.WriteMessage(websocket.PingMessage, nil)
			c.wmu.Unlock()
			if err != nil {
				log.Printf("signaling: ping: %v", err)
				_ = c.ws.Close()
				return
			}
		}
	}
}

func (c *conn) session() *session.Session {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.sess
}

// captureConfig is the config this viewer's next session should start with.
func (c *conn) captureConfig(base capture.Config) capture.Config {
	c.capMu.Lock()
	defer c.capMu.Unlock()
	if c.capCfg == nil {
		return base
	}
	return *c.capCfg
}

func (c *conn) closeDone() {
	c.doneOnce.Do(func() { close(c.done) })
}

func (c *conn) closeGone() {
	c.goneOnce.Do(func() { close(c.gone) })
}

// adopt makes c the one active viewer and evicts whoever held the slot. A phone
// that closed its browser without a clean WebSocket close leaves a socket the OS
// never reports as dead, so without this the old session keeps the capture
// pipeline pinned and the next connect looks like the Mac is busy.
func (s *Server) adopt(c *conn) {
	s.mu.Lock()
	prev := s.active
	s.active = c
	s.mu.Unlock()

	if prev == nil || prev == c {
		return
	}

	log.Print("signaling: evicting previous viewer for a new connection")
	prev.send("evicted", map[string]string{"message": "replaced by a newer connection"})
	prev.closeDone()
	// Closing the socket unblocks the old reader, so its deferred teardown runs.
	_ = prev.ws.Close()

	select {
	case <-prev.gone:
	case <-time.After(evictWait):
		log.Print("signaling: previous viewer did not release in time, continuing")
	}
}

// Shutdown closes the active viewer so its capture pipeline is released before
// the process exits. A hijacked WebSocket is invisible to http.Server.Shutdown,
// so without this the ffmpeg behind it outlives the agent and keeps holding the
// screen-recording device.
func (s *Server) Shutdown() {
	s.mu.Lock()
	c := s.active
	s.mu.Unlock()
	if c == nil {
		return
	}
	log.Print("signaling: closing active viewer for shutdown")
	c.closeDone()
	_ = c.ws.Close()
	select {
	case <-c.gone:
	case <-time.After(evictWait):
		log.Print("signaling: viewer did not release in time")
	}
}

// release clears the active slot, but only if c still owns it — an evicted
// connection must not clear the newer viewer that replaced it.
func (s *Server) release(c *conn) {
	s.mu.Lock()
	if s.active == c {
		s.active = nil
	}
	s.mu.Unlock()
}

// --- Rate limiter for auth attempts ---

const (
	maxAuthFails  = 5
	blockDuration = 30 * time.Second
	// entryTTL bounds how long an idle entry is kept. The agent is reachable from
	// the internet, so a scanner walking through source addresses would otherwise
	// grow this map for as long as the process runs.
	entryTTL = 10 * time.Minute
)

type rlEntry struct {
	fails      int
	blockUntil time.Time
	last       time.Time
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

	now := time.Now()
	for k, v := range r.entries {
		if now.Sub(v.last) > entryTTL && now.After(v.blockUntil) {
			delete(r.entries, k)
		}
	}

	e := r.entries[ip]
	if e == nil {
		e = &rlEntry{}
		r.entries[ip] = e
	}
	e.fails++
	e.last = now
	if e.fails >= maxAuthFails {
		e.blockUntil = now.Add(blockDuration)
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

// Handler returns the HTTP mux (WebSocket at /ws, health at /healthz, optional
// static files at /).
func (s *Server) Handler() http.Handler {
	s.startOnce.Do(func() { s.startedAt = time.Now() })
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/healthz", s.handleHealth)
	if s.WebRoot != "" {
		mux.Handle("/", http.FileServer(http.Dir(s.WebRoot)))
	}
	return securityHeaders(mux)
}

// handleHealth answers unauthenticated so the gateway can distinguish "the Mac's
// tunnel is down" from "the Mac is up but idle". It exposes no secrets.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "online",
		"viewers":  s.viewers.Load(),
		"uptime_s": int64(time.Since(s.startedAt).Seconds()),
		"has_turn": len(s.ICEServers) > 1,
	})
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
	c := &conn{ws: ws, done: make(chan struct{}), gone: make(chan struct{})}
	s.viewers.Add(1)
	log.Printf("signaling: viewer connected from %s", ip)
	defer s.teardown(c)

	// Auth required as first message.
	if s.Password != "" {
		if !s.authenticate(c, ip) {
			return
		}
	}

	// Only take the viewer slot after auth, so a wrong password can't kick off
	// the session that is currently working.
	s.adopt(c)

	_ = ws.SetReadDeadline(time.Now().Add(pongWait))
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(pongWait))
	})
	go c.keepalive()

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

	case "quality":
		if !s.setQuality(c, msg.Payload) {
			return
		}
		// Before "hello" there is nothing to restart — the new config is simply
		// what the first session will be built with.
		if c.session() != nil {
			s.startSession(c)
		}

	case "answer":
		sess := c.session()
		if sess == nil {
			return
		}
		sd, err := session.DecodeAnswer(msg.Payload)
		if err != nil {
			log.Printf("signaling: decode answer: %v", err)
			return
		}
		if err := sess.SetAnswer(sd); err != nil {
			log.Printf("signaling: set answer: %v", err)
		}

	case "candidate":
		sess := c.session()
		if sess == nil {
			return
		}
		cand, err := session.DecodeCandidate(msg.Payload)
		if err != nil {
			log.Printf("signaling: decode candidate: %v", err)
			return
		}
		if err := sess.AddCandidate(cand); err != nil {
			log.Printf("signaling: add candidate: %v", err)
		}

	default:
		log.Printf("signaling: unknown message type %q", msg.Type)
	}
}

// qualityReq is the viewer choosing a stream profile. The phone knows things the
// Mac cannot — that it is on cellular, or that the user wants detail over
// smoothness — so the preset is picked there and clamped here.
type qualityReq struct {
	Width   int    `json:"width"`
	FPS     int    `json:"fps"`
	Bitrate string `json:"bitrate"`
}

const (
	minCaptureWidth = 480
	maxCaptureWidth = 3840
	minCaptureFPS   = 5
	maxCaptureFPS   = 60
	minBitrateBps   = 300_000
	maxBitrateBps   = 12_000_000
)

// parseBitrate accepts ffmpeg-style rates ("900K", "6M", "2500000") and returns a
// clamped plain bits-per-second string. Anything unparseable is rejected rather
// than handed to ffmpeg.
func parseBitrate(b string) (string, bool) {
	if b == "" {
		return "", false
	}
	mult, num := 1, b
	switch b[len(b)-1] {
	case 'K', 'k':
		mult, num = 1_000, b[:len(b)-1]
	case 'M', 'm':
		mult, num = 1_000_000, b[:len(b)-1]
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return "", false
	}
	return strconv.Itoa(clampInt(n*mult, minBitrateBps, maxBitrateBps)), true
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// setQuality records the viewer's requested profile, leaving any field it did not
// ask about at the server's configured value.
func (s *Server) setQuality(c *conn, raw json.RawMessage) bool {
	var q qualityReq
	if err := json.Unmarshal(raw, &q); err != nil {
		log.Printf("signaling: bad quality payload: %v", err)
		return false
	}
	cfg := s.Capture
	if q.Width > 0 {
		cfg.Width = clampInt(q.Width, minCaptureWidth, maxCaptureWidth)
	}
	if q.FPS > 0 {
		cfg.FPS = clampInt(q.FPS, minCaptureFPS, maxCaptureFPS)
	}
	if b, ok := parseBitrate(q.Bitrate); ok {
		cfg.Bitrate = b
	}

	c.capMu.Lock()
	prev := c.capCfg
	// No override yet means the viewer is already running the server's settings.
	same := (prev == nil && cfg == s.Capture) || (prev != nil && *prev == cfg)
	c.capCfg = &cfg
	c.capMu.Unlock()
	if same {
		return false // nothing changed; don't interrupt a working stream
	}

	log.Printf("signaling: viewer requested %dpx wide @ %dfps, %s bps", cfg.Width, cfg.FPS, cfg.Bitrate)
	return true
}

func (s *Server) startSession(c *conn) {
	// Replace any existing session on this connection.
	s.teardownSession(c)

	cfg := c.captureConfig(s.Capture)
	ctx, cancel := context.WithCancel(context.Background())
	frames, err := capture.Start(ctx, cfg)
	if err != nil {
		cancel()
		log.Printf("signaling: capture start: %v", err)
		c.send("error", map[string]string{"message": "capture failed: " + err.Error()})
		return
	}

	sess, err := session.New(s.ICEServers, frames, cfg.FPS, s.Input, c.send)
	if err != nil {
		cancel()
		log.Printf("signaling: session: %v", err)
		c.send("error", map[string]string{"message": "session failed"})
		return
	}
	c.sessMu.Lock()
	c.sess = sess
	c.capCancel = cancel
	c.sessMu.Unlock()

	go s.watchSession(c, sess, cancel)

	if err := sess.Offer(); err != nil {
		log.Printf("signaling: offer: %v", err)
		s.teardownSession(c)
	}
}

// watchSession releases the capture pipeline when a session ends by itself —
// ICE failure, the drop timer expiring, ffmpeg dying. Nothing else notices those,
// so without this the Mac keeps encoding at full framerate for a viewer that is
// already gone.
func (s *Server) watchSession(c *conn, sess *session.Session, cancel context.CancelFunc) {
	<-sess.Done()
	cancel()

	c.sessMu.Lock()
	current := c.sess == sess
	if current {
		c.sess = nil
		c.capCancel = nil
	}
	c.sessMu.Unlock()

	// Only speak up if this is still the connection's live session; a session
	// replaced by teardownSession or a fresh "hello" is not news to the viewer.
	if current {
		log.Print("signaling: session ended on its own, capture released")
		c.send("session_ended", map[string]string{"message": "capture session ended"})
	}
}

func (s *Server) teardownSession(c *conn) {
	c.sessMu.Lock()
	sess, cancel := c.sess, c.capCancel
	c.sess, c.capCancel = nil, nil
	c.sessMu.Unlock()

	if sess != nil {
		sess.Close()
	}
	if cancel != nil {
		cancel()
	}
}

func (s *Server) teardown(c *conn) {
	c.closeDone()
	s.release(c)
	s.teardownSession(c)
	_ = c.ws.Close()
	s.viewers.Add(-1)
	c.closeGone()
	log.Print("signaling: viewer disconnected")
}
