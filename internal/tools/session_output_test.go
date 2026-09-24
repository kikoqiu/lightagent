package tools

import (
	"bytes"
	"strings"
	"testing"
)

// TestSessionOutputFollowsTerminalLines covers the terminal model the buffer
// applies to child output: a CR and a backspace move the cursor without erasing,
// a character overwrites the cell it lands on, so a shorter rewrite keeps the
// tail of the longer text it replaced, an LF ends the line, and the line-local
// CSI sequences a progress bar pairs with CR are honoured. The buffer is handed
// over once, the way a call does when it asks for the window.
func TestSessionOutputFollowsTerminalLines(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string // what handing the buffer over returns
	}{
		{
			// git/curl style: every refresh repaints the whole line with a CR,
			// so the window holds that line's last state and the line printed
			// after it.
			name:   "repaints of equal length collapse",
			chunks: []string{"\rDownloading 1%", "\rDownloading 2%", "\rDownloading 99%", "\nAll done\n"},
			want:   "Downloading 99%\nAll done\n",
		},
		{
			name:   "several rewrites inside one chunk",
			chunks: []string{"1%\r2%\r3%\n"},
			want:   "3%\n",
		},
		{
			name:   "a shorter rewrite keeps the longer tail, as a terminal does",
			chunks: []string{"abcdefgh\rXY\n"},
			want:   "XYcdefgh\n",
		},
		{
			name:   "the CR plus erase-in-line idiom erases",
			chunks: []string{"abcdefgh\r\x1b[KXY\n"},
			want:   "XY\n",
		},
		{
			name:   "erase-in-line with a parameter",
			chunks: []string{"abc\x1b[2Kdef\n"},
			want:   "def\n",
		},
		{
			name:   "a cursor move leaves the cells it skipped blank",
			chunks: []string{"ab\x1b[5GZ\n"},
			want:   "ab  Z\n",
		},
		{
			name:   "backspace moves one cell left",
			chunks: []string{"ab\bX\n"},
			want:   "aX\n",
		},
		{
			name:   "a rewritten line arrives as the state it holds",
			chunks: []string{"progress 1%\rprogress 2%", "\n"},
			want:   "progress 2%\n",
		},
		{
			name:   "CRLF ends the line",
			chunks: []string{"line\r\nnext"},
			want:   "line\nnext",
		},
		{
			name:   "a CRLF split across chunks keeps its line",
			chunks: []string{"abc\r", "\n"},
			want:   "abc\n",
		},
		{
			name:   "a chunk-ending CR is a cursor move when no LF follows",
			chunks: []string{"abc\r", "def\n"},
			want:   "def\n",
		},
		{
			name:   "a CSI sequence cut in half still applies",
			chunks: []string{"abc\r\x1b[", "Kxy\n"},
			want:   "xy\n",
		},
		{
			name:   "an unfinished line that was never rewritten is visible",
			chunks: []string{"Enter a value: "},
			want:   "Enter a value: ",
		},
		{
			name:   "colors never reach a reader",
			chunks: []string{"\x1b[1;31mred\x1b[0m\n"},
			want:   "red\n",
		},
		{
			name:   "finished lines stay in order",
			chunks: []string{"one\ntwo\n", "three\n"},
			want:   "one\ntwo\nthree\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var o sessionOutput
			for _, chunk := range tc.chunks {
				o.append([]byte(chunk))
			}
			if got := o.take(); got != tc.want {
				t.Fatalf("hand-over = %q, want %q", got, tc.want)
			}
			if got := o.take(); got != "" {
				t.Fatalf("the buffer was handed over twice: %q", got)
			}
		})
	}
}

// TestSessionOutputHandsTheBufferOver covers what a hand-over does with a line
// the child repaints in place: it arrives as the state it holds then, the buffer
// is emptied, and the child's next repaint therefore forms a fresh line instead
// of rewriting bytes a caller already got. Repaints of one window still collapse
// in the buffer, so only the state the line holds when it is handed over reaches
// the caller.
func TestSessionOutputHandsTheBufferOver(t *testing.T) {
	var o sessionOutput

	o.append([]byte("\rprogress 1%"))
	o.append([]byte("\rprogress 2%"))
	if got := o.take(); got != "progress 2%" {
		t.Fatalf("hand-over = %q, want the state the line holds", got)
	}

	// The child's next repaint starts from an empty buffer.
	o.append([]byte("\rprogress 3%"))
	o.append([]byte("\rprogress 4%"))
	if got := o.take(); got != "progress 4%" {
		t.Fatalf("hand-over = %q, want the state of the new window", got)
	}

	// A finished line and the line still being written are handed over together,
	// and a line that was never rewritten is visible unfinished.
	o.append([]byte("see you\n\rprogress 5%"))
	if got := o.take(); got != "see you\nprogress 5%" {
		t.Fatalf("hand-over = %q, want both lines", got)
	}
	o.append([]byte("Enter a value: "))
	if got := o.take(); got != "Enter a value: " {
		t.Fatalf("hand-over = %q, want the unfinished prompt", got)
	}

	// A break the child writes after a hand-over ends the line the emptied buffer
	// starts with: the state handed over already covered that line, so the break
	// shows up as an empty line rather than disappearing. What the child makes of
	// the line that follows is the next call's business.
	o = sessionOutput{}
	o.append([]byte("part1"))
	if got := o.take(); got != "part1" {
		t.Fatalf("hand-over = %q, want the unfinished line", got)
	}
	o.append([]byte("\npart2\n"))
	if got := o.take(); got != "\npart2\n" {
		t.Fatalf("hand-over = %q, want the break and the next line", got)
	}
}

// TestSessionOutputDeliversOnce keeps the hand-over honest: what a caller got is
// never handed out a second time, and the buffer is empty afterwards.
func TestSessionOutputDeliversOnce(t *testing.T) {
	var o sessionOutput

	o.append([]byte("Enter a value: "))
	if got := o.take(); got != "Enter a value: " {
		t.Fatalf("first hand-over = %q, want the prompt", got)
	}
	if got := o.take(); got != "" {
		t.Fatalf("the prompt was handed over twice: %q", got)
	}

	o.append([]byte("\nnext\n"))
	if got := o.take(); got != "\nnext\n" {
		t.Fatalf("hand-over = %q, want %q", got, "\nnext\n")
	}
}

// TestSessionOutputEscapeEdges covers the escapes a stream may cut in half and
// the ones that are not CSI sequences at all.
func TestSessionOutputEscapeEdges(t *testing.T) {
	// An unfinished sequence stands for nothing: it is still parked when the
	// buffer is handed over, so no escape byte reaches a caller.
	var o sessionOutput
	o.append([]byte("ab\x1b[3"))
	if got := o.take(); got != "ab" {
		t.Fatalf("hand-over = %q, want %q", got, "ab")
	}

	// ESC outside the CSI family is ordinary output (only ESC [ is emulated), and
	// a CSI sequence this buffer does not emulate is consumed and dropped.
	o = sessionOutput{}
	o.append([]byte("a\x1b(Bb\x1b[1Ac\n"))
	if got := o.take(); got != "a\x1b(Bbc\n" {
		t.Fatalf("hand-over = %q, want %q", got, "a\x1b(Bbc\n")
	}
}

// TestSessionOutputTruncatesAtTheCap covers the 1MB ceiling of one window:
// buffered output stays, the loss is marked once and the rest of that window is
// dropped, while the hand-over that ends it lets the next window buffer again.
func TestSessionOutputTruncatesAtTheCap(t *testing.T) {
	var o sessionOutput
	chunk := bytes.Repeat([]byte("x"), 64*1024)
	// One more chunk than fills the cap: that chunk is the one dropped.
	for written := 0; written <= maxOutputBufferSize; written += len(chunk) {
		o.append(chunk)
	}
	// Everything that arrives while the window is capped is dropped too.
	o.append([]byte("dropped\nmore\n"))

	got := o.take()
	if strings.Count(got, outputTruncateMarker) != 1 {
		t.Fatalf("the cap was not marked exactly once: %q", got)
	}
	if strings.Contains(got, "dropped") {
		t.Fatalf("output past the cap reached the caller: %q", got)
	}

	// The window that follows a hand-over starts empty, so output keeps being
	// reported instead of the session staying silent for good.
	o.append([]byte("back to normal\n"))
	if got := o.take(); got != "back to normal\n" {
		t.Fatalf("output after a hand-over = %q, want it buffered again", got)
	}
}
