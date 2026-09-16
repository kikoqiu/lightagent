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
	if c.shouldCompact(nil, "", 0) {
		t.Fatal("empty context should not trigger compaction")
	}
	big := []llm.Message{{Role: "user", Content: strings.Repeat("a", 3000)}}
	if !c.shouldCompact(big, "", 0) {
		t.Fatal("oversized context should trigger compaction")
	}
	// A reported usage larger than the estimate also triggers.
	if !c.shouldCompact(nil, "", 600) {
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

// TestCompactKeepsAUserMessageWhenEverythingIsCut covers the pass that cuts the
// whole tail: the tiny window leaves no room for even the newest turn, so
// nothing but the engine marker may survive — also on the summarize-failure
// fallback, which drops the batch and keeps that marker.
func TestCompactKeepsAUserMessageWhenEverythingIsCut(t *testing.T) {
	c := &compactor{contextWindow: 100, maxTokens: 40960}
	hist := []llm.Message{
		userRunes("q", 100),
		{Role: "assistant", Content: "answer"},
	}
	newHist, summary, changed, err := c.compact(context.Background(), hist, "carried", summarizeModeAuto)
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

func TestMergeSummaries(t *testing.T) {
	if got := mergeSummaries("", "b"); got != "b" {
		t.Fatalf("got %q", got)
	}
	if got := mergeSummaries("a", ""); got != "a" {
		t.Fatalf("got %q", got)
	}
	if got := mergeSummaries("a", "b"); got != "a\n\nb" {
		t.Fatalf("got %q", got)
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
