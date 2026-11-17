package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"log"
	"strings"
	"sync"
	"time"
)

// SignalFn ships a message to the viewer over the signaling socket.
type SignalFn func(kind string, payload any)

// Bridge serves one viewer's terminal requests. Requests run one at a time in
// arrival order, so two commands sent back to back reach the tab in that order;
// the watched session is polled on its own goroutine alongside.
type Bridge struct {
	signal SignalFn
	jobs   chan func()

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	known   map[string]App    // sessions from the last listing, by id
	screens map[string]uint64 // screen hashes from the last listing, for activity
	watches map[string]*watcher
}

// Wire protocol, over the signaling socket after auth:
//
//	→ term_list                               ← term_list {terminals, error?}
//	→ term_watch {ids}                        ← term_output {id, from, lines, reset, initial}
//	→ term_unwatch
//	→ term_send {id, text, nonce}             ← term_ack {id, nonce, action, ok, error?}
//	→ term_interrupt {id, nonce}              ← term_ack
//
// term_output describes the session's lines relative to what the viewer already
// holds: drop everything from index "from" on, then append "lines". With reset
// set, the viewer discards what it has and starts over at "from".
type request struct {
	ID    string   `json:"id"`
	IDs   []string `json:"ids"`
	Text  string   `json:"text"`
	Nonce string   `json:"nonce"`
}

// maxWatched bounds one group conversation. Each watched tab is a script run
// every poll, and they queue behind one another.
const maxWatched = 8

const (
	pollActive = 700 * time.Millisecond
	pollIdle   = 1500 * time.Millisecond
	// idlePolls is how many unchanged reads it takes to slow down.
	idlePolls = 6
	// snapshotLines is how much history a viewer gets when it starts watching.
	snapshotLines = 400
	// maxBurst bounds one update. Output faster than this between two polls is
	// skipped down to its tail rather than shipped whole to a phone.
	maxBurst = 1500
)

// NewBridge starts the request worker. Close stops it.
func NewBridge(signal SignalFn) *Bridge {
	ctx, cancel := context.WithCancel(context.Background())
	b := &Bridge{
		signal:  signal,
		jobs:    make(chan func(), 32),
		ctx:     ctx,
		cancel:  cancel,
		known:   map[string]App{},
		screens: map[string]uint64{},
		watches: map[string]*watcher{},
	}
	go b.work()
	return b
}

func (b *Bridge) work() {
	for {
		select {
		case <-b.ctx.Done():
			return
		case job := <-b.jobs:
			job()
		}
	}
}

// Close stops the worker and any watch.
func (b *Bridge) Close() {
	b.cancel()
	b.setWatches(nil)
}

// Handles reports whether kind is a terminal message.
func Handles(kind string) bool {
	return strings.HasPrefix(kind, "term_")
}

// Handle queues a terminal message from the viewer.
func (b *Bridge) Handle(kind string, raw json.RawMessage) {
	var req request
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			log.Printf("terminal: bad %s payload: %v", kind, err)
			return
		}
	}

	var job func()
	switch kind {
	case "term_list":
		job = b.list
	case "term_watch":
		ids := req.IDs
		if len(ids) == 0 && req.ID != "" {
			ids = []string{req.ID}
		}
		job = func() { b.watchOnly(ids) }
	case "term_unwatch":
		b.setWatches(nil)
		return
	case "term_send":
		job = func() { b.send(req) }
	case "term_interrupt":
		job = func() { b.interrupt(req) }
	default:
		log.Printf("terminal: unknown message type %q", kind)
		return
	}

	select {
	case b.jobs <- job:
	default:
		log.Printf("terminal: request queue full, dropping %s", kind)
		if kind == "term_send" || kind == "term_interrupt" {
			b.ack(req, kind, errors.New("the Mac is busy, try again"))
		}
	}
}

func (b *Bridge) list() {
	infos, err := List(b.ctx)
	if b.ctx.Err() != nil {
		return // the viewer left mid-request
	}
	if err != nil {
		log.Printf("terminal: list: %v", err)
		b.signal("term_list", map[string]any{"terminals": []Info{}, "error": errorCode(err), "message": err.Error()})
		return
	}

	b.mu.Lock()
	known := make(map[string]App, len(infos))
	screens := make(map[string]uint64, len(infos))
	for i := range infos {
		id := infos[i].ID
		h := hashString(infos[i].screen)
		prev, seen := b.screens[id]
		infos[i].Active = seen && prev != h
		known[id] = infos[i].App
		screens[id] = h
	}
	b.known, b.screens = known, screens
	b.mu.Unlock()

	b.signal("term_list", map[string]any{"terminals": infos})
}

// lookup resolves an id to its app, refreshing the listing once if the id is not
// in it — the viewer may be acting on a tab opened since it last listed.
func (b *Bridge) lookup(id string) (App, bool) {
	b.mu.Lock()
	app, ok := b.known[id]
	b.mu.Unlock()
	if ok {
		return app, true
	}
	infos, err := List(b.ctx)
	if err != nil {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, info := range infos {
		b.known[info.ID] = info.App
	}
	app, ok = b.known[id]
	return app, ok
}

func (b *Bridge) send(req request) {
	text := strings.TrimRight(req.Text, "\r\n")
	switch {
	case len(text) > MaxSendBytes:
		b.ack(req, "send", errors.New("command is too long"))
		return
	case strings.ContainsRune(text, 0):
		b.ack(req, "send", errors.New("command contains a NUL byte"))
		return
	}
	app, ok := b.lookup(req.ID)
	if !ok {
		b.ack(req, "send", ErrNotFound)
		return
	}
	err := Send(b.ctx, app, req.ID, text)
	if b.ctx.Err() != nil {
		return
	}
	if err != nil {
		log.Printf("terminal: send to %s: %v", req.ID, err)
	}
	b.ack(req, "send", err)
	b.pokeWatch(req.ID)
}

func (b *Bridge) interrupt(req request) {
	if _, ok := b.lookup(req.ID); !ok {
		b.ack(req, "interrupt", ErrNotFound)
		return
	}
	err := Interrupt(b.ctx, req.ID)
	if err != nil {
		log.Printf("terminal: interrupt %s: %v", req.ID, err)
	}
	b.ack(req, "interrupt", err)
	b.pokeWatch(req.ID)
}

func (b *Bridge) ack(req request, action string, err error) {
	payload := map[string]any{"id": req.ID, "nonce": req.Nonce, "action": action, "ok": err == nil}
	if err != nil {
		payload["error"] = errorCode(err)
		payload["message"] = err.Error()
	}
	b.signal("term_ack", payload)
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, ErrNotAuthorized):
		return "not_authorized"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	default:
		return "failed"
	}
}

// --- Watching one session ---

type watcher struct {
	id     string
	app    App
	cancel context.CancelFunc
	poke   chan struct{}
}

// watchOnly makes ids the watched set. Tabs already being watched keep their
// watcher, so a group gaining a member does not resend everyone's history.
func (b *Bridge) watchOnly(ids []string) {
	if len(ids) > maxWatched {
		ids = ids[:maxWatched]
	}
	want := map[string]App{}
	for _, id := range ids {
		app, ok := b.lookup(id)
		if !ok {
			b.signal("term_error", map[string]string{"id": id, "error": "not_found", "message": ErrNotFound.Error()})
			continue
		}
		want[id] = app
	}
	b.setWatches(want)
}

func (b *Bridge) setWatches(want map[string]App) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, w := range b.watches {
		if _, keep := want[id]; !keep {
			w.cancel()
			delete(b.watches, id)
		}
	}
	for id, app := range want {
		if _, running := b.watches[id]; running {
			continue
		}
		ctx, cancel := context.WithCancel(b.ctx)
		w := &watcher{id: id, app: app, cancel: cancel, poke: make(chan struct{}, 1)}
		b.watches[id] = w
		go b.runWatch(ctx, w)
	}
}

// pokeWatch makes the watcher read now instead of at its next tick, so the result
// of a command shows up on the phone as soon as the terminal has it.
func (b *Bridge) pokeWatch(id string) {
	b.mu.Lock()
	w := b.watches[id]
	b.mu.Unlock()
	if w == nil {
		return
	}
	select {
	case w.poke <- struct{}{}:
	default:
	}
}

func (b *Bridge) runWatch(ctx context.Context, w *watcher) {
	defer func() {
		b.mu.Lock()
		if b.watches[w.id] == w {
			delete(b.watches, w.id)
		}
		b.mu.Unlock()
	}()
	var prev []string
	first := true
	unchanged := 0
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.poke:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			// Give the shell a moment to echo before reading.
			timer.Reset(150 * time.Millisecond)
			unchanged = 0
			continue
		case <-timer.C:
		}

		cur, err := Read(ctx, w.app, w.id)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("terminal: read %s: %v", w.id, err)
			b.signal("term_error", map[string]string{"id": w.id, "error": errorCode(err), "message": err.Error()})
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotAuthorized) {
				return
			}
			timer.Reset(pollIdle)
			continue
		}

		if u, ok := diff(prev, cur, first); ok {
			u.ID = w.id
			b.signal("term_output", u)
			unchanged = 0
		} else {
			unchanged++
		}
		prev, first = cur, false

		if unchanged >= idlePolls {
			timer.Reset(pollIdle)
		} else {
			timer.Reset(pollActive)
		}
	}
}

// update is one term_output message.
type update struct {
	ID      string   `json:"id"`
	From    int      `json:"from"`
	Lines   []string `json:"lines"`
	Reset   bool     `json:"reset"`
	Initial bool     `json:"initial"`
}

// diff describes cur relative to prev. Comparing from the top rather than the
// bottom is what copes with full-screen tools: they rewrite their last few lines
// in place, which shows up as a small truncate-and-append instead of a redraw.
func diff(prev, cur []string, first bool) (update, bool) {
	if first {
		from := max(0, len(cur)-snapshotLines)
		return update{From: from, Lines: cur[from:], Reset: true, Initial: true}, true
	}
	common := 0
	for common < len(prev) && common < len(cur) && prev[common] == cur[common] {
		common++
	}
	if common == len(prev) && common == len(cur) {
		return update{}, false
	}
	// The history shrank to nothing in common: the user cleared the screen.
	if common == 0 && len(prev) > 0 {
		from := max(0, len(cur)-snapshotLines)
		return update{From: from, Lines: cur[from:], Reset: true}, true
	}
	if len(cur)-common > maxBurst {
		from := len(cur) - snapshotLines
		return update{From: from, Lines: cur[from:], Reset: true}, true
	}
	return update{From: common, Lines: cur[common:]}, true
}

func hashString(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
