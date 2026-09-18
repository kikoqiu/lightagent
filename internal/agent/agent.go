// Package agent implements the lightagent conversation loop: it drives the
// LLM, executes tool calls, supports steering (user messages inserted mid-turn)
// and performs context compression.
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"lightagent/internal/config"
	"lightagent/internal/llm"
	"lightagent/internal/tools"
)

// Agent orchestrates one conversation.
type Agent struct {
	mu     sync.Mutex // guards history, summary, busy, lastUsage, mcpInfo
	client *llm.Client
	reg    *tools.Registry
	bus    *Bus
	base   string
	// runtimeInfo is the environment line (platform the binary runs on) added
	// after the base prompt. It is generated in New instead of living in the
	// prompt template, so agent.md never carries a hard-coded platform.
	runtimeInfo string
	// dirListing is the optional current-directory section (working directory
	// plus its direct children). It is empty when agent.include_working_dir is
	// off or the directory cannot be read. It is set once in New.
	dirListing string
	// unlockRule is the single global "Tool Discovery & Unlock" mechanism
	// section. It is non-empty only while locked (deferred) functions exist,
	// so it is injected exactly once and only when it is meaningful.
	unlockRule string
	// mcpInfo holds one "MCP global info" entry per connected MCP server. It
	// never lists the servers' functions.
	mcpInfo []MCPServerInfo
	summary string
	history []llm.Message
	busy    bool
	// usage is the last provider-reported prompt-token count (the size of the
	// context the API saw), 0 when unknown. usageAt is len(history) at the time
	// it was reported, so messages appended afterwards can be estimated on top
	// of it.
	usage   int
	usageAt int

	maxIter   int
	compactor *compactor
	// autoSplitWrites spreads a write_file payload that exceeds the tool's
	// per-call line limit over several calls instead of letting it be
	// truncated (see autoSplitWriteCalls). It comes from
	// tools.write_file.auto_split and defaults to true.
	autoSplitWrites bool
	// contextWindow is the model context window in tokens (for usage display).
	contextWindow int

	// toolResultsVisible controls whether tool_result events are broadcast.
	toolResultsVisible bool

	// cancelTurn cancels the turn in flight (nil while idle). Interrupt uses it
	// to stop the current operation.
	cancelTurn context.CancelFunc

	steerCh chan steerMessage

	persist func(history []llm.Message, summary string)
}

// New builds an agent from the config, client and tool registry.
func New(cfg *config.Config, client *llm.Client, reg *tools.Registry, bus *Bus) *Agent {
	base := cfg.Agent.SystemPrompt
	if base == "" {
		base = DefaultSystemPrompt()
	}
	// The current-directory listing gives the model an immediate view of the
	// working directory without spending a tool call.
	var dirListing string
	if cfg.Agent.IncludeWorkingDir {
		if cwd, err := os.Getwd(); err == nil {
			dirListing = DirectoryListing(cwd)
		}
	}
	// The global unlock mechanism rule is injected exactly once, and only while
	// locked (deferred) functions actually exist — MCP tools always use this
	// mechanism. It never enumerates the locked functions, so the rendered
	// system prompt stays byte-stable across lock/unlock cycles.
	var unlockRule string
	if cfg.Tools.Discovery.EffectiveMode() == config.ToolDiscoveryModeUnlock &&
		reg != nil && reg.DeferredCount() > 0 {
		unlockRule = ToolUnlockRule()
	}
	maxIter := cfg.Agent.MaxToolIterations
	if maxIter <= 0 {
		maxIter = 20
	}
	contextWindow := cfg.Context.ContextWindow
	if contextWindow <= 0 {
		contextWindow = 131072
	}
	summarizePercent := cfg.Context.SummarizeTokenPercent
	if summarizePercent <= 0 || summarizePercent > 100 {
		summarizePercent = 75
	}
	a := &Agent{
		client:             client,
		reg:                reg,
		bus:                bus,
		base:               base,
		runtimeInfo:        RuntimeInfo(),
		dirListing:         dirListing,
		unlockRule:         unlockRule,
		maxIter:            maxIter,
		autoSplitWrites:    cfg.Tools.WriteFile.AutoSplit,
		contextWindow:      contextWindow,
		toolResultsVisible: true,
		steerCh:            make(chan steerMessage, 64),
	}
	a.compactor = &compactor{
		client:                client,
		contextWindow:         contextWindow,
		maxTokens:             cfg.OpenAI.MaxTokens,
		summarizeTokenPercent: summarizePercent,
	}
	return a
}

// Bus exposes the agent event bus for subscribers.
func (a *Agent) Bus() *Bus { return a.bus }

// SetMCPServers records the connected MCP servers rendered into the system
// prompt as the per-server "MCP global info" (server name, tool count and the
// locked-tools availability note). It must be called before the first turn; it
// never lists the servers' functions.
func (a *Agent) SetMCPServers(servers []MCPServerInfo) {
	a.mu.Lock()
	a.mcpInfo = append([]MCPServerInfo(nil), servers...)
	a.mu.Unlock()
}

// systemPrompt renders the system prompt: the base prompt followed by the
// capability sections — the runtime line, the current-directory listing, the
// single global unlock rule and, per connected server, the MCP global info
// line. The runtime line is generated here rather than coming from the base
// prompt, so an agent.md never carries a hard-coded platform. The caller must
// hold a.mu.
func (a *Agent) systemPrompt() string {
	parts := make([]string, 0, len(a.mcpInfo)+4)
	if trimmed := strings.TrimRight(a.base, "\n"); trimmed != "" {
		parts = append(parts, trimmed)
	}
	if a.runtimeInfo != "" {
		parts = append(parts, a.runtimeInfo)
	}
	if a.dirListing != "" {
		parts = append(parts, a.dirListing)
	}
	if a.unlockRule != "" {
		parts = append(parts, a.unlockRule)
	}
	if len(a.mcpInfo) > 0 {
		lines := make([]string, 0, len(a.mcpInfo))
		for _, info := range a.mcpInfo {
			lines = append(lines, mcpServerInfoLine(info))
		}
		parts = append(parts, strings.Join(lines, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

// systemMessageLocked renders the system message every request carries: the
// rendered system prompt with the current summary. It is the single place the
// system message is built, so the ordinary calls and the summarizing one cannot
// drift apart. The caller must hold a.mu.
func (a *Agent) systemMessageLocked() llm.Message {
	return llm.Message{Role: "system", Content: systemWithSummary(a.systemPrompt(), a.summary)}
}

// livePrefixLocked renders the fixed head of every request the conversation
// sends — the system message and the declared tool schemas — in one snapshot.
// Compaction passes it to the summarizing call verbatim, which keeps that call's
// prefix byte-identical to the live ones. The caller must hold a.mu.
func (a *Agent) livePrefixLocked() livePrefix {
	return livePrefix{
		systemPrompt: a.systemMessageLocked().Content,
		tools:        a.reg.Definitions(),
	}
}

// SetPersist registers an optional callback invoked after every turn and
// compaction. The default CLI does not register one: the conversation stays in
// memory and is only written on demand (/save) or on the exit confirmation.
func (a *Agent) SetPersist(fn func(history []llm.Message, summary string)) {
	a.persist = fn
}

// History returns a copy of the conversation history (system prompt excluded).
func (a *Agent) History() []llm.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]llm.Message(nil), a.history...)
}

// Summary returns the current context summary.
func (a *Agent) Summary() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.summary
}

// Load replaces the conversation state (used when resuming a saved session).
func (a *Agent) Load(history []llm.Message, summary string) {
	a.mu.Lock()
	a.history = append([]llm.Message(nil), history...)
	a.summary = summary
	a.usage = 0
	a.usageAt = 0
	a.mu.Unlock()
}

// Reset clears the conversation.
func (a *Agent) Reset() {
	a.mu.Lock()
	a.history = nil
	a.summary = ""
	a.usage = 0
	a.usageAt = 0
	a.mu.Unlock()
}

// Busy reports whether a turn is currently running.
func (a *Agent) Busy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.busy
}

// Stats describes the current context usage for the /history command.
type Stats struct {
	Messages int
	Summary  string
	// EstimatedTok is the best estimate of the context size in tokens: the
	// provider-reported prompt-token count plus the messages appended since
	// that report, or a pure local estimate when no usage was reported.
	EstimatedTok int
	// UsageTokens is the last prompt-token count reported by the API (0 when
	// the provider does not report usage).
	UsageTokens int
	// ContextWindow is the configured model context window in tokens.
	ContextWindow int
	Busy          bool
}

// Stats returns a usage snapshot.
func (a *Agent) Stats() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Stats{
		Messages:      len(a.history),
		Summary:       a.summary,
		EstimatedTok:  a.contextTokensLocked(),
		UsageTokens:   a.usage,
		ContextWindow: a.contextWindow,
		Busy:          a.busy,
	}
}

// contextTokensLocked returns the best estimate of the tokens the next request
// will carry. A provider-reported prompt-token count anchors the total and the
// messages appended since that report are estimated on top of it; without a
// report (or when the history shrank past the anchor) it falls back to the pure
// local estimate. The caller must hold a.mu.
func (a *Agent) contextTokensLocked() int {
	if a.usage > 0 && a.usageAt >= 0 && a.usageAt <= len(a.history) {
		return a.usage + EstimateMessagesTokens(a.history[a.usageAt:])
	}
	return EstimateMessagesTokens(a.history) + EstimateMessageTokens(a.systemMessageLocked())
}

// usageEvent builds a context-usage event. It is broadcast at every point the
// context grows — turn start, each model reply and each tool round — so the
// frontends track the usage while a turn is still running.
func (a *Agent) usageEvent() Event {
	stats := a.Stats()
	return Event{
		Type:          EventUsage,
		Tokens:        stats.EstimatedTok,
		ContextWindow: stats.ContextWindow,
	}
}

// Submit accepts a user message. When no turn is running it starts one in the
// background; otherwise the message is queued as steering and consumed by the
// running turn at its next safe point.
func (a *Agent) Submit(text string) { a.SubmitFrom("", text) }

// SubmitFrom is Submit with an explicit origin ("cli", "web", ...). The origin
// travels with the EventUser so frontends can avoid echoing their own input.
func (a *Agent) SubmitFrom(source, text string) {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		a.pushSteer(source, text)
		return
	}
	a.busy = true
	turnCtx, cancel := context.WithCancel(context.Background())
	a.cancelTurn = cancel
	a.mu.Unlock()

	a.bus.Publish(Event{Type: EventUser, Text: text, Source: source})
	go a.runLoop(turnCtx, text)
}

// Interrupt cancels the turn in flight (the model call or the tool it is
// running) and reports whether a turn was running. The next user message starts
// a fresh turn.
func (a *Agent) Interrupt() bool {
	a.mu.Lock()
	cancel := a.cancelTurn
	a.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// steerMessage is one queued steering message: its text plus the client it came
// from, which rides along to the user event published when the message is folded
// into the conversation.
type steerMessage struct {
	source string
	text   string
}

// pushSteer queues a steering message for the running turn. Nothing is announced
// here: the row is published when the turn folds the message into the
// conversation (see drainSteering), which is the point the message actually
// belongs at — after the reply it interrupted. Announcing it earlier would draw
// it in the middle of that reply (and would replay in the wrong place too, since
// the mirror records the rows the bus carries).
func (a *Agent) pushSteer(source, text string) {
	select {
	case a.steerCh <- steerMessage{source: source, text: text}:
	default:
		a.bus.Publish(Event{Type: EventError, Text: "steering queue is full; message dropped"})
	}
}

// SetToolResultsVisible toggles whether tool results are broadcast to the
// frontends. It defaults to true.
func (a *Agent) SetToolResultsVisible(v bool) {
	a.mu.Lock()
	a.toolResultsVisible = v
	a.mu.Unlock()
}

// ToolResultsVisible reports the current tool-result visibility.
func (a *Agent) ToolResultsVisible() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.toolResultsVisible
}

// maxConsecutiveTruncations bounds how many times one turn auto-continues after
// the provider cut a response off at max_tokens (finish_reason "length").
const maxConsecutiveTruncations = 3

// runLoop executes a full turn: LLM call → tool calls → repeat. The turn's
// context is cancelled by Interrupt, which stops the in-flight step and ends the
// turn (see finishInterrupt).
func (a *Agent) runLoop(ctx context.Context, userText string) {
	a.runTurn(ctx, []string{userText})
}

// runTurn is the body of a turn: it records the turn's user messages (which the
// caller has announced on the bus already), runs the LLM/tool loop and tears the
// turn down. A turn that picks up queued steering messages passes them all at
// once.
func (a *Agent) runTurn(ctx context.Context, userTexts []string) {
	defer func() {
		a.mu.Lock()
		a.busy = false
		a.cancelTurn = nil
		a.mu.Unlock()
		a.save()
		a.bus.Publish(a.usageEvent())
		a.bus.Publish(Event{Type: EventTurnDone})
		// A steering message that arrived after the loop's last drain (the
		// final reply was still streaming) has no iteration left to consume
		// it: it is picked up as a turn of its own instead of sitting in the
		// queue until the user types again.
		a.startSteeringTurn()
	}()

	turnStart := a.historyLen()
	userCount := len(userTexts)
	for _, text := range userTexts {
		a.appendMessage(llm.Message{Role: "user", Content: text})
	}
	// Report the context size right away so the frontends show the new user
	// message while the model call is still in flight.
	a.bus.Publish(a.usageEvent())

	// truncations counts how many responses in a row the provider cut off at
	// max_tokens (finish_reason "length") and the loop auto-continued. A
	// response that finishes normally resets it.
	truncations := 0

	for i := 0; i < a.maxIter; i++ {
		a.drainSteering()

		a.compactIfNeeded(ctx)

		resp, err := a.callLLM(ctx)
		if err != nil {
			if turnInterrupted(ctx, err) {
				a.finishInterrupt(turnStart, userCount)
				return
			}
			// Reserved recovery hook. llm.IsIncompleteResponse(err) is
			// true when the provider delivered a half-built reply (a
			// tool call whose arguments never closed, a frame cut in
			// half, finish_reason=length with calls pending; see
			// llm.IncompleteResponseError). Such a reply is exactly the
			// case the model itself could repair, so instead of ending
			// the turn here a future path could hand the parse failure
			// back to the model -- re-issue the completion, or answer
			// the partial calls with a "malformed call, try again" tool
			// message. The policy (retry once? repair? give up after
			// N?) is not decided, so the turn still fails today: branch
			// on that predicate right here to add it.
			a.bus.Publish(Event{Type: EventError, Text: err.Error()})
			return
		}

		a.setUsage(resp.Usage)

		// An oversized write_file payload is spread over several calls: the
		// call the model made keeps the first part and the remaining parts run
		// after this round as follow-up rounds (see runWriteSplits). The
		// rewrite happens before the assistant message is recorded, so the
		// history and the front-ends show the arguments that were executed.
		continuations := a.autoSplitWriteCalls(resp.ToolCalls)

		assistant := llm.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
			// Keep the thinking on the message so it is preserved in the
			// history and sent back with the next request.
			ReasoningContent: resp.Reasoning,
		}
		a.appendMessage(assistant)
		if resp.Content != "" {
			a.bus.Publish(Event{Type: EventAssistant, Text: resp.Content})
		}
		// The assistant message just joined the context: refresh the usage so
		// the badge reflects the provider's count plus this reply.
		a.bus.Publish(a.usageEvent())

		if len(resp.ToolCalls) == 0 {
			if resp.Finish != "length" {
				if a.steeringPending() {
					// A steering message arrived while this reply was
					// still streaming: the reply is not the end of the
					// turn then, so the turn carries on. The next
					// iteration folds the message into the context —
					// announcing its row there, after this reply — and
					// calls the model again.
					continue
				}
				return
			}
			// The provider ran out of max_tokens, typically while the model was
			// still thinking. The assistant message above — reasoning included,
			// and with no new user message — stays in the history, and the
			// model is called again so it can carry on from its own thinking.
			truncations++
			if truncations >= maxConsecutiveTruncations {
				a.bus.Publish(Event{
					Type: EventError,
					Text: fmt.Sprintf("stopped after %d consecutive truncations at max_tokens; raise openai.max_tokens",
						truncations),
				})
				return
			}
			a.bus.Publish(Event{
				Type: EventInfo,
				Text: fmt.Sprintf("response truncated at max_tokens; continuing (%d/%d)",
					truncations, maxConsecutiveTruncations),
			})
			continue
		}
		truncations = 0

		var pendingSplits []llm.ToolCall
		for idx, tc := range resp.ToolCalls {
			res, canceled := a.dispatchToolCall(ctx, tc)
			if canceled {
				// The user stopped the turn: report the interrupted call (and
				// every call after it, so the assistant/tool pairing stays
				// valid in the next request) and end the turn.
				a.reportInterruptedTools(resp.ToolCalls, idx)
				a.finishInterrupt(turnStart, userCount)
				return
			}
			rest, split := continuations[idx]
			if !split {
				continue
			}
			if res.IsError {
				// The first part failed (a create-only call on an existing
				// file, say): appending the rest would leave a file that stops
				// in the middle of the payload, so the parts are dropped.
				a.bus.Publish(Event{
					Type: EventInfo,
					Text: fmt.Sprintf("write_file: the first part failed, so the remaining %d part(s) were skipped", len(rest)),
				})
				continue
			}
			pendingSplits = append(pendingSplits, rest...)
		}

		// Expire unlock grants after each tool-execution round, exactly like
		// the original unlock mode. An expired locked function simply gets
		// re-unlocked on demand.
		if a.reg != nil {
			a.reg.TickTTL()
		}
		// Tool results grew the context without a model call; report the new
		// size before the next round starts.
		a.bus.Publish(a.usageEvent())

		// The remaining parts of an auto-split write run as their own
		// assistant/tool rounds right here, so the model is only asked again
		// once the payload is on disk in order.
		if len(pendingSplits) > 0 {
			if !a.runWriteSplits(ctx, pendingSplits) {
				a.finishInterrupt(turnStart, userCount)
				return
			}
		}
	}

	a.bus.Publish(Event{
		Type: EventInfo,
		Text: fmt.Sprintf("reached the maximum of %d tool iterations; stopping this turn", a.maxIter),
	})
}

// turnInterrupted reports whether err came from cancelling the turn context.
func turnInterrupted(ctx context.Context, err error) bool {
	return ctx.Err() != nil && errors.Is(err, context.Canceled)
}

// dispatchToolCall publishes one tool call, runs it and records its answer as a
// tool message, which is what keeps the assistant message's tool_calls paired.
// It reports the result and whether the turn was cancelled before the call
// finished.
func (a *Agent) dispatchToolCall(ctx context.Context, tc llm.ToolCall) (*tools.Result, bool) {
	a.bus.Publish(Event{Type: EventToolCall, Name: tc.Function.Name, Args: tc.Function.Arguments})
	res, canceled := a.executeTool(ctx, tc)
	if canceled {
		return nil, true
	}
	if a.ToolResultsVisible() {
		a.bus.Publish(Event{
			Type:    EventToolResult,
			Name:    tc.Function.Name,
			Text:    res.ForUser,
			IsError: res.IsError,
		})
	}
	a.appendMessage(llm.Message{
		Role:       "tool",
		ToolCallID: tc.ID,
		Name:       tc.Function.Name,
		Content:    res.ForLLM,
	})
	return res, false
}

// executeTool runs one tool call and reports whether the turn was cancelled
// before it finished. Tools receive the turn context, so those that can observe
// cancellation (exec_command kills its process) stop promptly instead of
// blocking the interrupt.
func (a *Agent) executeTool(ctx context.Context, tc llm.ToolCall) (*tools.Result, bool) {
	done := make(chan *tools.Result, 1)
	go func() {
		// unlock_tool skips resending a schema that is still present in the
		// current effective (uncompacted) context: the lookup scans the live
		// history, from which compaction has already dropped older messages, so
		// an evicted schema is re-delivered automatically.
		execCtx := tools.WithUnlockLookup(ctx, func(toolName string) bool {
			return tools.MessagesContainUnlockRecord(a.History(), toolName)
		})
		done <- a.reg.Execute(execCtx, tc.Function.Name, tc.Function.Arguments)
	}()
	select {
	case res := <-done:
		return res, false
	case <-ctx.Done():
		return nil, true
	}
}

// reportInterruptedTools publishes the interrupted feedback for the call that
// was stopped and records tool messages for it and every call after it, so the
// assistant message's tool_calls all have a matching answer.
func (a *Agent) reportInterruptedTools(calls []llm.ToolCall, from int) {
	for i := from; i < len(calls); i++ {
		a.bus.Publish(Event{
			Type:    EventToolResult,
			Name:    calls[i].Function.Name,
			Text:    "interrupted by user",
			IsError: true,
		})
		a.appendMessage(llm.Message{
			Role:       "tool",
			ToolCallID: calls[i].ID,
			Name:       calls[i].Function.Name,
			Content:    "interrupted by user before the tool finished",
		})
	}
}

// finishInterrupt ends a turn that the user cancelled. When the turn produced
// nothing yet (it was cancelled while waiting for the model), the user records it
// appended are dropped so the conversation is left exactly as it was before the
// turn; otherwise the records stay and only a marker is reported. The next user
// message then starts a fresh turn. userCount is how many user messages the turn
// recorded.
func (a *Agent) finishInterrupt(turnStart, userCount int) {
	if a.rollbackTurn(turnStart, userCount) {
		a.bus.Publish(Event{
			Type: EventInterrupted,
			Text: "interrupted while waiting for the model; the pending message was discarded",
		})
		return
	}
	a.bus.Publish(Event{Type: EventInterrupted, Text: "interrupted; the turn was stopped"})
}

// rollbackTurn drops the records appended for this turn when nothing else was
// produced. It reports whether it rolled back.
func (a *Agent) rollbackTurn(turnStart, userCount int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	end := turnStart + userCount
	if len(a.history) != end {
		// Something was produced (or the history moved on): nothing to drop.
		return false
	}
	for _, m := range a.history[turnStart:end] {
		if m.Role != "user" {
			return false
		}
	}
	a.history = a.history[:turnStart]
	return true
}

// historyLen returns the number of recorded messages.
func (a *Agent) historyLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.history)
}

// callLLM performs one completion over the current context.
func (a *Agent) callLLM(ctx context.Context) (*llm.Response, error) {
	a.mu.Lock()
	msgs := a.buildMessagesLocked()
	a.mu.Unlock()

	onDelta := func(text string) {
		a.bus.Publish(Event{Type: EventAssistantDelta, Text: text})
	}
	onReasoning := func(text string) {
		a.bus.Publish(Event{Type: EventReasoningDelta, Text: text})
	}
	return a.client.Chat(ctx, msgs, a.reg.Definitions(), onDelta, onReasoning)
}

// appendMessage appends a message to the history.
func (a *Agent) appendMessage(m llm.Message) {
	a.mu.Lock()
	a.history = append(a.history, m)
	a.mu.Unlock()
}

// buildMessagesLocked renders the full request message list. The caller must
// hold a.mu.
func (a *Agent) buildMessagesLocked() []llm.Message {
	msgs := make([]llm.Message, 0, len(a.history)+1)
	msgs = append(msgs, a.systemMessageLocked())
	msgs = append(msgs, a.history...)
	return msgs
}

// drainSteering folds the queued steering messages into the history, announcing
// each of them as a user event right here: this is the moment the message becomes
// part of the conversation, so the row every front-end draws (and the mirror
// records for a later reload) sits exactly where the message sits in the history —
// after the reply it interrupted, before the answer it steers.
func (a *Agent) drainSteering() {
	for _, m := range a.takeSteering() {
		a.bus.Publish(Event{Type: EventUser, Text: m.text, Source: m.source})
		a.appendMessage(llm.Message{Role: "user", Content: m.text})
	}
}

// steeringPending reports whether a steering message is still waiting to be
// folded into the conversation.
func (a *Agent) steeringPending() bool { return len(a.steerCh) > 0 }

// takeSteering removes and returns every queued steering message, oldest first.
func (a *Agent) takeSteering() []steerMessage {
	var msgs []steerMessage
	for {
		select {
		case m := <-a.steerCh:
			msgs = append(msgs, m)
		default:
			return msgs
		}
	}
}

// requeueSteering puts steering messages back into the queue, for the case where
// another turn claimed the agent before they could be handed to a turn of their
// own. A full queue drops them with the same error pushSteer reports.
func (a *Agent) requeueSteering(msgs []steerMessage) {
	for _, m := range msgs {
		select {
		case a.steerCh <- m:
		default:
			a.bus.Publish(Event{Type: EventError, Text: "steering queue is full; message dropped"})
		}
	}
}

// startSteeringTurn runs the steering messages still queued after a turn ended as
// a turn of their own. They are announced here, like drainSteering announces the
// ones a running turn folds in: the message joins the conversation at this point,
// so its row belongs after everything the previous turn produced. It is a no-op
// when nothing is queued.
func (a *Agent) startSteeringTurn() {
	msgs := a.takeSteering()
	if len(msgs) == 0 {
		return
	}
	a.mu.Lock()
	if a.busy {
		// A user message arrived while the turn was tearing down and already
		// started the next turn: hand the messages back to it.
		a.mu.Unlock()
		a.requeueSteering(msgs)
		return
	}
	a.busy = true
	turnCtx, cancel := context.WithCancel(context.Background())
	a.cancelTurn = cancel
	a.mu.Unlock()

	texts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		a.bus.Publish(Event{Type: EventUser, Text: m.text, Source: m.source})
		texts = append(texts, m.text)
	}
	go a.runTurn(turnCtx, texts)
}

// setUsage records the provider-reported token accounting for the request just
// made. It keeps the prompt-token count — the size of the context that was sent
// — and remembers how much of the history it covers, so messages appended
// afterwards can be estimated on top of it.
func (a *Agent) setUsage(u llm.Usage) {
	tokens := u.PromptTokens
	if tokens <= 0 {
		tokens = u.TotalTokens
	}
	if tokens <= 0 {
		return
	}
	a.mu.Lock()
	a.usage = tokens
	a.usageAt = len(a.history)
	a.mu.Unlock()
}

// compactIfNeeded compresses the context when it exceeds the configured trigger.
func (a *Agent) compactIfNeeded(ctx context.Context) {
	if a.compactor == nil {
		return
	}
	a.mu.Lock()
	// The trigger is estimated against the very prefix the next request will
	// carry, so the capability sections appended below the base prompt count.
	need := a.compactor.shouldCompact(a.history, a.livePrefixLocked(), a.usage)
	a.mu.Unlock()
	if need {
		a.doCompact(ctx, summarizeModeAuto)
	}
}

// CompactNow forces a compaction pass (used by the /compact command). It returns
// the note the caller should print: a pass that did compress reports itself on the
// bus (the "compacting" info and the compacted event), so it returns "" then.
func (a *Agent) CompactNow(ctx context.Context) string {
	if a.compactor == nil {
		return "compaction is not configured"
	}
	if !a.doCompact(ctx, summarizeModeManual) {
		return "nothing to compress yet"
	}
	return ""
}

// doCompact performs one compaction pass. It returns whether a change happened.
func (a *Agent) doCompact(ctx context.Context, mode summarizeMode) bool {
	a.mu.Lock()
	hist := append([]llm.Message(nil), a.history...)
	sum := a.summary
	// The summarizing call reuses the live request prefix verbatim — the very
	// system prompt and declared tools the ordinary calls send — so nothing
	// below the base prompt is lost and the provider's cached prompt prefix
	// stays valid across the pass.
	prefix := a.livePrefixLocked()
	a.mu.Unlock()

	// Summarizing is a model call that can take a while, so the pass announces
	// itself on the bus before it starts: both front-ends then show what the
	// wait is for. A pass with nothing to condense stays silent (its caller
	// reports that).
	cut, ok := a.compactor.cut(hist, mode)
	if !ok {
		return false
	}
	a.bus.Publish(Event{
		Type: EventInfo,
		Text: fmt.Sprintf("compacting context: summarizing %d of %d messages", cut, len(hist)),
	})

	newHist, newSum, changed, err := a.compactor.compact(ctx, hist, prefix, sum, mode)
	if err != nil {
		a.bus.Publish(Event{Type: EventError, Text: "compaction: " + err.Error()})
	}
	if !changed {
		return false
	}

	a.mu.Lock()
	a.history = newHist
	a.summary = newSum
	a.usage = 0
	a.usageAt = 0
	a.mu.Unlock()

	// The summary rides along with the event: it is what replaced the messages
	// that were just cut out of the context, so the front-ends print it at the
	// truncation point instead of reaching into the agent for it.
	a.bus.Publish(Event{
		Type:    EventCompacted,
		Text:    fmt.Sprintf("context compressed: %d -> %d messages", len(hist), len(newHist)),
		Summary: newSum,
	})
	a.save()
	return true
}

// save persists the current state through the registered hook.
func (a *Agent) save() {
	if a.persist == nil {
		return
	}
	a.mu.Lock()
	hist := append([]llm.Message(nil), a.history...)
	sum := a.summary
	a.mu.Unlock()
	a.persist(hist, sum)
}
