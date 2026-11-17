// Command mac-agent streams the Mac screen to a browser over WebRTC and accepts
// remote mouse/keyboard input. Run it on the Mac you want to control; open the
// client (served at / or hosted separately) from your iPhone.
//
//	MAC_AGENT_PASSWORD=secret go run . -screen 0 -fps 30 -width 1600 -addr :8443 -web client
//
// For TLS (recommended for internet use):
//
//	MAC_AGENT_PASSWORD=secret go run . -tls-cert cert.pem -tls-key key.pem -addr :8443 -web client
//
// Environment variables for secrets (recommended over flags):
//
//	MAC_AGENT_PASSWORD   — access password (required)
//	MAC_AGENT_TURN_URL   — TURN server URL
//	MAC_AGENT_TURN_USER  — TURN username
//	MAC_AGENT_TURN_PASS  — TURN credential
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/salimarizi/mac-agent/internal/capture"
	"github.com/salimarizi/mac-agent/internal/input"
	"github.com/salimarizi/mac-agent/internal/signaling"
)

func envOrFlag(envKey string, flagVal *string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return *flagVal
}

// splitList parses a comma-separated value into trimmed, non-empty entries.
func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	addr := flag.String("addr", ":8443", "listen address")
	screen := flag.String("screen", "0", "display index (0 = main display, 1 = second display, …)")
	fps := flag.Int("fps", 30, "capture framerate")
	width := flag.Int("width", 1600, "scaled output width (0 = native)")
	bitrate := flag.String("bitrate", "6M", "target video bitrate")
	ffmpegPath := flag.String("ffmpeg", "ffmpeg", "path to ffmpeg binary")
	webRoot := flag.String("web", "", "optional dir of static client files to serve at /")
	sensitivity := flag.Float64("sensitivity", 1.6, "trackpad move multiplier")
	password := flag.String("password", "", "access password (prefer MAC_AGENT_PASSWORD env)")
	turnURL := flag.String("turn", "", "comma-separated TURN urls (prefer MAC_AGENT_TURN_URL env)")
	stunURLs := flag.String("stun", "stun:stun.l.google.com:19302,stun:stun.cloudflare.com:3478", "comma-separated STUN urls")
	turnUser := flag.String("turn-user", "", "TURN username (prefer MAC_AGENT_TURN_USER env)")
	turnPass := flag.String("turn-pass", "", "TURN credential (prefer MAC_AGENT_TURN_PASS env)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file for HTTPS")
	tlsKey := flag.String("tls-key", "", "TLS private key file for HTTPS")
	flag.Parse()

	pw := envOrFlag("MAC_AGENT_PASSWORD", password)
	if pw == "" {
		log.Fatal("password required: set MAC_AGENT_PASSWORD env var or -password flag")
	}

	turn := envOrFlag("MAC_AGENT_TURN_URL", turnURL)
	tUser := envOrFlag("MAC_AGENT_TURN_USER", turnUser)
	tPass := envOrFlag("MAC_AGENT_TURN_PASS", turnPass)

	iceServers := []webrtc.ICEServer{
		{URLs: splitList(envOrFlag("MAC_AGENT_STUN_URLS", stunURLs))},
	}
	// One entry per TURN transport. Listing udp/tcp/tls separately means a network
	// that blocks UDP 3478 still has a TCP and a TLS-on-5349 path to fall back to.
	if urls := splitList(turn); len(urls) > 0 {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:       urls,
			Username:   tUser,
			Credential: tPass,
		})
		log.Printf("ICE: %d TURN url(s) configured: %s", len(urls), strings.Join(urls, " "))
	} else {
		log.Print("ICE: no TURN configured — connections from mobile data will likely fail")
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// robotgo tolerates exactly one calling goroutine, so the dispatch loop is
	// owned by the process rather than by whichever session happens to be live.
	ctrl := input.New(*sensitivity)
	go ctrl.Run(ctx.Done())

	srv := &signaling.Server{
		ICEServers: iceServers,
		Capture: capture.Config{
			ScreenIndex: *screen,
			FPS:         *fps,
			Width:       *width,
			Bitrate:     *bitrate,
			FFmpegPath:  *ffmpegPath,
		},
		Input:    ctrl,
		WebRoot:  *webRoot,
		Password: pw,
	}

	tls := envOrFlag("MAC_AGENT_TLS_CERT", tlsCert)
	tlsK := envOrFlag("MAC_AGENT_TLS_KEY", tlsKey)

	log.Printf("mac-agent listening on %s (screen=%s fps=%d width=%d)", *addr, *screen, *fps, *width)
	if input.AccessibilityTrusted() {
		log.Print("permissions: Accessibility granted — remote input will work")
	} else {
		log.Print("permissions: Accessibility NOT granted — the screen will stream but " +
			"nothing will respond to taps. Grant it to the process that launches this " +
			"agent (launchd runs /bin/bash) in System Settings > Privacy & Security > " +
			"Accessibility, then restart the service.")
	}

	// No WriteTimeout/ReadTimeout: they would kill long-lived WebSocket sessions.
	// Liveness is handled by the signaling layer's ping/pong instead.
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// launchd stops us with SIGTERM. Releasing the viewer first tears down its
	// ffmpeg; exiting without that leaves the encoder orphaned, still holding the
	// screen-recording device against the next start.
	go func() {
		<-ctx.Done()
		log.Print("shutting down")
		srv.Shutdown()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	var err error
	if tls != "" && tlsK != "" {
		log.Print("TLS enabled")
		err = httpSrv.ListenAndServeTLS(tls, tlsK)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
	log.Print("stopped")
}
