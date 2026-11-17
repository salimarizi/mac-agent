// Package terminal exposes the Mac's open Terminal.app and iTerm2 sessions to the
// viewer: listing them, mirroring their text, and typing commands into them.
//
// Neither app offers another process anything but Apple Events, so every call
// shells out to osascript running a JXA script. That needs the Automation grant
// (System Settings > Privacy & Security > Automation) for the agent binary.
package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// App names the terminal emulator that owns a session.
type App string

const (
	AppTerminal App = "terminal"
	AppITerm    App = "iterm"
)

// ErrNotAuthorized means macOS refused the Apple Event: the agent has not been
// granted Automation access to the terminal app.
var ErrNotAuthorized = errors.New("not authorized to control the terminal app")

// ErrNotFound means the session closed between being listed and being used.
var ErrNotFound = errors.New("terminal session not found")

// Info describes one open terminal tab or session. For Terminal and iTerm the tty
// is the identity: it is unique per tab for as long as the tab is open. Window
// groups the tabs that share a window, which the viewer shows as one group
// conversation; Group names that conversation when the window has a name of its
// own, as an editor's workspace does.
type Info struct {
	ID        string `json:"id"`
	App       App    `json:"app"`
	AppName   string `json:"appName,omitempty"`
	Window    string `json:"window"`
	Group     string `json:"group,omitempty"`
	Tab       int    `json:"tab"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
	Process   string `json:"process"`
	Busy      bool   `json:"busy"`
	Preview   string `json:"preview"`
	Active    bool   `json:"active"`

	screen string
}

const (
	scriptTimeout = 15 * time.Second
	// MaxSendBytes bounds a single command. It is typed into the tab as-is, so
	// anything larger is a paste and belongs on the clipboard instead.
	MaxSendBytes = 8 * 1024
	// historyLines caps what a read returns. Terminal.app keeps unlimited
	// scrollback by default, and a long-lived tab easily holds tens of thousands
	// of lines nobody will scroll back to from a phone.
	historyLines = 5000
)

var ttyPattern = regexp.MustCompile(`^/dev/ttys[0-9]{3,}$`)

// osascript calls are serialized: Terminal answers Apple Events one at a time
// anyway, and a burst of parallel scripts only queues up behind each other.
var scriptMu sync.Mutex

func runJXA(ctx context.Context, script string, args ...string) ([]byte, error) {
	scriptMu.Lock()
	defer scriptMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, scriptTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "osascript", append([]string{"-l", "JavaScript", "-e", script}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, scriptError(stderr.String(), err)
	}
	return out, nil
}

func scriptError(stderr string, err error) error {
	msg := strings.TrimSpace(stderr)
	if isNotAuthorized(msg) {
		return ErrNotAuthorized
	}
	if strings.Contains(msg, "MACREMOTE_NOT_FOUND") {
		return ErrNotFound
	}
	if msg == "" {
		return err
	}
	return fmt.Errorf("osascript: %s", msg)
}

func isNotAuthorized(msg string) bool {
	return strings.Contains(msg, "-1743") || strings.Contains(msg, "Not authorized to send Apple events")
}

const listScript = `
function run() {
  const out = [], errors = [];
  try {
    const t = Application('com.apple.Terminal');
    if (t.running()) {
      const w = t.windows;
      const names = w.name(), ids = w.id(), bounds = w.bounds(), ttys = w.tabs.tty(),
            titles = w.tabs.customTitle(), busy = w.tabs.busy(), contents = w.tabs.contents();
      // Scripting reports each macOS window tab as its own window. Tabs sharing a
      // tab bar share its exact frame, so the frame finds the group, and the
      // oldest window in it names the group so moving the window keeps it.
      const frameOf = i => JSON.stringify(bounds[i]);
      const groupOf = {}, rank = {};
      const order = ids.map((id, i) => i).filter(i => ids[i] !== null).sort((a, b) => ids[a] - ids[b]);
      for (const i of order) {
        const f = frameOf(i);
        if (!(f in groupOf)) { groupOf[f] = ids[i]; rank[f] = 0; }
      }
      for (const i of order) {
        const f = frameOf(i);
        for (let j = 0; j < (ttys[i] || []).length; j++) {
          out.push({app: 'terminal', tty: ttys[i][j], title: (titles[i] || [])[j] || '',
            window: names[i] || '', windowID: String(groupOf[f]), tab: rank[f]++,
            busy: !!(busy[i] || [])[j], screen: (contents[i] || [])[j] || ''});
        }
      }
    }
  } catch (e) { errors.push(String(e.message) + ' (' + e.errorNumber + ')'); }
  try {
    const it = Application('com.googlecode.iterm2');
    if (it.running()) {
      for (const w of it.windows()) {
        let name = '', id = '';
        try { name = w.name(); id = String(w.id()); } catch (e) {}
        let n = 0;
        for (const tab of w.tabs()) {
          for (const s of tab.sessions()) {
            out.push({app: 'iterm', tty: s.tty(), title: s.name(), window: name, windowID: id,
              tab: n++, busy: s.isProcessing(), screen: s.contents()});
          }
        }
      }
    }
  } catch (e) {
    // -2700: iTerm2 is not installed, which is the normal case.
    if (e.errorNumber !== -2700) errors.push(String(e.message) + ' (' + e.errorNumber + ')');
  }
  return JSON.stringify({sessions: out, errors: errors});
}`

const readScript = `
function run(argv) {
  const [app, tty, limit] = argv;
  if (app === 'terminal') {
    const t = Application('com.apple.Terminal');
    const ttys = t.windows.tabs.tty();
    for (let i = 0; i < ttys.length; i++) {
      for (let j = 0; j < ttys[i].length; j++) {
        if (ttys[i][j] !== tty) continue;
        const tab = t.windows[i].tabs[j];
        if (tab.tty() !== tty) continue;
        return tab.history().replace(/\s+$/, '').split('\n').slice(-Number(limit)).join('\n');
      }
    }
  } else {
    const it = Application('com.googlecode.iterm2');
    for (const w of it.windows()) for (const tab of w.tabs()) for (const s of tab.sessions()) {
      if (s.tty() === tty) return s.contents();
    }
  }
  throw new Error('MACREMOTE_NOT_FOUND');
}`

const sendScript = `
function run(argv) {
  const [app, tty, text] = argv;
  if (app === 'terminal') {
    const t = Application('com.apple.Terminal');
    const ttys = t.windows.tabs.tty();
    for (let i = 0; i < ttys.length; i++) {
      for (let j = 0; j < ttys[i].length; j++) {
        if (ttys[i][j] !== tty) continue;
        const tab = t.windows[i].tabs[j];
        if (tab.tty() !== tty) continue;
        t.doScript(text, {in: tab});
        return 'ok';
      }
    }
  } else {
    const it = Application('com.googlecode.iterm2');
    for (const w of it.windows()) for (const tab of w.tabs()) for (const s of tab.sessions()) {
      if (s.tty() === tty) { s.write({text: text}); return 'ok'; }
    }
  }
  throw new Error('MACREMOTE_NOT_FOUND');
}`

type rawSession struct {
	App      App    `json:"app"`
	TTY      string `json:"tty"`
	Title    string `json:"title"`
	Window   string `json:"window"`
	WindowID string `json:"windowID"`
	Tab      int    `json:"tab"`
	Busy     bool   `json:"busy"`
	Screen   string `json:"screen"`
}

// List returns every open session in Terminal.app and iTerm2, then the terminals
// of editors running the Mac Remote extension. Apps that are not running are
// skipped without being launched.
func List(ctx context.Context) ([]Info, error) {
	out, err := runJXA(ctx, listScript)
	if err != nil {
		return nil, err
	}
	var res struct {
		Sessions []rawSession `json:"sessions"`
		Errors   []string     `json:"errors"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("decode session list: %w", err)
	}
	for _, e := range res.Errors {
		if isNotAuthorized(e) {
			return nil, ErrNotAuthorized
		}
	}
	if len(res.Sessions) == 0 && len(res.Errors) > 0 {
		return nil, fmt.Errorf("list terminals: %s", strings.Join(res.Errors, "; "))
	}

	ps := processTable(ctx)
	foreground := parseForeground(ps)
	infos := make([]Info, 0, len(res.Sessions))
	for _, s := range res.Sessions {
		if !ttyPattern.MatchString(s.TTY) {
			continue
		}
		screen := splitLines(s.Screen)
		infos = append(infos, Info{
			ID:        s.TTY,
			App:       s.App,
			Window:    string(s.App) + ":" + s.WindowID,
			Tab:       s.Tab,
			Title:     sessionTitle(s),
			Directory: windowDirectory(s.Window),
			Process:   foreground[strings.TrimPrefix(s.TTY, "/dev/")],
			Busy:      s.Busy,
			Preview:   previewLine(screen),
			screen:    s.Screen,
		})
	}
	return append(infos, listEditors(ctx, foreground, parseTTYByPID(ps))...), nil
}

// Read returns the session's text as lines: the scrollback for Terminal.app, the
// visible screen for iTerm2 (which does not expose its scrollback), and for an
// editor terminal its mirrored scrollback with SGR colour codes left in.
func Read(ctx context.Context, app App, tty string) ([]string, error) {
	if app == AppEditor {
		return readEditor(ctx, tty)
	}
	if !ttyPattern.MatchString(tty) {
		return nil, ErrNotFound
	}
	out, err := runJXA(ctx, readScript, string(app), tty, strconv.Itoa(historyLines))
	if err != nil {
		return nil, err
	}
	return splitLines(string(out)), nil
}

// Send types text into the session followed by Return, exactly as if it had been
// typed at the Mac, so it shows up in the terminal window immediately.
func Send(ctx context.Context, app App, tty, text string) error {
	if app == AppEditor {
		return sendEditor(ctx, tty, text)
	}
	if !ttyPattern.MatchString(tty) {
		return ErrNotFound
	}
	_, err := runJXA(ctx, sendScript, string(app), tty, text)
	return err
}

// Interrupt delivers SIGINT to the tty's foreground process group — what Ctrl-C
// does — which neither app's scripting interface can express. An editor terminal
// is sent a literal Ctrl-C through the extension instead.
func Interrupt(ctx context.Context, app App, tty string) error {
	if app == AppEditor {
		return interruptEditor(ctx, tty)
	}
	if !ttyPattern.MatchString(tty) {
		return ErrNotFound
	}
	out, err := exec.CommandContext(ctx, "ps", "-t", strings.TrimPrefix(tty, "/dev/"), "-o", "tpgid=").Output()
	if err != nil {
		return ErrNotFound
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return ErrNotFound
	}
	pgid, err := strconv.Atoi(fields[0])
	if err != nil || pgid <= 1 {
		return ErrNotFound
	}
	return syscall.Kill(-pgid, syscall.SIGINT)
}

// processTable is one ps snapshot: tty, pid, process group, the tty's foreground
// group, and command, for every process.
func processTable(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "ps", "-axo", "tty=,pid=,pgid=,tpgid=,comm=").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// parseTTYByPID maps a process id to its "ttysNNN", which is how an editor
// terminal's shell pid finds the program in front of it.
func parseTTYByPID(psOutput string) map[int]string {
	ttys := map[int]string{}
	for line := range strings.SplitSeq(psOutput, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.HasPrefix(f[0], "ttys") {
			continue
		}
		if pid, err := strconv.Atoi(f[1]); err == nil {
			ttys[pid] = f[0]
		}
	}
	return ttys
}

// parseForeground maps "ttysNNN" to the name of the process leading that tty's
// foreground process group: the program the user is actually talking to.
func parseForeground(psOutput string) map[string]string {
	names := map[string]string{}
	for line := range strings.SplitSeq(psOutput, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || !strings.HasPrefix(f[0], "ttys") {
			continue
		}
		pid, pgid, tpgid := f[1], f[2], f[3]
		if pid != pgid || pgid != tpgid {
			continue
		}
		names[f[0]] = strings.TrimPrefix(filepath.Base(strings.Join(f[4:], " ")), "-")
	}
	return names
}

func sessionTitle(s rawSession) string {
	title := strings.TrimSpace(s.Title)
	if title == "" || title == "Terminal" {
		return windowDirectory(s.Window)
	}
	return title
}

// windowDirectory pulls the working directory out of a Terminal window name, which
// by default reads "<dir> — <title> — <process> — 80×24".
func windowDirectory(name string) string {
	first, _, _ := strings.Cut(name, " — ")
	return strings.TrimSpace(first)
}

func splitLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// previewLine picks the line a person would call "the latest message". Full-screen
// tools such as Claude Code pin an input box and status lines to the bottom, so
// the boxed-off tail below the lowest horizontal rule is skipped.
func previewLine(lines []string) string {
	content := lines
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-12; i-- {
		if isRule(lines[i]) {
			content = lines[:i]
		}
	}
	for i := len(content) - 1; i >= 0; i-- {
		line := strings.TrimSpace(content[i])
		if hasWordCharacter(line) && !isRule(line) {
			if r := []rune(line); len(r) > 140 {
				line = string(r[:140])
			}
			return line
		}
	}
	return ""
}

func isRule(line string) bool {
	line = strings.TrimSpace(line)
	return strings.Count(line, "─")+strings.Count(line, "━")+strings.Count(line, "═") >= 10
}

func hasWordCharacter(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}
