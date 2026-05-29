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
	"flag"
	"log"
	"net/http"
	"os"

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
	turnURL := flag.String("turn", "", "TURN url (prefer MAC_AGENT_TURN_URL env)")
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
		{URLs: []string{"stun:stun.l.google.com:19302"}},
	}
	if turn != "" {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:       []string{turn},
			Username:   tUser,
			Credential: tPass,
		})
	}

	srv := &signaling.Server{
		ICEServers: iceServers,
		Capture: capture.Config{
			ScreenIndex: *screen,
			FPS:         *fps,
			Width:       *width,
			Bitrate:     *bitrate,
			FFmpegPath:  *ffmpegPath,
		},
		Input:    input.New(*sensitivity),
		WebRoot:  *webRoot,
		Password: pw,
	}

	tls := envOrFlag("MAC_AGENT_TLS_CERT", tlsCert)
	tlsK := envOrFlag("MAC_AGENT_TLS_KEY", tlsKey)

	log.Printf("mac-agent listening on %s (screen=%s fps=%d width=%d)", *addr, *screen, *fps, *width)
	log.Print("NOTE: grant Screen Recording AND Accessibility permission to this binary/terminal.")

	if tls != "" && tlsK != "" {
		log.Print("TLS enabled")
		if err := http.ListenAndServeTLS(*addr, tls, tlsK, srv.Handler()); err != nil {
			log.Fatalf("server: %v", err)
		}
	} else {
		if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
			log.Fatalf("server: %v", err)
		}
	}
}
