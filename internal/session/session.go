// Package session owns a single WebRTC peer connection: it sends the screen as
// an H.264 video track and receives input over a data channel.
//
// The agent is the OFFERER. Rationale: the agent is the only party with media to
// publish, so it builds the PeerConnection with a sendonly video track plus the
// "input" data channel, emits the offer, and waits for the viewer's answer. This
// keeps the browser client almost stateless — it just answers and renders.
package session

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/salimarizi/mac-agent/internal/input"
)

// SignalFn ships an outbound signaling message (JSON-marshalable) to the viewer.
type SignalFn func(kind string, payload any)

// Session is one viewer connection.
type Session struct {
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample
	input *input.Controller

	fps    int
	signal SignalFn

	stopOnce sync.Once
	stop     chan struct{}
}

// New builds the peer connection, attaches the video track + input data channel,
// wires ICE/state callbacks, and starts the input dispatch goroutine.
func New(iceServers []webrtc.ICEServer, frames <-chan []byte, fps int, ctrl *input.Controller, signal SignalFn) (*Session, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: iceServers})
	if err != nil {
		return nil, err
	}

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "screen",
	)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	if _, err = pc.AddTrack(track); err != nil {
		_ = pc.Close()
		return nil, err
	}

	s := &Session{
		pc:     pc,
		track:  track,
		input:  ctrl,
		fps:    fps,
		signal: signal,
		stop:   make(chan struct{}),
	}

	// Input arrives on a data channel the agent creates.
	dc, err := pc.CreateDataChannel("input", nil)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		s.input.Handle(msg.Data)
	})

	// Send actual cursor position back to the client after each move/click.
	ctrl.SetCursorFn(func(x, y float64) {
		msg, _ := json.Marshal(map[string]any{"t": "cursor", "x": x, "y": y})
		_ = dc.SendText(string(msg))
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return // gathering finished
		}
		s.signal("candidate", c.ToJSON())
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("session: connection state -> %s", state)
		switch state {
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			s.Close()
		}
	})

	go s.input.Run(s.stop)
	go s.pump(frames)

	return s, nil
}

// Offer creates the SDP offer, sets it locally, and ships it to the viewer.
func (s *Session) Offer() error {
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err = s.pc.SetLocalDescription(offer); err != nil {
		return err
	}
	s.signal("offer", offer)
	return nil
}

// SetAnswer applies the viewer's SDP answer.
func (s *Session) SetAnswer(sdp webrtc.SessionDescription) error {
	return s.pc.SetRemoteDescription(sdp)
}

// AddCandidate adds a remote ICE candidate (trickle ICE).
func (s *Session) AddCandidate(c webrtc.ICECandidateInit) error {
	return s.pc.AddICECandidate(c)
}

// pump reads assembled access units and writes them as samples on the track.
func (s *Session) pump(frames <-chan []byte) {
	dur := time.Second / time.Duration(s.fps)
	for {
		select {
		case <-s.stop:
			return
		case frame, ok := <-frames:
			if !ok {
				s.Close()
				return
			}
			if err := s.track.WriteSample(media.Sample{Data: frame, Duration: dur}); err != nil {
				// ErrClosedPipe etc. — connection is gone.
				log.Printf("session: WriteSample: %v", err)
				return
			}
		}
	}
}

// Close tears down the peer connection and stops goroutines (idempotent).
func (s *Session) Close() {
	s.stopOnce.Do(func() {
		close(s.stop)
		_ = s.pc.Close()
		log.Print("session: closed")
	})
}

// Helpers for the signaling layer to (de)serialize candidates/answers.

func DecodeAnswer(raw json.RawMessage) (webrtc.SessionDescription, error) {
	var sd webrtc.SessionDescription
	err := json.Unmarshal(raw, &sd)
	return sd, err
}

func DecodeCandidate(raw json.RawMessage) (webrtc.ICECandidateInit, error) {
	var c webrtc.ICECandidateInit
	err := json.Unmarshal(raw, &c)
	return c, err
}
