package cli

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// TestClassifyEscapeSequence pins how a byte-stream terminal reports keys the
// editor cares about, in particular that Ctrl+Enter is recognised when the
// terminal disambiguates it (kitty keyboard protocol / xterm modifyOtherKeys)
// and that Alt+Enter works through the ESC prefix. Every case also checks that
// nothing of the sequence is left in the reader: a sequence that stops half way
// used to leak its tail into the editor as literal text.
func TestClassifyEscapeSequence(t *testing.T) {
	cases := []struct {
		name string
		in   string // bytes that follow the leading ESC
		rest string // bytes that must be left in the reader
		want keyKind
	}{
		{"alt+enter sends", "\r", "Z", keySubmit},
		{"alt+ctrl+enter (LF) sends", "\n", "Z", keySubmit},
		{"arrow up is ignored", "[A", "Z", keyIgnore},
		{"ctrl+arrow up is ignored", "[1;5A", "Z", keyIgnore},
		{"bracketed paste start is ignored", "[200~", "Z", keyIgnore},
		{"function key (SS3) is ignored", "OA", "Z", keyIgnore},
		{"alt+letter is ignored", "a", "Z", keyIgnore},
		{"kitty ctrl+enter sends", "[13;5u", "Z", keySubmit},
		{"kitty alt+enter sends", "[13;3u", "Z", keySubmit},
		{"kitty ctrl+shift+enter sends", "[13;6u", "Z", keySubmit},
		{"kitty shift+enter inserts a newline", "[13;2u", "Z", keyNewline},
		{"kitty plain enter inserts a newline", "[13u", "Z", keyNewline},
		{"kitty escape is ignored", "[27u", "Z", keyIgnore},
		{"kitty ctrl+c interrupts", "[99;5u", "Z", keyInterrupt},
		{"kitty ctrl+d is eof", "[100;5u", "Z", keyEOF},
		{"kitty ctrl+z is eof", "[122;5u", "Z", keyEOF},
		{"kitty ctrl+h backspaces", "[104;5u", "Z", keyBackspace},
		{"kitty ctrl+j sends", "[106;5u", "Z", keySubmit},
		{"kitty ctrl+m keeps the enter meaning", "[109;5u", "Z", keyNewline},
		{"kitty ctrl+u clears", "[117;5u", "Z", keyClearLine},
		{"kitty alt+letter is ignored", "[97;3u", "Z", keyIgnore},
		{"modifyOtherKeys ctrl+enter sends", "[27;5;13~", "Z", keySubmit},
		{"modifyOtherKeys alt+enter sends", "[27;3;13~", "Z", keySubmit},
		{"modifyOtherKeys shift+enter inserts a newline", "[27;2;13~", "Z", keyNewline},
		{"modifyOtherKeys ctrl+c interrupts", "[27;5;99~", "Z", keyInterrupt},
		{"modifyOtherKeys ctrl+space is ignored", "[27;5;32~", "Z", keyIgnore},
		{"not a modifyOtherKeys sequence", "[27;5~", "", keyIgnore},
		{"shift+tab is ignored", "[Z", "", keyIgnore},
		{"truncated sequence is ignored", "[1;5", "", keyIgnore},
		{"unknown CSI is ignored", "[99~", "", keyIgnore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(tc.in + tc.rest))
			if got := classifyEscapeSequence(r); got.kind != tc.want {
				t.Fatalf("classifyEscapeSequence(%q).kind = %d, want %d", tc.in, got.kind, tc.want)
			}
			left, _ := io.ReadAll(r)
			if string(left) != tc.rest {
				t.Fatalf("classifyEscapeSequence(%q) left %q behind, want %q", tc.in, left, tc.rest)
			}
		})
	}
}
