// Package cli implements the colored interactive REPL.
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"lightagent/internal/agent"
	"lightagent/internal/llm"
	"lightagent/internal/markdown"
	"lightagent/internal/slash"
	"lightagent/internal/store"
	"lightagent/internal/termcolor"
)

// SaveMode controls what happens to the session when the CLI exits.
type SaveMode int

const (
	// SaveAsk prompts whether to save (the default).
	SaveAsk SaveMode = iota
	// SaveAlways writes the session without prompting.
	SaveAlways
	// SaveNever discards the session without prompting.
	SaveNever
)

// CLI renders the chat to the terminal.
type CLI struct {
	agent *agent.Agent
	store *store.Store
	model string

	// out receives all rendered output (defaults to stdout; --log wraps it).
	out io.Writer
	// termWidth reports the terminal width in columns (0 when unknown). It is a
	// field so tests can lay the prompt region out for a narrow terminal.
	termWidth func() int
	// saveMode decides what happens on exit.
	saveMode SaveMode
	// batch is true for non-interactive one-shot runs (no prompt is drawn).
	batch bool

	mu           sync.Mutex
	promptActive bool
	// promptStale is true while the region is still on screen but the editor no
	// longer owns it: the next output erases it. The erasure is deferred so an
	// event that writes nothing leaves the region in place to be repainted.
	promptStale bool
	// promptRows is how many continuation lines the pending input occupies.
	promptRows int
	// promptDrawn holds the rows of the last drawn prompt region (top to
	// bottom), so a repaint can rewrite only the rows that changed instead of
	// blanking the region first.
	promptDrawn []promptRow
	// input is the in-progress multi-line message (raw-mode editing only).
	input []rune
	// editing is true while the raw-mode line editor owns the prompt.
	editing bool
	// reader is the active raw-mode reader (nil in line-scanner mode).
	reader termReader
	// lineStart reports whether the output cursor sits at the start of a line.
	lineStart  bool
	streaming  bool
	markdownOn bool
	md         *markdown.Stream
	// reasoningMD renders the model's thinking as markdown when enabled. It is
	// separate from md because the two streams interleave: thinking is flushed
	// as the visible answer begins.
	reasoningMD *markdown.Stream
	// busy is true while a turn runs; the prompt then shows a spinner.
	busy bool
	// busySince is when the running turn started. The prompt derives the
	// elapsed turn time from it, so the timer keeps counting between events.
	// It is zero while idle.
	busySince time.Time
	// spinner holds the current animation frame drawn after the prompt label.
	spinner string
	// preview is the still-arriving partial line, drawn above the prompt so it
	// can be redrawn as it grows and erased together with the prompt. It holds
	// the whole raw tail (never a newline); the rows it covers are laid out when
	// the region is drawn, so a line wider than the terminal wraps over several
	// rows instead of being cut off at the first one.
	preview string
	// reasoning is true while a "[thinking]" block is open.
	reasoning bool
	// reasoningBuf holds the still-arriving partial reasoning line: complete
	// lines are committed dim as they arrive, the tail stays a preview.
	reasoningBuf string
	// pendingSends holds the messages typed here that the agent has not sent yet:
	// they were queued behind a running turn. A terminal cannot move a row that is
	// already printed, so the transcript row is the official one (printed when the
	// message enters the conversation, i.e. after the reply it interrupted) while
	// the pending line keeps the text visible in the prompt region until then.
	pendingSends []string
	// ctxTokens/ctxWindow are the latest context-usage numbers reported by the
	// agent. They are drawn on the prompt row so the usage stays visible while
	// a turn runs (and after it ends); zero means nothing was reported yet.
	ctxTokens int
	ctxWindow int
}

// keyKind classifies a terminal key press for the raw line editor.
type keyKind int

const (
	// keyRune is a printable character to append to the input.
	keyRune keyKind = iota
	// keyNewline is Enter: it inserts a line break.
	keyNewline
	// keySubmit is the send key: Ctrl+J (LF), plus Ctrl+Enter and Alt+Enter
	// where the terminal can tell them apart. It sends the current input.
	keySubmit
	// keyBackspace deletes the last character.
	keyBackspace
	// keyClearLine discards the whole input (Ctrl+U).
	keyClearLine
	// keyInterrupt is Ctrl+C. It clears the input or exits when empty.
	keyInterrupt
	// keyEOF ends the session (Ctrl+D, Ctrl+Z, or a closed stdin).
	keyEOF
	// keyIgnore is a key with no effect (escape sequences, unknown controls).
	keyIgnore
)

// keyEvent is one decoded key press.
type keyEvent struct {
	kind keyKind
	r    rune
}

// termReader reads single keys from a terminal in raw mode.
type termReader interface {
	// ReadKey blocks until one key is available.
	ReadKey() (keyEvent, error)
	// Restore returns the terminal to its original mode.
	Restore()
}

// classifyRune maps a decoded rune to a key event. It is shared by the platform
// readers so the editing semantics stay identical everywhere.
func classifyRune(r rune) keyEvent {
	switch r {
	case '\r':
		return keyEvent{kind: keyNewline}
	case '\n':
		return keyEvent{kind: keySubmit}
	case 0x08, 0x7f:
		return keyEvent{kind: keyBackspace}
	case 0x03:
		return keyEvent{kind: keyInterrupt}
	case 0x04, 0x1a:
		return keyEvent{kind: keyEOF}
	case 0x15:
		return keyEvent{kind: keyClearLine}
	case '\t':
		return keyEvent{kind: keyIgnore}
	}
	if r < 0x20 {
		return keyEvent{kind: keyIgnore}
	}
	return keyEvent{kind: keyRune, r: r}
}

// New creates a CLI around an agent and its store. When markdownEnabled is true,
// assistant replies are rendered as markdown instead of raw text.
func New(a *agent.Agent, st *store.Store, model string, markdownEnabled bool) *CLI {
	c := &CLI{
		agent:      a,
		store:      st,
		model:      model,
		out:        os.Stdout,
		termWidth:  terminalWidth,
		markdownOn: markdownEnabled,
	}
	if markdownEnabled {
		c.md = markdown.NewStream()
		c.reasoningMD = markdown.NewStream()
	}
	return c
}

// SetOutput redirects rendered output (used by --log).
func (c *CLI) SetOutput(w io.Writer) {
	if w == nil {
		return
	}
	c.mu.Lock()
	c.out = w
	c.mu.Unlock()
}

// SetSaveMode selects the exit save behavior.
func (c *CLI) SetSaveMode(m SaveMode) { c.saveMode = m }

// Interactive reports whether in is attached to an interactive terminal.
func Interactive(in *os.File) bool { return isInteractive(in) }

// ConfirmLine prints question to stdout and reads a one-line yes/no answer from
// stdin. It is meant for use before the REPL starts (while the terminal is
// still in line mode). An empty answer, or anything that is not y/yes/n/no,
// yields def.
func ConfirmLine(question string, def bool) bool {
	return ConfirmLineTo(os.Stdout, question, def)
}

// ConfirmLineTo is ConfirmLine with an explicit output writer.
func ConfirmLineTo(w io.Writer, question string, def bool) bool {
	fmt.Fprint(w, question)
	line, _ := readLine(os.Stdin)
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return def
}

// readLine reads one line byte-by-byte so no extra input is buffered (the REPL
// must still see everything piped after the answer).
func readLine(r io.Reader) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			switch buf[0] {
			case '\n':
				return sb.String(), nil
			case '\r':
			default:
				sb.WriteByte(buf[0])
			}
		}
		if err != nil {
			return sb.String(), err
		}
	}
}

// write prints s to stdout while holding the display lock so event output and
// prompt redraws never interleave.
func (c *CLI) write(s string) {
	c.mu.Lock()
	c.outLocked(s)
	c.mu.Unlock()
}

// outLocked writes s and records whether the cursor ended at the start of a
// line. Anything printed takes the prompt region over, so the region is dropped
// first. The caller must hold c.mu.
func (c *CLI) outLocked(s string) {
	if s == "" {
		return
	}
	c.erasePromptLocked()
	fmt.Fprint(c.out, s)
	c.lineStart = strings.HasSuffix(s, "\n")
}

// rawLocked writes region bytes straight through, without taking the prompt
// region down (used while drawing that very region). The caller must hold c.mu.
func (c *CLI) rawLocked(s string) {
	fmt.Fprint(c.out, s)
	c.lineStart = strings.HasSuffix(s, "\n")
}

// promptLabel is the input prompt text.
const promptLabel = "> "

// emptyPromptHint is drawn in gray right after the prompt while the input box is
// empty, so the send key stays discoverable. It names Ctrl+J, the chord with a
// byte of its own and therefore the one every terminal can report, locally or
// over SSH (Ctrl+Enter needs a terminal that disambiguates key presses, and
// Alt+Enter can be taken over by the terminal itself). It shares the prompt's
// row, so it does not affect the prompt-region bookkeeping; the caret is moved
// back over it so that it blinks where typing starts.
const emptyPromptHint = "[Ctrl+J to Send]"

// inputIndent aligns the continuation lines of a multi-line message under its
// first line, i.e. one prompt label wide.
var inputIndent = strings.Repeat(" ", displayColumns(promptLabel))

// messageLine renders one user message: the label it is attributed to, followed
// by the text with continuation lines indented so a multi-line message stays one
// visual block.
func messageLine(source, text string) string {
	label := promptLabel
	if source != "" && source != "cli" {
		label = "you(" + source + ")> "
	}
	return termcolor.Color(label, termcolor.BoldCode, termcolor.GreenCode) +
		indentAfterFirst(text, strings.Repeat(" ", displayColumns(label)))
}

// userLine renders a message typed in this terminal with the input prompt label.
func userLine(text string) string { return messageLine("", text) }

// brailleSpinnerFrames is the smooth Unicode spinner. It needs a console font
// with braille coverage, which the legacy Windows console does not have (there
// the frames are drawn as a placeholder box).
var brailleSpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// asciiSpinnerFrames is the fallback for consoles that cannot draw braille.
var asciiSpinnerFrames = []string{"|", "/", "-", "\\"}

// spinnerFrames animates the busy indicator while a turn is running.
var spinnerFrames = busySpinnerFrames()

// busySpinnerFrames picks the animation the console can actually draw; every
// frame is one column wide either way, so the caret bookkeeping is unaffected.
func busySpinnerFrames() []string {
	if spinnerGlyphsSupported() {
		return brailleSpinnerFrames
	}
	return asciiSpinnerFrames
}

// promptRow is one drawn row of the prompt region: the exact bytes written plus
// the columns they occupy, so a repaint knows which rows changed and how far
// each of them reaches.
type promptRow struct {
	text  string
	width int
}

// prompt prints the input prompt.
func (c *CLI) prompt() {
	c.mu.Lock()
	c.printPromptLocked()
	c.mu.Unlock()
}

// promptHeadLocked renders the context-usage tag, the prompt label and the busy
// indicator, and reports the columns they occupy. The usage tag stays at the line
// start (exactly where it was); the busy indicator (animation frame plus elapsed
// turn time) follows the label, i.e. right at the caret, so it no longer pushes
// the label aside. The caller must hold c.mu.
func (c *CLI) promptHeadLocked() (string, int) {
	head := ""
	width := 0
	if status := c.contextStatusLocked(); status != "" {
		head += termcolor.Gray(status) + " "
		width += displayColumns(status) + 1
	}
	head += termcolor.Color(promptLabel, termcolor.BoldCode, termcolor.GreenCode)
	width += displayColumns(promptLabel)
	if c.busy {
		// The width is measured on the plain text: the escape codes around the
		// animation frame and the timer are not columns.
		plain := c.spinner
		busy := termcolor.Yellow(c.spinner)
		if elapsed := c.busyElapsedLocked(); elapsed != "" {
			plain += " " + elapsed
			busy += " " + termcolor.Cyan(elapsed)
		}
		head += busy + " "
		width += displayColumns(plain) + 1
	}
	return head, width
}

// contextStatusLocked renders the compact context-usage tag drawn on the prompt
// row, or "" when no usage has been reported yet. The caller must hold c.mu.
func (c *CLI) contextStatusLocked() string {
	if c.ctxTokens <= 0 || c.ctxWindow <= 0 {
		return ""
	}
	pct := float64(c.ctxTokens) * 100 / float64(c.ctxWindow)
	return fmt.Sprintf("[ctx %.1f%%]", pct)
}

// busyElapsedLocked formats how long the running turn has taken: tenths of a
// second up to a minute, then minutes and seconds, then hours and minutes. It
// returns "" when no turn is running, so a prompt that is only marked busy (no
// start time recorded) falls back to the bare animation frame. The caller must
// hold c.mu.
func (c *CLI) busyElapsedLocked() string {
	if c.busySince.IsZero() {
		return ""
	}
	d := time.Since(c.busySince)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// promptRegionLocked builds the rows of the prompt region, top to bottom: the
// still-arriving preview line (when there is one) followed by the prompt with
// the pending input. The caller must hold c.mu.
func (c *CLI) promptRegionLocked() []promptRow {
	var rows []promptRow
	// The streaming preview (the partially received line) sits above the prompt
	// so both are drawn (and repainted) as one region. It wraps like the input
	// does: a row wider than the terminal would be wrapped by the console itself,
	// which the in-place repaint (it steps the caret between rows) cannot follow.
	rows = append(rows, wrapPreviewRows(c.preview, c.rowLimit())...)
	// Messages typed here that are still waiting behind the running turn: dim
	// "(pending)" lines between the streaming preview and the prompt. They show
	// the text right away without putting a row into the transcript, which stays
	// ordered (the official row is printed when the message is sent).
	for _, text := range c.pendingSends {
		rows = append(rows, pendingMessageRows(text, c.rowLimit())...)
	}
	head, headWidth := c.promptHeadLocked()
	limit := c.rowLimit()
	if len(c.input) > 0 {
		// Continuation lines line up under the first one and a line that is
		// wider than the terminal is wrapped here, so the console never wraps a
		// region row on its own (see rowLimit).
		for i, line := range strings.Split(string(c.input), "\n") {
			prefix, prefixWidth := inputIndent, displayColumns(inputIndent)
			if i == 0 {
				prefix, prefixWidth = head, headWidth
			}
			rows = append(rows, wrapInputRows(prefix, prefixWidth, line, limit)...)
		}
		return rows
	}
	if c.editing {
		// Gray hint while the input box is empty. It only makes sense in the
		// raw editor, where Enter inserts a newline and Ctrl+J sends. On a
		// terminal too narrow for it the hint is dropped instead of wrapping the
		// row it shares with the prompt.
		if c.hintFitsLocked() {
			return append(rows, promptRow{
				text:  head + termcolor.DarkGray(emptyPromptHint),
				width: headWidth + displayColumns(emptyPromptHint),
			})
		}
	}
	return append(rows, promptRow{text: head, width: headWidth})
}

// hintFitsLocked reports whether the empty-input hint still fits on the prompt
// row. The caller must hold c.mu.
func (c *CLI) hintFitsLocked() bool {
	_, headWidth := c.promptHeadLocked()
	return headWidth+displayColumns(emptyPromptHint) <= c.rowLimit()
}

// wrapInputRows lays one logical input line out as the physical rows the console
// will show: rows are broken at limit columns, at the same indent as the logical
// lines, and a wide rune that would cross the limit starts the next row instead
// of straddling it.
func wrapInputRows(prefix string, prefixWidth int, text string, limit int) []promptRow {
	if limit <= prefixWidth {
		// The prompt head alone is already wider than a terminal row: there is
		// no room left to wrap into, so draw it as a single row.
		return []promptRow{{text: prefix + text, width: prefixWidth + displayColumns(text)}}
	}
	rows := []promptRow{}
	row, base, width := prefix, prefixWidth, prefixWidth
	for _, r := range text {
		cols := runeColumns(r)
		if width+cols > limit && width > base {
			// Every row keeps at least one rune, so this always advances.
			rows = append(rows, promptRow{text: row, width: width})
			row, base = inputIndent, displayColumns(inputIndent)
			width = base
		}
		row += string(r)
		width += cols
	}
	return append(rows, promptRow{text: row, width: width})
}

// printPromptLocked (re)draws the prompt and the pending multi-line input. The
// caller must hold c.mu.
//
// A region that is already on screen is repainted row by row and the rows that
// did not change are left alone. Erasing the region first (ESC[J) would make
// legacy Windows consoles flash a blank region on every animation step, since
// they repaint the console synchronously.
func (c *CLI) printPromptLocked() {
	rows := c.promptRegionLocked()
	prev := c.promptDrawn
	if len(prev) == 0 {
		// A fresh region must start on its own line: streamed text may not end
		// with one. CRLF rather than LF, so the first row starts at column one
		// whatever the terminal does with a bare line feed. The pending preview
		// then sits above the prompt.
		if c.editing && !c.lineStart {
			c.rawLocked("\r\n")
		}
	} else {
		// Back up to the region's first row without erasing it (the caret sits
		// on its last one).
		c.rawLocked("\r")
		for i := 1; i < len(prev); i++ {
			c.rawLocked("\x1b[1A")
		}
	}
	for i, row := range rows {
		if i < len(prev) && prev[i].text == row.text {
			// Unchanged: the console already shows it.
		} else {
			c.rawLocked(row.text)
			if i < len(prev) && prev[i].width > row.width {
				// Shorter than the row it overwrites: erase the tail it no
				// longer covers. The caret stays at the content end, so a
				// caret park further down measures from the right place.
				c.rawLocked("\x1b[K")
			}
		}
		if i < len(rows)-1 {
			// CRLF: every row is drawn from its first column, so a repaint can
			// never inherit the previous row's column.
			c.rawLocked("\r\n")
		}
	}
	// Place the caret explicitly: rows that did not change are not rewritten, so
	// the column the repaint happened to leave behind says nothing.
	last := rows[len(rows)-1].width
	c.rawLocked("\r")
	if last > 0 {
		c.rawLocked(fmt.Sprintf("\x1b[%dC", last))
	}
	if len(rows) < len(prev) {
		// Rows the region no longer covers are stale: erase what lies beyond the
		// last surviving row (the caret already sits past its content).
		c.rawLocked("\x1b[J")
	}
	if len(c.input) == 0 && c.editing && c.hintFitsLocked() {
		// Park the caret at the input start, not at the end of the hint.
		c.cursorBackLocked(displayColumns(emptyPromptHint))
	}
	c.promptActive = true
	c.promptStale = false
	c.promptDrawn = rows
	c.promptRows = len(rows) - 1
	c.lineStart = false
}

// setPreviewLocked records the partial line currently being received, so the
// prompt region can draw it while the rest of the line is still arriving. The
// whole tail is kept: a line wider than the terminal is not cut off, it wraps
// over as many rows as the region can show (see wrapPreviewRows). The caller
// must hold c.mu.
func (c *CLI) setPreviewLocked(pending string) {
	if !c.editing || pending == "" {
		c.preview = ""
		return
	}
	c.preview = strings.ReplaceAll(pending, "\r", "")
}

// rowLimit is the widest a prompt-region row may be: one terminal row minus one
// column, so the console never wraps a row on its own. The region is repainted
// by moving the caret between its rows, which only stays exact while every row
// covers exactly one physical line.
func (c *CLI) rowLimit() int {
	width := 0
	if c.termWidth != nil {
		width = c.termWidth()
	}
	if width <= 0 {
		if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("COLUMNS"))); err == nil {
			width = v
		}
	}
	if width <= 1 {
		width = 80
	}
	return width - 1
}

// displayColumns approximates the terminal columns s occupies: East Asian wide
// runes count as two, control characters as zero.
func displayColumns(s string) int {
	cols := 0
	for _, r := range s {
		cols += runeColumns(r)
	}
	return cols
}

// runeColumns returns the column width of a single rune.
func runeColumns(r rune) int {
	switch {
	case r == '\t':
		// A tab advances to the next tab stop, at most 8 columns. Measuring the
		// widest step keeps a measured row at least as wide as it draws, so a
		// row that is measured to fit is never wrapped by the console.
		return 8
	case r < 0x20, r == 0x7f:
		return 0
	case isWideRune(r):
		return 2
	}
	return 1
}

// isWideRune reports whether r renders as a double-width glyph.
func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115f, // Hangul Jamo
		r == 0x2329, r == 0x232a,
		r >= 0x2e80 && r <= 0x303e, // CJK radicals, Kangxi
		r >= 0x3041 && r <= 0x33ff, // kana, CJK symbols
		r >= 0x3400 && r <= 0x4dbf, // CJK extension A
		r >= 0x4e00 && r <= 0x9fff, // CJK unified ideographs
		r >= 0xa000 && r <= 0xa4cf, // Yi
		r >= 0xac00 && r <= 0xd7a3, // Hangul syllables
		r >= 0xf900 && r <= 0xfaff, // CJK compatibility
		r >= 0xfe30 && r <= 0xfe6f, // CJK compatibility forms
		r >= 0xff00 && r <= 0xff60, // fullwidth forms
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x20000 && r <= 0x3fffd:
		return true
	}
	return false
}

// maxPreviewRows bounds how many rows the region gives to the still-arriving
// partial line. A line that keeps growing without a newline then scrolls through
// that window above the prompt instead of pushing the prompt off the screen,
// while the newest text always stays visible.
const maxPreviewRows = 8

// wrapPreviewRows lays the still-arriving partial line out as the physical rows
// it occupies above the prompt. Rows break at the terminal width and never drop
// a rune (a wide rune that would straddle the limit starts the next row instead),
// so everything the model has produced so far stays on screen instead of waiting
// for the line to complete. Once the line covers more rows than maxPreviewRows,
// only the newest ones are kept.
func wrapPreviewRows(pending string, limit int) []promptRow {
	if pending == "" {
		return nil
	}
	if limit <= 0 {
		return []promptRow{{text: pending, width: displayColumns(pending)}}
	}
	rows := []promptRow{}
	row, width := "", 0
	for _, r := range pending {
		cols := runeColumns(r)
		if width+cols > limit && width > 0 {
			// Every row keeps at least one rune, so this always advances.
			rows = append(rows, promptRow{text: row, width: width})
			row, width = "", 0
		}
		row += string(r)
		width += cols
	}
	rows = append(rows, promptRow{text: row, width: width})
	if len(rows) > maxPreviewRows {
		rows = rows[len(rows)-maxPreviewRows:]
	}
	return rows
}

// pendingSuffix marks a message typed here that the agent has not sent yet.
const pendingSuffix = " (pending)"

// pendingMessageRows lays one still-waiting message out as the prompt-region rows
// it occupies: the prompt label plus the text, wrapped and indented exactly like
// the input box. The whole line is dim and the last row carries the pending mark.
func pendingMessageRows(text string, limit int) []promptRow {
	var rows []promptRow
	for i, line := range strings.Split(text, "\n") {
		prefix, prefixWidth := promptLabel, displayColumns(promptLabel)
		if i > 0 {
			prefix, prefixWidth = inputIndent, displayColumns(inputIndent)
		}
		rows = append(rows, wrapInputRows(prefix, prefixWidth, line, limit)...)
	}
	if len(rows) == 0 {
		return nil
	}
	for i := range rows {
		rows[i].text = termcolor.DimText(rows[i].text)
	}
	last := len(rows) - 1
	rows[last].text += termcolor.DimText(pendingSuffix)
	rows[last].width += displayColumns(pendingSuffix)
	return rows
}

// detachPromptLocked gives the on-screen region up for output without erasing
// it: the erasure happens just before the first byte of output (see outLocked),
// so an event that writes nothing leaves the region in place and it is simply
// repainted. The caller must hold c.mu.
func (c *CLI) detachPromptLocked() {
	if !c.promptActive {
		return
	}
	c.promptActive = false
	c.promptStale = true
	// Positions among the callers are computed as if the region were already
	// gone, which is the state the deferred erasure leaves behind.
	c.lineStart = true
}

// erasePromptLocked erases the prompt region when it is still on screen. The
// caller must hold c.mu.
func (c *CLI) erasePromptLocked() {
	if !c.promptStale || len(c.promptDrawn) == 0 {
		return
	}
	fmt.Fprint(c.out, "\r")
	for i := 1; i < len(c.promptDrawn); i++ {
		fmt.Fprint(c.out, "\x1b[1A")
	}
	fmt.Fprint(c.out, "\x1b[J")
	c.promptStale = false
	c.promptDrawn = nil
	c.promptRows = 0
	c.lineStart = true
}

// clearPromptLocked takes the pending prompt/input region down and erases it.
// The caller must hold c.mu.
func (c *CLI) clearPromptLocked() {
	c.detachPromptLocked()
	c.erasePromptLocked()
}

// endLineLocked terminates a pending line so the next output starts on a fresh
// row. The caller must hold c.mu.
func (c *CLI) endLineLocked() {
	if !c.lineStart {
		c.outLocked("\n")
	}
}

// cursorBackLocked moves the terminal cursor n columns to the left, so trailing
// decorations (such as the empty-input hint) do not leave the caret behind them.
// The caller must hold c.mu.
func (c *CLI) cursorBackLocked(n int) {
	if n <= 0 {
		return
	}
	fmt.Fprintf(c.out, "\x1b[%dD", n)
}

// flushMarkdownLocked emits any buffered markdown text and starts a fresh
// render state. The caller must hold c.mu. It is a no-op when markdown
// rendering is off.
func (c *CLI) flushMarkdownLocked() {
	c.preview = ""
	if c.md == nil || !c.markdownOn {
		return
	}
	c.outLocked(c.md.Flush())
	c.md.Reset()
}

// appendReasoningLocked streams a chunk of the model's reasoning text under a
// single dim "[thinking]" header. Complete lines are committed right away; the
// trailing partial line is carried as the prompt preview so it stays visible
// while it is still arriving. When markdown is on the thinking is rendered like
// the visible answer (ANSI styling, partial line as preview); otherwise it
// stays plain dim text. The caller must hold c.mu.
func (c *CLI) appendReasoningLocked(chunk string) {
	if chunk == "" {
		return
	}
	if !c.reasoning {
		c.flushMarkdownLocked()
		c.endLineLocked()
		c.outLocked(termcolor.Gray("[thinking]") + "\n")
		c.reasoning = true
	}
	if c.reasoningMD != nil {
		// Render thinking as markdown too: complete lines are emitted as they
		// arrive and the still-arriving tail is carried as the preview.
		c.outLocked(c.reasoningMD.Write(chunk))
		c.setPreviewLocked(c.reasoningMD.Pending())
		return
	}
	c.reasoningBuf += chunk
	if !c.editing {
		// No prompt region to redraw: write straight through, like plain
		// (non-markdown) assistant streaming.
		c.outLocked(termcolor.DimText(c.reasoningBuf))
		c.reasoningBuf = ""
		return
	}
	for {
		idx := strings.IndexByte(c.reasoningBuf, '\n')
		if idx < 0 {
			break
		}
		c.outLocked(termcolor.DimText(strings.TrimRight(c.reasoningBuf[:idx], "\r")) + "\n")
		c.reasoningBuf = c.reasoningBuf[idx+1:]
	}
	// The preview is drawn as plain text (its rows are measured in runes, not
	// escape sequences), so pass the raw tail.
	c.setPreviewLocked(c.reasoningBuf)
}

// flushReasoningLocked commits the buffered tail and closes the [thinking]
// block. It is a no-op when no block is open. The caller must hold c.mu.
func (c *CLI) flushReasoningLocked() {
	if !c.reasoning {
		return
	}
	if c.reasoningMD != nil {
		c.outLocked(c.reasoningMD.Flush())
		c.reasoningMD.Reset()
	} else if c.reasoningBuf != "" {
		c.outLocked(termcolor.DimText(c.reasoningBuf) + "\n")
		c.reasoningBuf = ""
	}
	c.preview = ""
	c.reasoning = false
	c.endLineLocked()
}

// toolArg is one argument of a tool call, kept in its original order.
type toolArg struct {
	Name  string
	Value string
}

// toolArguments splits a tool call's JSON argument object into ordered
// name/value pairs. ok is false when raw is empty or is not a JSON object, so
// callers can fall back to showing the payload verbatim.
func toolArguments(raw string) (args []toolArg, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if delim, isDelim := tok.(json.Delim); !isDelim || delim != '{' {
		return nil, false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			break
		}
		key, _ := keyTok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			break
		}
		args = append(args, toolArg{Name: key, Value: toolArgValue(value)})
	}
	return args, true
}

// toolArgValue turns a raw JSON value into the text shown after its name: a
// string loses its surrounding quotes, everything else keeps a compact JSON
// form.
func toolArgValue(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return trimmed
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err == nil {
		return buf.String()
	}
	return trimmed
}

// toolArgLine renders one argument without any indentation: a single-line value
// stays next to the name ("name - value"), while a multi-line value drops to the
// following lines. The value is always shown in full (never quoted or
// truncated). The palette mirrors the web UI: bold cyan name, gray separator and
// value.
func toolArgLine(arg toolArg) string {
	name := termcolor.Color(arg.Name, termcolor.BoldCode, termcolor.CyanCode)
	if !strings.Contains(arg.Value, "\n") {
		return name + termcolor.Gray(" - ") + termcolor.Gray(arg.Value) + "\n"
	}
	return name + "\n" + termcolor.Gray(arg.Value) + "\n"
}

// Run starts the interactive loop. It blocks until the user exits or stdin
// reaches EOF. The session is saved on the way out.
//
// When stdin is a console the raw line editor is used: Enter inserts a line
// break and Ctrl+J (LF) sends the message, as do Ctrl+Enter and Alt+Enter where
// the terminal reports them. Otherwise the plain line scanner is used and every
// line is submitted as-is.
func (c *CLI) Run(ctx context.Context) error {
	events, cancel := c.agent.Bus().Subscribe()
	defer cancel()

	go func() {
		for ev := range events {
			c.render(ev)
		}
	}()

	stop := make(chan struct{})
	defer close(stop)
	go c.animate(stop)

	if reader, ok := newTermReader(os.Stdin); ok {
		return c.runRaw(ctx, reader)
	}
	return c.runScanner(ctx)
}

// animate redraws the prompt with the next spinner frame while a turn is
// running, so the console shows that the agent is working. It exits when stop is
// closed (the interactive loop ended).
func (c *CLI) animate(stop <-chan struct{}) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for i := 1; ; i++ {
		select {
		case <-stop:
			return
		case <-ticker.C:
			c.mu.Lock()
			if c.busy && c.editing {
				c.spinner = spinnerFrames[i%len(spinnerFrames)]
				c.printPromptLocked()
			}
			c.mu.Unlock()
		}
	}
}

// RestoreTerminal returns the console to its normal mode when the raw editor is
// active. A signal-triggered exit cannot rely on runRaw's deferred restore, so
// the process shutdown path calls this instead; it is a no-op in line-scanner
// mode and for batch runs.
func (c *CLI) RestoreTerminal() {
	c.mu.Lock()
	reader := c.reader
	c.mu.Unlock()
	if reader != nil {
		reader.Restore()
	}
}

// runRaw drives the multi-line raw-mode editor.
func (c *CLI) runRaw(ctx context.Context, reader termReader) error {
	defer reader.Restore()

	c.mu.Lock()
	c.editing = true
	c.reader = reader
	c.input = nil
	c.lineStart = true
	c.printPromptLocked()
	c.mu.Unlock()

	for {
		ev, err := reader.ReadKey()
		if err != nil {
			break
		}
		switch ev.kind {
		case keyRune:
			c.mu.Lock()
			c.input = append(c.input, ev.r)
			c.printPromptLocked()
			c.mu.Unlock()
		case keyNewline:
			c.mu.Lock()
			c.input = append(c.input, '\n')
			c.printPromptLocked()
			c.mu.Unlock()
		case keyBackspace:
			c.mu.Lock()
			if len(c.input) > 0 {
				c.input = c.input[:len(c.input)-1]
			}
			c.printPromptLocked()
			c.mu.Unlock()
		case keyClearLine:
			c.mu.Lock()
			c.input = nil
			c.printPromptLocked()
			c.mu.Unlock()
		case keyInterrupt:
			// A running turn is interrupted first; only an idle CLI clears the
			// pending input, and an empty input exits.
			if c.agent.Interrupt() {
				c.redrawInput()
				continue
			}
			c.mu.Lock()
			if len(c.input) > 0 {
				c.input = nil
				c.printPromptLocked()
				c.mu.Unlock()
				continue
			}
			c.mu.Unlock()
			c.shutdown()
			return nil
		case keySubmit:
			c.mu.Lock()
			text := strings.TrimSpace(string(c.input))
			c.input = nil
			c.clearPromptLocked()
			c.mu.Unlock()

			if text == "" {
				c.redrawInput()
				continue
			}
			exit := c.dispatch(ctx, text)
			if exit {
				c.shutdown()
				return nil
			}
			c.redrawInput()
		}
	}

	c.shutdown()
	return nil
}

// redrawInput restores the prompt+input region after output was printed.
func (c *CLI) redrawInput() {
	c.mu.Lock()
	if c.editing {
		c.printPromptLocked()
	}
	c.mu.Unlock()
}

// dispatch routes one submitted line: slash commands are handled locally, every
// other line goes to the agent. The message is not echoed here: the agent
// announces it when it enters the conversation, so its row is drawn exactly where
// it belongs — after the reply it interrupted when the turn was already running.
// While such a message waits, its text stays visible as a pending line above the
// prompt (see queuePendingSend). It returns true to exit.
func (c *CLI) dispatch(ctx context.Context, line string) bool {
	if slash.IsCommandLine(line) {
		return c.handleCommand(ctx, line)
	}
	busy := c.agent.Busy()
	c.agent.SubmitFrom("cli", line)
	if busy {
		c.queuePendingSend(line)
	}
	return false
}

// queuePendingSend shows a message typed here that is waiting behind the running
// turn: the prompt region carries it as a dim "(pending)" line until the agent
// sends it, when the official row is printed and the line goes away. Modes without
// a prompt region (the line scanner, a one-shot run) get a dim note instead.
func (c *CLI) queuePendingSend(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.editing {
		c.pendingSends = append(c.pendingSends, text)
		c.printPromptLocked()
		return
	}
	c.outLocked(termcolor.DimText("(queued; it joins the conversation after the current reply)") + "\n")
}

// settlePendingSendLocked drops the pending line of a message that has just been
// sent, since the official row is written right after. The caller must hold c.mu.
func (c *CLI) settlePendingSendLocked(text string) {
	for i, pending := range c.pendingSends {
		if pending == text {
			c.pendingSends = append(c.pendingSends[:i], c.pendingSends[i+1:]...)
			return
		}
	}
}

// PromptResult is the JSON document printed by a one-shot run with --json.
type PromptResult struct {
	Assistant     string   `json:"assistant"`
	Tools         []string `json:"tools,omitempty"`
	Error         string   `json:"error,omitempty"`
	Tokens        int      `json:"tokens,omitempty"`
	ContextWindow int      `json:"context_window,omitempty"`
}

// RunOnce runs a single turn for text and returns when it completes. It is the
// non-interactive mode behind -p/--prompt: nothing is echoed for the prompt, no
// input prompt is drawn, and the turn's result is printed. With jsonOut the
// live rendering is suppressed in favor of a JSON document; quiet hides
// tool/info events but always shows errors.
func (c *CLI) RunOnce(ctx context.Context, text string, jsonOut, quiet bool) error {
	c.batch = true

	events, cancel := c.agent.Bus().Subscribe()
	defer cancel()

	type result struct {
		assistant []string
		tools     []string
		errText   string
		tokens    int
		window    int
	}
	var res result
	done := make(chan struct{})

	go func() {
		defer close(done)
		for ev := range events {
			switch ev.Type {
			case agent.EventAssistant:
				if strings.TrimSpace(ev.Text) != "" {
					res.assistant = append(res.assistant, ev.Text)
				}
				if !jsonOut {
					c.render(ev)
				}
			case agent.EventAssistantDelta, agent.EventReasoningDelta:
				if !jsonOut {
					c.render(ev)
				}
			case agent.EventToolCall:
				res.tools = append(res.tools, ev.Name)
				if !jsonOut && !quiet {
					c.render(ev)
				}
			case agent.EventToolResult:
				if !jsonOut && !quiet {
					c.render(ev)
				}
			case agent.EventInfo, agent.EventCompacted:
				if !jsonOut && !quiet {
					c.render(ev)
				}
			case agent.EventError:
				res.errText = ev.Text
				if !jsonOut {
					c.render(ev)
				}
			case agent.EventInterrupted:
				if !jsonOut {
					c.render(ev)
				}
			case agent.EventUsage:
				res.tokens = ev.Tokens
				res.window = ev.ContextWindow
			case agent.EventTurnDone:
				if !jsonOut {
					c.render(ev)
				}
				return
			}
		}
	}()

	c.agent.SubmitFrom("cli", text)
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}

	if !jsonOut {
		return nil
	}
	out := PromptResult{
		Assistant:     strings.Join(res.assistant, "\n\n"),
		Tools:         res.tools,
		Error:         res.errText,
		Tokens:        res.tokens,
		ContextWindow: res.window,
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	c.write(string(data) + "\n")
	return nil
}

// runScanner is the portable fallback: it reads whole lines and submits each
// one (Enter sends, there is no multi-line editing).
func (c *CLI) runScanner(ctx context.Context) error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	c.prompt()

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			if !c.agent.Busy() {
				c.prompt()
			}
			continue
		}
		if slash.IsCommandLine(line) {
			if c.handleCommand(ctx, line) {
				c.shutdown()
				return nil
			}
			c.prompt()
			continue
		}

		if c.agent.Busy() {
			c.agent.SubmitFrom("cli", line)
			c.write(termcolor.DimText("(queued; it joins the conversation after the current reply)") + "\n")
			continue
		}
		// Hide the prompt while the turn streams; it is restored on turn-done.
		c.mu.Lock()
		c.clearPromptLocked()
		c.mu.Unlock()
		c.agent.SubmitFrom("cli", line)
	}

	if err := scanner.Err(); err != nil {
		return err
	}
	c.shutdown()
	return nil
}

// shutdown waits for any running turn, then applies the save policy: ask (the
// default), always, or never.
func (c *CLI) shutdown() {
	c.waitIdle()

	switch c.saveMode {
	case SaveNever:
		c.write(termcolor.Gray("session not saved") + "\n")
		return
	case SaveAsk:
		if !c.confirm("save this session before exiting? [Y/n] ", true) {
			c.write(termcolor.Gray("session not saved") + "\n")
			return
		}
	}

	path, err := c.SaveSession()
	if err != nil {
		c.write(termcolor.Red("[error] ") + "failed to save session: " + err.Error() + "\n")
		return
	}
	c.write(termcolor.Gray("session saved to "+path) + "\n")
}

// confirm asks a yes/no question. In raw mode it reads a single key; elsewhere
// it cannot read interactively and returns def. An empty answer (Enter) or an
// interrupt yields def.
func (c *CLI) confirm(question string, def bool) bool {
	c.mu.Lock()
	reader := c.reader
	editing := c.editing
	if editing {
		// Stop redrawing the input prompt while the question is on screen.
		c.clearPromptLocked()
		c.editing = false
	}
	c.outLocked(question)
	c.mu.Unlock()

	if !editing || reader == nil {
		return def
	}
	return c.confirmRaw(reader, def)
}

// confirmRaw reads the answer keys for confirm.
func (c *CLI) confirmRaw(reader termReader, def bool) bool {
	for {
		ev, err := reader.ReadKey()
		if err != nil {
			return def
		}
		switch ev.kind {
		case keyRune:
			switch ev.r {
			case 'y', 'Y':
				c.write("y\n")
				return true
			case 'n', 'N':
				c.write("n\n")
				return false
			}
		case keyNewline, keySubmit:
			c.write(confirmEcho(def) + "\n")
			return def
		case keyInterrupt, keyEOF:
			c.write(confirmEcho(def) + "\n")
			return def
		}
	}
}

// confirmEcho renders the accepted default for display.
func confirmEcho(def bool) string {
	if def {
		return "y"
	}
	return "n"
}

// waitIdle blocks until any running turn completes (bounded).
func (c *CLI) waitIdle() {
	for i := 0; i < 1000 && c.agent.Busy(); i++ {
		time.Sleep(50 * time.Millisecond)
	}
}

// render prints one agent event.
func (c *CLI) render(ev agent.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Take the region down without erasing it yet: an event that writes nothing
	// (a single streamed token, a usage update) then only gets the region
	// repainted, instead of blanking it on every event.
	c.detachPromptLocked()
	if c.editing {
		// Keep the multi-line editor region at the bottom of the screen.
		defer c.printPromptLocked()
	}

	// Anything that is not a further reasoning chunk closes the [thinking]
	// block, so the visible answer (or a tool row) starts on a fresh line.
	if ev.Type != agent.EventReasoningDelta {
		c.flushReasoningLocked()
	}

	switch ev.Type {
	case agent.EventReasoningDelta:
		c.appendReasoningLocked(ev.Text)
	case agent.EventUser:
		// A user message starts a turn (or steers a running one), so mark the
		// prompt busy: it then shows a spinner and the elapsed time until
		// turn_done. A steering message joins the running turn, so it must not
		// restart the clock.
		if !c.busy {
			c.busySince = time.Now()
		}
		c.busy = true
		c.spinner = spinnerFrames[0]
		// A message is announced when it enters the conversation, which is the
		// point the agent publishes it from, so the row belongs exactly here:
		// after the reply it interrupted. Messages typed in this terminal are
		// drawn from here too (the editor no longer echoes them itself, see
		// dispatch); the line-scanner and batch modes show the reply only.
		if (ev.Source == "" || ev.Source == "cli") && !c.editing {
			return
		}
		// A message typed here was waiting as a pending line above the prompt:
		// the official row replaces it now.
		if ev.Source == "" || ev.Source == "cli" {
			c.settlePendingSendLocked(ev.Text)
		}
		c.flushMarkdownLocked()
		c.outLocked(messageLine(ev.Source, ev.Text) + "\n")
	case agent.EventAssistantDelta:
		if c.md != nil && c.markdownOn {
			// Complete lines are rendered as they arrive; the partial line is
			// carried as a preview so text shows up while it is still streaming.
			c.outLocked(c.md.Write(ev.Text))
			c.setPreviewLocked(c.md.Pending())
		} else {
			c.outLocked(ev.Text)
			c.preview = ""
		}
		c.streaming = true
	case agent.EventAssistant:
		if c.md != nil && c.markdownOn {
			if c.streaming {
				c.flushMarkdownLocked()
			} else {
				c.outLocked(markdown.ANSI(ev.Text))
			}
			c.md.Reset()
		} else if !c.streaming {
			c.outLocked(ev.Text)
		}
		c.outLocked("\n\n")
		c.streaming = false
	case agent.EventToolCall:
		c.flushMarkdownLocked()
		c.endLineLocked()
		// The tool name gets its own highlighted line; every argument then
		// follows as a "name - value" line, so the raw JSON wrapper never
		// reaches the terminal.
		c.outLocked(termcolor.Yellow("[tool] ") + termcolor.Color(ev.Name, termcolor.BoldCode, termcolor.YellowCode) + "\n")
		if args, ok := toolArguments(ev.Args); ok {
			for _, arg := range args {
				c.outLocked(toolArgLine(arg))
			}
		} else if raw := strings.TrimSpace(ev.Args); raw != "" {
			c.outLocked(termcolor.Gray(raw) + "\n")
		}
	case agent.EventToolResult:
		c.flushMarkdownLocked()
		c.endLineLocked()
		if ev.Text == "" {
			if ev.IsError {
				c.outLocked(termcolor.Red("[result] ") + "(error)\n")
			}
			return
		}
		color := termcolor.Gray
		if ev.IsError {
			color = termcolor.Red
		}
		body := indentAfterFirst(clipForDisplay(ev.Text, resultDisplayLines, resultDisplayChars), "  ")
		c.outLocked(color("[result] "+body) + "\n")
	case agent.EventCompacted:
		c.flushMarkdownLocked()
		c.endLineLocked()
		c.outLocked(termcolor.Cyan("[info] ") + ev.Text + "\n")
		// The summary replaced the messages that were just cut out of the
		// context, so it is printed right at the truncation point.
		c.writeSummaryLocked(ev.Summary)
	case agent.EventInfo:
		c.flushMarkdownLocked()
		c.endLineLocked()
		c.outLocked(termcolor.Cyan("[info] ") + ev.Text + "\n")
	case agent.EventInterrupted:
		c.flushMarkdownLocked()
		c.endLineLocked()
		c.outLocked(termcolor.Yellow("[interrupted] ") + ev.Text + "\n")
	case agent.EventError:
		c.flushMarkdownLocked()
		c.endLineLocked()
		c.outLocked(termcolor.Red("[error] ") + ev.Text + "\n")
	case agent.EventUsage:
		// Keep the latest numbers; the prompt repaint below (editing mode) draws
		// them, so the usage updates live while the tool loop is still running.
		c.ctxTokens = ev.Tokens
		c.ctxWindow = ev.ContextWindow
	case agent.EventTurnDone:
		c.busy = false
		c.busySince = time.Time{}
		c.spinner = ""
		c.flushMarkdownLocked()
		c.outLocked("\n")
		c.streaming = false
		// A queued message always turns into a row — the agent either folds it
		// into this turn or starts a new one for it — so nothing should be
		// pending once the agent is idle: drop what is left over (a message the
		// steering queue had to reject) instead of leaving a stale line above the
		// prompt.
		if !c.agent.Busy() {
			c.pendingSends = nil
		}
		if !c.editing && !c.batch {
			c.printPromptLocked()
		}
	}
}

// resultDisplayLines and resultDisplayChars bound a single tool result printed
// to the terminal. Newlines are preserved so command output stays readable.
const (
	resultDisplayLines = 200
	resultDisplayChars = 8000
)

// clipForDisplay trims a multi-line block to the given line/char budgets and
// appends a marker when anything was dropped.
func clipForDisplay(s string, maxLines, maxChars int) string {
	clipped := false
	lines := strings.Split(s, "\n")
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
		clipped = true
	}
	out := strings.Join(lines, "\n")
	if maxChars > 0 && len(out) > maxChars {
		out = out[:maxChars]
		clipped = true
	}
	if clipped {
		out += "\n... (truncated)"
	}
	return out
}

// indentAfterFirst prefixes every continuation line of s with indent.
func indentAfterFirst(s, indent string) string {
	return strings.ReplaceAll(s, "\n", "\n"+indent)
}

// handleCommand processes a slash command. It returns true to exit.
func (c *CLI) handleCommand(ctx context.Context, line string) bool {
	cmd, args, known := slash.Split(line)
	if !known {
		c.write(termcolor.Red("[error] ") + "unknown command " + cmd + "; try /help\n")
		return false
	}

	switch cmd {
	case "/help":
		c.write(helpText())
	case "/exit":
		c.write(termcolor.Gray("bye") + "\n")
		return true
	case "/new":
		c.agent.Reset()
		c.write(termcolor.Cyan("[info] ") + "started a new conversation (in memory; /save to persist)\n")
	case "/save":
		path, err := c.SaveSession()
		if err != nil {
			c.write(termcolor.Red("[error] ") + "failed to save session: " + err.Error() + "\n")
			break
		}
		c.write(termcolor.Cyan("[info] ") + "session saved to " + path + "\n")
	case "/compact":
		if c.agent.Busy() {
			c.write(termcolor.Red("[error] ") + "a turn is running; try again when idle\n")
			break
		}
		// A pass with nothing to condense is the only case that needs a line
		// here: one that did compress reports itself on the bus (the
		// "compacting" info, then the compacted event with the summary).
		if msg := c.agent.CompactNow(ctx); msg != "" {
			c.write(termcolor.Cyan("[info] ") + msg + "\n")
		}
	case "/stop":
		if !c.agent.Interrupt() {
			c.write(termcolor.Gray("[info] nothing to interrupt") + "\n")
		}
		// The interrupted marker and the end of the turn are rendered by the
		// event loop.
	case "/history":
		c.write(termcolor.Cyan("[info] ") + slash.UsageText(c.agent.Stats()) + "\n")
	case "/result":
		c.write(c.toggleToolResults(args) + "\n")
	case "/markdown":
		c.write(c.toggleMarkdown(args) + "\n")
	}
	return false
}

// toggleMarkdown applies a /markdown argument ("on"/"off"; no argument toggles)
// and reports the new state.
func (c *CLI) toggleMarkdown(args []string) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	on, ok := slash.ToggleArg(arg, c.markdownOn)
	if !ok {
		return termcolor.Red("[error] ") + "usage: /markdown [on|off]"
	}
	c.markdownOn = on
	if on && c.md == nil {
		c.md = markdown.NewStream()
	}
	if c.md != nil {
		c.md.Reset()
	}
	if on {
		return termcolor.Cyan("[info] ") + "markdown rendering on"
	}
	return termcolor.Cyan("[info] ") + "markdown rendering off (raw output)"
}

// toggleToolResults applies a /result argument ("on"/"off"; no argument
// toggles) and reports the new state. It defaults to on.
func (c *CLI) toggleToolResults(args []string) string {
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	on, ok := slash.ToggleArg(arg, c.agent.ToolResultsVisible())
	if !ok {
		return termcolor.Red("[error] ") + "usage: /result [on|off]"
	}
	c.agent.SetToolResultsVisible(on)
	if on {
		return termcolor.Cyan("[info] ") + "showing tool/exec results"
	}
	return termcolor.Cyan("[info] ") + "hiding tool/exec results"
}

// summaryMarker introduces a compressed-context summary: it states what the
// block below stands for, so the truncation point is obvious. The page draws
// the same wording on its summary row.
const summaryMarker = "older messages are condensed into the summary below"

// writeSummaryLocked prints the summary block: the marker line and the summary
// text indented under it. The summary is a model-written report, so it is
// rendered like assistant prose (markdown when that is on, matching the page's
// summary row) before it is indented. It is a no-op without a summary - nothing
// was condensed, so there is nothing to mark. The caller must hold c.mu.
func (c *CLI) writeSummaryLocked(summary string) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return
	}
	c.outLocked(termcolor.Cyan("[summary] ") + summaryMarker + "\n")
	if c.md != nil && c.markdownOn {
		summary = markdown.ANSI(summary)
	}
	c.outLocked("  " + indentAfterFirst(summary, "  ") + "\n")
}

// ShowHistory prints a resumed conversation so re-entering a session shows what
// was said before.
func (c *CLI) ShowHistory(messages []llm.Message, summary string) {
	// Everything before the last compaction is represented by the summary
	// alone, so it is printed first: it marks where this transcript starts.
	c.mu.Lock()
	c.writeSummaryLocked(summary)
	c.mu.Unlock()
	if len(messages) == 0 {
		return
	}
	c.write(termcolor.Gray(fmt.Sprintf("--- history: %d messages ---", len(messages))) + "\n")
	for _, m := range messages {
		switch m.Role {
		case "user":
			c.write(userLine(m.Content) + "\n")
		case "assistant":
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			if c.markdownOn {
				c.write(markdown.ANSI(m.Content))
			} else {
				c.write(m.Content)
			}
			c.write("\n\n")
		}
	}
	c.write(termcolor.Gray("--- end of history ---") + "\n\n")
}

// snapshot captures the current conversation for persistence.
func (c *CLI) snapshot() store.State {
	return store.State{
		Version:   1,
		UpdatedAt: time.Now(),
		Model:     c.model,
		Summary:   c.agent.Summary(),
		Messages:  c.agent.History(),
	}
}

// SaveSession writes the current conversation to the session file and returns
// the path it was written to. It backs /save in both front-ends: the mirror
// registers it through web.Server.SetSessionSaver, so a save from the browser
// writes exactly what the terminal would write.
func (c *CLI) SaveSession() (string, error) {
	if err := c.store.Save(c.snapshot()); err != nil {
		return "", err
	}
	return c.store.Path(), nil
}

// helpText lists the available commands. Command names are colored so they stand
// out from their descriptions.
func helpText() string {
	var b strings.Builder
	b.WriteString(termcolor.BoldText("commands") + "\n")
	// The table comes from the shared catalogue (internal/slash), so the
	// terminal REPL and the web mirror always describe the same commands.
	b.WriteString(slash.Table(func(name, rest string) string {
		return termcolor.Cyan(name) + termcolor.Gray(rest)
	}))
	notes := []string{
		"",
		"the conversation lives in memory only; use /save to persist it now.",
		"input: Enter inserts a newline; Ctrl+J sends (Ctrl+Enter and Alt+Enter where the terminal reports them).",
		"while a turn runs, type a message to insert it into the loop (steering).",
		"Ctrl+C interrupts a running turn; when idle it clears the input, then exits.",
	}
	b.WriteString(termcolor.Gray(strings.Join(notes, "\n")) + "\n")
	return b.String()
}

// Banner prints the startup banner. It does not draw the input prompt; Run does
// that once the editor is ready.
func (c *CLI) Banner(configPath, stateDir, webHost string, webPort int) {
	c.write(termcolor.BoldText("lightagent") + termcolor.Gray(" — a tiny OpenAI-compatible CLI agent") + "\n")
	// A label column keeps the values aligned; only the labels are dimmed so the
	// values stay easy to read.
	fields := [][2]string{
		{"model", c.model},
		{"config", configPath},
		{"state dir", stateDir},
	}
	if webPort > 0 {
		if strings.TrimSpace(webHost) == "" {
			webHost = "127.0.0.1"
		}
		fields = append(fields, [2]string{"web", termcolor.Cyan(fmt.Sprintf("http://%s:%d/", webHost, webPort))})
	}
	for _, f := range fields {
		c.write("  " + termcolor.Gray(fmt.Sprintf("%-9s", f[0])) + " " + f[1] + "\n")
	}
	c.write(termcolor.Gray("  /help or /? for commands; Enter = newline, Ctrl+J = send (Ctrl+Enter where reported)") + "\n\n")
}
