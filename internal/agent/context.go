package agent

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"lightagent/internal/llm"
)

// EstimateMessageTokens estimates the token cost of one message as it appears
// in the request. The message's
// character count (content, reasoning, tool call name/arguments/id) times 2/5 —
// roughly 2.5 characters per token — plus a small per-message overhead.
func EstimateMessageTokens(m llm.Message) int {
	chars := utf8.RuneCountInString(m.Content)
	chars += utf8.RuneCountInString(m.ReasoningContent)
	chars += utf8.RuneCountInString(m.Name)
	for _, tc := range m.ToolCalls {
		chars += len(tc.ID) + len(tc.Type)
		chars += len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	if m.ToolCallID != "" {
		chars += len(m.ToolCallID)
	}
	chars += 12 // per-message JSON/role overhead
	return chars * 2 / 5
}

// EstimateMessagesTokens sums EstimateMessageTokens over a message list.
func EstimateMessagesTokens(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += EstimateMessageTokens(m)
	}
	return total
}

// contextContinueMessage is the user message the engine inserts when a pass
// leaves no user message behind. Compaction may drop every message, including
// the user turn that started the running loop, but chat templates reject a
// request whose messages hold no user query — so the engine adds this marker
// and the conversation carries on from the summary alone.
const contextContinueMessage = "[engine] Context summarized, continue."

// ensureUserMessage appends contextContinueMessage to msgs when they hold no
// user message, and returns msgs unchanged otherwise. The appended result is a
// fresh slice, so the caller never writes into the input's spare capacity.
func ensureUserMessage(msgs []llm.Message) []llm.Message {
	for _, m := range msgs {
		if m.Role == "user" {
			return msgs
		}
	}
	out := make([]llm.Message, 0, len(msgs)+1)
	out = append(out, msgs...)
	return append(out, llm.Message{Role: "user", Content: contextContinueMessage})
}

// summarizeAppendInstruction is the single instruction appended as the final
// user message when compressing.
// Summarize mode so the compression call reuses the live conversation layout.
const summarizeAppendInstruction = `Context is full, summarize the conversation into a brief report with the following rules:
- Think carefully and organize the key information (goals, decisions, steps taken, key points, results, next steps) so that work can be resumed and continued correctly.
- After summarization, the conversation content before this point will be completely cleared from the context. The summary you provide here will replace the summary in the system prompt.
- Repeated details, irrelevant content, and unimportant side discussions in the earlier parts that are not important should be omitted or condensed, but keep such content that is in the latest rounds.
- System prompt except the original summary is fully preserved, so do NOT include it in the summary you generate.
- Output the summary and do NOT add any explanatory meta-language.`

// livePrefix is the fixed head of every request the running conversation sends:
// the rendered system message and the declared tool schemas. The summarizing
// call reuses it verbatim, so a compaction never changes what the provider sees
// in front of the compressed messages — nothing below the base prompt is lost
// and a provider-side prompt cache keeps its prefix.
type livePrefix struct {
	systemPrompt string
	tools        []llm.ToolDef
}

// compactor implements the single, global context-compression strategy:
// summarize the older portion of the conversation and store the digest in the
// system prompt, keeping the most recent messages intact.
//
// The request prefix is deliberately not stored here: every pass receives the
// one the live conversation renders (system prompt plus declared tools), so the
// summarizing call cannot drift away from the ordinary requests.
type compactor struct {
	client                *llm.Client
	contextWindow         int
	maxTokens             int
	summarizeTokenPercent int
}

// tokenLimit returns the effective compaction trigger in tokens.
func (c *compactor) tokenLimit() int {
	limit := c.contextWindow * c.summarizeTokenPercent / 100
	if limit <= 0 {
		limit = c.contextWindow * 3 / 4
	}
	return limit
}

// shouldCompact reports whether the conversation exceeds the trigger (a
// percentage of the context window). The prefix is the one the next request will
// carry, so the system prompt counts towards the trigger like any other context.
func (c *compactor) shouldCompact(history []llm.Message, prefix livePrefix, usageTokens int) bool {
	estimate := EstimateMessagesTokens(history)
	estimate += EstimateMessageTokens(llm.Message{Role: "system", Content: prefix.systemPrompt})
	if usageTokens > estimate {
		estimate = usageTokens
	}
	return estimate >= c.tokenLimit()
}

// summarizeMode distinguishes who requested a pass; automatic compaction keeps
// a larger recent window than an explicit manual request。
type summarizeMode int

const (
	// summarizeModeAuto is used by the proactive post-turn compaction.
	summarizeModeAuto summarizeMode = iota
	// summarizeModeManual is used by the /compact command.
	summarizeModeManual
)

// parseTurnBoundaries returns the starting index of each Turn. A Turn begins at
// a user message and extends through all following assistant/tool messages up
// to the next user message, so cutting at a Turn boundary never splits an
// assistant tool_call / tool result pair.
func parseTurnBoundaries(history []llm.Message) []int {
	var starts []int
	for i, msg := range history {
		if msg.Role == "user" {
			starts = append(starts, i)
		}
	}
	return starts
}

// summarizeMaxKeptTurns caps how many of the newest user messages (complete
// turns) a pass may retain regardless of the token budget: automatic keeps at
// most 3, manual at most 2.
func summarizeMaxKeptTurns(m summarizeMode) int {
	if m == summarizeModeManual {
		return 2
	}
	return 3
}

// retentionBudget returns how many tokens of the newest messages a pass may
// keep visible: a fraction (1/10 auto, 1/20 manual) of the available input
// budget (ContextWindow minus the MaxTokens output reserve).
func (c *compactor) retentionBudget(m summarizeMode) int {
	available := c.contextWindow - c.maxTokens
	if available <= 0 {
		available = c.contextWindow
	}
	if available <= 0 {
		return 0
	}
	divisor := 10
	if m == summarizeModeManual {
		divisor = 20
	}
	return available / divisor
}

// summarizeTailCut decides how many leading messages of the still-unsummarized
// history a pass may compress. Retention is turn- and token-based:
//
//   - the retained window always starts at a user message (a whole Turn), so
//     tool-call sequences are never split;
//   - turns are walked newest → oldest and retained only while adding the next
//     older turn keeps the window strictly below keepTokenBudget;
//   - at most maxKeptTurns user messages are retained; whichever limit is hit
//     first stops the walk;
//   - there is no "always keep the newest turn" fallback: when even the newest
//     turn alone would reach/exceed the budget nothing is retained and the
//     whole tail is cut (safeCut == len(history)).
//
// It returns (0, false) when the whole tail already fits (nothing to do).
func summarizeTailCut(history []llm.Message, keepTokenBudget, maxKeptTurns int) (safeCut int, ok bool) {
	n := len(history)
	if n == 0 {
		return 0, false
	}
	if keepTokenBudget < 0 {
		keepTokenBudget = 0
	}
	if maxKeptTurns < 1 {
		maxKeptTurns = 1
	}

	turns := parseTurnBoundaries(history)
	if len(turns) == 0 {
		if n < 2 {
			return 0, false
		}
		return n - 1, true
	}

	// Per-turn token totals: turn i covers [turns[i], turns[i+1]).
	turnTokens := make([]int, len(turns))
	for i := range turns {
		end := n
		if i+1 < len(turns) {
			end = turns[i+1]
		}
		for j := turns[i]; j < end; j++ {
			turnTokens[i] += EstimateMessageTokens(history[j])
		}
	}

	acc := 0
	oldestTurn := len(turns)
	keptTurns := 0
	for i := len(turns) - 1; i >= 0 && keptTurns < maxKeptTurns && acc+turnTokens[i] < keepTokenBudget; i-- {
		acc += turnTokens[i]
		oldestTurn = i
		keptTurns++
	}

	if oldestTurn == len(turns) {
		// No complete turn fits: the whole tail (newest turn included) must be
		// compressed instead of left raw.
		return n, true
	}
	start := turns[oldestTurn]
	if start <= 0 {
		return 0, false
	}
	return start, true
}

// cut reports how much of history a pass would compress: the number of leading
// messages that fall outside the retained window. ok is false when there is
// nothing to condense (the whole history already fits the window).
func (c *compactor) cut(history []llm.Message, mode summarizeMode) (int, bool) {
	if len(history) < 2 {
		return 0, false
	}
	cut, ok := summarizeTailCut(history, c.retentionBudget(mode), summarizeMaxKeptTurns(mode))
	if !ok || cut <= 0 {
		return 0, false
	}
	return cut, true
}

// compact compresses history. prefix is the live request prefix (system prompt
// with the existing summary plus the declared tools), which the digest call
// reuses verbatim (see digest). It returns the new history, the new summary,
// whether a change happened, and any error. On summarization failure it falls
// back to dropping the oldest messages so the turn can continue.
func (c *compactor) compact(ctx context.Context, history []llm.Message, prefix livePrefix, summary string, mode summarizeMode) ([]llm.Message, string, bool, error) {
	cut, ok := c.cut(history, mode)
	if !ok {
		return history, summary, false, nil
	}
	batch := history[:cut]
	// Cutting everything — including the user turn that started the running
	// loop — would leave the next request without a user message, which chat
	// templates reject. The retained window always starts at a user message
	// when it is non-empty, so this only fires on a fully compressed tail.
	tail := ensureUserMessage(history[cut:])

	digest, err := c.digest(ctx, batch, prefix)
	if err != nil || digest == "" {
		if err == nil {
			err = fmt.Errorf("empty summary returned")
		}
		return tail, summary, true, fmt.Errorf("summarize failed, dropped oldest messages: %w", err)
	}
	return tail, mergeSummaries(summary, digest), true, nil
}

// digest asks the model for a summary of batch. It reuses the live conversation
// layout — the very request prefix the running conversation sends (system prompt
// with the current summary, and the same declared tools) followed by the
// messages being compressed — and appends the summarize instruction as the final
// user message. Reusing that prefix verbatim matters twice over: the model
// summarizing knows the same environment (runtime, working directory, unlock
// rule, MCP servers) as it did while the messages were produced, and the call
// shares its prefix with the live requests, so the provider can serve it from
// its cached prompt prefix instead of re-processing a different one.
func (c *compactor) digest(ctx context.Context, batch []llm.Message, prefix livePrefix) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("no llm client")
	}
	msgs := make([]llm.Message, 0, len(batch)+2)
	msgs = append(msgs, llm.Message{Role: "system", Content: prefix.systemPrompt})
	msgs = append(msgs, batch...)
	msgs = append(msgs, llm.Message{Role: "user", Content: summarizeAppendInstruction})

	resp, err := c.client.Chat(ctx, msgs, prefix.tools, nil, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Content), nil
}

// mergeSummaries concatenates the previous summary with the new segment digest.
func mergeSummaries(existing, digest string) string {
	existing = strings.TrimSpace(existing)
	digest = strings.TrimSpace(digest)
	if existing == "" {
		return digest
	}
	if digest == "" {
		return existing
	}
	return existing + "\n\n" + digest
}

// systemWithSummary renders the system prompt with the optional summary.
func systemWithSummary(base, summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return base
	}
	return base + "\n\n# CONVERSATION SUMMARY\n" + summary
}
