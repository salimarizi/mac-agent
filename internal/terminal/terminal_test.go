package terminal

import (
	"reflect"
	"testing"
)

func TestDiff(t *testing.T) {
	tests := []struct {
		name      string
		prev, cur []string
		first     bool
		want      update
		changed   bool
	}{
		{
			name:    "first read is a snapshot",
			cur:     []string{"a", "b"},
			first:   true,
			want:    update{From: 0, Lines: []string{"a", "b"}, Reset: true, Initial: true},
			changed: true,
		},
		{
			name:    "unchanged",
			prev:    []string{"a", "b"},
			cur:     []string{"a", "b"},
			changed: false,
		},
		{
			name:    "prompt line completed by typing",
			prev:    []string{"out", "% ec"},
			cur:     []string{"out", "% echo hi", "hi", "% "},
			want:    update{From: 1, Lines: []string{"% echo hi", "hi", "% "}},
			changed: true,
		},
		{
			name:    "full-screen tool redraws its bottom lines",
			prev:    []string{"a", "b", "──────────", "> ", "status"},
			cur:     []string{"a", "b", "c", "──────────", "> ", "status"},
			want:    update{From: 2, Lines: []string{"c", "──────────", "> ", "status"}},
			changed: true,
		},
		{
			name:    "cleared screen resets",
			prev:    []string{"a", "b"},
			cur:     []string{"% "},
			want:    update{From: 0, Lines: []string{"% "}, Reset: true},
			changed: true,
		},
		{
			name:    "lines removed from the end",
			prev:    []string{"a", "b", "c"},
			cur:     []string{"a"},
			want:    update{From: 1, Lines: []string{}},
			changed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := diff(tt.prev, tt.cur, tt.first)
			if changed != tt.changed {
				t.Fatalf("changed = %v, want %v", changed, tt.changed)
			}
			if changed && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("diff = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDiffSkipsOversizedBurst(t *testing.T) {
	cur := make([]string, maxBurst+10)
	for i := range cur {
		cur[i] = "line"
	}
	got, _ := diff([]string{"line"}, cur, false)
	if !got.Reset || len(got.Lines) != snapshotLines || got.From != len(cur)-snapshotLines {
		t.Fatalf("burst = from %d, %d lines, reset %v", got.From, len(got.Lines), got.Reset)
	}
}

func TestPreviewLineSkipsBoxedFooter(t *testing.T) {
	screen := []string{
		"⏺ Deployed to staging.",
		"",
		"────────────────────────────",
		"> ",
		"────────────────────────────",
		"  ⏵⏵ bypass permissions on",
	}
	if got := previewLine(screen); got != "⏺ Deployed to staging." {
		t.Fatalf("previewLine = %q", got)
	}
	if got := previewLine([]string{"% ls", "a.txt", "% "}); got != "a.txt" {
		t.Fatalf("previewLine = %q", got)
	}
}

func TestParseForeground(t *testing.T) {
	ps := `ttys000    824   824  1242 login
ttys000   1242  1242  1242 caffeinate
ttys013   6099  6099  6099 -zsh
ttys015   2472  2472  2472 claude
ttys015   8711  2472  2472 caffeinate
??         100   100     0 launchd`
	got := parseForeground(ps)
	want := map[string]string{"ttys000": "caffeinate", "ttys013": "zsh", "ttys015": "claude"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseForeground = %v, want %v", got, want)
	}
}

func TestWindowDirectory(t *testing.T) {
	if got := windowDirectory("Evergroot — ✳ briefer — claude — 99×24"); got != "Evergroot" {
		t.Fatalf("windowDirectory = %q", got)
	}
}
