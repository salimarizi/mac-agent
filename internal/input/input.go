// Package input translates input events arriving over the WebRTC data channel
// into real mouse and keyboard actions on the host Mac via robotgo.
//
// Wire protocol (JSON, one object per data-channel message):
//
//	{"t":"move","dx":12,"dy":-4}              relative cursor move (trackpad mode)
//	{"t":"moveabs","x":0.51,"y":0.32}         absolute move, x/y normalized 0..1
//	{"t":"click","button":"left"}             single click ("left"|"right"|"center")
//	{"t":"dblclick","button":"left"}          double click
//	{"t":"down","button":"left"}              press-and-hold (start of a drag)
//	{"t":"up","button":"left"}                release
//	{"t":"scroll","dx":0,"dy":-3}             wheel scroll
//	{"t":"type","text":"hello world"}         type a literal string
//	{"t":"key","key":"escape","mods":["cmd"]} tap a key with optional modifiers
//	{"t":"clipset","text":"…"}                put text on the Mac's clipboard
//	{"t":"clipget"}                           ask for it back: {"t":"clip","text":"…"}
//	{"t":"release"}                           release every modifier key
//
// Mouse-move rates can be high (60/s); everything here is cheap and synchronous,
// but robotgo is not goroutine-safe, so all calls are funnelled through a single
// goroutine (see Controller.Run).
package input

/*
#cgo LDFLAGS: -framework ApplicationServices
#include <ApplicationServices/ApplicationServices.h>

static int axTrusted(void) { return AXIsProcessTrusted() ? 1 : 0; }
*/
import "C"

import (
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-vgo/robotgo"
)

// AccessibilityTrusted reports whether this process may post keyboard and mouse
// events. Without the grant robotgo fails silently — the picture keeps streaming
// and nothing moves — so the agent says so at startup rather than leaving it to
// be guessed at.
func AccessibilityTrusted() bool {
	return C.axTrusted() == 1
}

// Event is the decoded form of a single data-channel message.
type Event struct {
	T      string   `json:"t"`
	DX     float64  `json:"dx"`
	DY     float64  `json:"dy"`
	X      float64  `json:"x"`
	Y      float64  `json:"y"`
	Button string   `json:"button"`
	Text   string   `json:"text"`
	Key    string   `json:"key"`
	Mods   []string `json:"mods"`
}

// CursorFn is called after cursor-affecting events with the normalized
// position (0..1) of the cursor on the host screen.
type CursorFn func(x, y float64)

// ReplyFn ships a JSON-marshalable object back to the viewer over the same data
// channel the events arrived on.
type ReplyFn func(v any)

// Controller serializes input events onto a single OS-driving goroutine.
type Controller struct {
	events    chan Event
	screenW   int
	screenH   int
	moveScale float64 // multiplier applied to relative (trackpad) deltas

	// onCursor and onReply are swapped by each new session while Run reads them,
	// so they need a lock.
	cursorMu sync.RWMutex
	onCursor CursorFn
	onReply  ReplyFn

	running   atomic.Bool
	announced atomic.Bool
}

// New returns a Controller. moveScale tunes trackpad sensitivity (1.0 = 1:1).
func New(moveScale float64) *Controller {
	w, h := robotgo.GetScreenSize()
	// At launchd/boot time the window server may not be ready yet and reports 0,
	// which would turn every normalized coordinate into NaN.
	if w <= 0 || h <= 0 {
		log.Printf("input: screen size unavailable (%dx%d), assuming 1920x1080 until it resolves", w, h)
		w, h = 1920, 1080
	}
	if moveScale <= 0 {
		moveScale = 1.0
	}
	return &Controller{
		events:    make(chan Event, 256),
		screenW:   w,
		screenH:   h,
		moveScale: moveScale,
	}
}

// SetCursorFn registers a callback that fires after each cursor-affecting event.
// Sessions pass nil on teardown so events are not reported to a closed channel.
func (c *Controller) SetCursorFn(fn CursorFn) {
	c.cursorMu.Lock()
	c.onCursor = fn
	c.cursorMu.Unlock()
}

// Drain discards events queued by a previous session. Without it, taps made just
// before a drop are replayed against whatever is on screen after reconnecting.
func (c *Controller) Drain() {
	c.announced.Store(false)
	for {
		select {
		case <-c.events:
		default:
			// Start each session with every modifier up, in case the last one
			// left one stuck.
			select {
			case c.events <- Event{T: "release"}:
			default:
			}
			return
		}
	}
}

// announceFirst logs once per session that input is arriving. It separates "the
// phone's events never reached the Mac" from "they arrived and macOS dropped
// them on the floor", which otherwise look identical from the outside.
func (c *Controller) announceFirst(e Event) {
	if c.announced.CompareAndSwap(false, true) {
		log.Printf("input: receiving events (first was %q); accessibility trusted=%v, screen=%dx%d",
			e.T, AccessibilityTrusted(), c.screenW, c.screenH)
	}
}

// SetReplyFn registers the channel used to answer events that return data.
// Sessions pass nil on teardown.
func (c *Controller) SetReplyFn(fn ReplyFn) {
	c.cursorMu.Lock()
	c.onReply = fn
	c.cursorMu.Unlock()
}

func (c *Controller) reportCursor() {
	c.cursorMu.RLock()
	fn := c.onCursor
	c.cursorMu.RUnlock()
	if fn == nil {
		return
	}
	x, y := robotgo.Location()
	fn(float64(x)/float64(c.screenW), float64(y)/float64(c.screenH))
}

func (c *Controller) reply(v any) {
	c.cursorMu.RLock()
	fn := c.onReply
	c.cursorMu.RUnlock()
	if fn != nil {
		fn(v)
	}
}

// Handle decodes a raw data-channel payload and queues it. Decode errors are
// logged and dropped rather than killing the session.
func (c *Controller) Handle(raw []byte) {
	var e Event
	if err := json.Unmarshal(raw, &e); err != nil {
		log.Printf("input: bad event: %v", err)
		return
	}
	select {
	case c.events <- e:
	default:
		// Queue full: drop the oldest-style by skipping. Mouse moves are the
		// only thing that floods, and a dropped move is harmless.
		log.Print("input: event queue full, dropping event")
	}
}

// Run consumes events until stop is closed. robotgo is not safe to call from
// multiple goroutines, so this is the ONLY place that does — which is why it is
// started once for the process rather than once per session, and why a second
// caller is refused rather than allowed to race the first.
func (c *Controller) Run(stop <-chan struct{}) {
	if !c.running.CompareAndSwap(false, true) {
		log.Print("input: Run already active, ignoring duplicate start")
		return
	}
	defer c.running.Store(false)

	// The screen size is captured at startup but changes when a display is
	// plugged in, unplugged, or its resolution changes; stale values send every
	// absolute click to the wrong place.
	resize := time.NewTicker(5 * time.Second)
	defer resize.Stop()

	for {
		select {
		case <-stop:
			return
		case <-resize.C:
			if w, h := robotgo.GetScreenSize(); w > 0 && h > 0 && (w != c.screenW || h != c.screenH) {
				log.Printf("input: screen size changed %dx%d -> %dx%d", c.screenW, c.screenH, w, h)
				c.screenW, c.screenH = w, h
			}
		case e := <-c.events:
			c.dispatch(e)
		}
	}
}

func (c *Controller) dispatch(e Event) {
	c.announceFirst(e)

	switch e.T {
	case "move":
		x, y := robotgo.Location()
		nx := x + int(e.DX*c.moveScale)
		ny := y + int(e.DY*c.moveScale)
		nx, ny = clamp(nx, ny, c.screenW, c.screenH)
		robotgo.Move(nx, ny)
		c.reportCursor()

	case "moveabs":
		nx := int(e.X * float64(c.screenW))
		ny := int(e.Y * float64(c.screenH))
		nx, ny = clamp(nx, ny, c.screenW, c.screenH)
		robotgo.Move(nx, ny)
		c.reportCursor()

	case "click":
		click(e.Button)
		c.reportCursor()

	case "dblclick":
		doubleClick(e.Button)
		c.reportCursor()

	case "down":
		mouseButton(e.Button, true, 1)

	case "up":
		mouseButton(e.Button, false, 1)

	case "scroll":
		sx := int(e.DX)
		sy := int(e.DY)
		if sx != 0 || sy != 0 {
			scroll(sx, sy)
		}

	case "type":
		if e.Text != "" {
			typeText(e.Text)
		}

	case "key":
		if e.Key == "" {
			return
		}
		if pressKey(e.Key, e.Mods) {
			return
		}
		args := make([]any, 0, len(e.Mods))
		for _, m := range e.Mods {
			args = append(args, m)
		}
		if err := robotgo.KeyTap(e.Key, args...); err != nil {
			log.Printf("input: key %q: %v", e.Key, err)
		}
		// robotgo releases modifiers as plain key ups, which macOS can miss.
		releaseModifiers()

	case "release":
		releaseModifiers()

	case "clipset":
		// Putting the phone's text on the Mac's clipboard and letting the user
		// paste survives emoji, newlines and long passages that replaying
		// keystrokes one at a time mangles.
		if e.Text == "" {
			return
		}
		if err := robotgo.WriteAll(e.Text); err != nil {
			log.Printf("input: clipboard write: %v", err)
		}

	case "clipget":
		text, err := robotgo.ReadAll()
		if err != nil {
			log.Printf("input: clipboard read: %v", err)
			return
		}
		c.reply(map[string]any{"t": "clip", "text": text})

	default:
		log.Printf("input: unknown event type %q", e.T)
	}
}

func clamp(x, y, w, h int) (int, int) {
	if x < 0 {
		x = 0
	} else if x > w-1 {
		x = w - 1
	}
	if y < 0 {
		y = 0
	} else if y > h-1 {
		y = h - 1
	}
	return x, y
}
