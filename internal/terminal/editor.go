package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

// AppEditor is a terminal inside a VS Code-family editor (VS Code, Cursor,
// Antigravity). Those terminals are only readable from inside the editor, so the
// Mac Remote Terminals extension (editor-extension/) mirrors them and serves the
// agent over a token-protected port on 127.0.0.1, one per editor window.
const AppEditor App = "editor"

var editorIDPattern = regexp.MustCompile(`^editor:([0-9]+):([0-9]+)$`)

var editorClient = &http.Client{Timeout: 4 * time.Second}

// editorWindow is one editor window's discovery file, written by the extension.
type editorWindow struct {
	PID        int    `json:"pid"`
	Port       int    `json:"port"`
	Token      string `json:"token"`
	App        string `json:"app"`
	Window     string `json:"window"`
	FullOutput bool   `json:"fullOutput"`
}

type editorTerminal struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	PID     int      `json:"pid"`
	CWD     string   `json:"cwd"`
	Running bool     `json:"running"`
	Version int      `json:"version"`
	Tail    []string `json:"tail"`
}

func editorDiscoveryDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "MacRemote", "editors")
}

// editorWindows reads the discovery files of running editor windows. A file left
// behind by an editor that crashed names a dead process and is removed.
func editorWindows() []editorWindow {
	dir := editorDiscoveryDir()
	if dir == "" {
		return nil
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var windows []editorWindow
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var w editorWindow
		if err := json.Unmarshal(raw, &w); err != nil || w.PID <= 0 || w.Port <= 0 || w.Token == "" {
			continue
		}
		if err := syscall.Kill(w.PID, 0); errors.Is(err, syscall.ESRCH) {
			_ = os.Remove(p)
			continue
		}
		windows = append(windows, w)
	}
	return windows
}

func editorWindowByPID(pid int) (editorWindow, bool) {
	for _, w := range editorWindows() {
		if w.PID == pid {
			return w, true
		}
	}
	return editorWindow{}, false
}

func (w editorWindow) call(ctx context.Context, method, path string, body, out any) error {
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", w.Port, path), reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+w.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := editorClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: editor answered %s", w.App, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// listEditors returns the terminals of every editor window running the extension.
// An editor that does not answer is skipped rather than failing the whole list.
func listEditors(ctx context.Context, foreground map[string]string, ttyOf map[int]string) []Info {
	var infos []Info
	for _, w := range editorWindows() {
		var res struct {
			App       string           `json:"app"`
			Window    string           `json:"window"`
			Terminals []editorTerminal `json:"terminals"`
		}
		if err := w.call(ctx, http.MethodGet, "/terminals", nil, &res); err != nil {
			log.Printf("terminal: %s window %d: %v", w.App, w.PID, err)
			continue
		}
		group := res.Window
		if group == "" {
			group = res.App
		}
		for i, t := range res.Terminals {
			dir := filepath.Base(t.CWD)
			if t.CWD == "" {
				dir = res.Window
			}
			infos = append(infos, Info{
				ID:        "editor:" + strconv.Itoa(w.PID) + ":" + t.ID,
				App:       AppEditor,
				AppName:   res.App,
				Window:    "editor:" + strconv.Itoa(w.PID),
				Group:     group,
				Tab:       i,
				Title:     t.Name,
				Directory: dir,
				Process:   foreground[ttyOf[t.PID]],
				Busy:      t.Running,
				Preview:   previewLine(t.Tail),
				screen:    strconv.Itoa(t.Version),
			})
		}
	}
	return infos
}

func parseEditorID(id string) (pid int, terminalID string, ok bool) {
	m := editorIDPattern.FindStringSubmatch(id)
	if m == nil {
		return 0, "", false
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, "", false
	}
	return pid, m[2], true
}

func editorFor(id string) (editorWindow, string, error) {
	pid, tid, ok := parseEditorID(id)
	if !ok {
		return editorWindow{}, "", ErrNotFound
	}
	w, ok := editorWindowByPID(pid)
	if !ok {
		return editorWindow{}, "", ErrNotFound
	}
	return w, tid, nil
}

func readEditor(ctx context.Context, id string) ([]string, error) {
	w, tid, err := editorFor(id)
	if err != nil {
		return nil, err
	}
	var res struct {
		Lines []string `json:"lines"`
	}
	if err := w.call(ctx, http.MethodGet, "/terminals/"+tid, nil, &res); err != nil {
		return nil, err
	}
	return res.Lines, nil
}

func sendEditor(ctx context.Context, id, text string) error {
	w, tid, err := editorFor(id)
	if err != nil {
		return err
	}
	return w.call(ctx, http.MethodPost, "/terminals/"+tid+"/send", map[string]string{"text": text}, nil)
}

func interruptEditor(ctx context.Context, id string) error {
	w, tid, err := editorFor(id)
	if err != nil {
		return err
	}
	return w.call(ctx, http.MethodPost, "/terminals/"+tid+"/interrupt", nil, nil)
}
