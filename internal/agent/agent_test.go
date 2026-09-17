package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lightagent/internal/config"
	"lightagent/internal/llm"
	"lightagent/internal/tools"
)

// blockingTool blocks inside Execute until the turn is cancelled (or the test
// releases it), so an interrupt lands during a tool call.
type blockingTool struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (t *blockingTool) Name() string               { return "block" }
func (t *blockingTool) Description() string        { return "blocks until cancelled" }
func (t *blockingTool) Parameters() map[string]any { return map[string]any{"type": "object"} }

func (t *blockingTool) Execute(ctx context.Context, _ map[string]any) *tools.Result {
	t.once.Do(func() { close(t.started) })
	select {
	case <-ctx.Done():
	case <-t.release:
	}
	return tools.OK("finished")
}

// waitForTurnEnd drains events until the turn ends and reports whether an
// interrupted event was published.
func waitForTurnEnd(t *testing.T, events <-chan Event) bool {
	t.Helper()
	deadline := time.After(10 * time.Second)
	interrupted := false
	for {
		select {
		case ev := <-events:
			if ev.Type == EventInterrupted {
				interrupted = true
			}
			if ev.Type == EventTurnDone {
				return interrupted
			}
		case <-deadline:
			t.Fatal("the turn did not finish")
			return interrupted
		}
	}
}

// TestInterruptDiscardsPendingTurn covers interrupting while the model call is in
// flight: nothing was produced, so the pending user record is dropped and an
// interrupted marker is published before the turn ends.
func TestInterruptDiscardsPendingTurn(t *testing.T) {
	requested := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(requested) })
		// Hold the call open until the test is done with the server; the client
		// cancels long before that.
		<-release
	}))
	// Registered in this order so release runs before Close (cleanups are LIFO).
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	a := New(cfg, llm.NewClient(cfg.OpenAI), tools.NewRegistry(), bus)

	a.Submit("do something")
	select {
	case <-requested:
	case <-time.After(5 * time.Second):
		t.Fatal("the model call never started")
	}
	if !a.Busy() {
		t.Fatal("the agent should be busy")
	}
	if !a.Interrupt() {
		t.Fatal("Interrupt reported no running turn")
	}
	if !waitForTurnEnd(t, events) {
		t.Fatal("no interrupted event was published")
	}
	if a.Busy() {
		t.Fatal("the agent is still busy after the interrupt")
	}
	if msgs := a.History(); len(msgs) != 0 {
		t.Fatalf("history = %d messages, want the pending turn discarded", len(msgs))
	}
	// The next user message starts a fresh turn right away.
	a.Submit("again")
	if !a.Busy() {
		t.Fatal("the next user message should start a new turn")
	}
	if !a.Interrupt() {
		t.Fatal("the new turn should be interruptible too")
	}
	waitForTurnEnd(t, events)
}

// TestInterruptStopsToolCall covers interrupting during a tool call: the tool is
// reported as interrupted, the call is answered so the transcript stays valid,
// and the records produced so far are kept.
func TestInterruptStopsToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"block","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	block := &blockingTool{started: make(chan struct{}), release: make(chan struct{})}
	defer close(block.release)
	reg := tools.NewRegistry()
	reg.Register(block)
	a := New(cfg, llm.NewClient(cfg.OpenAI), reg, bus)

	a.Submit("run block")
	select {
	case <-block.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the tool never started")
	}
	if !a.Interrupt() {
		t.Fatal("Interrupt reported no running turn")
	}

	var sawToolResult, sawInterrupted bool
	deadline := time.After(10 * time.Second)
drain:
	for {
		select {
		case ev := <-events:
			switch {
			case ev.Type == EventToolResult && strings.Contains(ev.Text, "interrupted"):
				sawToolResult = true
			case ev.Type == EventInterrupted:
				sawInterrupted = true
			case ev.Type == EventTurnDone:
				break drain
			}
		case <-deadline:
			t.Fatal("no turn_done after the interrupt")
		}
	}
	if !sawToolResult {
		t.Fatal("the interrupted tool call was not reported")
	}
	if !sawInterrupted {
		t.Fatal("no interrupted event was published")
	}

	msgs := a.History()
	if len(msgs) != 3 {
		t.Fatalf("history = %d messages, want user + assistant + tool", len(msgs))
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "c1" || !strings.Contains(msgs[2].Content, "interrupted") {
		t.Fatalf("tool record = %+v, want an interrupted answer", msgs[2])
	}
}

// agentStubTool is a minimal tools.Tool for prompt/registry tests.
type agentStubTool struct{ name, desc string }

func (s agentStubTool) Name() string               { return s.name }
func (s agentStubTool) Description() string        { return s.desc }
func (s agentStubTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (s agentStubTool) Execute(context.Context, map[string]any) *tools.Result {
	return tools.OK("ok")
}

// newTestAgent builds an agent with no LLM client (no network) for loop-state
// tests.
func newTestAgent(t *testing.T) *Agent {
	t.Helper()
	cfg := config.Default()
	return New(cfg, nil, tools.NewRegistry(), NewBus())
}

// TestDrainSteering verifies that queued steering messages become user turns.
func TestDrainSteering(t *testing.T) {
	a := newTestAgent(t)
	a.steerCh <- "one"
	a.steerCh <- "two"

	a.drainSteering()

	history := a.History()
	if len(history) != 2 {
		t.Fatalf("history = %d messages, want 2", len(history))
	}
	if history[0].Role != "user" || history[0].Content != "one" {
		t.Fatalf("first = %+v", history[0])
	}
	if history[1].Role != "user" || history[1].Content != "two" {
		t.Fatalf("second = %+v", history[1])
	}
}

// TestBuildMessagesIncludesSummary verifies the compressed summary is rendered
// into the system prompt while the history is appended after it.
func TestBuildMessagesIncludesSummary(t *testing.T) {
	a := newTestAgent(t)
	a.Load([]llm.Message{{Role: "user", Content: "hi"}}, "the summary")

	a.mu.Lock()
	msgs := a.buildMessagesLocked()
	a.mu.Unlock()

	if len(msgs) != 2 {
		t.Fatalf("len = %d, want 2 (system + history)", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Fatalf("first role = %q, want system", msgs[0].Role)
	}
	if !strings.Contains(msgs[0].Content, "# CONVERSATION SUMMARY") ||
		!strings.Contains(msgs[0].Content, "the summary") {
		t.Fatalf("system prompt missing summary: %q", msgs[0].Content)
	}
	if msgs[1].Role != "user" || msgs[1].Content != "hi" {
		t.Fatalf("history message = %+v", msgs[1])
	}
}

// TestResetAndStats verifies Load/Reset and the Stats snapshot.
func TestResetAndStats(t *testing.T) {
	a := newTestAgent(t)
	a.Load([]llm.Message{{Role: "user", Content: "hello"}}, "s")

	if s := a.Stats(); s.Messages != 1 || s.Summary != "s" {
		t.Fatalf("stats = %+v", s)
	}

	a.Reset()
	if s := a.Stats(); s.Messages != 0 || s.Summary != "" {
		t.Fatalf("stats after reset = %+v", s)
	}
	if h := a.History(); len(h) != 0 {
		t.Fatalf("history after reset = %d", len(h))
	}
}

// TestCompactionPublishesProgressAndSummary verifies the compaction bus contract:
// the pass announces itself before the (slow) summarizing call, reports the
// summary it produced with the compacted event, and stays silent when there is
// nothing to condense (its caller then reports that instead).
func TestCompactionPublishesProgressAndSummary(t *testing.T) {
	a := newTestAgent(t)
	// Three turns; the manual retention window keeps two, so the oldest turn is
	// compressed. The agent has no LLM client, so summarizing fails and the pass
	// falls back to dropping those messages while keeping the current summary.
	a.Load([]llm.Message{
		{Role: "user", Content: "one"},
		{Role: "assistant", Content: "two"},
		{Role: "user", Content: "three"},
		{Role: "assistant", Content: "four"},
		{Role: "user", Content: "five"},
	}, "carried over")
	events, cancel := a.Bus().Subscribe()
	defer cancel()

	if msg := a.CompactNow(context.Background()); msg != "" {
		t.Fatalf("CompactNow after compressing = %q, want no extra note", msg)
	}

	var kinds []EventType
	var texts []string
	deadline := time.After(2 * time.Second)
	for len(kinds) < 3 {
		select {
		case ev := <-events:
			kinds = append(kinds, ev.Type)
			texts = append(texts, ev.Text)
			if ev.Type == EventCompacted && ev.Summary != "carried over" {
				t.Fatalf("compacted event summary = %q, want the carried-over one", ev.Summary)
			}
		case <-deadline:
			t.Fatalf("events = %v %v, want the info, the error and the compacted one", kinds, texts)
		}
	}
	if kinds[0] != EventInfo || texts[0] != "compacting context: summarizing 2 of 5 messages" {
		t.Fatalf("first event = %v %q, want the compacting info", kinds[0], texts[0])
	}
	if kinds[1] != EventError || kinds[2] != EventCompacted {
		t.Fatalf("event kinds = %v, want info, error, compacted", kinds)
	}
	if texts[2] != "context compressed: 5 -> 3 messages" {
		t.Fatalf("compacted event text = %q", texts[2])
	}

	// The retained window now holds every turn, so a further pass has nothing to
	// condense: it publishes nothing and says so to its caller.
	if msg := a.CompactNow(context.Background()); msg != "nothing to compress yet" {
		t.Fatalf("CompactNow on a compacted history = %q", msg)
	}
	select {
	case ev := <-events:
		t.Fatalf("a pass with nothing to do published %+v", ev)
	default:
	}
}

// TestAutoCompactionKeepsAUserMessage covers a pass that compresses the whole
// context, the user turn of the running loop included: the request that follows
// must still carry a user message (chat templates reject one without a user
// query), so the engine inserts its continue marker.
func TestAutoCompactionKeepsAUserMessage(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies [][]capturedMessage
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []capturedMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, req.Messages)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	// A tiny window with a 1% trigger compresses on every iteration, and a
	// retention budget of a few tokens leaves no room for the newest turn, so
	// the whole tail is cut — the user turn included.
	cfg.Context.ContextWindow = 100
	cfg.Context.SummarizeTokenPercent = 1
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	a := New(cfg, llm.NewClient(cfg.OpenAI), tools.NewRegistry(), bus)

	a.Load([]llm.Message{
		userRunes("old ", 100),
		{Role: "assistant", Content: "old answer"},
	}, "")
	a.Submit(userRunes("question ", 100).Content)
	drainEvents(t, events)

	hist := a.History()
	if len(hist) != 2 {
		t.Fatalf("history = %+v, want the engine marker and the model reply", hist)
	}
	if hist[0].Role != "user" || hist[0].Content != contextContinueMessage {
		t.Fatalf("history[0] = %+v, want the engine continue marker", hist[0])
	}
	if hist[1].Role != "assistant" || hist[1].Content != "done" {
		t.Fatalf("history[1] = %+v, want the model reply", hist[1])
	}

	// The model call that follows the pass must have carried that marker, and
	// only it: a request without a user message is what the provider rejected.
	mu.Lock()
	last := bodies[len(bodies)-1]
	mu.Unlock()
	var users []string
	for _, m := range last {
		if m.Role == "user" {
			users = append(users, m.Content)
		}
	}
	if len(users) != 1 || users[0] != contextContinueMessage {
		t.Fatalf("final request user messages = %q, want only the engine continue marker", users)
	}
}

// TestCompactionRequestReusesLiveSystemPrompt pins the layout of the summarizing
// call: it must carry the very system prompt the live conversation sends — the
// sections appended below the base prompt (runtime line, working directory,
// unlock rule, MCP info) included — followed by the messages being compressed
// and the summarize instruction. Dropping those sections would leave the summary
// without the environment its messages came from, and would invalidate the
// provider's cached prompt prefix on every compaction.
func TestCompactionRequestReusesLiveSystemPrompt(t *testing.T) {
	// The request is mirrored so the summarizing call can be compared with the
	// ordinary ones field by field.
	type capturedRequest struct {
		Messages []capturedMessage `json:"messages"`
		Tools    json.RawMessage   `json:"tools"`
	}
	var (
		mu     sync.Mutex
		bodies []capturedRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req capturedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, req)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	cfg.Agent.SystemPrompt = "custom base"
	// A tiny window with a 1% trigger compresses on the first iteration, and the
	// retention budget leaves no room for the newest turn, so the whole tail is
	// summarized.
	cfg.Context.ContextWindow = 100
	cfg.Context.SummarizeTokenPercent = 1
	reg := tools.NewRegistry()
	reg.Register(agentStubTool{name: "exec_command", desc: "run a command"})
	reg.RegisterDeferred(agentStubTool{name: "mcp_github_create_issue", desc: "Create a GitHub issue"})
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	a := New(cfg, llm.NewClient(cfg.OpenAI), reg, bus)
	a.SetMCPServers([]MCPServerInfo{{
		Server:        "playwright",
		ToolCount:     26,
		ServerName:    "Playwright",
		ServerVersion: "1.64.0",
	}})

	a.Load([]llm.Message{
		userRunes("old ", 100),
		{Role: "assistant", Content: "old answer"},
	}, "")
	a.Submit(userRunes("question ", 100).Content)
	drainEvents(t, events)

	mu.Lock()
	captured := append([]capturedRequest(nil), bodies...)
	mu.Unlock()

	var digest, live *capturedRequest
	for i := range captured {
		body := &captured[i]
		if len(body.Messages) == 0 {
			continue
		}
		if last := body.Messages[len(body.Messages)-1]; last.Content == summarizeAppendInstruction {
			digest = body
			continue
		}
		if body.Messages[0].Role == "system" {
			live = body
		}
	}
	if digest == nil {
		t.Fatalf("no summarizing call captured (%d requests)", len(captured))
	}
	if live == nil {
		t.Fatal("no ordinary model call captured")
	}

	// The pass ran with an empty summary and produced "done", so the capability
	// sections alone are what the summarizing call had to send, and the ordinary
	// request after it must start with exactly that plus the new summary.
	a.mu.Lock()
	capabilities := a.systemPrompt()
	a.mu.Unlock()
	for _, want := range []string{
		"custom base",
		RuntimeInfo(),
		"working directory: ",
		"**Tool Discovery & Unlock**",
		"MCP server `playwright` is connected.",
	} {
		if !strings.Contains(capabilities, want) {
			t.Fatalf("the live system prompt lost %q:\n%s", want, capabilities)
		}
	}
	if digest.Messages[0].Role != "system" {
		t.Fatalf("the summarizing call starts with %q, want the system prompt", digest.Messages[0].Role)
	}
	// Byte for byte: the summarizing call carries the live system prompt
	// verbatim, so the model summarizes with the same environment it had while
	// producing those messages, and the provider's cached prefix still applies.
	if got := digest.Messages[0].Content; got != capabilities {
		t.Fatalf("the summarizing call must carry the live system prompt byte for byte:\ngot:\n%q\nwant:\n%q",
			got, capabilities)
	}
	if want := systemWithSummary(capabilities, "done"); live.Messages[0].Content != want {
		t.Fatalf("the live request system prompt = %q, want %q", live.Messages[0].Content, want)
	}
	// The declared tools ride along unchanged too.
	if string(digest.Tools) != string(live.Tools) {
		t.Fatalf("the summarizing call declares different tools:\ndigest: %s\nlive: %s", digest.Tools, live.Tools)
	}
	if !strings.Contains(string(digest.Tools), `"exec_command"`) {
		t.Fatalf("the declared tools were not captured: %s", digest.Tools)
	}

	// The messages being compressed sit between that prefix and the instruction.
	sawBatch := false
	for _, m := range digest.Messages {
		if m.Role == "assistant" && m.Content == "old answer" {
			sawBatch = true
		}
	}
	if !sawBatch {
		t.Fatalf("the summarizing call dropped the batch: %+v", digest.Messages)
	}
}

// TestContextTokensAnchorsToProviderUsage verifies a reported prompt-token
// count anchors the context size while messages appended afterwards are
// estimated on top of it.
func TestContextTokensAnchorsToProviderUsage(t *testing.T) {
	a := newTestAgent(t)
	a.Load([]llm.Message{{Role: "user", Content: "hello"}}, "")
	a.setUsage(llm.Usage{PromptTokens: 5000, CompletionTokens: 10, TotalTokens: 5010})

	if got := a.Stats().EstimatedTok; got != 5000 {
		t.Fatalf("EstimatedTok after the report = %d, want the prompt count 5000", got)
	}

	assistant := llm.Message{Role: "assistant", Content: strings.Repeat("x", 100)}
	a.appendMessage(assistant)
	want := 5000 + EstimateMessageTokens(assistant)
	if got := a.Stats().EstimatedTok; got != want {
		t.Fatalf("EstimatedTok after the append = %d, want %d", got, want)
	}
}

// TestUsageEventsStreamDuringToolLoop pins the live usage reporting: a usage
// event is broadcast while the turn is still running (before the first tool
// result), and the provider-reported prompt tokens become the reported count.
func TestUsageEventsStreamDuringToolLoop(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"stub","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1000,"completion_tokens":10,"total_tokens":1010}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1200,"completion_tokens":5,"total_tokens":1205}}`)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	reg := tools.NewRegistry()
	reg.Register(agentStubTool{name: "stub", desc: "stub"})
	a := New(cfg, llm.NewClient(cfg.OpenAI), reg, bus)

	a.Submit("go")

	var usageCount int
	midTurnUsage := false
	deadline := time.After(10 * time.Second)
drain:
	for {
		select {
		case ev := <-events:
			switch {
			case ev.Type == EventUsage:
				usageCount++
				if ev.Tokens >= 1000 && !midTurnUsage {
					midTurnUsage = true
				}
			case ev.Type == EventToolResult:
				if usageCount == 0 {
					t.Fatal("no usage event before the first tool result")
				}
			case ev.Type == EventTurnDone:
				break drain
			}
		case <-deadline:
			t.Fatal("the turn did not finish")
		}
	}
	if !midTurnUsage {
		t.Fatalf("no usage event carrying the provider count before tool results (seen %d usage events)", usageCount)
	}
	if usageCount < 3 {
		t.Fatalf("usage events = %d, want one per context growth (turn start, model reply, tool round)", usageCount)
	}
}

// TestSystemPromptUnlockRule verifies the global unlock rule is injected
// exactly once and only while locked (deferred) functions exist. Built-in core
// tools never trigger it, and locked function names are never enumerated.
func TestSystemPromptUnlockRule(t *testing.T) {
	// Core tools only: no rule.
	core := tools.NewRegistry()
	core.Register(agentStubTool{name: "exec_command", desc: "run a command"})
	plainAgent := New(config.Default(), nil, core, NewBus())
	plainAgent.mu.Lock()
	plain := plainAgent.buildMessagesLocked()[0].Content
	plainAgent.mu.Unlock()
	if strings.Contains(plain, "Tool Discovery & Unlock") {
		t.Fatalf("core tools alone must not add the unlock rule:\n%s", plain)
	}

	// With a locked function the rule appears exactly once.
	reg := tools.NewRegistry()
	reg.Register(agentStubTool{name: "exec_command", desc: "run a command"})
	reg.RegisterDeferred(agentStubTool{name: "mcp_github_create_issue", desc: "Create a GitHub issue"})
	a := New(config.Default(), nil, reg, NewBus())

	a.mu.Lock()
	content := a.buildMessagesLocked()[0].Content
	a.mu.Unlock()

	if n := strings.Count(content, "**Tool Discovery & Unlock**"); n != 1 {
		t.Fatalf("unlock rule must be injected exactly once, got %d:\n%s", n, content)
	}
	for _, name := range []string{tools.BM25SearchToolName, tools.UnlockToolName, tools.DynamicCallToolName} {
		if !strings.Contains(content, `"`+name+`"`) {
			t.Fatalf("unlock rule should name %q:\n%s", name, content)
		}
	}
	if strings.Contains(content, "mcp_github_create_issue") {
		t.Fatal("locked function names must not be enumerated in the system prompt")
	}
}

// TestSystemPromptMCPGlobalInfo verifies the per-server MCP global info line:
// configured name + tool count + availability, plus the information the server
// reported in its initialize result.
func TestSystemPromptMCPGlobalInfo(t *testing.T) {
	reg := tools.NewRegistry()
	reg.RegisterDeferred(agentStubTool{name: "mcp_github_create_issue", desc: "Create a GitHub issue"})
	a := New(config.Default(), nil, reg, NewBus())
	a.SetMCPServers([]MCPServerInfo{{
		Server:        "github",
		ToolCount:     3,
		ServerName:    "GitHub MCP Server",
		ServerTitle:   "GitHub",
		ServerVersion: "1.4.0",
		Instructions:  "Prefer the search tool before creating anything.",
	}})

	a.mu.Lock()
	content := a.buildMessagesLocked()[0].Content
	a.mu.Unlock()

	for _, want := range []string{
		"MCP server `github` is connected.",
		"It contributes 3 tool(s), currently registered as locked tools;",
		`Reported by: name "GitHub MCP Server", title "GitHub", version 1.4.0.`,
		"Server instructions: Prefer the search tool before creating anything.",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, content)
		}
	}
	if !strings.Contains(content, tools.UnlockToolName) || !strings.Contains(content, tools.DynamicCallToolName) {
		t.Fatalf("MCP info should name the control-plane tools:\n%s", content)
	}
	// The rule is still injected exactly once alongside the MCP info.
	if n := strings.Count(content, "**Tool Discovery & Unlock**"); n != 1 {
		t.Fatalf("unlock rule count = %d, want 1", n)
	}
}

// TestSystemPromptWorkingDirListing verifies the current-directory section is
// injected by default and suppressed when agent.include_working_dir is off.
func TestSystemPromptWorkingDirListing(t *testing.T) {
	on := New(config.Default(), nil, tools.NewRegistry(), NewBus())
	on.mu.Lock()
	enabled := on.buildMessagesLocked()[0].Content
	on.mu.Unlock()
	if !strings.Contains(enabled, "working directory: ") {
		t.Fatalf("working-directory listing missing by default:\n%s", enabled)
	}

	cfg := config.Default()
	cfg.Agent.IncludeWorkingDir = false
	off := New(cfg, nil, tools.NewRegistry(), NewBus())
	off.mu.Lock()
	disabled := off.buildMessagesLocked()[0].Content
	off.mu.Unlock()
	if strings.Contains(disabled, "working directory: ") {
		t.Fatalf("working-directory listing present while disabled:\n%s", disabled)
	}
}

// TestSystemPromptRuntimeLine verifies the runtime line is appended at run time
// right after the base prompt — also when the base prompt comes from agent.md —
// so the environment is never baked into the prompt file.
func TestSystemPromptRuntimeLine(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.SystemPrompt = "custom base"
	a := New(cfg, nil, tools.NewRegistry(), NewBus())

	a.mu.Lock()
	content := a.buildMessagesLocked()[0].Content
	a.mu.Unlock()

	want := "custom base\n\n" + RuntimeInfo()
	if !strings.HasPrefix(content, want) {
		t.Fatalf("system prompt should start with %q:\n%s", want, content)
	}
	if n := strings.Count(content, "Runtime: "); n != 1 {
		t.Fatalf("runtime line count = %d, want 1:\n%s", n, content)
	}
}

// drainEvents collects events until the turn ends.
func drainEvents(t *testing.T, events <-chan Event) []Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	var got []Event
	for {
		select {
		case ev := <-events:
			got = append(got, ev)
			if ev.Type == EventTurnDone {
				return got
			}
		case <-deadline:
			t.Fatal("the turn did not finish")
			return got
		}
	}
}

// hasEvent reports whether drained events contain one of the given type whose
// text includes substr.
func hasEvent(events []Event, typ EventType, substr string) bool {
	for _, ev := range events {
		if ev.Type == typ && strings.Contains(ev.Text, substr) {
			return true
		}
	}
	return false
}

// capturedMessage mirrors a wire message so tests can assert on the exact field
// names the provider sees (notably reasoning_content).
type capturedMessage struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
}

// TestTruncatedTurnContinuesWithoutUserMessage covers a response the provider
// cut off at max_tokens (finish_reason "length") while the model was thinking:
// the assistant message — reasoning included, under the DeepSeek-compatible
// reasoning_content field — stays in the history and is resent to the model on
// the next call, without a new user message.
func TestTruncatedTurnContinuesWithoutUserMessage(t *testing.T) {
	var (
		mu     sync.Mutex
		calls  int
		bodies [][]capturedMessage
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []capturedMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		calls++
		n := calls
		bodies = append(bodies, req.Messages)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if n <= 2 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"think\",\"content\":\"part\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"done\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	a := New(cfg, llm.NewClient(cfg.OpenAI), tools.NewRegistry(), bus)

	a.Submit("hi")
	got := drainEvents(t, events)

	if calls != 3 {
		t.Fatalf("model calls = %d, want 3", calls)
	}
	hist := a.History()
	if len(hist) != 4 {
		t.Fatalf("history = %d messages, want user + 3 assistant", len(hist))
	}
	if hist[0].Role != "user" {
		t.Fatalf("history[0] = %q, want user", hist[0].Role)
	}
	for i := 1; i < len(hist); i++ {
		if hist[i].Role != "assistant" {
			t.Fatalf("history[%d] = %q, want assistant (no user message inserted)", i, hist[i].Role)
		}
	}
	if hist[1].ReasoningContent != "think" {
		t.Fatalf("reasoning was not preserved: %q", hist[1].ReasoningContent)
	}
	if hist[3].Content != "done" {
		t.Fatalf("final content = %q, want done", hist[3].Content)
	}
	if !hasEvent(got, EventInfo, "continuing") {
		t.Fatal("no continuation info was published")
	}

	// The continuation request must replay the earlier assistant messages with
	// their thinking under the DeepSeek-compatible reasoning_content field, and
	// must not have added a user message.
	mu.Lock()
	last := bodies[len(bodies)-1]
	mu.Unlock()
	var reasoningBlocks, users int
	for _, m := range last {
		switch m.Role {
		case "user":
			users++
		case "assistant":
			if m.ReasoningContent == "think" {
				reasoningBlocks++
			}
		}
	}
	if reasoningBlocks != 2 {
		t.Fatalf("continuation request carried %d reasoning_content blocks, want 2", reasoningBlocks)
	}
	if users != 1 {
		t.Fatalf("continuation request carried %d user messages, want 1", users)
	}
}

// TestTurnStopsAfterConsecutiveTruncations covers a runaway thinking loop: after
// three consecutive truncations the turn stops with an error instead of
// continuing forever.
func TestTurnStopsAfterConsecutiveTruncations(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"},\"finish_reason\":\"length\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	a := New(cfg, llm.NewClient(cfg.OpenAI), tools.NewRegistry(), bus)

	a.Submit("hi")
	got := drainEvents(t, events)

	if calls != maxConsecutiveTruncations {
		t.Fatalf("model calls = %d, want %d", calls, maxConsecutiveTruncations)
	}
	if !hasEvent(got, EventError, "truncations") {
		t.Fatal("no stop error was published")
	}
	if hist := a.History(); len(hist) != maxConsecutiveTruncations+1 {
		t.Fatalf("history = %d messages, want user + %d assistant", len(hist), maxConsecutiveTruncations)
	}
}
