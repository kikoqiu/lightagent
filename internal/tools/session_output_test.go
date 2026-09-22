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
// CSI sequences a progress bar pairs with CR are honoured.
func TestSessionOutputFollowsTerminalLines(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   []string // what a read delivers after each chunk
	}{
		{
			// git/curl style: every refresh repaints the whole line with a CR.
			name:   "repaints of equal length",
			chunks: []string{"\rDownloading 1%", "\rDownloading 2%", "\rDownloading 99%", "\nAll done\n"},
			want:   []string{"", "", "", "Downloading 99%\nAll done\n"},
		},
		{
			name:   "several rewrites inside one chunk",
			chunks: []string{"1%\r2%\r3%\n"},
			want:   []string{"3%\n"},
		},
		{
			name:   "a shorter rewrite keeps the longer tail, as a terminal does",
			chunks: []string{"abcdefgh\rXY\n"},
			want:   []string{"XYcdefgh\n"},
		},
		{
			name:   "the CR plus erase-in-line idiom erases",
			chunks: []string{"abcdefgh\r\x1b[KXY\n"},
			want:   []string{"XY\n"},
		},
		{
			name:   "erase-in-line with a parameter",
			chunks: []string{"abc\x1b[2Kdef\n"},
			want:   []string{"def\n"},
		},
		{
			name:   "a cursor move leaves the cells it skipped blank",
			chunks: []string{"ab\x1b[5GZ\n"},
			want:   []string{"ab  Z\n"},
		},
		{
			name:   "backspace moves one cell left",
			chunks: []string{"ab\bX\n"},
			want:   []string{"aX\n"},
		},
		{
			name:   "a rewritten line arrives once it is completed",
			chunks: []string{"progress 1%\rprogress 2%", "\n"},
			want:   []string{"", "progress 2%\n"},
		},
		{
			name:   "CRLF ends the line",
			chunks: []string{"line\r\nnext"},
			want:   []string{"line\nnext"},
		},
		{
			name:   "a CRLF split across chunks keeps its line",
			chunks: []string{"abc\r", "\n"},
			want:   []string{"", "abc\n"},
		},
		{
			name:   "a chunk-ending CR is a cursor move when no LF follows",
			chunks: []string{"abc\r", "def\n"},
			want:   []string{"", "def\n"},
		},
		{
			name:   "a CSI sequence cut in half still applies",
			chunks: []string{"abc\r\x1b[", "Kxy\n"},
			want:   []string{"", "xy\n"},
		},
		{
			name:   "a line that was never rewritten stays visible",
			chunks: []string{"Enter a value: "},
			want:   []string{"Enter a value: "},
		},
		{
			name:   "colors never reach a reader",
			chunks: []string{"\x1b[1;31mred\x1b[0m\n"},
			want:   []string{"red\n"},
		},
		{
			name:   "completed lines stay incremental",
			chunks: []string{"one\ntwo\n", "three\n"},
			want:   []string{"one\ntwo\n", "three\n"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var o sessionOutput
			for i, chunk := range tc.chunks {
				o.append([]byte(chunk))
				if got := o.read(); got != tc.want[i] {
					t.Fatalf("chunk %d (%q): read = %q, want %q", i+1, chunk, got, tc.want[i])
				}
			}
		})
	}
}

// TestSessionOutputWithholdsRewrittenLines covers the one rule that is ours and
// not the terminal's: a line rewritten in place is withheld from readers until
// it is final, so a progress repaint never reaches them.
func TestSessionOutputWithholdsRewrittenLines(t *testing.T) {
	var o sessionOutput

	o.append([]byte("\rprogress 1%"))
	if got := o.read(); got != "" {
		t.Fatalf("a rewritten line was delivered: %q", got)
	}
	o.append([]byte("\rprogress 2%"))
	if got := o.read(); got != "" {
		t.Fatalf("a growing rewritten line was delivered: %q", got)
	}
	o.append([]byte("\n"))
	if got := o.read(); got != "progress 2%\n" {
		t.Fatalf("completed line = %q, want %q", got, "progress 2%\n")
	}

	// End of stream: the child is gone, so the line it was still rewriting is
	// released as its final version.
	o.append([]byte("see you\rprogress 3%\rprogress 4%"))
	if got := o.read(); got != "" {
		t.Fatalf("a rewritten line was delivered before the child exited: %q", got)
	}
	o.flush()
	if got := o.read(); got != "progress 4%" {
		t.Fatalf("flushed line = %q, want %q", got, "progress 4%")
	}
}

// TestSessionOutputDeliversOnce keeps the delivery cursor honest: what a reader
// got is never handed out a second time.
func TestSessionOutputDeliversOnce(t *testing.T) {
	var o sessionOutput

	o.append([]byte("Enter a value: "))
	if got := o.read(); got != "Enter a value: " {
		t.Fatalf("first read = %q, want the prompt", got)
	}
	if got := o.read(); got != "" {
		t.Fatalf("the prompt was delivered twice: %q", got)
	}

	o.append([]byte("\nnext\n"))
	if got := o.read(); got != "\nnext\n" {
		t.Fatalf("read = %q, want %q", got, "\nnext\n")
	}
}

// TestSessionOutputEscapeEdges covers the escapes a stream may cut in half and
// the ones that are not CSI sequences at all.
func TestSessionOutputEscapeEdges(t *testing.T) {
	// An unfinished sequence at end of stream stands for nothing: it is dropped,
	// so no escape byte reaches a reader.
	var o sessionOutput
	o.append([]byte("ab\x1b[3"))
	o.flush()
	if got := o.read(); got != "ab" {
		t.Fatalf("read = %q, want %q", got, "ab")
	}

	// ESC outside the CSI family is ordinary output (only ESC [ is emulated), and
	// a CSI sequence this buffer does not emulate is consumed and dropped.
	o = sessionOutput{}
	o.append([]byte("a\x1b(Bb\x1b[1Ac\n"))
	if got := o.read(); got != "a\x1b(Bbc\n" {
		t.Fatalf("read = %q, want %q", got, "a\x1b(Bbc\n")
	}
}

// TestSessionOutputTruncatesAtTheCap covers the 1MB ceiling: buffered output
// stays, the loss is marked once and later output is dropped.
func TestSessionOutputTruncatesAtTheCap(t *testing.T) {
	var o sessionOutput
	chunk := bytes.Repeat([]byte("x"), 64*1024)
	for written := 0; written < maxOutputBufferSize; written += len(chunk) {
		o.append(chunk)
	}
	if got := o.read(); len(got) != maxOutputBufferSize {
		t.Fatalf("buffered %d bytes, want %d", len(got), maxOutputBufferSize)
	}

	// The chunk that finds the cap reached is dropped, not buffered.
	o.append([]byte("dropped\nmore\n"))
	got := o.read()
	if strings.Count(got, outputTruncateMarker) != 1 {
		t.Fatalf("the cap was not marked exactly once: %q", got)
	}
	if strings.Contains(got, "dropped") {
		t.Fatalf("output past the cap reached the reader: %q", got)
	}

	o.append([]byte("still arriving\n"))
	if again := o.read(); again != "" {
		t.Fatalf("output kept arriving after the cap: %q", again)
	}
}
