package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"lightagent/internal/agent"
	"lightagent/internal/config"
	"lightagent/internal/llm"
	"lightagent/internal/slash"
	"lightagent/internal/store"
	"lightagent/internal/termcolor"
	"lightagent/internal/tools"
)

// newTestCLI builds a CLI wired to an offline agent (no LLM calls are made).
func newTestCLI(t *testing.T) *CLI {
	t.Helper()
	cfg := config.Default()
	client := llm.NewClient(cfg.OpenAI)
	reg := tools.NewRegistry()
	ag := agent.New(cfg, client, reg, agent.NewBus())
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return New(ag, st, "test-model", true)
}

// TestClassifyRune pins the raw-editor key mapping: Enter is a newline and
// Ctrl+Enter (LF) submits.
func TestClassifyRune(t *testing.T) {
	cases := []struct {
		name string
		in   rune
		want keyKind
	}{
		{"enter (CR) inserts newline", '\r', keyNewline},
		{"ctrl+enter (LF) sends", '\n', keySubmit},
		{"backspace (BS)", 0x08, keyBackspace},
		{"backspace (DEL)", 0x7f, keyBackspace},
		{"ctrl+c interrupts", 0x03, keyInterrupt},
		{"ctrl+d is eof", 0x04, keyEOF},
		{"ctrl+z is eof", 0x1a, keyEOF},
		{"ctrl+u clears", 0x15, keyClearLine},
		{"tab is ignored", '\t', keyIgnore},
		{"printable rune", 'a', keyRune},
		{"non-ascii rune", '中', keyRune},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRune(tc.in); got.kind != tc.want {
				t.Fatalf("classifyRune(%q).kind = %d, want %d", tc.in, got.kind, tc.want)
			}
		})
	}
	if got := classifyRune('x'); got.r != 'x' {
		t.Fatalf("classifyRune('x').r = %q, want 'x'", got.r)
	}
}

// TestClipForDisplay checks the tool-result clipping keeps newlines and marks
// truncated blocks.
func TestClipForDisplay(t *testing.T) {
	full := "line1\nline2\nline3"
	if got := clipForDisplay(full, 10, 100); got != full {
		t.Fatalf("clipForDisplay kept %q, want %q", got, full)
	}
	got := clipForDisplay(full, 2, 100)
	if !strings.Contains(got, "line1\nline2") || !strings.HasSuffix(got, "(truncated)") {
		t.Fatalf("line clipping = %q", got)
	}
	got = clipForDisplay(full, 10, 5)
	if !strings.HasPrefix(got, "line1") || !strings.HasSuffix(got, "(truncated)") {
		t.Fatalf("char clipping = %q", got)
	}
}

// TestIndentAfterFirst verifies continuation lines are indented.
func TestIndentAfterFirst(t *testing.T) {
	got := indentAfterFirst("a\nb\nc", "  ")
	if got != "a\n  b\n  c" {
		t.Fatalf("indentAfterFirst = %q", got)
	}
}

// TestToggleToolResults covers the /result on|off switch (default on).
func TestToggleToolResults(t *testing.T) {
	c := newTestCLI(t)
	if !c.agent.ToolResultsVisible() {
		t.Fatal("tool results should default to visible")
	}
	if out := c.toggleToolResults([]string{"off"}); !strings.Contains(out, "hiding") {
		t.Fatalf("toggle off = %q", out)
	}
	if c.agent.ToolResultsVisible() {
		t.Fatal("tool results should be hidden after /result off")
	}
	if out := c.toggleToolResults([]string{"on"}); !strings.Contains(out, "showing") {
		t.Fatalf("toggle on = %q", out)
	}
	if !c.agent.ToolResultsVisible() {
		t.Fatal("tool results should be visible after /result on")
	}
	if out := c.toggleToolResults([]string{"bogus"}); !strings.Contains(out, "usage") {
		t.Fatalf("invalid argument = %q", out)
	}
}

// TestCompactedEventPrintsTheSummary pins the live compaction marker: the info
// line counts what was compressed, and the summary block right below it shows
// what replaced it, so the truncation point is obvious in the transcript.
func TestCompactedEventPrintsTheSummary(t *testing.T) {
	noColors(t)
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.lineStart = true

	c.render(agent.Event{
		Type:    agent.EventCompacted,
		Text:    "context compressed: 9 -> 3 messages",
		Summary: "goal: ship it",
	})
	got := buf.String()
	info := strings.Index(got, "[info] context compressed: 9 -> 3 messages")
	marker := strings.Index(got, "[summary] "+summaryMarker)
	if info < 0 || marker < 0 {
		t.Fatalf("compaction marker missing: %q", got)
	}
	if info > marker {
		t.Fatalf("the info line should come first: %q", got)
	}
	if !strings.Contains(got, "\n  goal: ship it\n") {
		t.Fatalf("the summary text is not indented under its marker: %q", got)
	}

	// A compaction without a summary (the fallback dropped the oldest messages
	// without condensing them) prints the info line alone.
	buf.Reset()
	c.render(agent.Event{Type: agent.EventCompacted, Text: "context compressed: 3 -> 2 messages"})
	if strings.Contains(buf.String(), "[summary]") {
		t.Fatalf("an empty summary should not print a block: %q", buf.String())
	}
}

// TestCompactCommandReportsWhenIdle pins the /compact answer when there is
// nothing to condense. A pass that does compress needs no line here: it reports
// itself on the bus (the "compacting" info and the compacted event).
func TestCompactCommandReportsWhenIdle(t *testing.T) {
	noColors(t)
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf

	if exit := c.handleCommand(context.Background(), "/compact"); exit {
		t.Fatal("/compact must not exit the CLI")
	}
	if !strings.Contains(buf.String(), "[info] nothing to compress yet") {
		t.Fatalf("/compact on an empty agent = %q", buf.String())
	}
}

// TestShowHistoryPrintsTheSummaryFirst pins that a resumed conversation shows the
// summary before the messages it replaced: it is the head of the restored
// transcript, marking where it was truncated, not a trailing note.
func TestShowHistoryPrintsTheSummaryFirst(t *testing.T) {
	noColors(t)
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf

	c.ShowHistory([]llm.Message{{Role: "user", Content: "hi"}}, "goal: ship it")
	got := buf.String()
	marker := strings.Index(got, "[summary] "+summaryMarker)
	history := strings.Index(got, "--- history: 1 messages ---")
	if marker < 0 || history < 0 {
		t.Fatalf("history output = %q", got)
	}
	if marker > history {
		t.Fatalf("the summary should precede the history listing: %q", got)
	}
	if !strings.Contains(got, "\n  goal: ship it\n") {
		t.Fatalf("the summary text is missing: %q", got)
	}
}

// TestInterruptAffordances covers the console interrupt markers: the interrupted
// event renders a marker and /stop reports when there is nothing to stop.
func TestInterruptAffordances(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf

	c.render(agent.Event{Type: agent.EventInterrupted, Text: "interrupted; the turn was stopped"})
	if !strings.Contains(buf.String(), "[interrupted]") {
		t.Fatalf("interrupted marker missing: %q", buf.String())
	}

	buf.Reset()
	if exit := c.handleCommand(context.Background(), "/stop"); exit {
		t.Fatal("/stop must not exit the CLI")
	}
	if !strings.Contains(buf.String(), "nothing to interrupt") {
		t.Fatalf("/stop on an idle agent = %q", buf.String())
	}
}

// TestHelpTextListsAliases ensures /? and the input keys are documented.
func TestHelpTextListsAliases(t *testing.T) {
	text := helpText()
	for _, want := range []string{"/help, /?", "Ctrl+J", "Ctrl+Enter", "/save", "/stop"} {
		if !strings.Contains(text, want) {
			t.Fatalf("help text is missing %q:\n%s", want, text)
		}
	}
}

// TestFakeReader feeds a fixed sequence of keys to the raw-mode editor.
type fakeReader struct{ keys []keyEvent }

func (f *fakeReader) ReadKey() (keyEvent, error) {
	if len(f.keys) == 0 {
		return keyEvent{}, io.EOF
	}
	k := f.keys[0]
	f.keys = f.keys[1:]
	return k, nil
}

func (f *fakeReader) Restore() {}

// TestConfirm covers the exit/save prompt: explicit keys win, Enter and
// interrupt accept the default.
func TestConfirm(t *testing.T) {
	cases := []struct {
		name string
		keys []keyEvent
		def  bool
		want bool
	}{
		{"y accepts", []keyEvent{{kind: keyRune, r: 'y'}}, false, true},
		{"N declines", []keyEvent{{kind: keyRune, r: 'N'}}, true, false},
		{"enter takes the default (yes)", []keyEvent{{kind: keyNewline}}, true, true},
		{"enter takes the default (no)", []keyEvent{{kind: keySubmit}}, false, false},
		{"interrupt takes the default", []keyEvent{{kind: keyInterrupt}}, true, true},
		{"eof takes the default", nil, false, false},
		{"unknown keys are ignored", []keyEvent{{kind: keyRune, r: 'x'}, {kind: keyRune, r: 'n'}}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCLI(t)
			c.editing = true
			c.reader = &fakeReader{keys: tc.keys}
			if got := c.confirm("save? [Y/n] ", tc.def); got != tc.want {
				t.Fatalf("confirm = %v, want %v", got, tc.want)
			}
			if c.editing {
				t.Fatal("confirm should stop the editor redraw while asking")
			}
		})
	}
}

// TestConfirmLine covers the startup prompt: an empty answer (or an unknown
// answer) takes the default, y/n override it.
func TestConfirmLine(t *testing.T) {
	cases := []struct {
		input string
		def   bool
		want  bool
	}{
		{"\n", true, true},
		{"\n", false, false},
		{"y\n", false, true},
		{"No\n", true, false},
		{"maybe\n", true, true},
	}
	for _, tc := range cases {
		old := os.Stdin
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdin = r
		go func() {
			_, _ = w.WriteString(tc.input)
			_ = w.Close()
		}()
		got := ConfirmLine("resume? [Y/n] ", tc.def)
		os.Stdin = old
		_ = r.Close()
		if got != tc.want {
			t.Fatalf("ConfirmLine(%q, def=%v) = %v, want %v", tc.input, tc.def, got, tc.want)
		}
	}
}

// TestReadLine checks CRLF/LF handling and that pipelined input is not swallowed.
func TestReadLine(t *testing.T) {
	r := strings.NewReader("yes\r\nrest\n")
	line, err := readLine(r)
	if err != nil || line != "yes" {
		t.Fatalf("readLine = (%q, %v), want (\"yes\", nil)", line, err)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != "rest\n" {
		t.Fatalf("remaining input = %q, want %q", string(rest), "rest\n")
	}
}

// TestStreamingPreviewShowsPartialLine pins the console streaming preview: the
// still-arriving partial line is shown above the prompt, and the completed
// (styled) line replaces it once its newline arrives.
func TestStreamingPreviewShowsPartialLine(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true

	c.render(agent.Event{Type: agent.EventAssistantDelta, Text: "hello wor"})
	if c.preview != "hello wor" {
		t.Fatalf("preview = %q, want the partial line", c.preview)
	}
	if !strings.Contains(buf.String(), "hello wor") {
		t.Fatalf("the prompt region does not show the preview: %q", buf.String())
	}

	buf.Reset()
	c.render(agent.Event{Type: agent.EventAssistantDelta, Text: "ld\n"})
	if c.preview != "" {
		t.Fatalf("preview = %q after the line completed, want empty", c.preview)
	}
	if !strings.Contains(buf.String(), "hello world") {
		t.Fatalf("completed line was not printed: %q", buf.String())
	}

	// Without the raw editor (piped stdin) there is no prompt region to redraw,
	// so nothing is buffered as a preview.
	c.editing = false
	c.render(agent.Event{Type: agent.EventAssistantDelta, Text: "raw"})
	if c.preview != "" {
		t.Fatalf("preview = %q without the editor, want empty", c.preview)
	}
}

// TestStreamingPreviewWrapsPastTheFirstRow pins the reported symptom: while a
// line was still arriving, only its first terminal row was drawn above the
// prompt and everything past it was held back until the line completed (when its
// newline committed the whole line at once). The preview now wraps, so a line
// wider than the terminal keeps streaming row by row.
func TestStreamingPreviewWrapsPastTheFirstRow(t *testing.T) {
	c := newTestCLI(t)
	noColors(t)
	narrowTerminal(c, 10) // 9 columns per row
	var buf strings.Builder
	c.out = &buf
	c.editing = true
	c.lineStart = true

	c.render(agent.Event{Type: agent.EventAssistantDelta, Text: "abcdefghijkl"})
	if c.preview != "abcdefghijkl" {
		t.Fatalf("preview = %q, want the whole partial line", c.preview)
	}
	rows := c.promptRegionLocked()
	want := []string{"abcdefghi", "jkl", promptLabel}
	if len(rows) != len(want) {
		t.Fatalf("region rows = %v, want %v", rows, want)
	}
	for i, row := range rows {
		if row.text != want[i] {
			t.Fatalf("row %d = %q, want %q", i, row.text, want[i])
		}
		if row.width != displayColumns(row.text) {
			t.Fatalf("row %d declares %d columns but draws %d", i, row.width, displayColumns(row.text))
		}
	}
	// Both preview rows reach the console, above the prompt.
	if got := buf.String(); !strings.Contains(got, "abcdefghi\r\njkl\r\n"+promptLabel) {
		t.Fatalf("the wrapped preview was not drawn: %q", got)
	}
}

// TestWrappedPreviewRepaintsInPlace pins the repaint while a streamed line grows
// past its first row: the rows already on screen are left alone and only the new
// row is written, so the text that already arrived is never reprinted as it
// keeps streaming.
func TestWrappedPreviewRepaintsInPlace(t *testing.T) {
	c := newTestCLI(t)
	noColors(t)
	narrowTerminal(c, 10) // 9 columns per row
	var buf strings.Builder
	c.out = &buf
	c.editing = true
	c.lineStart = true

	// The line that has arrived covers the first row only.
	c.render(agent.Event{Type: agent.EventAssistantDelta, Text: "abcdefghi"})
	buf.Reset()

	// The rest of the line arrives: it becomes the second preview row, and the
	// prompt moves down below it.
	c.render(agent.Event{Type: agent.EventAssistantDelta, Text: "jkl"})
	if got, want := buf.String(), "\r\x1b[1A\r\njkl\r\n" + promptLabel + "\r\x1b[2C"; got != want {
		t.Fatalf("repaint = %q, want %q", got, want)
	}
}

// TestReasoningPreviewWrapsPastTheFirstRow pins the same for the [thinking]
// block, markdown on and off: model thinking wider than the terminal keeps
// drawing while the line is still arriving.
func TestReasoningPreviewWrapsPastTheFirstRow(t *testing.T) {
	for _, markdownOn := range []bool{true, false} {
		c := newTestCLI(t)
		noColors(t)
		narrowTerminal(c, 10) // 9 columns per row
		if !markdownOn {
			c.markdownOn = false
			c.md = nil
			c.reasoningMD = nil
		}
		var buf strings.Builder
		c.out = &buf
		c.editing = true
		c.lineStart = true

		const thinking = "let me think about a long line"
		c.render(agent.Event{Type: agent.EventReasoningDelta, Text: thinking})
		rows := c.promptRegionLocked()
		if len(rows) < 3 {
			t.Fatalf("markdown=%v: the thinking preview stayed on one row: %v", markdownOn, rows)
		}
		// Every row but the last (the prompt) carries preview text, and together
		// they rebuild the whole thinking chunk without dropping a rune.
		rebuilt := ""
		for i, row := range rows[:len(rows)-1] {
			rebuilt += row.text
			if limit := c.rowLimit(); row.width > limit {
				t.Fatalf("markdown=%v: preview row %d is %d columns wide, over the %d column limit: %q",
					markdownOn, i, row.width, limit, row.text)
			}
		}
		if rebuilt != thinking {
			t.Fatalf("markdown=%v: the preview rows rebuild %q, want %q", markdownOn, rebuilt, thinking)
		}
		if rows[len(rows)-1].text != promptLabel {
			t.Fatalf("markdown=%v: the prompt row is missing: %v", markdownOn, rows)
		}
	}
}

// TestEmptyPromptShowsSendHint pins the gray hint drawn after the prompt while
// the input box is empty (raw editor only): it disappears as soon as there is
// input, and it names the send chord every terminal can report.
func TestEmptyPromptShowsSendHint(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true

	c.printPromptLocked()
	if !strings.Contains(buf.String(), emptyPromptHint) {
		t.Fatalf("empty prompt is missing the send hint: %q", buf.String())
	}
	if !strings.Contains(emptyPromptHint, "Ctrl+J") {
		t.Fatalf("the hint must name Ctrl+J, the send chord every terminal reports: %q", emptyPromptHint)
	}

	buf.Reset()
	c.input = []rune("hi")
	c.printPromptLocked()
	if strings.Contains(buf.String(), emptyPromptHint) {
		t.Fatalf("hint must be hidden once there is input: %q", buf.String())
	}

	// Outside the raw editor (line scanner) Enter already submits, so the hint
	// is not shown.
	buf.Reset()
	c.input = nil
	c.editing = false
	c.printPromptLocked()
	if strings.Contains(buf.String(), emptyPromptHint) {
		t.Fatalf("hint must not appear outside the raw editor: %q", buf.String())
	}
}

// TestReasoningStreamRendersThinkingBlock pins the thinking display: streamed
// reasoning opens a single [thinking] block, commits complete lines, and is
// closed before the visible answer starts.
func TestReasoningStreamRendersThinkingBlock(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true

	c.render(agent.Event{Type: agent.EventReasoningDelta, Text: "let me "})
	if !c.reasoning {
		t.Fatal("the thinking block should be open")
	}
	if c.preview != "let me " {
		t.Fatalf("preview = %q, want the reasoning tail", c.preview)
	}
	if !strings.Contains(buf.String(), "[thinking]") {
		t.Fatalf("missing the thinking header: %q", buf.String())
	}

	buf.Reset()
	c.render(agent.Event{Type: agent.EventReasoningDelta, Text: "think\n"})
	if c.preview != "" || c.reasoningBuf != "" {
		t.Fatalf("preview = %q, buffer = %q, want the line committed", c.preview, c.reasoningBuf)
	}
	if !strings.Contains(buf.String(), "let me think") {
		t.Fatalf("reasoning line was not printed: %q", buf.String())
	}

	// The visible answer closes the block and starts on a fresh line.
	buf.Reset()
	c.render(agent.Event{Type: agent.EventAssistantDelta, Text: "answer\n"})
	if c.reasoning {
		t.Fatal("the thinking block should be closed once the answer starts")
	}
	if !strings.Contains(buf.String(), "answer") {
		t.Fatalf("the answer was not printed: %q", buf.String())
	}
}

// TestReasoningStreamRendersMarkdown pins that the thinking block honors the
// markdown setting: when on, streamed reasoning is parsed as markdown (its
// markers are consumed and styling applied); when off it stays raw dim text.
func TestReasoningStreamRendersMarkdown(t *testing.T) {
	original := termcolor.Enabled()
	termcolor.SetEnabled(true)
	t.Cleanup(func() { termcolor.SetEnabled(original) })

	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true

	c.render(agent.Event{Type: agent.EventReasoningDelta, Text: "**bold**\n"})
	got := buf.String()
	if strings.Contains(got, "**") {
		t.Fatalf("reasoning markdown markers leaked: %q", got)
	}
	if !strings.Contains(got, termcolor.BoldCode+"bold"+termcolor.ResetCode) {
		t.Fatalf("reasoning bold styling missing: %q", got)
	}

	// With markdown off the reasoning is written through unchanged.
	plain := newTestCLI(t)
	plain.markdownOn = false
	plain.md = nil
	plain.reasoningMD = nil
	buf.Reset()
	plain.out = &buf
	plain.editing = true
	plain.render(agent.Event{Type: agent.EventReasoningDelta, Text: "**bold**\n"})
	if !strings.Contains(buf.String(), "**bold**") {
		t.Fatalf("plain reasoning should keep the raw markers: %q", buf.String())
	}
}

// TestDisplayColumns covers wide-rune and tab accounting, which the preview rows,
// the input rows and the caret placement are all measured with.
func TestDisplayColumns(t *testing.T) {
	if got := displayColumns("中文ab"); got != 6 {
		t.Fatalf("displayColumns = %d, want 6", got)
	}
	// A tab draws up to 8 columns wide, so it must be measured that way: a row
	// measured to fit then never wraps.
	if got := displayColumns("\t"); got != 8 {
		t.Fatalf("displayColumns(tab) = %d, want 8", got)
	}
}

// TestWrapPreviewRows pins the preview layout: the still-arriving partial line is
// broken into the rows it covers (never inside a rune, never dropping one) so its
// text past the first terminal row streams instead of waiting for the newline,
// and only the newest rows are kept once the line covers more than the window.
func TestWrapPreviewRows(t *testing.T) {
	// The empty preview adds no row at all.
	if rows := wrapPreviewRows("", 9); rows != nil {
		t.Fatalf("wrapPreviewRows(\"\") = %v, want no rows", rows)
	}
	// Without a width (unknown terminal) the line stays one row.
	rows := wrapPreviewRows("abcdefghijkl", 0)
	if len(rows) != 1 || rows[0].text != "abcdefghijkl" {
		t.Fatalf("wrapPreviewRows without a limit = %v", rows)
	}
	// 12 columns over a 9 column limit: two rows, none of the text dropped.
	rows = wrapPreviewRows("abcdefghijkl", 9)
	if len(rows) != 2 || rows[0].text != "abcdefghi" || rows[1].text != "jkl" {
		t.Fatalf("wrapPreviewRows = %v, want two rows", rows)
	}
	// A wide rune that would straddle the limit starts the next row.
	rows = wrapPreviewRows("中文中文", 5)
	if len(rows) != 2 || rows[0].text != "中文" || rows[1].text != "中文" {
		t.Fatalf("wrapPreviewRows wide = %v, want two rows", rows)
	}
	// A single rune wider than the limit still gets a row of its own.
	rows = wrapPreviewRows("中", 1)
	if len(rows) != 1 || rows[0].text != "中" {
		t.Fatalf("wrapPreviewRows wide single rune = %v", rows)
	}
	// A long line keeps only the newest window of rows.
	rows = wrapPreviewRows(strings.Repeat("x", 9*(maxPreviewRows+4)), 9)
	if len(rows) != maxPreviewRows {
		t.Fatalf("wrapPreviewRows kept %d rows, want the %d row window", len(rows), maxPreviewRows)
	}
	if got := displayColumns(rows[len(rows)-1].text); got != 9 {
		t.Fatalf("the last preview row is %d columns wide, want the newest text", got)
	}
}

// TestEveryCataloguedCommandIsHandled pins that the terminal REPL has a case for
// every entry of the shared catalogue: a command added to internal/slash without
// wiring it here would answer "unknown command".
func TestEveryCataloguedCommandIsHandled(t *testing.T) {
	for _, cmd := range slash.Commands {
		c := newTestCLI(t)
		var buf strings.Builder
		c.out = &buf
		exit := c.handleCommand(context.Background(), cmd.Name)
		if strings.Contains(buf.String(), "unknown command") {
			t.Errorf("%s is not handled by the CLI: %q", cmd.Name, buf.String())
		}
		if wantExit := cmd.Name == "/exit"; exit != wantExit {
			t.Errorf("%s exit = %v, want %v", cmd.Name, exit, wantExit)
		}
	}
}

// TestCanonicalAliasesShareTheCase pins that an alias reaches the same handler
// as its primary name.
func TestCanonicalAliasesShareTheCase(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.handleCommand(context.Background(), "/interrupt")
	if !strings.Contains(buf.String(), "nothing to interrupt") {
		t.Fatalf("/interrupt = %q, want the /stop handler", buf.String())
	}
	buf.Reset()
	c.handleCommand(context.Background(), "／？")
	if !strings.Contains(buf.String(), "commands") {
		t.Fatalf("full-width ／？ = %q, want the /help listing", buf.String())
	}
}

// TestPromptShowsSpinnerWhileBusy pins the console running indicator: a spinner
// frame is drawn on the prompt row only while a turn is running.
func TestPromptShowsSpinnerWhileBusy(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true

	c.mu.Lock()
	c.printPromptLocked()
	c.mu.Unlock()
	if strings.Contains(buf.String(), spinnerFrames[0]) {
		t.Fatalf("idle prompt shows a spinner: %q", buf.String())
	}

	buf.Reset()
	c.render(agent.Event{Type: agent.EventUser, Text: "hi", Source: "web"})
	if !c.busy {
		t.Fatal("a user event should mark the CLI busy")
	}
	if !strings.Contains(buf.String(), spinnerFrames[0]) {
		t.Fatalf("busy prompt is missing the spinner: %q", buf.String())
	}

	buf.Reset()
	c.render(agent.Event{Type: agent.EventTurnDone})
	if c.busy {
		t.Fatal("turn_done should clear the busy state")
	}
	if strings.Contains(buf.String(), spinnerFrames[0]) {
		t.Fatalf("prompt still shows a spinner after turn_done: %q", buf.String())
	}
}

// caretBackSeq is the escape sequence that parks the caret at the input start.
func caretBackSeq() string {
	return fmt.Sprintf("\x1b[%dD", displayColumns(emptyPromptHint))
}

// TestPromptParksCaretAfterLabel pins the caret position of the empty prompt: the
// gray hint follows the label, but the caret moves back over it so typing starts
// right after "> " instead of at the end of the hint.
func TestPromptParksCaretAfterLabel(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true

	c.mu.Lock()
	c.printPromptLocked()
	c.mu.Unlock()

	out := buf.String()
	if !strings.Contains(out, emptyPromptHint) {
		t.Fatalf("the hint should still be drawn: %q", out)
	}
	if !strings.HasSuffix(out, caretBackSeq()) {
		t.Fatalf("the caret should be parked after the label (%q): %q", caretBackSeq(), out)
	}
	if got := strings.Count(out, caretBackSeq()); got != 1 {
		t.Fatalf("caret moves = %d, want 1: %q", got, out)
	}
}

// TestPromptShowsContextUsage pins the live context-usage tag on the prompt row:
// it appears once a usage event has been reported (after the busy glyph, so the
// label never moves) and updates when a new report arrives.
func TestPromptShowsContextUsage(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true

	c.mu.Lock()
	c.printPromptLocked()
	c.mu.Unlock()
	if strings.Contains(buf.String(), "[ctx ") {
		t.Fatalf("usage tag drawn before any report: %q", buf.String())
	}

	buf.Reset()
	c.render(agent.Event{Type: agent.EventUsage, Tokens: 500, ContextWindow: 1000})
	if !strings.Contains(buf.String(), "[ctx 50.0%]") {
		t.Fatalf("usage tag missing after the report: %q", buf.String())
	}

	buf.Reset()
	c.render(agent.Event{Type: agent.EventUsage, Tokens: 250, ContextWindow: 1000})
	if !strings.Contains(buf.String(), "[ctx 25.0%]") {
		t.Fatalf("usage tag not refreshed: %q", buf.String())
	}
}

// TestBusyIndicatorFollowsLabel pins the prompt order: the busy spinner is drawn
// after the label, so the label never shifts right while the frames animate.
func TestBusyIndicatorFollowsLabel(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true
	c.busy = true
	c.spinner = spinnerFrames[0]

	c.mu.Lock()
	c.printPromptLocked()
	c.mu.Unlock()

	out := buf.String()
	label := strings.Index(out, promptLabel)
	spin := strings.Index(out, spinnerFrames[0])
	if label < 0 || spin < 0 {
		t.Fatalf("prompt is missing the label or the spinner: %q", out)
	}
	if spin != label+len(promptLabel) {
		t.Fatalf("the spinner must follow the label directly: %q", out)
	}
	// The caret points at the input start, i.e. after the busy glyph.
	if !strings.HasSuffix(out, caretBackSeq()) {
		t.Fatalf("caret not parked at the input start: %q", out)
	}
}

// TestBusyIndicatorShowsElapsedTime pins the running-turn timer: the prompt shows
// how long the turn has been running, in color, right after the animation frame,
// and drops it as soon as the turn ends.
func TestBusyIndicatorShowsElapsedTime(t *testing.T) {
	original := termcolor.Enabled()
	termcolor.SetEnabled(true)
	t.Cleanup(func() { termcolor.SetEnabled(original) })

	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true

	c.mu.Lock()
	c.printPromptLocked()
	c.mu.Unlock()
	// Cyan is used for the timer alone, so its presence (and absence) pins the
	// timer without knowing its text.
	if strings.Contains(buf.String(), termcolor.CyanCode) {
		t.Fatalf("the idle prompt shows a timer: %q", buf.String())
	}

	c.render(agent.Event{Type: agent.EventUser, Text: "hi", Source: "cli"})
	if c.busySince.IsZero() {
		t.Fatal("a user event should start the turn clock")
	}

	// Age the clock by exactly one minute and five seconds: the text is then
	// exact, so the colored timer can be pinned byte for byte.
	buf.Reset()
	c.mu.Lock()
	c.busySince = time.Now().Add(-(time.Minute + 5*time.Second))
	c.printPromptLocked()
	c.mu.Unlock()

	out := buf.String()
	if !strings.Contains(out, termcolor.Cyan("1m05s")) {
		t.Fatalf("the prompt should show the elapsed turn time in color: %q", out)
	}
	if !strings.Contains(out, termcolor.Yellow(c.spinner)+" "+termcolor.Cyan("1m05s")) {
		t.Fatalf("the timer must follow the animation frame: %q", out)
	}

	buf.Reset()
	c.render(agent.Event{Type: agent.EventTurnDone})
	if !c.busySince.IsZero() {
		t.Fatal("turn_done should stop the turn clock")
	}
	if strings.Contains(buf.String(), termcolor.CyanCode) {
		t.Fatalf("turn_done still shows the timer: %q", buf.String())
	}
}

// TestSteeringKeepsTheTurnClock pins that a steering message (a user event that
// joins the running turn) does not restart the elapsed-time display.
func TestSteeringKeepsTheTurnClock(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true

	c.render(agent.Event{Type: agent.EventUser, Text: "first", Source: "cli"})
	started := c.busySince
	if started.IsZero() {
		t.Fatal("a user event should start the turn clock")
	}

	c.render(agent.Event{Type: agent.EventUser, Text: "steer", Source: "cli"})
	if !c.busySince.Equal(started) {
		t.Fatalf("busySince moved on a steering message: %v, want %v", c.busySince, started)
	}
}

// TestInjectedSteeringIsDrawnWhereItLands pins the console rendering of a message
// another client submitted while the turn was running: the agent announces the row
// when the message enters the conversation (after the reply it interrupted), so the
// terminal simply draws it there — nothing has to be held back or moved.
func TestInjectedSteeringIsDrawnWhereItLands(t *testing.T) {
	noColors(t)
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf

	c.render(agent.Event{Type: agent.EventAssistant, Text: "the reply"})
	buf.Reset()
	c.render(agent.Event{Type: agent.EventUser, Text: "steer from web", Source: "web"})
	if out := buf.String(); !strings.Contains(out, "you(web)> steer from web") {
		t.Fatalf("the injected message was not drawn: %q", out)
	}
	if !c.busy {
		t.Fatal("a user event should mark the CLI busy")
	}
	// A multi-line message keeps its continuation lines aligned under the label.
	buf.Reset()
	c.render(agent.Event{Type: agent.EventUser, Text: "first\nsecond", Source: "web"})
	if out := buf.String(); !strings.Contains(out, "first\n"+strings.Repeat(" ", displayColumns("you(web)> "))+"second") {
		t.Fatalf("the continuation line is not aligned: %q", out)
	}
}

// TestOwnMessageComesFromTheStream pins that a message typed in this terminal is
// drawn by the event stream instead of being echoed by the editor: the row then
// lands where the message enters the conversation, and it cannot be printed twice.
// The line-scanner and batch modes keep showing the reply only.
func TestOwnMessageComesFromTheStream(t *testing.T) {
	noColors(t)
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true

	c.render(agent.Event{Type: agent.EventUser, Text: "hello", Source: "cli"})
	if out := buf.String(); !strings.Contains(out, "> hello") {
		t.Fatalf("the submitted message was not drawn: %q", out)
	}

	scanner := newTestCLI(t)
	var scannerBuf strings.Builder
	scanner.out = &scannerBuf
	scanner.render(agent.Event{Type: agent.EventUser, Text: "hello", Source: "cli"})
	if scannerBuf.String() != "" {
		t.Fatalf("the scanner mode drew its own message: %q", scannerBuf.String())
	}
}

// TestInjectedSteeringLandsBetweenTheRounds drives the whole path — the real
// agent, a streaming provider and the console render — for a message injected by
// another client (the web mirror) while a reply is still arriving: it is written
// once that reply's block is complete, so the two rounds of the turn stay in one
// piece with the message between them.
func TestInjectedSteeringLandsBetweenTheRounds(t *testing.T) {
	noColors(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first part\"}}]}\n\n")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		once.Do(func() { close(started) })
		// Hold the reply open until the other client has submitted its message.
		<-release
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\" and rest\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	ag := agent.New(cfg, llm.NewClient(cfg.OpenAI), tools.NewRegistry(), agent.NewBus())
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	c := New(ag, st, "test-model", true)
	var buf strings.Builder
	c.out = &buf
	// Subscribe the CLI to the bus the way Run does: every event is rendered.
	events, cancel := ag.Bus().Subscribe()
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range events {
			c.render(ev)
			if ev.Type == agent.EventTurnDone {
				return
			}
		}
	}()

	ag.SubmitFrom("cli", "ask")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the model call never started")
	}
	// Wait for the console to have drawn part of the reply: only a block that is
	// open holds an injected message back.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		streaming := c.streaming
		c.mu.Unlock()
		if streaming {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the console never rendered a streamed chunk")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ag.SubmitFrom("web", "steer from web")
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never finished")
	}

	out := buf.String()
	first := strings.Index(out, "first part and rest")
	message := strings.Index(out, "you(web)> steer from web")
	second := strings.LastIndex(out, "first part and rest")
	if first < 0 || message < 0 || second <= first {
		t.Fatalf("output = %q, want two rendered replies with the message between them", out)
	}
	if !(first < message && message < second) {
		t.Fatalf("the injected message must sit between the two rounds: %q", out)
	}
	if n := strings.Count(out, "you(web)> steer from web"); n != 1 {
		t.Fatalf("the injected message was drawn %d times: %q", n, out)
	}
}

// TestPendingSendIsShownAboveThePrompt pins the display of a message typed while
// the turn is running: the prompt region carries its text dim and marked
// "(pending)" right away, while the transcript row is only written when the agent
// sends it — which keeps the rows in the order the model receives them.
func TestPendingSendIsShownAboveThePrompt(t *testing.T) {
	noColors(t)
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true

	c.queuePendingSend("later")
	if out := buf.String(); !strings.Contains(out, "> later"+pendingSuffix) {
		t.Fatalf("the pending line is missing: %q", out)
	}
	if len(c.pendingSends) != 1 {
		t.Fatalf("pending sends = %d, want 1", len(c.pendingSends))
	}

	// The agent sends it: the official row is written and the pending line goes.
	buf.Reset()
	c.render(agent.Event{Type: agent.EventUser, Text: "later", Source: "cli"})
	out := buf.String()
	if !strings.Contains(out, "> later\n") {
		t.Fatalf("the official row is missing: %q", out)
	}
	if strings.Contains(out, pendingSuffix) {
		t.Fatalf("the pending line is still drawn: %q", out)
	}
	if len(c.pendingSends) != 0 {
		t.Fatalf("pending sends = %d, want none", len(c.pendingSends))
	}

	// A mode without a prompt region (the line scanner) prints the dim note
	// instead: there is no region to show a pending line in.
	scanner := newTestCLI(t)
	var scannerBuf strings.Builder
	scanner.out = &scannerBuf
	scanner.queuePendingSend("later")
	if out := scannerBuf.String(); !strings.Contains(out, "queued") {
		t.Fatalf("the scanner mode printed no note: %q", out)
	}
	if len(scanner.pendingSends) != 0 {
		t.Fatalf("the scanner mode queued a pending line: %d", len(scanner.pendingSends))
	}
}

// TestAnimationRepaintsInPlace pins that a repaint only rewrites the rows that
// changed and never erases the region first: blanking it makes legacy Windows
// consoles flash on every animation step.
func TestAnimationRepaintsInPlace(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true
	c.busy = true
	c.spinner = spinnerFrames[0]
	c.preview = "partial line"

	c.mu.Lock()
	c.printPromptLocked()
	buf.Reset()
	// The next animation frame: only the prompt row changes.
	c.spinner = spinnerFrames[1]
	c.printPromptLocked()
	c.mu.Unlock()

	out := buf.String()
	if strings.Contains(out, "\x1b[J") {
		t.Fatalf("the animation erased the region: %q", out)
	}
	if strings.Contains(out, "partial line") {
		t.Fatalf("the unchanged preview row was rewritten: %q", out)
	}
	if !strings.Contains(out, spinnerFrames[1]) {
		t.Fatalf("the new frame is missing: %q", out)
	}
	if !strings.HasSuffix(out, caretBackSeq()) {
		t.Fatalf("the caret is not parked at the input start: %q", out)
	}
}

// TestRepaintParksCaretWhenNothingChanged pins the caret of a repaint that
// rewrites no row at all (an event without output): rows that did not change are
// left alone, so the caret has to be placed from the row start instead of being
// inherited from wherever the terminal happened to leave it.
func TestRepaintParksCaretWhenNothingChanged(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true

	c.mu.Lock()
	c.printPromptLocked()
	buf.Reset()
	c.printPromptLocked()
	c.mu.Unlock()

	out := buf.String()
	if strings.Contains(out, promptLabel) {
		t.Fatalf("an unchanged row was rewritten: %q", out)
	}
	forward := fmt.Sprintf("\x1b[%dC", displayColumns(promptLabel)+displayColumns(emptyPromptHint))
	if !strings.Contains(out, forward) {
		t.Fatalf("the caret is not measured from the row start (%q): %q", forward, out)
	}
	if !strings.HasSuffix(out, caretBackSeq()) {
		t.Fatalf("the caret must still be parked at the input start: %q", out)
	}
}

// TestPromptShrinkClearsTheStaleRow pins the other half of the in-place repaint:
// when the region loses a row (the preview line was committed) the row it no
// longer covers is cleared.
func TestPromptShrinkClearsTheStaleRow(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true
	c.preview = "pending tail"

	c.mu.Lock()
	c.printPromptLocked()
	buf.Reset()
	c.preview = ""
	c.printPromptLocked()
	c.mu.Unlock()

	out := buf.String()
	if !strings.Contains(out, "\x1b[J") {
		t.Fatalf("the freed row was not erased: %q", out)
	}
	if !strings.Contains(out, promptLabel) {
		t.Fatalf("the prompt row was not repainted: %q", out)
	}
}

// TestRenderErasesTheRegionOnlyWhenItWrites pins the deferred erasure: an event
// that produces no output leaves the region alone (so it is just repainted),
// while an event with output takes the region over before printing.
func TestRenderErasesTheRegionOnlyWhenItWrites(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true

	c.mu.Lock()
	c.printPromptLocked()
	c.mu.Unlock()

	buf.Reset()
	c.render(agent.Event{Type: agent.EventUsage, Tokens: 1, ContextWindow: 2})
	if out := buf.String(); strings.Contains(out, "\x1b[J") {
		t.Fatalf("an event that writes nothing erased the region: %q", out)
	}

	buf.Reset()
	c.render(agent.Event{Type: agent.EventInfo, Text: "hello"})
	out := buf.String()
	erase := strings.Index(out, "\x1b[J")
	text := strings.Index(out, "[info] hello")
	if erase < 0 || text < 0 {
		t.Fatalf("both the erase and the info row are expected: %q", out)
	}
	if erase > text {
		t.Fatalf("the region must be erased before the event text: %q", out)
	}
}

// TestSpinnerFramesAreOneColumnWide guards the caret bookkeeping: whichever
// animation is in use, a frame occupies exactly one column and one row.
func TestSpinnerFramesAreOneColumnWide(t *testing.T) {
	for _, frames := range [][]string{brailleSpinnerFrames, asciiSpinnerFrames} {
		if len(frames) < 2 {
			t.Fatalf("an animation needs at least two frames: %q", frames)
		}
		for _, frame := range frames {
			if got := displayColumns(frame); got != 1 {
				t.Fatalf("frame %q is %d columns wide, want 1", frame, got)
			}
			if strings.ContainsAny(frame, "\r\n") {
				t.Fatalf("frame %q spans more than one row", frame)
			}
		}
	}
}

// TestPromptShrinkKeepsTheLastRow pins the caret handling of a shrink: a row
// that did not change is not rewritten, so the stale rows are erased from the
// end of the last surviving row and never from the middle of it.
func TestPromptShrinkKeepsTheLastRow(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true
	c.input = []rune("first\nsecond")

	c.mu.Lock()
	c.printPromptLocked()
	buf.Reset()
	// Dropping the second line leaves the (unchanged) first row in place.
	c.input = []rune("first")
	c.printPromptLocked()
	c.mu.Unlock()

	out := buf.String()
	// The caret is parked at the end of the surviving row, which is measured on
	// the plain text (the label plus the input) of that row.
	wantCaret := fmt.Sprintf("\r\x1b[%dC\x1b[J", displayColumns(promptLabel)+displayColumns("first"))
	if !strings.Contains(out, wantCaret) {
		t.Fatalf("unexpected shrink sequence (%q): %q", wantCaret, out)
	}
	if strings.Contains(out, promptLabel) {
		t.Fatalf("the unchanged row was rewritten: %q", out)
	}
}

// TestMultiLineInputIsIndented pins the block layout: continuation lines of a
// multi-line message line up under the first one, both while it is being typed
// and once it is echoed into the scrollback.
func TestMultiLineInputIsIndented(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true
	c.input = []rune("first\nsecond")

	c.mu.Lock()
	c.printPromptLocked()
	c.mu.Unlock()

	if !strings.Contains(buf.String(), "first\r\n"+inputIndent+"second") {
		t.Fatalf("the prompt does not indent the continuation line: %q", buf.String())
	}
	if c.promptRows != 1 {
		t.Fatalf("promptRows = %d, want 1", c.promptRows)
	}

	if !strings.Contains(userLine("first\nsecond"), "first\n"+inputIndent+"second") {
		t.Fatalf("the echoed message does not indent the continuation line: %q", userLine("first\nsecond"))
	}
}

// narrowTerminal lays the prompt region out for a fixed terminal width, so the
// wrapping can be pinned without a real console.
func narrowTerminal(c *CLI, width int) {
	c.termWidth = func() int { return width }
}

// noColors turns the ANSI palette off so the drawn rows compare literally.
func noColors(t *testing.T) {
	t.Helper()
	original := termcolor.Enabled()
	termcolor.SetEnabled(false)
	t.Cleanup(func() { termcolor.SetEnabled(original) })
}

// TestPromptWrapsLongInputRows pins that an input wider than the terminal is
// broken into physical rows by the editor itself. The console must never wrap a
// region row on its own: the region is repainted by moving the caret between its
// rows, so a row the terminal wrapped twice would shift every row below it.
func TestPromptWrapsLongInputRows(t *testing.T) {
	c := newTestCLI(t)
	noColors(t)
	narrowTerminal(c, 10) // 9 columns per row
	c.editing = true
	c.input = []rune("abcdefghijkl")

	rows := c.promptRegionLocked()
	want := []string{promptLabel + "abcdefg", inputIndent + "hijkl"}
	if len(rows) != len(want) {
		t.Fatalf("an over-wide input was drawn as %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i, row := range rows {
		if row.text != want[i] {
			t.Fatalf("row %d = %q, want %q", i, row.text, want[i])
		}
		if limit := c.rowLimit(); row.width > limit {
			t.Fatalf("row %d is %d columns wide, over the %d column limit: %q", i, row.width, limit, row.text)
		}
	}
}

// TestPromptWrapKeepsWideRunesIntact pins the wrap point of a Chinese input: a
// double-width rune starts the next row instead of straddling the limit, so the
// rows still occupy exactly one terminal row each.
func TestPromptWrapKeepsWideRunesIntact(t *testing.T) {
	c := newTestCLI(t)
	noColors(t)
	narrowTerminal(c, 6) // 5 columns per row
	c.editing = true
	c.input = []rune("中文中文")

	rows := c.promptRegionLocked()
	want := []string{
		promptLabel + "中",
		inputIndent + "文",
		inputIndent + "中",
		inputIndent + "文",
	}
	if len(rows) != len(want) {
		t.Fatalf("wide input drawn as %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	rebuilt := strings.TrimPrefix(rows[0].text, promptLabel)
	for i, row := range rows {
		if row.text != want[i] {
			t.Fatalf("row %d = %q, want %q", i, row.text, want[i])
		}
		if limit := c.rowLimit(); row.width > limit {
			t.Fatalf("row %d is %d columns wide, over the %d column limit: %q", i, row.width, limit, row.text)
		}
		if i > 0 {
			rebuilt += strings.TrimPrefix(row.text, inputIndent)
		}
	}
	if rebuilt != "中文中文" {
		t.Fatalf("the wrapped rows rebuild %q, want the input %q", rebuilt, "中文中文")
	}
}

// TestPromptRowsRebuildTheInput pins the wrap invariant over a range of widths
// and inputs: wrapping only breaks rows at the terminal width (never inside a
// rune and never dropping one), and the width recorded for a row is exactly what
// that row draws, which is what the caret is placed from.
func TestPromptRowsRebuildTheInput(t *testing.T) {
	c := newTestCLI(t)
	noColors(t)
	c.editing = true

	inputs := []string{
		"short",
		strings.Repeat("x", 200),
		strings.Repeat("中文", 40),
		strings.Repeat("mixed 混合 text ", 10),
		"first line\nsecond line long enough to wrap several times over\n\nlast",
	}
	for _, width := range []int{4, 8, 13, 41, 80} {
		narrowTerminal(c, width)
		for _, in := range inputs {
			c.input = []rune(in)
			rows := c.promptRegionLocked()
			if len(rows) == 0 {
				t.Fatalf("width %d, input %q: the region is empty", width, in)
			}
			rebuilt := ""
			// A row is allowed to reach past the limit only on a terminal where
			// not even one wide rune fits next to the prompt label.
			floor := displayColumns(promptLabel) + 2
			for i, row := range rows {
				if want := displayColumns(row.text); row.width != want {
					t.Fatalf("width %d, input %q: row %d declares %d columns but draws %d: %q",
						width, in, i, row.width, want, row.text)
				}
				if limit := c.rowLimit(); row.width > limit && row.width > floor {
					t.Fatalf("width %d, input %q: row %d is %d columns wide, so the console would wrap it: %q",
						width, in, i, row.width, row.text)
				}
				prefix := inputIndent
				if i == 0 {
					prefix = promptLabel
				}
				rebuilt += strings.TrimPrefix(row.text, prefix)
			}
			if want := strings.ReplaceAll(in, "\n", ""); rebuilt != want {
				t.Fatalf("width %d: the wrapped rows rebuild %q, want %q", width, rebuilt, want)
			}
		}
	}
}

// TestWrappedInputRepaintsInPlace pins the reported symptom: once the input
// spans more than one terminal row, typing one more character must not reprint
// the rows above it (the console then showed a duplicate of them on every
// keystroke).
func TestWrappedInputRepaintsInPlace(t *testing.T) {
	c := newTestCLI(t)
	noColors(t)
	narrowTerminal(c, 10) // 9 columns per row
	var buf strings.Builder
	c.out = &buf
	c.editing = true
	c.lineStart = true
	c.input = []rune("abcdefghi")

	c.mu.Lock()
	c.printPromptLocked()
	c.mu.Unlock()
	if got, want := buf.String(), promptLabel+"abcdefg\r\n"+inputIndent+"hi\r\x1b[4C"; got != want {
		t.Fatalf("first draw = %q, want %q", got, want)
	}
	if c.promptRows != 1 {
		t.Fatalf("promptRows = %d, want 1 (the input wraps into one more row)", c.promptRows)
	}

	buf.Reset()
	c.mu.Lock()
	c.input = append(c.input, 'j')
	c.printPromptLocked()
	c.mu.Unlock()

	// The caret goes back to the region's first row, the unchanged row above the
	// caret is left alone, and only the row that changed is rewritten.
	if got, want := buf.String(), "\r\x1b[1A\r\n"+inputIndent+"hij\r\x1b[5C"; got != want {
		t.Fatalf("repaint = %q, want %q", got, want)
	}
	if strings.Contains(buf.String(), promptLabel) {
		t.Fatalf("the unchanged row was rewritten: %q", buf.String())
	}
}

// TestPromptStartsAtColumnOne pins that a region drawn while the caret sits
// mid-row starts on its own row from column one: every region row is repainted
// from there, so a row drawn at any other column would shift the whole region.
func TestPromptStartsAtColumnOne(t *testing.T) {
	c := newTestCLI(t)
	noColors(t)
	var buf strings.Builder
	c.out = &buf
	c.editing = true
	c.lineStart = false // streamed text left the caret mid-row

	c.mu.Lock()
	c.printPromptLocked()
	c.mu.Unlock()

	if !strings.HasPrefix(buf.String(), "\r\n") {
		t.Fatalf("the region does not start on a fresh row: %q", buf.String())
	}
}

// TestEmptyPromptHintIsDroppedWhenItDoesNotFit pins that the gray send hint can
// not wrap the prompt row on a terminal too narrow for it; the caret is then
// parked at the end of the label instead of over the missing hint.
func TestEmptyPromptHintIsDroppedWhenItDoesNotFit(t *testing.T) {
	c := newTestCLI(t)
	noColors(t)
	narrowTerminal(c, 8) // 7 columns per row, the hint needs 22
	var buf strings.Builder
	c.out = &buf
	c.editing = true
	c.lineStart = true

	c.mu.Lock()
	c.printPromptLocked()
	c.mu.Unlock()

	if strings.Contains(buf.String(), emptyPromptHint) {
		t.Fatalf("the hint does not fit but was drawn: %q", buf.String())
	}
	if got, want := buf.String(), promptLabel+"\r\x1b[2C"; got != want {
		t.Fatalf("prompt row = %q, want %q (the bare label and the caret after it)", got, want)
	}
}

// TestToolRowKeepsNameAndArgs pins the tool call row: the function name gets its
// own highlighted line and every argument is shown as "name - value" without the
// JSON wrapper, while a call without arguments still names the tool.
func TestToolRowKeepsNameAndArgs(t *testing.T) {
	original := termcolor.Enabled()
	termcolor.SetEnabled(false)
	t.Cleanup(func() { termcolor.SetEnabled(original) })

	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.lineStart = true

	c.render(agent.Event{Type: agent.EventToolCall, Name: "read_file", Args: `{"path":"a.txt"}`})
	got := buf.String()
	if want := "[tool] read_file\npath - a.txt\n"; got != want {
		t.Fatalf("tool row = %q, want %q", got, want)
	}
	if strings.Contains(got, `{"path"`) {
		t.Fatalf("tool row leaks the JSON wrapper: %q", got)
	}

	buf.Reset()
	c.render(agent.Event{Type: agent.EventToolCall, Name: "exec_command", Args: `{"command":"ls","timeout_seconds":30,"flag":true}`})
	if want := "[tool] exec_command\ncommand - ls\ntimeout_seconds - 30\nflag - true\n"; buf.String() != want {
		t.Fatalf("multi-argument tool row = %q, want %q", buf.String(), want)
	}

	buf.Reset()
	c.render(agent.Event{Type: agent.EventToolCall, Name: "list_tools"})
	if want := "[tool] list_tools\n"; buf.String() != want {
		t.Fatalf("argument-less tool row = %q, want %q", buf.String(), want)
	}
}

// TestToolRowShowsFullValues pins that an argument value is printed in full (no
// truncation) and that a multi-line value keeps its continuation lines aligned
// under the value.
func TestToolRowShowsFullValues(t *testing.T) {
	original := termcolor.Enabled()
	termcolor.SetEnabled(false)
	t.Cleanup(func() { termcolor.SetEnabled(original) })

	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.lineStart = true

	long := strings.Repeat("x", 400)
	c.render(agent.Event{Type: agent.EventToolCall, Name: "write_file", Args: `{"content":"` + long + `"}`})
	if got, want := buf.String(), "[tool] write_file\ncontent - "+long+"\n"; got != want {
		t.Fatalf("long value was not printed in full (got %d bytes)", len(got))
	}

	buf.Reset()
	c.render(agent.Event{Type: agent.EventToolCall, Name: "exec_command", Args: `{"command":"line1\nline2"}`})
	if got, want := buf.String(), "[tool] exec_command\ncommand\nline1\nline2\n"; got != want {
		t.Fatalf("multi-line value = %q, want %q", got, want)
	}

	// A single-line value stays next to its name even when another argument of
	// the same call is multi-line; neither name nor value is indented.
	buf.Reset()
	c.render(agent.Event{Type: agent.EventToolCall, Name: "write_file", Args: `{"path":"a.txt","content":"l1\nl2"}`})
	if got, want := buf.String(), "[tool] write_file\npath - a.txt\ncontent\nl1\nl2\n"; got != want {
		t.Fatalf("mixed argument row = %q, want %q", got, want)
	}
}

// TestToolArgumentsKeepsOrderAndRawValues pins the argument parsing behind the
// tool row: order is preserved, strings are unquoted and nested containers stay
// compact JSON, while a non-object payload is rejected for the raw fallback.
func TestToolArgumentsKeepsOrderAndRawValues(t *testing.T) {
	args, ok := toolArguments(`{"command":"echo hi","n":30,"ok":true,"paths":["a","b"],"cfg":{"k":"v"}}`)
	if !ok {
		t.Fatal("toolArguments rejected a JSON object")
	}
	want := []toolArg{
		{Name: "command", Value: "echo hi"},
		{Name: "n", Value: "30"},
		{Name: "ok", Value: "true"},
		{Name: "paths", Value: `["a","b"]`},
		{Name: "cfg", Value: `{"k":"v"}`},
	}
	if len(args) != len(want) {
		t.Fatalf("args = %+v, want %+v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %+v, want %+v", i, args[i], want[i])
		}
	}

	if _, ok := toolArguments("not json"); ok {
		t.Fatal("toolArguments accepted a non-JSON payload")
	}
	if _, ok := toolArguments(""); ok {
		t.Fatal("toolArguments accepted an empty payload")
	}
	if got, ok := toolArguments("{}"); !ok || len(got) != 0 {
		t.Fatalf("toolArguments({}) = %+v, %v; want empty, true", got, ok)
	}
}

// TestBannerAlignsFields pins the banner layout: the label column is fixed so
// the values line up, and the web URL is only shown while the server is on.
func TestBannerAlignsFields(t *testing.T) {
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.Banner("cfg.yaml", "state-dir", "127.0.0.1", 8080)

	out := buf.String()
	field := func(label string) string { return "  " + fmt.Sprintf("%-9s", label) + " " }
	for _, want := range [][2]string{
		{"model", "test-model"},
		{"config", "cfg.yaml"},
		{"state dir", "state-dir"},
		{"web", "http://127.0.0.1:8080/"},
	} {
		if !strings.Contains(out, field(want[0])+want[1]) {
			t.Fatalf("banner is missing the %s field:\n%s", want[0], out)
		}
	}
	if !strings.Contains(out, "/help or /?") {
		t.Fatalf("banner is missing the tip line:\n%s", out)
	}

	buf.Reset()
	c.Banner("cfg.yaml", "state-dir", "", 0)
	if strings.Contains(buf.String(), "http://") {
		t.Fatalf("banner shows a web URL while the server is off:\n%s", buf.String())
	}
}
