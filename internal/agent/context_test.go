package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"lightagent/internal/config"
	"lightagent/internal/llm"
)

// userRunes builds a user message whose content has exactly n runes.
func userRunes(prefix string, n int) llm.Message {
	pad := n - len(prefix)
	if pad < 0 {
		pad = 0
	}
	return llm.Message{Role: "user", Content: prefix + strings.Repeat("x", pad)}
}

// singleTurnMessages builds n user messages, each its own single-row turn.
func singleTurnMessages(n int) []llm.Message {
	msgs := make([]llm.Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, llm.Message{Role: "user", Content: fmt.Sprintf("m%d", i)})
	}
	return msgs
}

// exactTokenMessage builds a user message whose EstimateMessageTokens is tokens:
// one unbroken run counts one unit per four characters (see estimateUnits).
func exactTokenMessage(tokens int) llm.Message {
	if tokens < 1 {
		tokens = 1
	}
	return llm.Message{Role: "user", Content: strings.Repeat("x", tokens*4)}
}

// budgetMessages builds n single-message turns each estimated at exactly tokens.
func budgetMessages(n, tokens int) []llm.Message {
	msgs := make([]llm.Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, exactTokenMessage(tokens))
	}
	return msgs
}

func TestEstimateMessageTokens(t *testing.T) {
	// One unit per whitespace-separated run, one per CJK character, and a run
	// longer than four characters costs one unit per four characters.
	cases := []struct {
		name    string
		content string
		want    int
	}{
		{"empty", "", 0},
		{"words", "the quick brown fox", 4},
		{"punctuation rides with the run it touches", "hello, world!", 2},
		{"chinese per character", "上下文压缩", 5},
		{"runs and characters mixed", "压缩 context is full", 5},
		{"a long unbroken run is not one token", strings.Repeat("x", 88), 22},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EstimateMessageTokens(llm.Message{Role: "user", Content: tc.content}); got != tc.want {
				t.Fatalf("estimate of %q = %d, want %d", tc.content, got, tc.want)
			}
		})
	}

	// Every field that travels on the wire counts, tool calls included.
	withCall := llm.Message{
		Role:    "assistant",
		Content: "x",
		ToolCalls: []llm.ToolCall{{
			ID: "call_1", Type: "function",
			Function: llm.ToolCallFunction{Name: "run_script", Arguments: "{}"},
		}},
	}
	if got, plain := EstimateMessageTokens(withCall), EstimateMessageTokens(llm.Message{Role: "assistant", Content: "x"}); got <= plain {
		t.Fatalf("tool call tokens not counted: %d <= %d", got, plain)
	}
}

func TestParseTurnBoundaries(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user"}, {Role: "assistant"}, {Role: "tool"},
		{Role: "user"}, {Role: "assistant"},
	}
	got := parseTurnBoundaries(msgs)
	if len(got) != 2 || got[0] != 0 || got[1] != 3 {
		t.Fatalf("boundaries = %v, want [0 3]", got)
	}
}

// TestRetentionPolicyComesFromConfig pins where the retention policy of a pass
// comes from: the automatic and the manual pass read the configured share and turn
// cap (which default to keeping nothing — some inference engines do not preserve
// their prompt cache across the rollback a retention performs, see
// config.SummarizeKeepPolicy), while the overflow recovery keeps nothing whatever
// the configuration says.
func TestRetentionPolicyComesFromConfig(t *testing.T) {
	c := &compactor{
		contextWindow: 10000,
		maxTokens:     1000,
		keepAuto:      config.SummarizeKeepPolicy{BudgetPercent: 50, Turns: 3},
		keepManual:    config.SummarizeKeepPolicy{BudgetPercent: 20, Turns: 2},
	}
	// The share is taken from the available input budget (window - output
	// reserve).
	if got := c.retentionBudget(summarizeModeAuto); got != 4500 {
		t.Fatalf("auto budget = %d, want 4500 (50%% of 9000)", got)
	}
	if got := c.retentionBudget(summarizeModeManual); got != 1800 {
		t.Fatalf("manual budget = %d, want 1800 (20%% of 9000)", got)
	}
	if got := c.maxKeptTurns(summarizeModeAuto); got != 3 {
		t.Fatalf("auto turn cap = %d, want 3", got)
	}
	if got := c.maxKeptTurns(summarizeModeManual); got != 2 {
		t.Fatalf("manual turn cap = %d, want 2", got)
	}
	// maxTokens >= contextWindow falls back to the full context window.
	c2 := &compactor{contextWindow: 1000, maxTokens: 5000, keepAuto: config.SummarizeKeepPolicy{BudgetPercent: 50}}
	if got := c2.retentionBudget(summarizeModeAuto); got != 500 {
		t.Fatalf("fallback budget = %d, want 500", got)
	}

	// The overflow recovery has no setting: the provider has just proven the
	// estimate too small there, so only the summary may survive the retry.
	if got := c.retentionBudget(summarizeModeOverflow); got != 0 {
		t.Fatalf("overflow budget = %d, want 0", got)
	}
	if got := c.maxKeptTurns(summarizeModeOverflow); got != 0 {
		t.Fatalf("overflow turn cap = %d, want 0", got)
	}

	// The built-in policy keeps nothing: a compactor built from the defaults
	// summarizes the whole history, and a zero share is safe before any
	// arithmetic runs (an unset window included).
	for _, zero := range []*compactor{{}, {contextWindow: 1000, maxTokens: 5000}, {contextWindow: 10000, maxTokens: 1000}} {
		for _, mode := range []summarizeMode{summarizeModeAuto, summarizeModeManual, summarizeModeOverflow} {
			if got := zero.retentionBudget(mode); got != 0 {
				t.Fatalf("default budget (mode %d) = %d, want 0", mode, got)
			}
			if got := zero.maxKeptTurns(mode); got != 0 {
				t.Fatalf("default turn cap (mode %d) = %d, want 0", mode, got)
			}
		}
	}
}

// TestEveryPassCutsTheWholeHistory pins what the default retention policy means
// for a pass: every mode condenses the entire history, the newest turn included,
// so the summary is all that survives and the request that follows is the summary
// plus the engine's continue marker.
func TestEveryPassCutsTheWholeHistory(t *testing.T) {
	c := &compactor{contextWindow: 100000, maxTokens: 1000}
	// Three short turns: they fit any window, yet a zero retention budget keeps
	// none of them.
	msgs := budgetMessages(3, 40)
	for _, mode := range []summarizeMode{summarizeModeAuto, summarizeModeManual, summarizeModeOverflow} {
		cut, ok := c.cut(msgs, mode)
		if !ok {
			t.Fatalf("mode %d condensed nothing: it must cut the whole history", mode)
		}
		if cut != len(msgs) {
			t.Fatalf("mode %d cut = %d, want %d (whole history)", mode, cut, len(msgs))
		}
	}
}

// TestOverflowRollbackCut pins how much of a rejected request's tail the recovery
// rolls back: the assistant/tool rows this turn produced and the user messages no
// answer followed (the turn's own messages plus a steering message folded in before
// the failing call). Everything before them — the answered history the pass is
// about to compress — stays, and the rolled-back user messages are what the replay
// puts back on top of the summary.
func TestOverflowRollbackCut(t *testing.T) {
	toolRow := llm.Message{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("x", 5000)}
	cases := []struct {
		name    string
		history []llm.Message
		want    int
	}{
		{
			name: "tool feedback of the running turn",
			history: []llm.Message{
				{Role: "user", Content: "one"},
				{Role: "assistant", Content: "answer"},
				{Role: "user", Content: "read the log"},
				{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1"}}},
				toolRow,
			},
			want: 2,
		},
		{
			name: "steering message folded in before the failing call",
			history: []llm.Message{
				{Role: "user", Content: "read the log"},
				{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1"}}},
				toolRow,
				{Role: "user", Content: "also check the tail"},
			},
			want: 3,
		},
		{
			name: "whole start batch of the turn",
			history: []llm.Message{
				{Role: "user", Content: "one"},
				{Role: "user", Content: "two"},
				{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1"}}},
				toolRow,
			},
			want: 0,
		},
		{
			name:    "the turn's user message alone",
			history: []llm.Message{{Role: "user", Content: "hello"}},
			want:    0,
		},
		{
			name:    "nothing to roll back",
			history: nil,
			want:    0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := overflowRollbackCut(tc.history); got != tc.want {
				t.Fatalf("overflowRollbackCut = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSummarizeTailCutTurnCapBinds pins the algorithm's turn cap, which the
// retention policy feeds a zero (see TestSummarizeTailCutZeroBudgetCutsWholeTail):
// with a budget far larger than the history, the walk keeps exactly the newest
// maxKeptTurns user messages.
func TestSummarizeTailCutTurnCapBinds(t *testing.T) {
	cases := []struct {
		name         string
		turns        int
		maxKeptTurns int
	}{
		{"three of twelve", 12, 3},
		{"two of eight", 8, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := singleTurnMessages(tc.turns)
			safeCut, ok := summarizeTailCut(msgs, 1<<30, tc.maxKeptTurns)
			if !ok {
				t.Fatal("expected a cut to be possible with a large budget")
			}
			if want := len(msgs) - tc.maxKeptTurns; safeCut != want {
				t.Fatalf("safeCut = %d, want %d (keep the newest %d user messages of %d)",
					safeCut, want, tc.maxKeptTurns, tc.turns)
			}
		})
	}
}

// TestSummarizeTailCutZeroBudgetCutsWholeTail pins the input the retention policy
// hands the algorithm: a zero token budget (and the turn cap the policy zeroes as
// well, which summarizeTailCut lifts to its minimum of one) leaves no turn inside
// the window, so the whole history is compressed.
func TestSummarizeTailCutZeroBudgetCutsWholeTail(t *testing.T) {
	c := &compactor{} // the default policy: keeps nothing
	msgs := singleTurnMessages(3)
	safeCut, ok := summarizeTailCut(msgs, c.retentionBudget(summarizeModeAuto), c.maxKeptTurns(summarizeModeAuto))
	if !ok {
		t.Fatal("expected a zero budget to condense everything")
	}
	if safeCut != len(msgs) {
		t.Fatalf("safeCut = %d, want %d (whole history)", safeCut, len(msgs))
	}
}

func TestSummarizeTailCutEverythingFitsNoop(t *testing.T) {
	msgs := singleTurnMessages(3)
	if safeCut, ok := summarizeTailCut(msgs, 1<<30, 3); ok || safeCut != 0 {
		t.Fatalf("expected no-op (safeCut=0, ok=false), got safeCut=%d ok=%v", safeCut, ok)
	}
}

func TestSummarizeTailCutTurnCapCountsUserMessagesNotRows(t *testing.T) {
	// 7 turns, each spanning two rows; the cap must bind on turns, not rows.
	var msgs []llm.Message
	for i := 0; i < 7; i++ {
		msgs = append(msgs,
			llm.Message{Role: "user", Content: fmt.Sprintf("u%d", i)},
			llm.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i)},
		)
	}
	safeCut, ok := summarizeTailCut(msgs, 1<<30, 2)
	if !ok {
		t.Fatal("expected a cut to be possible with a large budget")
	}
	if safeCut != 10 {
		t.Fatalf("safeCut = %d, want 10 (keep newest 2 of 7 user messages)", safeCut)
	}
	if msgs[safeCut].Role != "user" {
		t.Fatalf("retained window must start at a user message, got %q", msgs[safeCut].Role)
	}
}

func TestSummarizeTailCutRetainsWholeTurnIncludingToolRows(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "new question"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_new", Type: "function",
			Function: llm.ToolCallFunction{Name: "run_script", Arguments: "{}"},
		}}},
		{Role: "tool", ToolCallID: "call_new", Content: "tool output"},
	}
	safeCut, ok := summarizeTailCut(msgs, 1<<30, 1)
	if !ok {
		t.Fatal("expected a cut to be possible with a large budget")
	}
	if safeCut != 2 {
		t.Fatalf("safeCut = %d, want 2 (start of the newest user turn)", safeCut)
	}
	kept := msgs[safeCut:]
	if len(kept) != 3 {
		t.Fatalf("retained rows = %d, want 3 (whole newest turn)", len(kept))
	}
	if kept[0].Role != "user" || kept[1].ToolCalls[0].ID != "call_new" || kept[2].ToolCallID != "call_new" {
		t.Fatalf("tool-call pair was split: %+v", kept)
	}
}

func TestSummarizeTailCutBudgetBelowNewestTurnCutsWholeTail(t *testing.T) {
	msgs := budgetMessages(4, 40)
	safeCut, ok := summarizeTailCut(msgs, 30, 3)
	if !ok {
		t.Fatal("expected an over-budget tail to be summarizable")
	}
	if safeCut != len(msgs) {
		t.Fatalf("safeCut = %d, want %d (whole tail compressed)", safeCut, len(msgs))
	}
}

func TestSummarizeTailCutBudgetHoldsOnlyNewestTurn(t *testing.T) {
	msgs := budgetMessages(4, 40)
	// The newest turn (40 tokens) fits, but adding the next older one (80 >= 60)
	// does not, so exactly the newest turn stays raw.
	safeCut, ok := summarizeTailCut(msgs, 60, 3)
	if !ok {
		t.Fatal("expected a cut to be possible")
	}
	if want := len(msgs) - 1; safeCut != want {
		t.Fatalf("safeCut = %d, want %d (keep only the newest turn)", safeCut, want)
	}
}

// TestShouldCompact pins the trigger arithmetic only: the caller measures the
// request (see Agent.contextTokensLocked) and passes that number in, so a pass can
// never fire on a competing estimate in different units.
func TestShouldCompact(t *testing.T) {
	c := &compactor{contextWindow: 1000, summarizeTokenPercent: 50}
	if c.shouldCompact(0) {
		t.Fatal("an empty context should not trigger compaction")
	}
	if c.shouldCompact(499) {
		t.Fatal("just below the trigger should not compact")
	}
	if !c.shouldCompact(500) {
		t.Fatal("reaching the trigger should compact")
	}
	if !c.shouldCompact(1500) {
		t.Fatal("a context past the trigger should compact")
	}
	// A percentage that leaves no usable limit falls back to 3/4 of the window.
	c2 := &compactor{contextWindow: 1000}
	if c2.shouldCompact(749) || !c2.shouldCompact(750) {
		t.Fatal("the fallback trigger is not 3/4 of the context window")
	}
}

// TestEnsureUserMessage pins the marker the engine inserts after a pass that
// leaves no user message behind: chat templates reject a request whose messages
// hold no user query.
func TestEnsureUserMessage(t *testing.T) {
	if got := ensureUserMessage(nil); len(got) != 1 ||
		got[0].Role != "user" || got[0].Content != contextContinueMessage {
		t.Fatalf("empty tail = %+v, want the engine continue marker", got)
	}

	// A tail without a user message gets the marker appended, so an assistant
	// tool_call / tool result pair in front of it stays intact.
	orphan := []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function"}}},
		{Role: "tool", ToolCallID: "c1"},
	}
	got := ensureUserMessage(orphan)
	if len(got) != 3 || got[0].Role != "assistant" || got[1].Role != "tool" {
		t.Fatalf("orphan tail = %+v, want the original rows first", got)
	}
	if got[2].Role != "user" || got[2].Content != contextContinueMessage {
		t.Fatalf("orphan tail last row = %+v, want the engine continue marker", got[2])
	}
	if len(orphan) != 2 {
		t.Fatalf("input slice was grown in place: %+v", orphan)
	}

	// A tail that already holds a user message is left alone.
	withUser := []llm.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "answer"}}
	if same := ensureUserMessage(withUser); len(same) != 2 || same[0].Content != "hi" {
		t.Fatalf("tail with a user message = %+v, want it unchanged", same)
	}
}

// TestSummaryMessage pins the standalone summary message: its own user message,
// always introduced by the [engine] tag, and nothing at all when the summary is
// blank.
func TestSummaryMessage(t *testing.T) {
	if _, ok := summaryMessage("   "); ok {
		t.Fatal("a blank summary must produce no message")
	}
	msg, ok := summaryMessage("  sum  ")
	if !ok || msg.Role != "user" || msg.Content != summaryUserPrefix+"sum" {
		t.Fatalf("summaryMessage = %+v ok=%v, want the [engine] block", msg, ok)
	}
}

// TestHeadMessagesPlacement pins the two layouts a request head can have: the
// default one sends the summary as its own message right after the system message
// (in front of the history), the configured one leaves it to the (already
// rendered) system prompt.
func TestHeadMessagesPlacement(t *testing.T) {
	rest := []llm.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yo"}}

	got := headMessages("sys", rest, "sum", false)
	if len(got) != 4 {
		t.Fatalf("default head = %+v, want system + summary + history", got)
	}
	if got[0].Role != "system" || got[0].Content != "sys" {
		t.Fatalf("first message = %+v, want the system prompt", got[0])
	}
	if got[1].Role != "user" || got[1].Content != summaryUserPrefix+"sum" {
		t.Fatalf("second message = %+v, want the [engine] summary message", got[1])
	}
	// The history rows follow the summary in their recorded order.
	if got[2].Content != "hi" || got[3].Content != "yo" {
		t.Fatalf("history rows = %+v, want them after the summary", got[2:])
	}

	// A blank summary adds no message.
	if got := headMessages("sys", rest, "  ", false); len(got) != 3 || got[1].Content != "hi" {
		t.Fatalf("blank summary head = %+v, want just the history behind the system prompt", got)
	}

	got = headMessages("sys", rest, "sum", true)
	if len(got) != 3 || got[0].Content != "sys" || got[1].Content != "hi" {
		t.Fatalf("system-prompt head = %+v, want the system prompt alone to carry it", got)
	}
	if rest[0].Content != "hi" {
		t.Fatalf("the input messages were written into: %+v", rest)
	}
}

// TestHeadMessagesSummaryLeadsTheHistory pins the default layout of a request
// that carries a summary and several turns: the summary is the first user
// message, followed by the history rows in their recorded order.
func TestHeadMessagesSummaryLeadsTheHistory(t *testing.T) {
	rest := []llm.Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
		{Role: "user", Content: contextContinueMessage},
	}
	got := headMessages("sys", rest, "sum", false)

	users := 0
	for _, m := range got {
		if m.Role == "user" {
			users++
		}
	}
	if users != 3 {
		t.Fatalf("request user messages = %d, want the two turns plus the summary message: %+v", users, got)
	}
	if got[1].Content != summaryUserPrefix+"sum" {
		t.Fatalf("second message = %+v, want the [engine] summary message", got[1])
	}
	if got[2].Content != "one" || got[3].Content != "two" || got[4].Content != contextContinueMessage {
		t.Fatalf("history rows = %+v, want the recorded rows after the summary", got[2:])
	}
}

// whole tail: the tiny window leaves no room for even the newest turn, so
// nothing but the engine marker may survive — also on the summarize-failure
// fallback, which drops the batch and keeps that marker.
func TestCompactKeepsAUserMessageWhenEverythingIsCut(t *testing.T) {
	c := &compactor{contextWindow: 100, maxTokens: 40960}
	hist := []llm.Message{
		userRunes("q", 100),
		{Role: "assistant", Content: "answer"},
	}
	newHist, summary, changed, err := c.compact(context.Background(), hist, livePrefix{}, "carried", summarizeModeAuto)
	if !changed {
		t.Fatal("expected the whole tail to be compressed")
	}
	if err == nil {
		t.Fatal("expected the summarize failure of the client-less compactor")
	}
	if summary != "carried" {
		t.Fatalf("summary = %q, want the untouched one", summary)
	}
	if len(newHist) != 1 || newHist[0].Role != "user" || newHist[0].Content != contextContinueMessage {
		t.Fatalf("history = %+v, want only the engine continue marker", newHist)
	}
}

// TestSummarizeInstruction pins the wording rules of the summarizing instruction:
// it always says the report becomes the only history context of the next
// conversation, and the system-prompt layout — the case that needs spelling out —
// adds the note that the report replaces that section's summary once one exists.
func TestSummarizeInstruction(t *testing.T) {
	const onlyHistory = "the only history context of the next conversation"

	fresh := summarizeInstruction("", false)
	if !strings.Contains(fresh, onlyHistory) {
		t.Fatalf("instruction does not name the report's role:\n%s", fresh)
	}
	for _, absent := range []string{"[engine]", "CONVERSATION SUMMARY", "replaces"} {
		if strings.Contains(fresh, absent) {
			t.Fatalf("instruction without a summary mentions %q:\n%s", absent, fresh)
		}
	}

	// Default layout: the report is the [engine] message the instruction already
	// described, so there is no extra note.
	userMsg := summarizeInstruction("earlier report", false)
	if !strings.Contains(userMsg, onlyHistory) || strings.Contains(userMsg, "replaces") {
		t.Fatalf("instruction for the user-message layout:\n%s", userMsg)
	}

	// System-prompt layout: with a summary in place, the instruction says the
	// report replaces it; without one there is nothing to replace.
	sysPrompt := summarizeInstruction("earlier report", true)
	if !strings.Contains(sysPrompt, `The new report replaces the summary of the "# CONVERSATION SUMMARY" section of the system prompt.`) {
		t.Fatalf("instruction for the system-prompt layout:\n%s", sysPrompt)
	}
	if strings.Contains(sysPrompt, "[engine]") {
		t.Fatalf("instruction for the system-prompt layout:\n%s", sysPrompt)
	}
	cold := summarizeInstruction("", true)
	if strings.Contains(cold, "replaces") {
		t.Fatalf("instruction for an empty system-prompt summary:\n%s", cold)
	}
}

// call cannot be made, so the previous summary stays and the messages are dropped.
func TestCompactKeepsTheSummaryWhenSummarizingFails(t *testing.T) {
	// A compactor without an LLM client fails the summarizing call.
	c := &compactor{contextWindow: 100, maxTokens: 40960}
	hist := []llm.Message{
		{Role: "user", Content: strings.Repeat("q", 200)},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "next"},
	}
	_, summary, changed, err := c.compact(context.Background(), hist, livePrefix{}, "carried over", summarizeModeAuto)
	if err == nil {
		t.Fatal("expected the client-less compactor to report a summarizing failure")
	}
	if !changed {
		t.Fatal("expected the oldest messages to be dropped")
	}
	if summary != "carried over" {
		t.Fatalf("summary = %q, want the previous summary kept", summary)
	}
}

func TestSystemWithSummary(t *testing.T) {
	if got := systemWithSummary("base", "  "); got != "base" {
		t.Fatalf("got %q", got)
	}
	got := systemWithSummary("base", "sum")
	if !strings.Contains(got, "base") || !strings.Contains(got, "sum") {
		t.Fatalf("got %q", got)
	}
}

// TestEstimateMessageTokensCountsMedia verifies a message carrying media is
// estimated above the same message without it, and that the estimate follows the
// number of parts (the encoded bytes themselves are deliberately not counted:
// they are not a token proxy).
func TestEstimateMessageTokensCountsMedia(t *testing.T) {
	plain := llm.Message{Role: "user", Content: "look at this"}
	withMedia := plain
	withMedia.Media = []llm.ContentPart{
		{Type: llm.PartTypeImageURL, ImageURL: &llm.ImageURLPart{URL: "data:image/png;base64," + strings.Repeat("A", 4000)}},
	}
	plainTokens := EstimateMessageTokens(plain)
	mediaTokens := EstimateMessageTokens(withMedia)
	if mediaTokens <= plainTokens {
		t.Fatalf("media estimate = %d, want more than %d", mediaTokens, plainTokens)
	}
	if mediaTokens != plainTokens+mediaPartTokens {
		t.Fatalf("media estimate = %d, want %d (one part, whatever its size)", mediaTokens, plainTokens+mediaPartTokens)
	}

	withMedia.Media = append(withMedia.Media, llm.ContentPart{Type: llm.PartTypeFile, File: &llm.FilePart{Filename: "a.pdf"}})
	if got := EstimateMessageTokens(withMedia); got != plainTokens+2*mediaPartTokens {
		t.Fatalf("two parts estimate = %d, want %d", got, plainTokens+2*mediaPartTokens)
	}
}
