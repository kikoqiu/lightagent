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

// TestDrainSteering verifies that queued steering messages become user turns and
// are announced (with the client they came from) at that very moment: the row is
// published when the message enters the conversation, not when it was queued.
func TestDrainSteering(t *testing.T) {
	a := newTestAgent(t)
	events, cancel := a.Bus().Subscribe()
	defer cancel()
	a.steerCh <- steerMessage{source: "web", text: "one"}
	a.steerCh <- steerMessage{source: "cli", text: "two"}

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
	for _, want := range []steerMessage{{source: "web", text: "one"}, {source: "cli", text: "two"}} {
		select {
		case ev := <-events:
			if ev.Type != EventUser || ev.Text != want.text || ev.Source != want.source {
				t.Fatalf("event = %+v, want user %q from %s", ev, want.text, want.source)
			}
		default:
			t.Fatalf("no user event was announced for %q", want.text)
		}
	}
}

// TestSteeringDuringTheToolRoundLandsAfterTheRound covers the tool loop: a message
// that arrives while the round's tool is still running cannot be wedged between the
// assistant message and its tool feedback (a provider requires the tool messages to
// follow the tool_calls message directly), so it joins the conversation after the
// whole round — exactly where the model sees it — and the row is drawn there.
func TestSteeringDuringTheToolRoundLandsAfterTheRound(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"calling the tool","tool_calls":[{"id":"c1","type":"function","function":{"name":"block","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"after steering"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	reg := tools.NewRegistry()
	tool := &blockingTool{started: make(chan struct{}), release: make(chan struct{})}
	reg.Register(tool)
	a := New(cfg, llm.NewClient(cfg.OpenAI), reg, bus)

	a.Submit("ask")
	select {
	case <-tool.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the tool never ran")
	}
	// The round is still in flight: the message waits for its end.
	a.Submit("steer")
	close(tool.release)

	got := drainEvents(t, events)
	want := "user:ask, assistant:calling the tool, tool_call:block, tool_result:block, user:steer, assistant:after steering"
	if order := strings.Join(eventOrder(got), ", "); order != want {
		t.Fatalf("event order = %q, want %q", order, want)
	}
	if hist := a.History(); len(hist) != 5 {
		t.Fatalf("history = %d messages, want 5", len(hist))
	}
}

// TestSteeringDuringFinalReplyContinuesTheTurn covers a steering message that
// arrives while the last reply is still streaming: the reply is not the end of the
// turn then, so the message must reach the model in this very turn instead of
// sitting in the queue until the user types again. It is announced once (the
// front-ends already drew its row when it was queued).
func TestSteeringDuringFinalReplyContinuesTheTurn(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first part\"}}]}\n\n")
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			once.Do(func() { close(started) })
			// Hold the reply open until the steering message has been queued.
			<-release
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"after steering\"}}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	a := New(cfg, llm.NewClient(cfg.OpenAI), tools.NewRegistry(), bus)

	a.Submit("ask")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first model call never started")
	}
	// The reply is streaming: this joins the running turn as steering.
	a.Submit("steer")
	close(release)

	got := drainEvents(t, events)

	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 2 {
		t.Fatalf("model calls = %d, want 2: the steering message must be answered in this turn", n)
	}
	// The rows must follow the conversation: the steering message is announced
	// when the turn folds it in, i.e. after the reply it interrupted.
	want := "user:ask, assistant:first part, user:steer, assistant:after steering"
	if order := strings.Join(eventOrder(got), ", "); order != want {
		t.Fatalf("event order = %q, want %q", order, want)
	}
	hist := a.History()
	roles := make([]string, 0, len(hist))
	for _, m := range hist {
		roles = append(roles, m.Role+":"+m.Content)
	}
	if got := strings.Join(roles, ", "); got != want {
		t.Fatalf("history = %q, want %q", got, want)
	}
}

// TestQueuedSteeringStartsAFollowUpTurn covers the other end of the turn: a
// steering message that is still queued when the loop runs out (here the only
// iteration is spent on a tool round) is not stranded. It is announced as a
// silent user event — its row was drawn when it was queued — and answered by a
// turn of its own.
func TestQueuedSteeringStartsAFollowUpTurn(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			once.Do(func() { close(started) })
			// Hold the response until the steering message has been queued.
			<-release
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"stub","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		default:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"after steering"},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	// One iteration: it is spent on the tool round, so the queued steering
	// message has no iteration left and the follow-up turn has to run it.
	cfg.Agent.MaxToolIterations = 1
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	reg := tools.NewRegistry()
	reg.Register(agentStubTool{name: "stub", desc: "stub"})
	a := New(cfg, llm.NewClient(cfg.OpenAI), reg, bus)

	a.Submit("ask")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first model call never started")
	}
	a.Submit("steer")
	close(release)

	got := drainTurns(t, events, 2)

	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 2 {
		t.Fatalf("model calls = %d, want the follow-up turn to ask the model again", n)
	}
	// The row is announced when the follow-up turn folds the message in, so it
	// lands after everything the previous turn produced.
	wantOrder := "user:ask, tool_call:stub, tool_result:stub, user:steer, assistant:after steering"
	if order := strings.Join(eventOrder(got), ", "); order != wantOrder {
		t.Fatalf("event order = %q, want %q", order, wantOrder)
	}
	hist := a.History()
	roles := make([]string, 0, len(hist))
	for _, m := range hist {
		roles = append(roles, m.Role+":"+m.Content)
	}
	want := "user:ask, assistant:, tool:ok, user:steer, assistant:after steering"
	if got := strings.Join(roles, ", "); got != want {
		t.Fatalf("history = %q, want %q", got, want)
	}
}

// eventOrder renders the events a front-end draws a row for as a compact
// "kind:label" list, so a test can pin the order the transcript is drawn in.
func eventOrder(events []Event) []string {
	var out []string
	for _, ev := range events {
		switch ev.Type {
		case EventUser, EventAssistant:
			out = append(out, string(ev.Type)+":"+ev.Text)
		case EventToolCall, EventToolResult:
			out = append(out, string(ev.Type)+":"+ev.Name)
		}
	}
	return out
}

// TestBuildMessagesSummaryPlacement pins where a request carries the accumulated
// summary: by default as the first user message, and with
// agent.summary_in_system_prompt appended to the system prompt instead.
func TestBuildMessagesSummaryPlacement(t *testing.T) {
	hist := []llm.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yo"}}

	a := newTestAgent(t)
	a.Load(hist, "the summary")

	a.mu.Lock()
	msgs := a.buildMessagesLocked()
	a.mu.Unlock()

	if len(msgs) != 4 {
		t.Fatalf("len = %d, want 4 (system + summary + history)", len(msgs))
	}
	if msgs[0].Role != "system" || strings.Contains(msgs[0].Content, "the summary") {
		t.Fatalf("system prompt = %q, want the capability sections only", msgs[0].Content)
	}
	if want := summaryUserPrefix + "the summary"; msgs[1].Role != "user" || msgs[1].Content != want {
		t.Fatalf("summary message = %q, want %q", msgs[1].Content, want)
	}
	// The history rows follow the summary in their recorded order.
	if msgs[2].Role != "user" || msgs[2].Content != "hi" {
		t.Fatalf("first history message = %+v", msgs[2])
	}
	if msgs[3].Role != "assistant" || msgs[3].Content != "yo" {
		t.Fatalf("second history message = %+v", msgs[3])
	}
	if got := a.History(); len(got) != 2 || got[0].Content != "hi" {
		t.Fatalf("stored history = %+v, want the recorded rows", got)
	}

	// agent.summary_in_system_prompt: the summary rides in the system prompt and
	// no extra message is sent.
	cfg := config.Default()
	cfg.Agent.SummaryInSystemPrompt = true
	b := New(cfg, nil, tools.NewRegistry(), NewBus())
	b.Load(hist, "the summary")

	b.mu.Lock()
	msgs = b.buildMessagesLocked()
	b.mu.Unlock()

	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3 (system + history)", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "# CONVERSATION SUMMARY") ||
		!strings.Contains(msgs[0].Content, "the summary") {
		t.Fatalf("system prompt missing summary: %q", msgs[0].Content)
	}
	if msgs[1].Content != "hi" {
		t.Fatalf("first history message = %q, want the recorded row", msgs[1].Content)
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

	// The request that follows the pass must still carry the engine continue
	// marker: a request without a user message is what the provider rejected.
	// The summary of the pass is the first user message, the marker follows at
	// the end.
	mu.Lock()
	last := bodies[len(bodies)-1]
	mu.Unlock()
	var users []string
	for _, m := range last {
		if m.Role == "user" {
			users = append(users, m.Content)
		}
	}
	want := []string{summaryUserPrefix + "done", contextContinueMessage}
	if len(users) != len(want) {
		t.Fatalf("final request user messages = %q, want %q", users, want)
	}
	for i := range want {
		if users[i] != want[i] {
			t.Fatalf("final request user messages = %q, want %q", users, want)
		}
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
		if last := body.Messages[len(body.Messages)-1]; strings.HasPrefix(last.Content, summarizeInstructionIntro) {
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
	// The summary lands where the configuration asks for it: as the first user
	// message of the request that follows the pass, with the capability sections
	// carried byte for byte in front of it.
	if got := live.Messages[0].Content; got != capabilities {
		t.Fatalf("the live request must carry the capability sections byte for byte:\ngot:\n%q\nwant:\n%q",
			got, capabilities)
	}
	if want := summaryUserPrefix + "done"; live.Messages[1].Role != "user" || live.Messages[1].Content != want {
		t.Fatalf("live summary message = %+v, want %q", live.Messages[1], want)
	}
	if want := contextContinueMessage; live.Messages[2].Content != want {
		t.Fatalf("live first history message = %q, want %q", live.Messages[2].Content, want)
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

// TestCompactionDigestCarriesSummaryWhereTheLiveCallDoes verifies the
// summarizing call mirrors the placement of the accumulated summary, which is
// what keeps its prefix identical to the live requests: by default the summary is
// the first user message, in front of the batch, and with
// agent.summary_in_system_prompt it rides in the system prompt instead.
func TestCompactionDigestCarriesSummaryWhereTheLiveCallDoes(t *testing.T) {
	for _, inSystem := range []bool{false, true} {
		name := "summary message"
		if inSystem {
			name = "system prompt"
		}
		t.Run(name, func(t *testing.T) {
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
			cfg.Agent.SummaryInSystemPrompt = inSystem
			// A tiny window with a 1% trigger compresses on the first iteration,
			// and a retention budget of a few tokens leaves no room for the newest
			// turn, so the whole tail — the batch being summarized included — is
			// compressed.
			cfg.Context.ContextWindow = 100
			cfg.Context.SummarizeTokenPercent = 1
			bus := NewBus()
			events, cancel := bus.Subscribe()
			defer cancel()
			a := New(cfg, llm.NewClient(cfg.OpenAI), tools.NewRegistry(), bus)

			a.Load([]llm.Message{
				userRunes("old ", 100),
				{Role: "assistant", Content: "old answer"},
			}, "carried over")
			a.Submit(userRunes("question ", 100).Content)
			drainEvents(t, events)

			mu.Lock()
			captured := append([][]capturedMessage(nil), bodies...)
			mu.Unlock()

			var digest []capturedMessage
			for _, msgs := range captured {
				if len(msgs) > 0 && strings.HasPrefix(msgs[len(msgs)-1].Content, summarizeInstructionIntro) {
					digest = msgs
				}
			}
			if digest == nil {
				t.Fatalf("no summarizing call captured (%d requests)", len(captured))
			}
			if digest[0].Role != "system" || digest[1].Role != "user" {
				t.Fatalf("the summarizing call does not start with system + user: %+v", digest[:2])
			}
			if inSystem {
				if !strings.Contains(digest[0].Content, "# CONVERSATION SUMMARY") ||
					!strings.Contains(digest[0].Content, "carried over") {
					t.Fatalf("the summarizing system prompt lost the summary: %q", digest[0].Content)
				}
				if strings.Contains(digest[1].Content, summaryUserPrefix) {
					t.Fatalf("the summary must not be duplicated in a message of its own: %q", digest[1].Content)
				}
				return
			}
			if strings.Contains(digest[0].Content, "carried over") {
				t.Fatalf("the summarizing system prompt must not carry the summary: %q", digest[0].Content)
			}
			if want := summaryUserPrefix + "carried over"; digest[1].Content != want {
				t.Fatalf("the summarizing summary message = %q, want %q", digest[1].Content, want)
			}
			// The batch follows the summary as it was recorded.
			if want := userRunes("old ", 100).Content; digest[2].Role != "user" || digest[2].Content != want {
				t.Fatalf("the summarizing batch starts with %+v, want the user turn %q", digest[2], want)
			}
		})
	}
}

// TestCompactionReplacesTheSummary verifies a pass stores the report the model
// wrote as the new accumulated summary: the summarizing call carried the previous
// summary and asked for the complete text, so the field ends up holding that full
// update.
func TestCompactionReplacesTheSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"rewritten report"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	// A tiny window with a 1% trigger compresses on the first pass, and the
	// retention budget leaves no room for the newest turn, so the whole history
	// is summarized.
	cfg.Context.ContextWindow = 100
	cfg.Context.SummarizeTokenPercent = 1
	a := New(cfg, llm.NewClient(cfg.OpenAI), tools.NewRegistry(), NewBus())

	a.Load([]llm.Message{
		userRunes("old ", 100),
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "more"},
		{Role: "assistant", Content: "more answer"},
	}, "carried over")

	if msg := a.CompactNow(context.Background()); msg != "" {
		t.Fatalf("CompactNow = %q, want no extra note", msg)
	}
	if got := a.Summary(); got != "rewritten report" {
		t.Fatalf("summary = %q, want the report the model returned", got)
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

// drainTurns collects events until n turns have ended (each turn ends with its
// own turn_done).
func drainTurns(t *testing.T, events <-chan Event, n int) []Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	var got []Event
	done := 0
	for {
		select {
		case ev := <-events:
			got = append(got, ev)
			if ev.Type == EventTurnDone {
				done++
				if done >= n {
					return got
				}
			}
		case <-deadline:
			t.Fatalf("only %d of %d turns finished", done, n)
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

// TestIncompleteStreamedToolCallEndsTheTurnWithAnError covers the provider
// defect where the stream closes with finish_reason "stop" after only half of a
// tool call was sent: the turn must fail loudly (no tool is run, no reply is
// recorded and no half tool_calls are replayed to the provider), not end as if
// the model had finished.
func TestIncompleteStreamedToolCallEndsTheTurnWithAnError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\\\"path\\\":\\\"a.txt\\\",\\\"content\\\":\\\"half\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	a := New(cfg, llm.NewClient(cfg.OpenAI), tools.NewRegistry(), bus)

	a.Submit("write it")
	got := drainEvents(t, events)

	if calls != 1 {
		t.Fatalf("model calls = %d, want 1 (the turn must stop, not retry)", calls)
	}
	if !hasEvent(got, EventError, "not valid JSON") {
		t.Fatalf("no error naming the incomplete tool call was published: %+v", got)
	}
	if hasEvent(got, EventToolCall, "") {
		t.Fatal("the half-delivered tool call was dispatched")
	}
	if hist := a.History(); len(hist) != 1 || hist[0].Role != "user" {
		t.Fatalf("history = %+v, want only the user message", hist)
	}
	if a.Busy() {
		t.Fatal("the agent is still busy after the failure")
	}
}
