package tools

import "bytes"

// maxOutputBufferSize caps the output buffer at 1MB per hand-over window: what a
// call is about to take is buffered up to this, then the loss is marked and the
// rest of that window is dropped (see sessionOutput).
const maxOutputBufferSize = 1 << 20

// outputTruncateMarker is appended once when the buffer cap is reached.
const outputTruncateMarker = "\n... [output truncated, exceeded 1MB]\n"

// maxCursorColumn bounds a cursor move to the right, so a program that jumps to
// a silly column cannot pad the buffer with an unbounded run of blanks.
const maxCursorColumn = 4096

// sessionOutput replays a child's raw bytes (already converted to UTF-8, see
// console_encoding.go) through the terminal rules a program writes against, so a
// reader gets what a terminal would show:
//
//   - a CR moves the cursor to the first column; it does not erase. The line
//     keeps its text until later writes overwrite it cell by cell, so a shorter
//     rewrite leaves the tail of a longer one behind — exactly what the terminal
//     shows;
//   - a backspace moves the cursor one column left (it does not erase either),
//     so the usual "rub it out" idiom (backspace, space, backspace) works as it
//     does on a terminal;
//   - the line-local CSI sequences are honoured: EL (CSI K, CSI 0/1/2 K) erases
//     within the line, CHA (CSI <n> G) and CUF/CUB (CSI <n> C / D) move the
//     cursor. They are how a progress bar pairs with CR (\r + CSI K). Every
//     other CSI sequence is consumed and dropped, so no escape byte reaches a
//     reader;
//   - an LF ends the line. A CRLF is that CR followed by this LF, so the
//     delivered line holds no CR.
//
// The buffer is the window between two hand-overs: it holds what the child wrote
// since the last call took the output, and a hand-over returns it as it stands
// and empties the buffer (see take). The window is what keeps a progress bar
// quiet: a line the child repaints in place keeps being overwritten in the
// buffer, so the intermediate states of one window never reach a reader — only
// the state the line holds when the buffer is handed over, together with every
// line the child finished on the way. A line that was never rewritten in place is
// visible as soon as it arrives, so a prompt without a trailing newline reaches a
// reader too.
//
// All methods are called with ProcessSession.mu held.
type sessionOutput struct {
	// published is the visible stream of the current window: finished lines
	// (each ending in LF) followed by the bytes of the line being written.
	published bytes.Buffer
	// rowStart is where the current line begins in published.
	rowStart int
	// col is the cell the next character lands in, counted from rowStart.
	col int
	// crPending marks a CR that ended its chunk: an LF may still follow, which
	// makes it the line break of a CRLF instead of a cursor move.
	crPending bool
	// escape carries a CSI sequence that a chunk boundary cut in half.
	escape []byte
	// truncated is set once the cap was hit; the rest of the window is dropped.
	truncated bool
}

// append adds one chunk of raw child output.
func (o *sessionOutput) append(p []byte) {
	if o.truncated {
		return
	}
	if len(o.escape) > 0 {
		// Finish the CSI sequence the previous chunk cut in half.
		p = append(append([]byte(nil), o.escape...), p...)
		o.escape = nil
	}
	if o.published.Len() >= maxOutputBufferSize {
		// The cap is reached: what is buffered stays, the loss is marked once
		// and the rest of this window is dropped. The marker ends the line it
		// lands on, so the line being rewritten is not glued to it.
		o.published.WriteString(outputTruncateMarker)
		o.endLine()
		o.truncated = true
		return
	}
	if o.crPending {
		if len(p) == 0 {
			return
		}
		o.crPending = false
		if p[0] == '\n' {
			// CRLF: the CR only moved the cursor, this LF ends the line.
			o.endLine()
			p = p[1:]
		}
	}
	for i := 0; i < len(p); {
		switch p[i] {
		case '\n':
			o.endLine()
			i++
		case '\r':
			switch {
			case i+1 == len(p):
				// The chunk ends here, so only the next one says whether an LF
				// follows; the cursor moves either way.
				o.crPending = true
				o.cursorTo(0)
				i++
			case p[i+1] == '\n':
				o.endLine()
				i += 2
			default:
				o.cursorTo(0)
				i++
			}
		case '\b':
			o.cursorTo(o.col - 1)
			i++
		case '\x1b':
			n := o.consumeEscape(p[i:])
			if n == 0 {
				return // the sequence continues in the next chunk
			}
			i += n
		default:
			o.writeCell(p[i])
			i++
		}
	}
}

// consumeEscape parses the escape sequence starting at the ESC byte in p and
// returns how many bytes it used. 0 means the chunk ended inside the sequence:
// its bytes are parked in o.escape until the rest arrives. Only the CSI family
// (ESC [) is emulated; any other escape is ordinary output, and a sequence that
// never completes is never handed over, since a terminal would not act on it
// either.
func (o *sessionOutput) consumeEscape(p []byte) int {
	if len(p) < 2 {
		o.escape = append(o.escape[:0], p...)
		return 0
	}
	if p[1] != '[' {
		o.writeCell(p[0])
		return 1
	}
	params := make([]byte, 0, len(p))
	for i := 2; i < len(p); i++ {
		switch c := p[i]; {
		case c >= 0x30 && c <= 0x3f: // parameter bytes
			params = append(params, c)
		case c >= 0x20 && c <= 0x2f: // intermediate bytes: no emulated sequence has one
		case c >= 0x40 && c <= 0x7e: // final byte closes the sequence
			o.applyCSI(string(params), c)
			return i + 1
		default:
			// Not a CSI sequence after all: the ESC is text and the byte that
			// broke the pattern is ordinary output.
			o.writeCell(p[0])
			return 1
		}
	}
	o.escape = append(o.escape[:0], p...)
	return 0
}

// applyCSI runs the line-local CSI sequences a progress writer uses (EL, CHA,
// CUF, CUB); colors, cursor up/down, private modes and the rest are dropped.
func (o *sessionOutput) applyCSI(params string, final byte) {
	n, ok := csiNumber(params)
	if !ok {
		return // a parameter list or a private marker, not a move we emulate
	}
	switch final {
	case 'K': // erase in line
		switch n {
		case 1: // from the start of the line through the cursor
			o.blankCells(0, o.col)
		case 2: // the whole line
			o.cursorTo(0)
			o.eraseToCursor()
		default: // 0: from the cursor to the end of the line
			o.eraseToCursor()
		}
	case 'G': // cursor to column (1 based)
		o.cursorTo(orOne(n) - 1)
	case 'C': // cursor forward
		o.cursorTo(o.col + orOne(n))
	case 'D': // cursor back
		o.cursorTo(o.col - orOne(n))
	}
}

// csiNumber reads a CSI sequence's numeric parameter: n is 0 when the sequence
// carries none, and ok is false for parameters this buffer does not emulate (a
// list such as "1;2", or a private marker such as "?25"). Values are capped, so
// an absurd column cannot overflow.
func csiNumber(params string) (n int, ok bool) {
	for i := 0; i < len(params); i++ {
		if params[i] < '0' || params[i] > '9' {
			return 0, false
		}
		n = n*10 + int(params[i]-'0')
		if n > maxCursorColumn {
			n = maxCursorColumn
		}
	}
	return n, true
}

// orOne is the count a CSI move uses when its parameter is absent or zero: those
// sequences move a single cell.
func orOne(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// cursorTo moves the write cursor inside the current line, so what the child
// writes next lands in the cells of the line it is repainting.
func (o *sessionOutput) cursorTo(col int) {
	if col < 0 {
		col = 0
	}
	if col > maxCursorColumn {
		col = maxCursorColumn
	}
	o.col = col
}

// writeCell puts one character at the cursor, overwriting the cell already
// there: a terminal erases nothing, so a shorter rewrite keeps the tail of the
// longer text it replaced.
func (o *sessionOutput) writeCell(c byte) {
	at := o.rowStart + o.col
	switch {
	case at < o.published.Len():
		// Bytes() hands out the buffer's own array and nothing was written since
		// it was taken, so the cell can be overwritten in place.
		o.published.Bytes()[at] = c
	case at == o.published.Len():
		o.published.WriteByte(c)
	default:
		// The cursor moved past the line's end: those cells are blank on screen.
		for o.published.Len() < at {
			o.published.WriteByte(' ')
		}
		o.published.WriteByte(c)
	}
	o.col++
}

// eraseToCursor drops the cells from the cursor to the end of the line (EL 0).
func (o *sessionOutput) eraseToCursor() {
	if end := o.rowStart + o.col; end < o.published.Len() {
		o.published.Truncate(end)
	}
}

// blankCells overwrites cells [from, to] of the current line with spaces, the
// cells a terminal blanks for EL 1.
func (o *sessionOutput) blankCells(from, to int) {
	row := o.published.Bytes()[o.rowStart:]
	for i := from; i <= to && i < len(row); i++ {
		row[i] = ' '
	}
}

// endLine finishes the current line: the LF is all a reader sees, so a CRLF
// leaves no CR behind.
func (o *sessionOutput) endLine() {
	o.published.WriteByte('\n')
	o.rowStart = o.published.Len()
	o.col = 0
	o.crPending = false
}

// take hands the whole buffer over to a caller: everything the child has written
// since the last hand-over is returned as it stands — including the line it is
// still rewriting, which is what a terminal would show right now — and the buffer
// starts empty. The child's next write therefore forms a fresh line instead of
// rewriting bytes a caller already got, nothing is reported twice, and how the
// fresh line relates to what was handed over is the next call's business (a
// repeated repaint simply arrives as the state it is). An escape sequence a chunk
// boundary cut in half stays parked: it belongs to the child's stream, not to the
// output handed over.
func (o *sessionOutput) take() string {
	data := string(o.published.Bytes())
	o.published.Reset()
	o.rowStart = 0
	o.col = 0
	o.crPending = false
	o.truncated = false
	return data
}
