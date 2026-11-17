package signaling

import (
	"encoding/json"
	"testing"

	"github.com/salimarizi/mac-agent/internal/capture"
)

func TestParseBitrate(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"6M", "6000000", true},
		{"1200K", "1200000", true},
		{"2500000", "2500000", true},
		{"3m", "3000000", true},
		{"900k", "900000", true},
		{"50K", "300000", true},     // below the floor, clamped up
		{"999M", "12000000", true},  // above the ceiling, clamped down
		{"", "", false},             // unset
		{"abc", "", false},          // not a number
		{"-5M", "", false},          // negative
		{"6M; rm -rf /", "", false}, // never reaches ffmpeg
		{"$(whoami)", "", false},
	}
	for _, tc := range tests {
		got, ok := parseBitrate(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseBitrate(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSetQuality(t *testing.T) {
	base := capture.Config{ScreenIndex: "0", FPS: 30, Width: 1600, Bitrate: "6M"}
	newConn := func() *conn { return &conn{} }

	t.Run("applies a preset", func(t *testing.T) {
		s := &Server{Capture: base}
		c := newConn()
		if !s.setQuality(c, json.RawMessage(`{"width":960,"fps":20,"bitrate":"1200K"}`)) {
			t.Fatal("setQuality reported no change")
		}
		got := c.captureConfig(base)
		want := capture.Config{ScreenIndex: "0", FPS: 20, Width: 960, Bitrate: "1200000"}
		if got != want {
			t.Errorf("config = %+v, want %+v", got, want)
		}
	})

	t.Run("clamps out-of-range values", func(t *testing.T) {
		s := &Server{Capture: base}
		c := newConn()
		s.setQuality(c, json.RawMessage(`{"width":99999,"fps":500,"bitrate":"6M"}`))
		got := c.captureConfig(base)
		if got.Width != maxCaptureWidth || got.FPS != maxCaptureFPS {
			t.Errorf("got %dx@%dfps, want %d@%d", got.Width, got.FPS, maxCaptureWidth, maxCaptureFPS)
		}
	})

	t.Run("empty payload means the server defaults", func(t *testing.T) {
		s := &Server{Capture: base}
		c := newConn()
		// Matching the defaults is not a change, so a working stream is left alone.
		if s.setQuality(c, json.RawMessage(`{}`)) {
			t.Error("setQuality asked for a restart when nothing changed")
		}
		if got := c.captureConfig(base); got != base {
			t.Errorf("config = %+v, want the server default %+v", got, base)
		}
	})

	t.Run("switching back to auto restores the defaults", func(t *testing.T) {
		s := &Server{Capture: base}
		c := newConn()
		s.setQuality(c, json.RawMessage(`{"width":960,"fps":20,"bitrate":"1200K"}`))
		if !s.setQuality(c, json.RawMessage(`{}`)) {
			t.Fatal("returning to auto should restart the stream")
		}
		if got := c.captureConfig(base); got != base {
			t.Errorf("config = %+v, want the server default %+v", got, base)
		}
	})

	t.Run("repeating a preset does not restart", func(t *testing.T) {
		s := &Server{Capture: base}
		c := newConn()
		s.setQuality(c, json.RawMessage(`{"width":1280,"fps":30,"bitrate":"3M"}`))
		if s.setQuality(c, json.RawMessage(`{"width":1280,"fps":30,"bitrate":"3M"}`)) {
			t.Error("the same preset asked for a restart")
		}
	})

	t.Run("a bad payload is ignored", func(t *testing.T) {
		s := &Server{Capture: base}
		c := newConn()
		if s.setQuality(c, json.RawMessage(`not json`)) {
			t.Error("a malformed payload should not restart the stream")
		}
		if got := c.captureConfig(base); got != base {
			t.Errorf("config = %+v, want it untouched", got)
		}
	})
}
