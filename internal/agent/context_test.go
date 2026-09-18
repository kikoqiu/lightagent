package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

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

// exactTokenMessage builds a user message whose EstimateMessageTokens is tokens.
func exactTokenMessage(prefix string, tokens int) llm.Message {
	target := tokens*5/2 - 12
	for runes := target; runes <= target+8; runes++ {
		if m := userRunes(prefix, runes); EstimateMessageTokens(m) == tokens {
			return m
		}
	}
	panic("exactTokenMessage: no rune count produced the requested tokens")
}

// budgetMessages builds n single-message turns each estimated at exactly tokens.
func budgetMessages(n, tokens int) []llm.Message {
	msgs := make([]llm.Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, exactTokenMessage(fmt.Sprintf("m%d", i), tokens))
	}
	return msgs
}

func TestEstimateMessageTokens(t *testing.T) {
	// tokens = (runeCount + 12) * 2 / 5
	if got := EstimateMessageTokens(llm.Message{Role: "user"}); got != 4 {
		t.Fatalf("empty = %d, want 4", got)
	}
	if got := EstimateMessageTokens(userRunes("ab", 88)); got != 40 {
		t.Fatalf("88-rune message = %d, want 40", got)
	}
	withCall := llm.Message{
		Role:    "assistant",
		Content: "x",
		ToolCalls: []llm.ToolCall{{
			ID: "call_1", Type: "function",
			Function: llm.ToolCallFunction{Name: "exec_command", Arguments: "{}"},
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

func TestRetentionBudget(t *testing.T) {
	c := &compactor{contextWindow: 10000, maxTokens: 1000}
	if got := c.retentionBudget(summarizeModeAuto); got != 900 {
		t.Fatalf("auto budget = %d, want 900", got)
	}
	if got := c.retentionBudget(summarizeModeManual); got != 450 {
		t.Fatalf("manual budget = %d, want 450", got)
	}
	// maxTokens >= contextWindow falls back to the full context window.
	c2 := &compactor{contextWindow: 1000, maxTokens: 5000}
	if got := c2.retentionBudget(summarizeModeAuto); got != 100 {
		t.Fatalf("fallback budget = %d, want 100", got)
	}
}

func TestSummarizeTailCutAutoTurnCapThree(t *testing.T) {
	msgs := singleTurnMessages(12) // 12 user messages = 12 turns
	safeCut, ok := summarizeTailCut(msgs, 1<<30, summarizeMaxKeptTurns(summarizeModeAuto))
	if !ok {
		t.Fatal("expected a cut to be possible with a large budget")
	}
	if want := len(msgs) - 3; safeCut != want {
		t.Fatalf("safeCut = %d, want %d (keep the newest 3 user messages of 12)", safeCut, want)
	}
}

func TestSummarizeTailCutManualTurnCapTwo(t *testing.T) {
	msgs := singleTurnMessages(8)
	safeCut, ok := summarizeTailCut(msgs, 1<<30, summarizeMaxKeptTurns(summarizeModeManual))
	if !ok {
		t.Fatal("expected a cut to be possible with a large budget")
	}
	if want := len(msgs) - 2; safeCut != want {
		t.Fatalf("safeCut = %d, want %d (keep the newest 2 user messages of 8)", safeCut, want)
	}
}

func TestSummarizeTailCutEverythingFitsNoop(t *testing.T) {
	msgs := singleTurnMessages(3)
	if safeCut, ok := summarizeTailCut(msgs, 1<<30, summarizeMaxKeptTurns(summarizeModeAuto)); ok || safeCut != 0 {
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
	safeCut, ok := summarizeTailCut(msgs, 1<<30, summarizeMaxKeptTurns(summarizeModeManual))
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
			Function: llm.ToolCallFunction{Name: "exec_command", Arguments: "{}"},
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

func TestShouldCompact(t *testing.T) {
	c := &compactor{contextWindow: 1000, summarizeTokenPercent: 50}
	if c.shouldCompact(nil, livePrefix{}, 0) {
		t.Fatal("empty context should not trigger compaction")
	}
	big := []llm.Message{{Role: "user", Content: strings.Repeat("a", 3000)}}
	if !c.shouldCompact(big, livePrefix{}, 0) {
		t.Fatal("oversized context should trigger compaction")
	}
	// The system prompt counts as context too, capability sections included.
	if !c.shouldCompact(nil, livePrefix{systemPrompt: strings.Repeat("p", 3000)}, 0) {
		t.Fatal("an oversized system prompt should trigger compaction")
	}
	// A reported usage larger than the estimate also triggers.
	if !c.shouldCompact(nil, livePrefix{}, 600) {
		t.Fatal("usage above the limit should trigger compaction")
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
