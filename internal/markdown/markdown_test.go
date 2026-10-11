package markdown

import (
	"strings"
	"testing"

	"lightagent/internal/termcolor"
)

// withColor forces ANSI output on for the duration of a test.
func withColor(t *testing.T) {
	t.Helper()
	original := termcolor.Enabled()
	termcolor.SetEnabled(true)
	t.Cleanup(func() { termcolor.SetEnabled(original) })
}

func TestANSIHeadingsEmphasisAndCode(t *testing.T) {
	withColor(t)
	got := ANSI("# Title\n\ntext with **bold**, *italic* and `code`\n")
	for _, marker := range []string{"#", "**", "`"} {
		if strings.Contains(got, marker) {
			t.Fatalf("marker %q leaked into output: %q", marker, got)
		}
	}
	if !strings.Contains(got, termcolor.BoldCode+termcolor.BlueCode+"Title"+termcolor.ResetCode) {
		t.Fatalf("heading styling missing: %q", got)
	}
	if !strings.Contains(got, termcolor.BoldCode+"bold"+termcolor.ResetCode) {
		t.Fatalf("bold styling missing: %q", got)
	}
	if !strings.Contains(got, termcolor.ItalicCode+"italic"+termcolor.ResetCode) {
		t.Fatalf("italic styling missing: %q", got)
	}
	if !strings.Contains(got, termcolor.CyanCode+"code"+termcolor.ResetCode) {
		t.Fatalf("code styling missing: %q", got)
	}
}

func TestANSIFencedCodeIsLiteral(t *testing.T) {
	withColor(t)
	got := ANSI("before\n```go\n# not a heading\nfunc main() {}\n```\nafter\n")
	if strings.Contains(got, "```") {
		t.Fatalf("fence markers leaked: %q", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Fatalf("surrounding text lost: %q", got)
	}
	if !strings.Contains(got, "before\n"+termcolor.CyanCode+"# not a heading"+termcolor.ResetCode) {
		t.Fatalf("fence markers left stray lines: %q", got)
	}
}

func TestANSIHeadingInlineEmphasis(t *testing.T) {
	withColor(t)
	got := ANSI("# Title **strong**\n")
	if strings.Contains(got, "**") {
		t.Fatalf("emphasis markers leaked: %q", got)
	}
	if !strings.Contains(got, termcolor.BlueCode+"strong"+termcolor.ResetCode) {
		t.Fatalf("inline emphasis inside a heading not styled: %q", got)
	}
}

func TestANSIStreamBuffersPartialLines(t *testing.T) {
	withColor(t)
	s := NewStream()
	if got := s.Write("## Ti"); got != "" {
		t.Fatalf("partial line produced output: %q", got)
	}
	out := s.Write("tle\nsecond ")
	if !strings.Contains(out, "Title") || strings.Contains(out, "second") {
		t.Fatalf("unexpected flushed output: %q", out)
	}
	if got := s.Flush(); got != "second" {
		t.Fatalf("Flush() = %q, want the buffered tail", got)
	}
	if got := s.Flush(); got != "" {
		t.Fatalf("second Flush() = %q, want empty", got)
	}
}

// TestStreamPending exposes the not-yet-complete line so a caller can show it as
// a streaming preview.
func TestStreamPending(t *testing.T) {
	original := termcolor.Enabled()
	termcolor.SetEnabled(false)
	t.Cleanup(func() { termcolor.SetEnabled(original) })

	s := NewStream()
	if got := s.Write("Hello "); got != "" {
		t.Fatalf("Write returned %q before a newline", got)
	}
	if got := s.Pending(); got != "Hello " {
		t.Fatalf("Pending() = %q, want the partial line", got)
	}
	if got := s.Write("world\n"); got != "Hello world\n" {
		t.Fatalf("Write completed line = %q", got)
	}
	if got := s.Pending(); got != "" {
		t.Fatalf("Pending() after a newline = %q, want empty", got)
	}
	if got := s.Write("tail"); got != "" {
		t.Fatalf("Write = %q, want nothing yet", got)
	}
	if got := s.Flush(); got != "tail" {
		t.Fatalf("Flush() = %q, want the buffered tail", got)
	}
	if got := s.Pending(); got != "" {
		t.Fatalf("Pending() after Flush() = %q, want empty", got)
	}
}

// TestStreamDrawsCREndingsAsBreaks pins the CR handling: a file read verbatim can
// carry CRLF or a lone CR (a classic-Mac file), and neither may reach the screen
// as a bare CR — the terminal would move its cursor back over the row.
func TestStreamDrawsCREndingsAsBreaks(t *testing.T) {
	original := termcolor.Enabled()
	termcolor.SetEnabled(false)
	t.Cleanup(func() { termcolor.SetEnabled(original) })

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"crlf", "one\r\ntwo\r\n", "one\ntwo\n"},
		{"lone cr", "one\rtwo\r", "one\ntwo\n"},
		{"mixed", "one\ntwo\r\nthree\r", "one\ntwo\nthree\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ANSI(tc.input)
			if got != tc.want {
				t.Fatalf("ANSI(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestStreamHoldsATrailingCR pins the chunk boundary: a CR that ends a chunk may
// still turn out to be the first half of a CRLF, so it is held back instead of
// being drawn as a break of its own.
func TestStreamHoldsATrailingCR(t *testing.T) {
	original := termcolor.Enabled()
	termcolor.SetEnabled(false)
	t.Cleanup(func() { termcolor.SetEnabled(original) })

	s := NewStream()
	if got := s.Write("one\r"); got != "" {
		t.Fatalf("Write returned %q, want nothing while the CR may still be a CRLF", got)
	}
	if got := s.Pending(); got != "one" {
		t.Fatalf("Pending() = %q, want the row without its CR", got)
	}
	if got := s.Write("\ntwo\r\n"); got != "one\ntwo\n" {
		t.Fatalf("Write = %q, want both rows", got)
	}
	if got := s.Flush(); got != "" {
		t.Fatalf("Flush() = %q, want empty", got)
	}
}

func TestANSIDegradesToPlainText(t *testing.T) {
	original := termcolor.Enabled()
	termcolor.SetEnabled(false)
	t.Cleanup(func() { termcolor.SetEnabled(original) })

	got := ANSI("# Title\n\n**bold** and `code`\n")
	if strings.Contains(got, "\x1b") {
		t.Fatalf("escape codes emitted while disabled: %q", got)
	}
	if !strings.Contains(got, "Title") || !strings.Contains(got, "bold") {
		t.Fatalf("text lost: %q", got)
	}
}

func TestANSIIntrawordUnderscoresStayLiteral(t *testing.T) {
	withColor(t)
	if got, want := ANSI("call read_file_lines now\n"), "call read_file_lines now\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
