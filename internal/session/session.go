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

// dropGrace is how long a "disconnected" peer is given to recover before the
// session is torn down.
const dropGrace = 20 * time.Second

// Session is one viewer connection.
type Session struct {
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample
	input *input.Controller

	fps    int
	signal SignalFn

	stopOnce sync.Once
	stop     chan struct{}

	dropMu    sync.Mutex
	dropTimer *time.Timer
}

// Done closes when the session has torn itself down. A session can die on its
// own — ICE failure, the drop timer expiring, the frame source ending — and the
// owner has to hear about it, otherwise the capture pipeline behind it keeps
// encoding for a viewer that is no longer there.
func (s *Session) Done() <-chan struct{} { return s.stop }

func (s *Session) startDropTimer() {
	s.dropMu.Lock()
	defer s.dropMu.Unlock()
	if s.dropTimer != nil {
		return
	}
	s.dropTimer = time.AfterFunc(dropGrace, func() {
		log.Printf("session: still disconnected after %s, closing", dropGrace)
		s.Close()
	})
}

func (s *Session) cancelDropTimer() {
	s.dropMu.Lock()
	defer s.dropMu.Unlock()
	if s.dropTimer != nil {
		s.dropTimer.Stop()
		s.dropTimer = nil
	}
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
	sender, err := pc.AddTrack(track)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}

	// Drain sender RTCP. Without this the NACK/TWCC interceptors never see the
	// viewer's feedback, so lost packets are never retransmitted — which on a
	// mobile link shows up as a frozen or smeared picture.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()

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

	// Send actual cursor position back to the client after each move/click. The
	// controller holds one callback for the whole process, so this one checks it
	// is still the live session rather than clearing itself on close — a session
	// that closes late would otherwise silence the one that replaced it.
	ctrl.SetCursorFn(func(x, y float64) {
		select {
		case <-s.stop:
			return
		default:
		}
		msg, _ := json.Marshal(map[string]any{"t": "cursor", "x": x, "y": y})
		_ = dc.SendText(string(msg))
	})

	// Same ownership rule as the cursor callback: answer only while this session
	// is the live one.
	ctrl.SetReplyFn(func(v any) {
		select {
		case <-s.stop:
			return
		default:
		}
		msg, err := json.Marshal(v)
		if err != nil {
			log.Printf("session: marshal reply: %v", err)
			return
		}
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
		case webrtc.PeerConnectionStateConnected:
			s.cancelDropTimer()
		case webrtc.PeerConnectionStateDisconnected:
			// A phone switching Wi-Fi↔cellular, or briefly losing signal, lands
			// here and usually recovers on its own. Tearing down immediately is
			// what makes the session die on every network blip, so hold the
			// capture pipeline open and only give up if it never comes back.
			s.startDropTimer()
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			s.Close()
		}
	})

	// The controller's dispatch goroutine is process-wide (started in main), not
	// per-session: robotgo tolerates exactly one caller, and overlapping sessions
	// during a viewer handover would otherwise drive it from two goroutines.
	ctrl.Drain()
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
				// ErrClosedPipe etc. — the connection is gone. Close rather than
				// just returning, or the session lingers with nothing pumping it
				// and the viewer stares at a frozen frame indefinitely.
				log.Printf("session: WriteSample: %v", err)
				s.Close()
				return
			}
		}
	}
}

// Close tears down the peer connection and stops goroutines (idempotent).
func (s *Session) Close() {
	s.stopOnce.Do(func() {
		s.cancelDropTimer()
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
