package agent

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"lightagent/internal/config"
	"lightagent/internal/llm"
)

// mediaPartTokens is the flat token cost one media part contributes to the
// estimate. The real cost is provider- and size-dependent (an image is billed by
// its resolution, audio by its length), so a rough average is used instead of
// the encoded size: counting the base64 characters would estimate a single
// picture at hundreds of thousands of tokens and compact the context for it.
// The provider's own prompt-token report (which counts the media it received)
// corrects the estimate on the very next call.
const mediaPartTokens = 1200

// EstimateMessageTokens estimates the token cost of one message as it appears in
// the request. The count is a lower bound built from the units a tokenizer really
// splits text into — one token per whitespace-separated run (an English word, a
// number, a chunk of code) and one token per CJK character — instead of a fixed
// characters-per-token ratio, which has to be calibrated for one script and then
// over-counts every other one. Every field that travels on the wire is counted
// (content, reasoning, tool-call name/arguments/id and the tool call id of a
// result), plus a flat cost per media part (see mediaPartTokens).
//
// The function never sees the whole request: the provider's own prompt-token
// report is the ground truth the caller anchors on (see Agent.contextTokensLocked),
// and this estimate only covers the messages appended after that report — which is
// why a lower bound is the right shape for it. A long unbroken run is not one
// token: its cost grows by one token per four characters, so a minified JSON line
// or a base64 payload cannot collapse into a single token (see runUnits).
func EstimateMessageTokens(m llm.Message) int {
	units := estimateUnits(m.Content)
	units += estimateUnits(m.ReasoningContent)
	units += estimateUnits(m.Name)
	units += estimateUnits(m.ToolCallID)
	for _, tc := range m.ToolCalls {
		units += estimateUnits(tc.ID) + estimateUnits(tc.Type)
		units += estimateUnits(tc.Function.Name) + estimateUnits(tc.Function.Arguments)
	}
	return units + len(m.Media)*mediaPartTokens
}

// estimateUnits counts the token units of s: one per CJK character (they carry a
// token each and their scripts have no spaces to split runs on) and one per
// whitespace-separated run of anything else, with a long run costing more (see
// runUnits). Punctuation rides with the run it touches, so "hello, world!" is two
// units.
func estimateUnits(s string) int {
	units, runes := 0, 0
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			units += runUnits(runes)
			runes = 0
		case isCJK(r):
			units += runUnits(runes) + 1
			runes = 0
		default:
			runes++
		}
	}
	return units + runUnits(runes)
}

// runUnits is the cost of one whitespace-separated run of runes characters: one
// token, plus one more for every four characters the run grows past that. A run is
// the unit because a word (or a number, or a short code fragment) is what a
// tokenizer keeps whole, and the growth keeps a long unbroken run — a minified
// JSON line, a base64 payload — from collapsing into a single token.
func runUnits(runes int) int {
	if units := runes / 4; units > 1 {
		return units
	}
	if runes > 0 {
		return 1
	}
	return 0
}

// isCJK reports whether r belongs to a script written without spaces, where a
// character is the unit that carries a token: Chinese characters (the case a
// character-ratio estimate got wrong) and the kana that share their layout.
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r)
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

// summarizeInstructionIntro opens the summarizing instruction and lets the tests
// recognize the summarizing call.
const summarizeInstructionIntro = "Context is full, summarize the conversation into a report with the following rules:"

// summarizeInstruction renders the instruction appended as the final user message
// when compressing. It tells the model what its report is for — the only history
// context the next conversations see.
//
// The system prompt is the case that needs spelling out: when the summary lives
// there and one already exists, the instruction says the new report replaces the
// summary of that section. The default layout needs no such note — the report is
// exactly the [engine] message the instruction just described as the only history
// context (see headMessages).
func summarizeInstruction(summary string, summaryInSystemPrompt bool) string {
	lines := []string{
		summarizeInstructionIntro,
		"- Think carefully and organize the key information (goals, decisions, steps taken, key points, results, next steps) so that work can be resumed and continued correctly.",
		"- The report you write here becomes the only history context of the next conversation, so it has to stand on its own.",
	}
	if summaryInSystemPrompt && strings.TrimSpace(summary) != "" {
		lines = append(lines, `- The new report replaces the summary of the "# CONVERSATION SUMMARY" section of the system prompt.`)
	}
	return strings.Join(append(lines,
		"- Repeated details, irrelevant content, and unimportant side discussions in the earlier parts that are not important should be omitted or condensed, but keep such content that is in the latest rounds.",
		"- The system prompt itself stays as it is, so do NOT include it in the report you generate.",
		"- Output the report and do NOT add any explanatory meta-language.",
	), "\n")
}

// livePrefix is the fixed head of every request the running conversation sends:
// the rendered system message and the declared tool schemas, plus the accumulated
// summary and the place it occupies. The summarizing call reuses it verbatim (see
// head and digest), so a compaction never changes what the provider sees in front
// of the compressed messages — nothing below the base prompt is lost and a
// provider-side prompt cache keeps its prefix.
type livePrefix struct {
	systemPrompt string
	tools        []llm.ToolDef
	// summary is the accumulated context summary, and summaryInSystemPrompt
	// says whether a request carries it in the system prompt (true) or as its
	// first user message (false, the default).
	summary               string
	summaryInSystemPrompt bool
}

// head renders the fixed head of a request followed by rest: the live system
// prompt and rest's messages, with the accumulated summary in the place this
// prefix came with. Both the live calls and the summarizing one build their
// message list here, so the two cannot drift apart and the summarizing call's
// prefix stays byte-identical to the live one. The summarizing call is therefore
// a one-sided (tail-only) extension of the live request: the head it sends is the
// head the live requests send, the messages being compressed follow unchanged,
// and the only tokens appended are the summarize instruction at the very end.
func (p livePrefix) head(rest []llm.Message) []llm.Message {
	return headMessages(p.systemPrompt, rest, p.summary, p.summaryInSystemPrompt)
}

// headMessages renders the messages a request starts with: the system message,
// then the accumulated summary where the configuration asks for it, then rest.
// summaryInSystemPrompt keeps the summary in the system prompt (the caller then
// passes a system message that already carries it); otherwise the summary goes
// out as the first user message.
func headMessages(system string, rest []llm.Message, summary string, summaryInSystemPrompt bool) []llm.Message {
	msgs := make([]llm.Message, 0, len(rest)+2)
	msgs = append(msgs, llm.Message{Role: "system", Content: system})
	if !summaryInSystemPrompt {
		if msg, ok := summaryMessage(summary); ok {
			msgs = append(msgs, msg)
		}
	}
	return append(msgs, rest...)
}

// summaryUserPrefix introduces the summary when it goes out as the first user
// message. The [engine] tag marks engine-inserted text, like
// contextContinueMessage above.
const summaryUserPrefix = "[engine] CONVERSATION SUMMARY:\n"

// summaryMessage builds the user message that carries the accumulated summary,
// and reports false when there is nothing to send.
func summaryMessage(summary string) (llm.Message, bool) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return llm.Message{}, false
	}
	return llm.Message{Role: "user", Content: summaryUserPrefix + summary}, true
}

// compactor implements the single, global context-compression strategy:
// summarize the older portion of the conversation and store the digest as the
// accumulated summary, keeping as much of the newest history raw as the retention
// policy allows — nothing by default, so a pass summarizes the whole history (see
// retention). Where a request carries that summary — in the system prompt or as
// its first user message — is decided by the request builder (see
// livePrefix.head).
//
// The request prefix is deliberately not stored here: every pass receives the
// one the live conversation renders (system message plus declared tools), so the
// summarizing call cannot drift away from the ordinary requests.
type compactor struct {
	client                *llm.Client
	contextWindow         int
	maxTokens             int
	summarizeTokenPercent int
	// keepAuto and keepManual are the configured retention policies of the
	// automatic pass and of the one /compact triggers (see retention).
	keepAuto   config.SummarizeKeepPolicy
	keepManual config.SummarizeKeepPolicy
}

// tokenLimit returns the effective compaction trigger in tokens.
func (c *compactor) tokenLimit() int {
	limit := c.contextWindow * c.summarizeTokenPercent / 100
	if limit <= 0 {
		limit = c.contextWindow * 3 / 4
	}
	return limit
}

// shouldCompact reports whether the context has reached the trigger (a percentage
// of the context window). estimate is the size of the request the next iteration
// will carry: the provider-reported prompt-token count plus the messages appended
// since that report (see Agent.contextTokensLocked). A whole-context estimate is
// deliberately not compared against it — a lower bound and a real count must not
// compete, or the pass fires at a fraction of the configured percentage while the
// frontends show that very number. A request the provider rejects for its size is
// recovered from instead (see Agent.recoverContextOverflow).
func (c *compactor) shouldCompact(estimate int) bool {
	return estimate >= c.tokenLimit()
}

// summarizeMode distinguishes who requested a pass: the automatic and the manual
// pass carry the retention policy the configuration gives them (they keep no raw
// message by default, see config.SummarizeKeepPolicy), and the overflow recovery
// — the pass that follows a request the provider rejected for its size — keeps
// nothing raw at all.
type summarizeMode int

const (
	// summarizeModeAuto is used by the proactive post-turn compaction.
	summarizeModeAuto summarizeMode = iota
	// summarizeModeManual is used by the /compact command.
	summarizeModeManual
	// summarizeModeOverflow is used by the recovery pass that follows a request
	// the provider rejected for context length (see
	// Agent.recoverContextOverflow).
	summarizeModeOverflow
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

// retention returns the retention policy mode runs with: the configured one for
// the automatic and the manual pass, and none for the overflow recovery — the
// provider has just proven the estimate too small there, so only the summary may
// survive the retry. A zero policy keeps no raw message, which is what the
// defaults ask for: some inference engines do not preserve their prompt cache
// across the rollback a retention performs (see config.SummarizeKeepPolicy).
func (c *compactor) retention(m summarizeMode) config.SummarizeKeepPolicy {
	switch m {
	case summarizeModeManual:
		return c.keepManual
	case summarizeModeOverflow:
		return config.SummarizeKeepPolicy{}
	}
	return c.keepAuto
}

// retentionBudget returns how many tokens of the newest messages a pass may keep
// visible: the configured share of the available input budget (ContextWindow
// minus the MaxTokens output reserve). A zero share (the default) keeps nothing.
func (c *compactor) retentionBudget(m summarizeMode) int {
	percent := c.retention(m).BudgetPercent
	if percent <= 0 {
		return 0
	}
	available := c.contextWindow - c.maxTokens
	if available <= 0 {
		available = c.contextWindow
	}
	if available <= 0 {
		return 0
	}
	return available * percent / 100
}

// maxKeptTurns caps how many of the newest user messages (complete turns) a pass
// may retain regardless of the token budget (see the retention policy).
// summarizeTailCut lifts a zero to its minimum of one, which never binds: a zero
// budget already keeps nothing.
func (c *compactor) maxKeptTurns(m summarizeMode) int {
	return c.retention(m).Turns
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
	cut, ok := summarizeTailCut(history, c.retentionBudget(mode), c.maxKeptTurns(mode))
	if !ok || cut <= 0 {
		return 0, false
	}
	return cut, true
}

// wouldCut reports whether a pass in this mode would condense anything of
// history. The overflow recovery asks before it touches the context: a retry is
// only worth issuing when the pass (or the rollback) really frees room.
func (c *compactor) wouldCut(history []llm.Message, mode summarizeMode) bool {
	_, ok := c.cut(history, mode)
	return ok
}

// overflowRollbackCut returns the index the newest messages of a rejected request
// start at: the assistant/tool rows this turn appended after its user message,
// and every user message no answer followed (the turn's own messages plus the
// steering messages folded in before the failing call). The recovery replays
// those user messages on top of the summary, while the assistant/tool rows are
// dropped for good — the model produced them, but the request carrying their
// results never reached it (see Agent.recoverContextOverflow).
//
// It returns len(history) when there is nothing to roll back.
func overflowRollbackCut(history []llm.Message) int {
	cut := len(history)
	for cut > 0 && history[cut-1].Role != "user" {
		cut--
	}
	for cut > 0 && history[cut-1].Role == "user" {
		cut--
	}
	return cut
}

// compact compresses history. prefix is the live request prefix (system message
// plus declared tools, with the accumulated summary in the place the
// configuration asks for), which the digest call reuses verbatim (see digest). It
// returns the new history, the new summary, whether a change happened, and any
// error.
//
// The report the model writes is a full update of the summary: the digest call
// carried the previous one and asked for the complete text, so the pass stores what
// came back as the new accumulated summary. On summarization failure the previous
// summary is kept and the oldest messages are dropped, so the turn can continue.
func (c *compactor) compact(ctx context.Context, history []llm.Message, prefix livePrefix, summary string, mode summarizeMode) ([]llm.Message, string, bool, error) {
	cut, ok := c.cut(history, mode)
	if !ok {
		return history, summary, false, nil
	}
	batch := history[:cut]
	// Cutting everything — including the user turn that started the running
	// loop — would leave the next request without a user message, which chat
	// templates reject, so ensureUserMessage adds the engine marker. A manual
	// pass (/compact) runs outside a turn, so the marker is not added there:
	// the next request only goes out when the user sends a new message, which
	// is the user query itself, and a marker left behind would only linger as a
	// stray engine note. The retention policy keeps no raw message by default
	// (see retention), so an automatic pass normally leaves that marker and
	// nothing else, and a manual one leaves nothing.
	tail := history[cut:]
	if mode != summarizeModeManual {
		tail = ensureUserMessage(tail)
	}

	digest, err := c.digest(ctx, batch, prefix)
	if err != nil || digest == "" {
		if err == nil {
			err = fmt.Errorf("empty summary returned")
		}
		return tail, summary, true, fmt.Errorf("summarize failed, dropped oldest messages: %w", err)
	}
	return tail, digest, true, nil
}

// digest asks the model for a summary of batch. It reuses the live conversation
// layout — the very request prefix the running conversation sends (system message
// plus declared tools, with the accumulated summary where the configuration puts
// it) followed by the messages being compressed — and appends the summarize
// instruction as the final user message. Reusing that prefix verbatim matters
// twice over: the model summarizing sees the same environment (runtime, working
// directory, unlock rule, MCP servers) and the same accumulated summary as it did
// while the messages were produced, and the call shares its prefix with the live
// requests, so the provider can serve it from its cached prompt prefix instead of
// re-processing a different one. The batch is appended to that prefix and the
// instruction to the batch, so nothing in front of the instruction is rendered,
// reordered or rewritten: the pass only ever extends the request at its tail.
// The instruction matches that same layout, so it only mentions the summary
// where the model can see it.
func (c *compactor) digest(ctx context.Context, batch []llm.Message, prefix livePrefix) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("no llm client")
	}
	// The accumulated summary is placed by the same helper the live requests use,
	// so the prefix stays byte-identical even when it lives in a user message.
	msgs := prefix.head(batch)
	msgs = append(msgs, llm.Message{Role: "user", Content: summarizeInstruction(prefix.summary, prefix.summaryInSystemPrompt)})

	resp, err := c.client.Chat(ctx, msgs, prefix.tools, nil, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Content), nil
}

// systemWithSummary renders the system prompt with the optional summary, as the
// agent.summary_in_system_prompt layout does; the default layout sends the summary
// as the first user message instead (see headMessages).
func systemWithSummary(base, summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return base
	}
	return base + "\n\n# CONVERSATION SUMMARY\n" + summary
}
