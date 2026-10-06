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
	"time"

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
	// at the bottom of the prompt. It is generated in New instead of living in
	// the prompt template, so agent.md never carries a hard-coded platform.
	runtimeInfo string
	// workingDirInfo is the optional working-directory line (the absolute
	// directory the process runs in, nothing more). It is empty when
	// agent.include_working_dir is off or the directory cannot be resolved. It
	// is set once in New.
	workingDirInfo string
	// unlockRule is the single global "Tool Discovery & Unlock" mechanism
	// section. It is non-empty only while locked (deferred) functions exist,
	// so it is injected exactly once and only when it is meaningful.
	unlockRule string
	// mcpInfo holds one "MCP global info" entry per connected MCP server. It
	// never lists the servers' functions.
	mcpInfo []MCPServerInfo
	summary string
	// summaryInSystem says where a request carries the accumulated summary:
	// true appends it to the system prompt, false (the default, from
	// agent.summary_in_system_prompt) sends it as the first user message.
	summaryInSystem bool
	history         []llm.Message
	busy            bool
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

	// onlyThink handling: a reply that carried only the model's thinking (no
	// visible text, no tool calls). includeOnlyThink keeps such a message in
	// the history (agent.include_only_think); continueOnlyThink asks the model
	// again instead of ending the turn (agent.continue_only_think) and only
	// applies while includeOnlyThink is on.
	includeOnlyThink  bool
	continueOnlyThink bool

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
	// The working-directory line tells the model where the process runs without
	// spending a tool call. Only the path is reported: the directory's children
	// are not listed.
	var workingDirInfo string
	if cfg.Agent.IncludeWorkingDir {
		if cwd, err := os.Getwd(); err == nil {
			workingDirInfo = WorkingDirectoryInfo(cwd)
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
	// The context window and the output reserve are properties of the active
	// interface's model; SwitchLLM replaces them when the user picks another
	// interface at runtime.
	_, activeLLM, _ := cfg.ActiveLLM()
	contextWindow := activeLLM.ContextWindow
	if contextWindow <= 0 {
		contextWindow = 131072
	}
	maxTokens := activeLLM.MaxTokens
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
		workingDirInfo:     workingDirInfo,
		unlockRule:         unlockRule,
		summaryInSystem:    cfg.Agent.SummaryInSystemPrompt,
		maxIter:            maxIter,
		autoSplitWrites:    cfg.Tools.WriteFile.AutoSplit,
		contextWindow:      contextWindow,
		toolResultsVisible: true,
		includeOnlyThink:   cfg.Agent.IncludeOnlyThink,
		continueOnlyThink:  cfg.Agent.ContinueOnlyThink,
		steerCh:            make(chan steerMessage, 64),
	}
	a.compactor = &compactor{
		client:                client,
		contextWindow:         contextWindow,
		maxTokens:             maxTokens,
		summarizeTokenPercent: summarizePercent,
		keepAuto:              cfg.Context.SummarizeKeep.Auto,
		keepManual:            cfg.Context.SummarizeKeep.Manual,
	}
	return a
}

// Bus exposes the agent event bus for subscribers.
func (a *Agent) Bus() *Bus { return a.bus }

// Model returns the model name of the active interface, or "" when none is set.
func (a *Agent) Model() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil {
		return ""
	}
	return a.client.Model()
}

// SwitchLLM swaps the client and the numbers derived from the active interface
// — its context window (compaction trigger and usage display) and its output
// reserve. It refuses while a turn is running, so callLLM and the compactor
// never observe a half-applied switch: a turn holds no lock across its request,
// and this runs only when the agent is idle. It is the lightweight equivalent
// of re-initializing the program on a new interface.
func (a *Agent) SwitchLLM(client *llm.Client, contextWindow, maxTokens int) error {
	if client == nil {
		return errors.New("switch llm: no client")
	}
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return errors.New("a turn is running; try again when idle")
	}
	a.client = client
	if contextWindow > 0 {
		a.contextWindow = contextWindow
	}
	if a.compactor != nil {
		a.compactor.client = client
		if contextWindow > 0 {
			a.compactor.contextWindow = contextWindow
		}
		if maxTokens > 0 {
			a.compactor.maxTokens = maxTokens
		}
	}
	a.mu.Unlock()
	// The context window belongs to the interface: report the usage again so the
	// meter follows the window that is now active instead of keeping the old one
	// until the next model reply.
	a.bus.Publish(a.usageEvent())
	return nil
}

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
// capability sections — the single global unlock rule and, per connected server,
// the MCP global info line — and closing with the host sections, the runtime
// line and the working-directory line. The host sections are generated here
// rather than coming from the base prompt, so an agent.md never carries a
// hard-coded platform or path. The caller must hold a.mu.
func (a *Agent) systemPrompt() string {
	parts := make([]string, 0, len(a.mcpInfo)+4)
	if trimmed := strings.TrimRight(a.base, "\n"); trimmed != "" {
		parts = append(parts, trimmed)
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
	if a.runtimeInfo != "" {
		parts = append(parts, a.runtimeInfo)
	}
	if a.workingDirInfo != "" {
		parts = append(parts, a.workingDirInfo)
	}
	return strings.Join(parts, "\n\n")
}

// systemMessageLocked renders the system message every request carries: the
// rendered system prompt, with the accumulated summary appended while
// agent.summary_in_system_prompt is on. The caller must hold a.mu.
func (a *Agent) systemMessageLocked() llm.Message {
	content := a.systemPrompt()
	if a.summaryInSystem {
		content = systemWithSummary(content, a.summary)
	}
	return llm.Message{Role: "system", Content: content}
}

// livePrefixLocked renders the fixed head of every request the conversation
// sends — the system message, the declared tool schemas and the accumulated
// summary with the place it occupies — in one snapshot. Compaction passes it to
// the summarizing call verbatim, which keeps that call's prefix byte-identical to
// the live ones. The caller must hold a.mu.
func (a *Agent) livePrefixLocked() livePrefix {
	return livePrefix{
		systemPrompt:          a.systemMessageLocked().Content,
		tools:                 a.reg.Definitions(),
		summary:               a.summary,
		summaryInSystemPrompt: a.summaryInSystem,
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
	// The context was replaced: report its new size so both front-ends drop the
	// previous conversation's usage right away (a loaded session can be far
	// larger or smaller than what was on screen).
	a.bus.Publish(a.usageEvent())
}

// Reset clears the conversation.
func (a *Agent) Reset() {
	a.mu.Lock()
	a.history = nil
	a.summary = ""
	a.usage = 0
	a.usageAt = 0
	a.mu.Unlock()
	// The context is empty now: refresh the usage so the front-ends stop showing
	// the size of the conversation that was just cleared.
	a.bus.Publish(a.usageEvent())
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
// will carry, and the number the compaction trigger is measured on (see
// compactIfNeeded). A provider-reported prompt-token count anchors the total and
// the messages appended since that report are estimated on top of it — a
// lower-bound count, never a competing whole-context estimate (see
// EstimateMessageTokens). Without a report (or when the history shrank past the
// anchor) it falls back to the pure local estimate. The caller must hold a.mu.
func (a *Agent) contextTokensLocked() int {
	if a.usage > 0 && a.usageAt >= 0 && a.usageAt <= len(a.history) {
		return a.usage + EstimateMessagesTokens(a.history[a.usageAt:])
	}
	// The request is estimated instead of the pieces: that counts the summary
	// wherever agent.summary_in_system_prompt puts it.
	return EstimateMessagesTokens(a.buildMessagesLocked())
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
func (a *Agent) SubmitFrom(source, text string) { a.submit(source, text, nil) }

// SubmitMedia is SubmitFrom with media parts: files the message carries along
// with its text (the web mirror's attachments). They are attached to the user
// message, so the model receives them together with the text.
func (a *Agent) SubmitMedia(source, text string, media []llm.ContentPart) {
	a.submit(source, text, media)
}

func (a *Agent) submit(source, text string, media []llm.ContentPart) {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		a.pushSteer(source, text, media)
		return
	}
	a.busy = true
	turnCtx, cancel := context.WithCancel(context.Background())
	a.cancelTurn = cancel
	a.mu.Unlock()

	a.bus.Publish(Event{Type: EventUser, Text: text, Source: source, Attachments: llm.MediaAttachments(media)})
	go a.runLoop(turnCtx, userInput{text: text, media: media})
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
// into the conversation. media are the attachments the message carries (empty
// for a plain message).
type steerMessage struct {
	source string
	text   string
	media  []llm.ContentPart
}

// pushSteer queues a steering message for the running turn. Nothing is announced
// here: the row is published when the turn folds the message into the
// conversation (see drainSteering), which is the point the message actually
// belongs at — after the reply it interrupted. Announcing it earlier would draw
// it in the middle of that reply (and would replay in the wrong place too, since
// the mirror records the rows the bus carries).
func (a *Agent) pushSteer(source, text string, media []llm.ContentPart) {
	select {
	case a.steerCh <- steerMessage{source: source, text: text, media: media}:
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

// maxConsecutiveAutoContinues bounds how many times one turn auto-continues
// without a new user message: an only-thinking reply (while
// agent.continue_only_think is on) and a reply the provider cut off at
// max_tokens (finish_reason "length") share this one budget, so a model stuck
// producing thinking alone — or never finishing inside max_tokens — cannot spin
// the loop. A round that runs tools resets it.
const maxConsecutiveAutoContinues = 3

// userInput is one user message of a turn: its text plus the media parts it
// carries (the files the web mirror attached to it; empty for plain input).
type userInput struct {
	text  string
	media []llm.ContentPart
}

// runLoop executes a full turn: LLM call → tool calls → repeat. The turn's
// context is cancelled by Interrupt, which stops the in-flight step and ends the
// turn (see finishInterrupt).
func (a *Agent) runLoop(ctx context.Context, input userInput) {
	a.runTurn(ctx, []userInput{input})
}

// runTurn is the body of a turn: it records the turn's user messages (which the
// caller has announced on the bus already), runs the LLM/tool loop and tears the
// turn down. A turn that picks up queued steering messages passes them all at
// once.
func (a *Agent) runTurn(ctx context.Context, inputs []userInput) {
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

	for _, input := range inputs {
		a.appendMessage(llm.Message{Role: "user", Content: input.text, Media: input.media})
	}
	// Report the context size right away so the frontends show the new user
	// message while the model call is still in flight.
	a.bus.Publish(a.usageEvent())

	// autoContinues counts the auto-continuations in a row this turn has made
	// without a new user message: an only-thinking reply and a max_tokens
	// truncation share the count (see maxConsecutiveAutoContinues). A tool
	// round resets it.
	autoContinues := 0
	// overflowRecoveries counts how many times this turn recovered from a
	// request the provider rejected for its size (see recoverContextOverflow).
	overflowRecoveries := 0
	// skipCompact suppresses the proactive pass on the iteration right after
	// such a recovery: the recovery has just compressed everything it could,
	// and a pass here could only condense the user messages it replayed — the
	// very input being sent again.
	skipCompact := false

	for i := 0; i < a.maxIter; i++ {
		if ctx.Err() != nil {
			// The turn was interrupted while the previous step was running
			// and that step still completed (a tool call that cannot be cut
			// short is allowed to finish): the turn ends here instead of
			// asking the model for another reply it would never get.
			a.finishInterrupt()
			return
		}
		a.drainSteering()

		if skipCompact {
			skipCompact = false
		} else {
			a.compactIfNeeded(ctx)
		}

		resp, started, err := a.callLLM(ctx)
		if err != nil {
			if turnInterrupted(ctx, err) {
				// The reply the provider had already streamed (if any) stays
				// in the history; the tool calls it carried do not.
				a.finishInterruptedModelCall(resp, started)
				return
			}
			// A request the provider rejected for its size is recovered
			// instead of failed: the newest messages are rolled back out of
			// the context, what is left is compressed into the summary and
			// those user messages are replayed on top of it, so the retry
			// sends them in front of a context that fits (see
			// recoverContextOverflow). Oversized tool feedback and the media
			// the local estimate undercounts are the usual causes, which is
			// why the provider's own rejection is the only reliable signal
			// (llm.IsContextLengthError).
			if llm.IsContextLengthError(err) && overflowRecoveries < maxContextOverflowRecoveries &&
				a.recoverContextOverflow(ctx) {
				overflowRecoveries++
				skipCompact = true
				a.bus.Publish(a.usageEvent())
				continue
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
		// call the model made keeps the first part and the remaining parts are
		// appended to the same assistant message as extra tool calls (see
		// runWriteSplits), which is the shape a provider expects for a reply
		// that asks for several calls at once. The rewrite happens before the
		// assistant message is recorded, so the history and the front-ends
		// show the arguments that were executed.
		continuations := a.autoSplitWriteCalls(resp.ToolCalls)

		// A reply that carries only the model's thinking — no visible text and
		// no tool calls — is not an answer, whatever the provider's
		// finish_reason says (a normal stop, a cut-off at max_tokens, or no
		// reason at all). agent.include_only_think decides whether it is
		// recorded at all; agent.continue_only_think then decides whether the
		// loop asks the model again, with the recorded thinking in front of it.
		onlyThink := len(resp.ToolCalls) == 0 &&
			strings.TrimSpace(resp.Content) == "" && strings.TrimSpace(resp.Reasoning) != ""

		// assistantIdx points at the assistant message in the history: the
		// auto-split write parts attach to it, so the whole split travels as
		// one assistant message carrying several tool calls (see
		// runWriteSplits).
		assistantIdx := -1
		if !onlyThink || a.includeOnlyThink {
			assistant := llm.Message{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: resp.ToolCalls,
				// Keep the thinking on the message so it is preserved in the
				// history and sent back with the next request.
				ReasoningContent: resp.Reasoning,
			}
			assistantIdx = a.appendMessage(assistant)
			if resp.Content != "" {
				a.bus.Publish(Event{Type: EventAssistant, Text: resp.Content, Time: started})
			}
			// The assistant message just joined the context: refresh the usage so
			// the badge reflects the provider's count plus this reply.
			a.bus.Publish(a.usageEvent())
		}

		if len(resp.ToolCalls) == 0 {
			if a.steeringPending() {
				// A steering message arrived while this reply was
				// still streaming: the reply is not the end of the
				// turn then, so the turn carries on. The next
				// iteration folds the message into the context —
				// announcing its row there, after this reply — and
				// calls the model again.
				continue
			}
			// Ask the model again without a new user message when the reply
			// carried only thinking (and agent.continue_only_think is on) or
			// the provider cut it off at max_tokens after some text. Both draw
			// on the same budget: a run of such continuations stops the turn
			// (see maxConsecutiveAutoContinues), so a model stuck producing
			// nothing but thinking cannot spin the loop.
			var retry string
			switch {
			case onlyThink && a.includeOnlyThink && a.continueOnlyThink:
				retry = "the reply carried only thinking; asking the model again"
			case onlyThink:
				// Kept or dropped, the turn ends here: the configuration does
				// not ask the model again.
				return
			case resp.Finish == "length":
				retry = "response truncated at max_tokens; continuing"
			default:
				return
			}
			autoContinues++
			if autoContinues >= maxConsecutiveAutoContinues {
				a.bus.Publish(Event{
					Type: EventError,
					Text: fmt.Sprintf("stopped after %d consecutive continuations (only thinking or cut off at max_tokens); raise openai.max_tokens or turn off agent.continue_only_think",
						autoContinues),
				})
				return
			}
			a.bus.Publish(Event{
				Type: EventInfo,
				Text: fmt.Sprintf("%s (%d/%d)", retry, autoContinues, maxConsecutiveAutoContinues),
			})
			continue
		}
		autoContinues = 0

		var pendingSplits []llm.ToolCall
		for idx, tc := range resp.ToolCalls {
			if ctx.Err() != nil {
				if idx == 0 {
					// The user stopped the turn before the round's first call
					// could start: the calls come off the reply (none of them
					// ran, and a reply that is still arriving may hold
					// fragments) and the message stays only while it has text
					// left. Nothing is answered — the message no longer asks
					// for anything — and the turn ends here.
					a.finishUnstartedToolRound()
					return
				}
				// The user stopped the turn while the previous call was
				// running and that call returned on its own (a tool that
				// cannot be cut short finishes, and so does a poll, which
				// stops waiting). Every call that never started is answered
				// as interrupted — the calls from this index on, then the
				// write parts an earlier call still owes — which keeps the
				// assistant message's tool_calls paired, and the turn ends
				// without asking the model again. The batch's calls are
				// answered first and the parts after them: the order they sit
				// in on the assistant message.
				a.reportInterruptedTools(resp.ToolCalls, idx)
				a.attachSplitCalls(assistantIdx, pendingSplits)
				a.reportInterruptedTools(pendingSplits, 0)
				a.finishInterrupt()
				return
			}
			res := a.dispatchToolCall(ctx, tc)
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

		// The remaining parts of an auto-split write run right here, attached
		// to the assistant message that requested the write, so the model is
		// only asked again once the payload is on disk in order.
		if len(pendingSplits) > 0 {
			if !a.runWriteSplits(ctx, assistantIdx, pendingSplits) {
				a.finishInterrupt()
				return
			}
		}
	}

	if ctx.Err() != nil {
		// The last round's call finished after the user interrupted the turn
		// and the iteration budget is spent: report the interrupted end
		// instead of a turn that used up all of its rounds.
		a.finishInterrupt()
		return
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
// The call always ends with an answer: a tool that can react to the turn's
// cancellation returns early with what it has, and one that cannot is allowed to
// finish, so the answer is its real result either way.
func (a *Agent) dispatchToolCall(ctx context.Context, tc llm.ToolCall) *tools.Result {
	a.bus.Publish(Event{Type: EventToolCall, Name: tc.Function.Name, Args: tc.Function.Arguments})
	res := a.executeTool(ctx, tc)
	if res == nil {
		res = tools.Fail("the tool produced no result")
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
		Media:      res.Media,
	})
	return res
}

// executeTool runs one tool call and waits for its answer. The turn context
// travels with the call, so the tools that can observe an interrupt return early
// on their own: exec_command terminates its process tree and reports the output
// the process had produced until then, manage_session poll stops waiting and
// reports the output it has (the process keeps running). A tool that cannot be
// cut short is simply allowed to finish — abandoning it would throw its work
// away — and its own result is what the call is answered with.
func (a *Agent) executeTool(ctx context.Context, tc llm.ToolCall) *tools.Result {
	// unlock_tool skips resending a schema that is still present in the
	// current effective (uncompacted) context: the lookup scans the live
	// history, from which compaction has already dropped older messages, so
	// an evicted schema is re-delivered automatically.
	execCtx := tools.WithUnlockLookup(ctx, func(toolName string) bool {
		return tools.MessagesContainUnlockRecord(a.History(), toolName)
	})
	return a.reg.Execute(execCtx, tc.Function.Name, tc.Function.Arguments)
}

// reportInterruptedTools publishes the interrupted feedback for every call from
// from on and records a tool message for each of them, so the assistant
// message's tool_calls all have a matching answer. These calls never started:
// the turn was interrupted after the call before them had returned. The answers
// are recorded only — the turn ends here, so the model is never asked with them.
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
			Content:    "interrupted by user before the call started",
		})
	}
}

// finishUnstartedToolRound ends a tool round the user interrupted before its
// first call could start. The reply keeps its text but loses every tool call it
// carried, and the message goes too when that leaves it without text and without
// thinking — an assistant message with nothing in it has nothing to tell the next
// request. No tool answer is recorded: the message no longer asks for anything,
// and the turn ends here, so the model is never asked with these results.
func (a *Agent) finishUnstartedToolRound() {
	dropped, messageGone := a.dropUnstartedToolCalls()
	text := fmt.Sprintf("interrupted before any tool call started; the reply was kept without its %d tool call(s)", dropped)
	if messageGone {
		text = fmt.Sprintf("interrupted before any tool call started; the reply had no text, so it and its %d tool call(s) were dropped", dropped)
	}
	a.bus.Publish(Event{Type: EventInterrupted, Text: text})
}

// dropUnstartedToolCalls takes the tool calls off the reply the user interrupted
// before any of them could start, and drops the message itself when that leaves
// it with nothing to say: no text, and — unless agent.include_only_think keeps
// the thinking — no reasoning either. It reports how many calls were dropped and
// whether the message went with them.
func (a *Agent) dropUnstartedToolCalls() (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.history)
	if n == 0 || a.history[n-1].Role != "assistant" {
		return 0, false
	}
	dropped := len(a.history[n-1].ToolCalls)
	a.history[n-1].ToolCalls = nil
	if strings.TrimSpace(a.history[n-1].Content) == "" &&
		!(a.includeOnlyThink && strings.TrimSpace(a.history[n-1].ReasoningContent) != "") {
		a.history = a.history[:n-1]
		return dropped, true
	}
	return dropped, false
}

// finishInterruptedModelCall ends a turn the user interrupted while the model was
// answering. The user message always stays: an interrupt never rewrites what the
// user sent. What is dropped is the assistant side of the turn —
//
//   - text the provider had already delivered is kept as one assistant message
//     (see keepPartialReply);
//   - a reply whose text is empty after trimming is dropped entirely — an
//     assistant message without content has nothing to tell the next request —
//     unless it carried thinking and agent.include_only_think is on, in which
//     case the thinking is kept on its own;
//   - a reply that never arrived leaves no record at all.
//
// Whatever is kept travels with the next user message, never as a call of its own.
func (a *Agent) finishInterruptedModelCall(resp *llm.Response, started time.Time) {
	switch {
	case a.keepPartialReply(resp, started):
		a.bus.Publish(Event{
			Type: EventInterrupted,
			Text: "interrupted while the reply was streaming; the partial reply was kept",
		})
	case resp == nil || (resp.Content == "" && resp.Reasoning == "" && len(resp.ToolCalls) == 0):
		a.bus.Publish(Event{
			Type: EventInterrupted,
			Text: "interrupted while waiting for the model; nothing had been produced",
		})
	default:
		text := "interrupted; the reply had no text, so its message was dropped"
		if n := len(resp.ToolCalls); n > 0 {
			text = fmt.Sprintf("interrupted; the reply had no text, so its message and its %d tool call(s) were dropped", n)
		}
		a.bus.Publish(Event{Type: EventInterrupted, Text: text})
	}
}

// keepPartialReply records the reply a cancelled model call had streamed so far
// as one assistant message: its text (with the thinking that came with it) is
// kept, the tool calls it carried are not. None of those calls started, and the
// reply never finished, so even a call that looks complete can be a fragment; the
// turn ends here, which means nothing would ever run them. It reports whether a
// message was kept — a reply whose text is empty after trimming is only kept
// while it carried thinking and agent.include_only_think is on, because an empty
// message otherwise has nothing to contribute — and the kept text is published
// as the assistant row the front-ends had been streaming, so the row is
// finalized live and replayed from the mirror.
func (a *Agent) keepPartialReply(resp *llm.Response, started time.Time) bool {
	if resp == nil {
		return false
	}
	if strings.TrimSpace(resp.Content) == "" &&
		!(a.includeOnlyThink && strings.TrimSpace(resp.Reasoning) != "") {
		return false
	}
	a.appendMessage(llm.Message{
		Role:             "assistant",
		Content:          resp.Content,
		ReasoningContent: resp.Reasoning,
	})
	if resp.Content != "" {
		a.bus.Publish(Event{Type: EventAssistant, Text: resp.Content, Time: started})
	}
	if n := len(resp.ToolCalls); n > 0 {
		a.bus.Publish(Event{
			Type: EventInfo,
			Text: fmt.Sprintf("the interrupted reply carried %d tool call(s); none of them ran and they were dropped", n),
		})
	}
	return true
}

// finishInterrupt ends a turn the user cancelled after the turn had already
// produced records: the tool round that was interrupted (the running call's own
// answer plus the interrupted answers of the calls that never started) stays in
// the history and only a marker is reported. The user messages stay too — an
// interrupt never rewrites what the user sent — and the next user message starts
// a fresh turn whose request carries everything recorded here.
func (a *Agent) finishInterrupt() {
	a.bus.Publish(Event{Type: EventInterrupted, Text: "interrupted; the turn was stopped"})
}

// callLLM performs one completion over the current context. The request head —
// the system message, the accumulated summary where the configuration puts it
// and the declared tools — comes from one livePrefix snapshot, the very function
// the compaction pass calls (see livePrefixLocked): the ordinary calls and the
// summarizing one cannot then carry heads taken from different states, so the
// provider's cached prompt prefix stays valid across a pass.
//
// The returned start time is the moment the reply began: the arrival of its
// first streamed chunk (the model's thinking included). With a streamed reply
// the front-ends stamp the message with it; the whole reply carries the same
// value, so a delta and the final assistant event cannot disagree. It stays
// zero when nothing streamed (a non-streaming reply, or an empty answer), and
// the caller then falls back to the moment the whole reply arrived.
func (a *Agent) callLLM(ctx context.Context) (*llm.Response, time.Time, error) {
	a.mu.Lock()
	prefix := a.livePrefixLocked()
	msgs := prefix.head(a.history)
	// The client is captured under the lock so a runtime interface switch (which
	// also takes a.mu, and only while idle) can never race the read.
	client := a.client
	a.mu.Unlock()

	var started time.Time
	var once sync.Once
	markStart := func() time.Time {
		once.Do(func() { started = time.Now() })
		return started
	}
	onDelta := func(text string) {
		a.bus.Publish(Event{Type: EventAssistantDelta, Text: text, Time: markStart()})
	}
	onReasoning := func(text string) {
		a.bus.Publish(Event{Type: EventReasoningDelta, Text: text, Time: markStart()})
	}
	if client == nil {
		return nil, started, errors.New("no llm client")
	}
	resp, err := client.Chat(ctx, msgs, prefix.tools, onDelta, onReasoning)
	return resp, started, err
}

// appendMessage appends a message to the history and returns its index, so a
// caller can reach the message again: the auto-split write parts attach to the
// assistant message that requested the write (see attachSplitCalls).
func (a *Agent) appendMessage(m llm.Message) int {
	a.mu.Lock()
	a.history = append(a.history, m)
	idx := len(a.history) - 1
	a.mu.Unlock()
	return idx
}

// buildMessagesLocked renders the full request message list: the system message,
// the accumulated summary (as the first user message, or in the system prompt
// when agent.summary_in_system_prompt asks for it) and the history. It is the
// livePrefix rendering itself (see livePrefix.head), so the ordinary calls, the
// usage estimate and the summarizing call all take their head from one place.
// The caller must hold a.mu.
func (a *Agent) buildMessagesLocked() []llm.Message {
	return a.livePrefixLocked().head(a.history)
}

// drainSteering folds the queued steering messages into the history, announcing
// each of them as a user event right here: this is the moment the message becomes
// part of the conversation, so the row every front-end draws (and the mirror
// records for a later reload) sits exactly where the message sits in the history —
// after the reply it interrupted, before the answer it steers.
func (a *Agent) drainSteering() {
	for _, m := range a.takeSteering() {
		a.bus.Publish(Event{Type: EventUser, Text: m.text, Source: m.source, Attachments: llm.MediaAttachments(m.media)})
		a.appendMessage(llm.Message{Role: "user", Content: m.text, Media: m.media})
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

	inputs := make([]userInput, 0, len(msgs))
	for _, m := range msgs {
		a.bus.Publish(Event{Type: EventUser, Text: m.text, Source: m.source, Attachments: llm.MediaAttachments(m.media)})
		inputs = append(inputs, userInput{text: m.text, media: m.media})
	}
	go a.runTurn(turnCtx, inputs)
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

// compactionDue reports whether the automatic compaction pass has to run. It
// measures the request the next iteration will send — the provider-reported
// prompt-token count plus the lower-bound estimate of the messages appended since
// that report (see contextTokensLocked), i.e. the number the frontends display.
// The character-count estimate of the whole context is deliberately NOT compared
// against it: a lower bound in different units is not a competing measurement, and
// letting the two compete made a pass fire at 60% of the window while the setting
// said 80%. A request the provider rejects for its size is recovered from instead
// (see recoverContextOverflow).
func (a *Agent) compactionDue() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.compactor == nil {
		return false
	}
	return a.compactor.shouldCompact(a.contextTokensLocked())
}

// compactIfNeeded compresses the context when it exceeds the configured trigger.
func (a *Agent) compactIfNeeded(ctx context.Context) {
	if a.compactionDue() {
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
	// system prompt, accumulated summary (in the place the request carries it)
	// and declared tools the ordinary calls send — so nothing below the base
	// prompt is lost and the provider's cached prompt prefix stays valid across
	// the pass.
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
	// The context shrank with the pass: report the new size so the meter drops to
	// the compacted estimate instead of keeping the pre-pass number (the pass
	// runs outside a turn too, from /compact, so nothing else would refresh it).
	a.bus.Publish(a.usageEvent())
	a.save()
	return true
}

// maxContextOverflowRecoveries bounds how many times one turn recovers from the
// provider rejecting a request for its size (roll back, compress, replay).
const maxContextOverflowRecoveries = 2

// recoverContextOverflow reacts to a request the provider rejected because it did
// not fit the model's context window. That rejection is the estimate's failure,
// not the request's: oversized tool feedback and media parts (an attachment the
// user sent, an image a tool read back) can cost far more than
// EstimateMessageTokens counts, so a request can be over the window while the
// estimate still says it fits. The recovery therefore
//
//  1. rolls the newest messages back out of the context — the assistant/tool rows
//     this turn appended and the user messages no answer followed, i.e. exactly
//     the content that made the request too large (see overflowRollbackCut),
//  2. compresses everything that is left with the overflow mode, which keeps
//     nothing raw (only the summary may survive a retry, whatever the configured
//     retention policies say): the surviving history is replaced by the
//     accumulated summary. Those rolled-back user messages are deliberately
//     not part of that batch, so an oversized attachment among them cannot make
//     the summarizing call overflow too,
//  3. replays them on top of the summary, so the model is asked again with the
//     same user input in front of a context that is now as small as it gets.
//
// The pass reports itself on the bus (an info naming the rollback, then the usual
// compacting info and the compacted event) so both front-ends explain the extra
// wait. It reports whether the context really became smaller, i.e. whether a
// retry can behave differently: when neither the rollback nor the pass frees
// anything, the caller reports the provider's error instead of sending the very
// request that was just rejected.
func (a *Agent) recoverContextOverflow(ctx context.Context) bool {
	a.mu.Lock()
	cut := overflowRollbackCut(a.history)
	var (
		replay []llm.Message
		other  int
	)
	for _, m := range a.history[cut:] {
		switch {
		case m.Role != "user":
			// The assistant/tool rows go away for good.
			other++
		case m.Content == contextContinueMessage && len(m.Media) == 0:
			// The engine marker is not a user message: it only keeps a request
			// legal, which the replayed messages do themselves.
		default:
			replay = append(replay, m)
		}
	}
	// The overflow pass keeps nothing raw, so it frees room exactly when the
	// surviving history still holds something to condense.
	frees := a.compactor != nil && a.compactor.wouldCut(a.history[:cut], summarizeModeOverflow)
	if !frees && other == 0 {
		a.mu.Unlock()
		return false
	}
	dropped := len(a.history) - cut
	a.history = a.history[:cut]
	a.mu.Unlock()

	a.bus.Publish(Event{Type: EventInfo, Text: fmt.Sprintf(
		"context length exceeded: rolled back %d message(s), compressing the context and retrying", dropped)})
	a.doCompact(ctx, summarizeModeOverflow)
	a.replayMessages(replay)
	return true
}

// replayMessages puts the user messages an overflow recovery rolled back on top
// of the context again and keeps the request legal afterwards. Nothing is
// published: those rows were announced when they entered the conversation (turn
// start or drainSteering), so announcing them again would draw them twice.
func (a *Agent) replayMessages(msgs []llm.Message) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// A pass that compressed everything leaves its continue marker behind; the
	// replayed messages are the request's user query again, so the marker is
	// dropped exactly as it is not added to a history that already holds one.
	if len(msgs) > 0 {
		if n := len(a.history); n > 0 && a.history[n-1].Role == "user" && a.history[n-1].Content == contextContinueMessage {
			a.history = a.history[:n-1]
		}
		a.history = append(a.history, msgs...)
	}
	// The rollback can take every user message with it (they were the very
	// messages making the request too large) and chat templates reject a request
	// without a user query, so the marker takes their place when none is left.
	a.history = ensureUserMessage(a.history)
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
