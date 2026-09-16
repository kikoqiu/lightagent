//go:build windows

package cli

import (
	"testing"
	"unsafe"
)

// TestClassifyKeyRecord pins the Win32 key mapping, in particular that
// Ctrl+Enter submits while a plain Enter inserts a newline.
func TestClassifyKeyRecord(t *testing.T) {
	cases := []struct {
		name string
		rec  keyEventRecord
		want keyKind
		ok   bool
	}{
		{"enter inserts newline", keyEventRecord{VirtualKeyCode: vkReturn, UnicodeChar: '\r'}, keyNewline, true},
		{"ctrl+enter submits", keyEventRecord{VirtualKeyCode: vkReturn, UnicodeChar: '\r', ControlKeyState: leftCtrlPressed}, keySubmit, true},
		{"right ctrl+enter submits", keyEventRecord{VirtualKeyCode: vkReturn, ControlKeyState: rightCtrlPressed}, keySubmit, true},
		{"ctrl+j as vk_j submits", keyEventRecord{VirtualKeyCode: 'J', UnicodeChar: '\n', ControlKeyState: leftCtrlPressed}, keySubmit, true},
		{"bare LF submits", keyEventRecord{VirtualKeyCode: vkReturn, UnicodeChar: '\n'}, keySubmit, true},
		{"ctrl+enter without modifier info is a newline", keyEventRecord{VirtualKeyCode: vkReturn, UnicodeChar: '\r'}, keyNewline, true},
		{"backspace", keyEventRecord{VirtualKeyCode: vkBack}, keyBackspace, true},
		{"ctrl+c interrupts", keyEventRecord{VirtualKeyCode: 'C', ControlKeyState: leftCtrlPressed}, keyInterrupt, true},
		{"ctrl+d eof", keyEventRecord{VirtualKeyCode: 'D', ControlKeyState: leftCtrlPressed}, keyEOF, true},
		{"ctrl+u clears", keyEventRecord{VirtualKeyCode: 'U', ControlKeyState: leftCtrlPressed}, keyClearLine, true},
		{"arrow is ignored", keyEventRecord{VirtualKeyCode: vkUp}, keyIgnore, true},
		{"alt+letter is ignored", keyEventRecord{VirtualKeyCode: 'A', UnicodeChar: 'a', ControlKeyState: leftAltPressed}, keyIgnore, true},
		{"letter is a rune", keyEventRecord{VirtualKeyCode: 'A', UnicodeChar: 'A'}, keyRune, true},
		{"empty record is skipped", keyEventRecord{}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classifyKeyRecord(tc.rec)
			if ok != tc.ok {
				t.Fatalf("classifyKeyRecord ok = %v, want %v", ok, tc.ok)
			}
			if ok && got.kind != tc.want {
				t.Fatalf("classifyKeyRecord kind = %d, want %d", got.kind, tc.want)
			}
		})
	}

	if ev, _ := classifyKeyRecord(keyEventRecord{VirtualKeyCode: 'A', UnicodeChar: 'A'}); ev.r != 'A' {
		t.Fatalf("rune = %q, want 'A'", ev.r)
	}
}

// TestSpinnerGlyphsSupported pins the legacy-console fallback: the braille
// spinner is only used when the host renders with its own font stack (Windows
// Terminal and friends), unless an explicit override asks for it.
func TestSpinnerGlyphsSupported(t *testing.T) {
	t.Setenv("LIGHTAGENT_UNICODE_UI", "")
	for _, name := range []string{"ConEmuANSI", "TERM_PROGRAM", "WEZTERM_EXECUTABLE", "WT_SESSION"} {
		t.Setenv(name, "")
	}
	if spinnerGlyphsSupported() {
		t.Fatal("a bare console must fall back to the ASCII spinner")
	}

	t.Setenv("WT_SESSION", "1")
	if !spinnerGlyphsSupported() {
		t.Fatal("Windows Terminal can draw the braille spinner")
	}

	t.Setenv("WT_SESSION", "")
	t.Setenv("LIGHTAGENT_UNICODE_UI", "1")
	if !spinnerGlyphsSupported() {
		t.Fatal("LIGHTAGENT_UNICODE_UI should force the braille spinner")
	}
}

// TestInputRecordLayout guards the Win32 INPUT_RECORD layout the raw reader
// relies on: 2-byte event type, 2 bytes alignment padding, then
// KEY_EVENT_RECORD (16 bytes).
func TestInputRecordLayout(t *testing.T) {
	if got := unsafe.Sizeof(inputRecord{}); got != 20 {
		t.Fatalf("inputRecord size = %d, want 20", got)
	}
	if got := unsafe.Offsetof(inputRecord{}.Key); got != 4 {
		t.Fatalf("Key offset = %d, want 4", got)
	}
	if got := unsafe.Sizeof(keyEventRecord{}); got != 16 {
		t.Fatalf("keyEventRecord size = %d, want 16", got)
	}
}
