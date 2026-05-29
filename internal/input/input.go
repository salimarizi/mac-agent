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
//
// Mouse-move rates can be high (60/s); everything here is cheap and synchronous,
// but robotgo is not goroutine-safe, so all calls are funnelled through a single
// goroutine (see Controller.Run).
package input

import (
	"encoding/json"
	"log"

	"github.com/go-vgo/robotgo"
)

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

// Controller serializes input events onto a single OS-driving goroutine.
type Controller struct {
	events     chan Event
	screenW    int
	screenH    int
	moveScale  float64 // multiplier applied to relative (trackpad) deltas
	onCursor   CursorFn
}

// New returns a Controller. moveScale tunes trackpad sensitivity (1.0 = 1:1).
func New(moveScale float64) *Controller {
	w, h := robotgo.GetScreenSize()
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
func (c *Controller) SetCursorFn(fn CursorFn) {
	c.onCursor = fn
}

func (c *Controller) reportCursor() {
	if c.onCursor == nil {
		return
	}
	x, y := robotgo.GetMousePos()
	c.onCursor(float64(x)/float64(c.screenW), float64(y)/float64(c.screenH))
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

// Run consumes events until ctx-like stop signal (channel close). robotgo is not
// safe to call from multiple goroutines, so this is the ONLY place that does.
func (c *Controller) Run(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case e := <-c.events:
			c.dispatch(e)
		}
	}
}

func (c *Controller) dispatch(e Event) {
	switch e.T {
	case "move":
		x, y := robotgo.GetMousePos()
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
		robotgo.Click(button(e.Button), false)
		c.reportCursor()

	case "dblclick":
		robotgo.Click(button(e.Button), true)
		c.reportCursor()

	case "down":
		robotgo.Toggle(button(e.Button), "down")

	case "up":
		robotgo.Toggle(button(e.Button), "up")

	case "scroll":
		// robotgo.Scroll(x, y) takes integer "click" counts.
		sx := int(e.DX)
		sy := int(e.DY)
		if sx != 0 || sy != 0 {
			robotgo.Scroll(sx, sy)
		}

	case "type":
		if e.Text != "" {
			robotgo.TypeStr(e.Text)
		}

	case "key":
		if e.Key == "" {
			return
		}
		// robotgo.KeyTap(key, modifiers...) — modifiers are variadic strings.
		args := make([]interface{}, 0, len(e.Mods))
		for _, m := range e.Mods {
			args = append(args, m)
		}
		robotgo.KeyTap(e.Key, args...)

	default:
		log.Printf("input: unknown event type %q", e.T)
	}
}

func button(b string) string {
	switch b {
	case "right":
		return "right"
	case "center", "middle":
		return "center"
	default:
		return "left"
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
