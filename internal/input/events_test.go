package input

import "testing"

func TestKeyCode(t *testing.T) {
	tests := []struct {
		key   string
		code  int
		shift bool
	}{
		{"enter", 36, false},
		{"Escape", 53, false},
		{"pagedown", 121, false},
		{"a", 0, false},
		{"w", 13, false},
		{"4", 21, false},
		{"V", 9, true},
	}
	for _, tt := range tests {
		code, shift, ok := keyCode(tt.key)
		if !ok || int(code) != tt.code || shift != tt.shift {
			t.Errorf("keyCode(%q) = %d shift=%v ok=%v, want %d shift=%v", tt.key, code, shift, ok, tt.code, tt.shift)
		}
	}
	for _, bad := range []string{"", "nosuchkey", "ab", "😀"} {
		if _, _, ok := keyCode(bad); ok {
			t.Errorf("keyCode(%q) accepted", bad)
		}
	}
}
