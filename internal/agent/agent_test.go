package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lightagent/internal/config"
	"lightagent/internal/llm"
	"lightagent/internal/tools"
)

// blockingTool blocks inside Execute until the turn is cancelled (or the test
// releases it), so an interrupt lands during a tool call. It is not interruptible
// mid-flight: whatever the turn context says, it answers with its normal result.
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
	return &tools.Result{ForLLM: "finished", ForUser: "finished"}
}

// cancelAwareTool stops as soon as the turn is cancelled and answers with what it
// has, the way exec_command terminates its process (reporting the output it had
// produced) and manage_session poll stops waiting.
type cancelAwareTool struct {
	started chan struct{}
	once    sync.Once
	text    string
}

func (t *cancelAwareTool) Name() string               { return "cancelaware" }
func (t *cancelAwareTool) Description() string        { return "stops when the turn is cancelled" }
func (t *cancelAwareTool) Parameters() map[string]any { return map[string]any{"type": "object"} }

func (t *cancelAwareTool) Execute(ctx context.Context, _ map[string]any) *tools.Result {
	t.once.Do(func() { close(t.started) })
	<-ctx.Done()
	return &tools.Result{ForLLM: t.text, ForUser: t.text}
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

// TestInterruptKeepsTheUserMessage covers interrupting while the model call is in
// flight, i.e. before anything was produced: the assistant side leaves no record
// at all, while the user message stays in the history — an interrupt never
// rewrites what the user sent — and an interrupted marker is published before the
// turn ends.
func TestInterruptKeepsTheUserMessage(t *testing.T) {
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
	msgs := a.History()
	if len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content != "do something" {
		t.Fatalf("history = %+v, want the user message kept", msgs)
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
	if msgs := a.History(); len(msgs) != 2 || msgs[1].Content != "again" {
		t.Fatalf("history = %+v, want both user messages kept", msgs)
	}
}

// TestInterruptDuringAToolRound covers interrupting a round with several tool
// calls. A tool call that cannot be cut short is allowed to finish, so the call
// that was running reports its real result; the calls that never started are
// answered as interrupted, which keeps the assistant message's tool_calls paired.
// The answers only enter the history — the model is never asked with them.
func TestInterruptDuringAToolRound(t *testing.T) {
	var (
		mu       sync.Mutex
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[`+
			`{"id":"c1","type":"function","function":{"name":"block","arguments":"{}"}},`+
			`{"id":"c2","type":"function","function":{"name":"block","arguments":"{}"}}]},`+
			`"finish_reason":"tool_calls"}]}`)
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

	var sawRealResult, sawInterrupted, sawMarker bool
	deadline := time.After(10 * time.Second)
drain:
	for {
		select {
		case ev := <-events:
			switch {
			case ev.Type == EventToolResult && ev.Text == "finished" && !ev.IsError:
				sawRealResult = true
			case ev.Type == EventToolResult && strings.Contains(ev.Text, "interrupted"):
				sawInterrupted = true
			case ev.Type == EventInterrupted:
				sawMarker = true
			case ev.Type == EventTurnDone:
				break drain
			}
		case <-deadline:
			t.Fatal("no turn_done after the interrupt")
		}
	}
	if !sawRealResult {
		t.Fatal("the running tool call's real result was not reported")
	}
	if !sawInterrupted {
		t.Fatal("the call that never started was not answered as interrupted")
	}
	if !sawMarker {
		t.Fatal("no interrupted event was published")
	}
	mu.Lock()
	asked := requests
	mu.Unlock()
	if asked != 1 {
		t.Fatalf("the model was asked %d time(s), want 1: the interrupted round is not sent back to it", asked)
	}

	msgs := a.History()
	if len(msgs) != 4 {
		t.Fatalf("history = %d messages, want user + assistant + 2 tool answers", len(msgs))
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "c1" || msgs[2].Content != "finished" {
		t.Fatalf("the running call's record = %+v, want its real result", msgs[2])
	}
	if msgs[3].Role != "tool" || msgs[3].ToolCallID != "c2" || !strings.Contains(msgs[3].Content, "interrupted") {
		t.Fatalf("the pending call's record = %+v, want an interrupted answer", msgs[3])
	}
}

// TestInterruptStopsACancelAwareTool covers the other half of a tool round: a tool
// that reacts to the cancellation — exec_command terminates its process and
// reports what it had printed, manage_session poll stops waiting — answers with
// what it has, and that answer is what its call records. The call after it, which
// never started, is still answered as interrupted, and the answers are not sent
// back to the model.
func TestInterruptStopsACancelAwareTool(t *testing.T) {
	var (
		mu       sync.Mutex
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[`+
			`{"id":"c1","type":"function","function":{"name":"cancelaware","arguments":"{}"}},`+
			`{"id":"c2","type":"function","function":{"name":"cancelaware","arguments":"{}"}}]},`+
			`"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	tool := &cancelAwareTool{started: make(chan struct{}), text: "half the work"}
	reg := tools.NewRegistry()
	reg.Register(tool)
	a := New(cfg, llm.NewClient(cfg.OpenAI), reg, bus)

	a.Submit("run it")
	select {
	case <-tool.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the tool never started")
	}
	if !a.Interrupt() {
		t.Fatal("Interrupt reported no running turn")
	}
	if !waitForTurnEnd(t, events) {
		t.Fatal("no interrupted event was published")
	}

	msgs := a.History()
	if len(msgs) != 4 {
		t.Fatalf("history = %d messages, want user + assistant + 2 tool answers", len(msgs))
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "c1" || msgs[2].Content != "half the work" {
		t.Fatalf("the running call's record = %+v, want what the tool reported", msgs[2])
	}
	if msgs[3].Role != "tool" || msgs[3].ToolCallID != "c2" || !strings.Contains(msgs[3].Content, "interrupted") {
		t.Fatalf("the call that never started = %+v, want an interrupted answer", msgs[3])
	}
	mu.Lock()
	asked := requests
	mu.Unlock()
	if asked != 1 {
		t.Fatalf("the model was asked %d time(s), want 1: the interrupted round is not sent back to it", asked)
	}
}

// TestInterruptKeepsTheStreamedPartialReply covers interrupting while the reply is
// still streaming: the text the provider had already delivered is kept as the
// assistant message, the tool calls the partial reply carried are dropped — none
// of them started, and a reply cut short can carry fragments — and the turn ends
// there instead of being sent to the model as if it were complete.
func TestInterruptKeepsTheStreamedPartialReply(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("the test server cannot flush")
			return
		}
		// The call arrives between two text chunks, so it is assembled before
		// the test interrupts: seeing the third chunk means the second was read.
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"echo\",\"arguments\":\"{}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n")
		flusher.Flush()
		<-release
	}))
	// Registered in this order so release runs before Close (cleanups are LIFO).
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = true
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	a := New(cfg, llm.NewClient(cfg.OpenAI), tools.NewRegistry(), bus)

	a.Submit("say hello")
	// The last streamed chunk means the frames before it were assembled: the
	// reply holds "Hello" and a tool call by the time the interrupt lands.
	for {
		select {
		case ev := <-events:
			if ev.Type == EventAssistantDelta && ev.Text == "lo" {
				goto interrupt
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the reply never streamed its last chunk")
		}
	}

interrupt:
	if !a.Interrupt() {
		t.Fatal("Interrupt reported no running turn")
	}

	var sawPartial, sawDropped, sawToolCall, sawMarker bool
	deadline := time.After(10 * time.Second)
drain:
	for {
		select {
		case ev := <-events:
			switch {
			case ev.Type == EventAssistant && ev.Text == "Hello":
				sawPartial = true
			case ev.Type == EventInfo && strings.Contains(ev.Text, "dropped"):
				sawDropped = true
			case ev.Type == EventToolCall:
				sawToolCall = true
			case ev.Type == EventInterrupted:
				sawMarker = true
			case ev.Type == EventTurnDone:
				break drain
			}
		case <-deadline:
			t.Fatal("no turn_done after the interrupt")
		}
	}
	if !sawPartial {
		t.Fatal("the kept partial reply was not published")
	}
	if !sawDropped {
		t.Fatal("the dropped tool calls were not reported")
	}
	if sawToolCall {
		t.Fatal("a tool call of the partial reply was announced")
	}
	if !sawMarker {
		t.Fatal("no interrupted event was published")
	}

	msgs := a.History()
	if len(msgs) != 2 {
		t.Fatalf("history = %d messages, want user + the partial reply", len(msgs))
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != "Hello" {
		t.Fatalf("partial reply = %+v, want the streamed text kept", msgs[1])
	}
	if len(msgs[1].ToolCalls) != 0 {
		t.Fatalf("the partial reply kept %d tool call(s), want none", len(msgs[1].ToolCalls))
	}
}

// TestInterruptDropsATextlessReply covers a reply that was cut short before it
// produced any text: only thinking and a tool call had arrived. Nothing of it
// enters the history — an assistant message without content has nothing to tell
// the next request — while the user message stays, and the turn ends there.
func TestInterruptDropsATextlessReply(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("the test server cannot flush")
			return
		}
		// The call arrives before the last thinking chunk, so the test knows it
		// was assembled by the time it interrupts.
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"thinking\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"echo\",\"arguments\":\"{}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\" more\"}}]}\n\n")
		flusher.Flush()
		<-release
	}))
	// Registered in this order so release runs before Close (cleanups are LIFO).
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = true
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	a := New(cfg, llm.NewClient(cfg.OpenAI), tools.NewRegistry(), bus)

	a.Submit("think about it")
	for {
		select {
		case ev := <-events:
			if ev.Type == EventReasoningDelta && ev.Text == " more" {
				goto interrupt
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the reply never streamed its last chunk")
		}
	}

interrupt:
	if !a.Interrupt() {
		t.Fatal("Interrupt reported no running turn")
	}

	var sawAssistant, sawToolCall, sawMarker bool
	var markerText string
	deadline := time.After(10 * time.Second)
drain:
	for {
		select {
		case ev := <-events:
			switch {
			case ev.Type == EventAssistant:
				sawAssistant = true
			case ev.Type == EventToolCall:
				sawToolCall = true
			case ev.Type == EventInterrupted:
				sawMarker = true
				markerText = ev.Text
			case ev.Type == EventTurnDone:
				break drain
			}
		case <-deadline:
			t.Fatal("no turn_done after the interrupt")
		}
	}
	if sawAssistant {
		t.Fatal("a reply without text was finalized as an assistant row")
	}
	if sawToolCall {
		t.Fatal("a tool call of the dropped reply was announced")
	}
	if !sawMarker || !strings.Contains(markerText, "dropped") {
		t.Fatalf("marker = %q (seen=%v), want a note that the message was dropped", markerText, sawMarker)
	}

	msgs := a.History()
	if len(msgs) != 1 {
		t.Fatalf("history = %d messages, want only the user message", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "think about it" {
		t.Fatalf("history[0] = %+v, want the user message kept", msgs[0])
	}
}

// TestInterruptBeforeTheFirstCallDropsTheRound covers the rule in the real tool
// loop: the reply is recorded and the turn is cancelled before its first call
// starts. The round is dropped whole — the text stays, the calls do not, and no
// tool answer is recorded, because the message no longer asks for anything.
func TestInterruptBeforeTheFirstCallDropsTheRound(t *testing.T) {
	var (
		mu       sync.Mutex
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"writing","tool_calls":[`+
			`{"id":"c1","type":"function","function":{"name":"write_file","arguments":"{}"}}]},`+
			`"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	writer := &interruptingWriter{}
	reg := tools.NewRegistry()
	reg.Register(writer)
	a := New(cfg, llm.NewClient(cfg.OpenAI), reg, bus)
	// The planning hook runs once the reply is recorded and before the first
	// call starts: cancelling there is exactly the window this test pins.
	writer.interrupt = func() { a.Interrupt() }

	a.Submit("write it")
	if !waitForTurnEnd(t, events) {
		t.Fatal("no interrupted event was published")
	}
	if writer.ran {
		t.Fatal("a call of the dropped round was executed")
	}

	msgs := a.History()
	if len(msgs) != 2 {
		t.Fatalf("history = %d messages, want the user message and the reply", len(msgs))
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != "writing" || len(msgs[1].ToolCalls) != 0 {
		t.Fatalf("reply = %+v, want the text kept without its tool call", msgs[1])
	}
	mu.Lock()
	asked := requests
	mu.Unlock()
	if asked != 1 {
		t.Fatalf("the model was asked %d time(s), want 1", asked)
	}
}

// interruptingWriter stands in for write_file: its PlanWriteCalls hook is called
// between the reply being recorded and the first call starting, so a test can
// cancel the turn exactly there. It never plans a split.
type interruptingWriter struct {
	interrupt func()
	ran       bool
}

func (w *interruptingWriter) Name() string               { return writeFileToolName }
func (w *interruptingWriter) Description() string        { return "cancels the turn before its own call runs" }
func (w *interruptingWriter) Parameters() map[string]any { return map[string]any{"type": "object"} }

func (w *interruptingWriter) PlanWriteCalls(map[string]any) ([]map[string]any, bool) {
	w.interrupt()
	return nil, false
}

func (w *interruptingWriter) Execute(context.Context, map[string]any) *tools.Result {
	w.ran = true
	return tools.OK("wrote")
}

// TestDropUnstartedToolCalls pins what a round interrupted before its first call
// records: the reply keeps its text and loses every tool call, a reply left
// without text goes away entirely, and no tool answer is written for either —
// nothing ran, and no call is left to answer.
func TestDropUnstartedToolCalls(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantMsgs int
		wantText string
	}{
		{name: "the text is kept", content: "Here is what I found", wantMsgs: 2, wantText: "kept without its 2 tool call(s)"},
		{name: "a reply without text is dropped", content: "  \n", wantMsgs: 1, wantText: "it and its 2 tool call(s) were dropped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAgent(t)
			events, cancel := a.Bus().Subscribe()
			defer cancel()

			a.appendMessage(llm.Message{Role: "user", Content: "ask"})
			a.appendMessage(llm.Message{
				Role:    "assistant",
				Content: tc.content,
				ToolCalls: []llm.ToolCall{
					{ID: "c1", Type: "function", Function: llm.ToolCallFunction{Name: "echo", Arguments: "{}"}},
					{ID: "c2", Type: "function", Function: llm.ToolCallFunction{Name: "echo", Arguments: "{}"}},
				},
			})

			a.finishUnstartedToolRound()

			msgs := a.History()
			if len(msgs) != tc.wantMsgs {
				t.Fatalf("history = %d messages, want %d: %+v", len(msgs), tc.wantMsgs, msgs)
			}
			if msgs[0].Role != "user" {
				t.Fatalf("history[0] = %+v, want the user message to stay", msgs[0])
			}
			if tc.wantMsgs > 1 && (msgs[1].Role != "assistant" || msgs[1].Content != tc.content || len(msgs[1].ToolCalls) != 0) {
				t.Fatalf("history[1] = %+v, want the text kept without tool calls", msgs[1])
			}
			for _, m := range msgs {
				if m.Role == "tool" {
					t.Fatalf("history = %+v, want no tool answer for a round that never started", msgs)
				}
			}

			select {
			case ev := <-events:
				if ev.Type != EventInterrupted || !strings.Contains(ev.Text, tc.wantText) {
					t.Fatalf("event = %+v, want a marker mentioning %q", ev, tc.wantText)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no interrupted marker was published")
			}
		})
	}
}

// TestWriteSplitsAnsweredAsInterrupted covers the write parts of a round
// interrupted after the round's own calls had finished: the parts are ordinary
// calls of that batch, so the ones that never started are answered like any other
// call that never ran — one assistant/tool pair each with an interrupted answer —
// and nothing of theirs is executed.
func TestWriteSplitsAnsweredAsInterrupted(t *testing.T) {
	a := newTestAgent(t)
	events, cancel := a.Bus().Subscribe()
	defer cancel()

	ctx, cancelTurn := context.WithCancel(context.Background())
	cancelTurn()

	calls := []llm.ToolCall{
		{ID: "c_part2", Type: "function", Function: llm.ToolCallFunction{Name: writeFileToolName, Arguments: `{}`}},
		{ID: "c_part3", Type: "function", Function: llm.ToolCallFunction{Name: writeFileToolName, Arguments: `{}`}},
	}
	if a.runWriteSplits(ctx, calls) {
		t.Fatal("runWriteSplits reported a completed turn")
	}
	msgs := a.History()
	if len(msgs) != 4 {
		t.Fatalf("history = %d messages, want 2 assistant/tool pairs: %+v", len(msgs), msgs)
	}
	for i, id := range []string{"c_part2", "c_part3"} {
		call, answer := msgs[i*2], msgs[i*2+1]
		if call.Role != "assistant" || len(call.ToolCalls) != 1 || call.ToolCalls[0].ID != id {
			t.Fatalf("msgs[%d] = %+v, want the assistant message of %s", i*2, call, id)
		}
		if answer.Role != "tool" || answer.ToolCallID != id || !strings.Contains(answer.Content, "interrupted") {
			t.Fatalf("msgs[%d] = %+v, want %s answered as interrupted", i*2+1, answer, id)
		}
	}
	// Both answers were announced (publishing is synchronous, so everything is
	// already queued).
	var announced int
drain:
	for {
		select {
		case ev := <-events:
			if ev.Type == EventToolResult && strings.Contains(ev.Text, "interrupted") {
				announced++
			}
		default:
			break drain
		}
	}
	if announced != 2 {
		t.Fatalf("announced interrupted answers = %d, want 2", announced)
	}
}

// cancelOnRunTool stands in for write_file in the split tests: it cancels the
// context it runs with (the way a user interrupt lands while the parts are
// running) and answers normally.
type cancelOnRunTool struct {
	cancel func()
	ran    int
}

func (t *cancelOnRunTool) Name() string               { return writeFileToolName }
func (t *cancelOnRunTool) Description() string        { return "cancels the turn while it runs" }
func (t *cancelOnRunTool) Parameters() map[string]any { return map[string]any{"type": "object"} }

func (t *cancelOnRunTool) Execute(context.Context, map[string]any) *tools.Result {
	t.ran++
	t.cancel()
	return &tools.Result{ForLLM: "wrote part", ForUser: "wrote part"}
}

// TestWriteSplitsInterruptedWhileRunning covers the parts of an auto-split write
// interrupted while the batch is already running: the part in flight is answered
// with its real result, the parts after it are answered as interrupted, and the
// assistant/tool pairing stays complete for the next request.
func TestWriteSplitsInterruptedWhileRunning(t *testing.T) {
	a := newTestAgent(t)
	events, cancel := a.Bus().Subscribe()
	defer cancel()

	ctx, cancelTurn := context.WithCancel(context.Background())
	defer cancelTurn()
	tool := &cancelOnRunTool{cancel: cancelTurn}
	a.reg.Register(tool)

	calls := []llm.ToolCall{
		{ID: "c_part2", Type: "function", Function: llm.ToolCallFunction{Name: writeFileToolName, Arguments: `{}`}},
		{ID: "c_part3", Type: "function", Function: llm.ToolCallFunction{Name: writeFileToolName, Arguments: `{}`}},
	}
	if a.runWriteSplits(ctx, calls) {
		t.Fatal("runWriteSplits reported a completed turn")
	}
	if tool.ran != 1 {
		t.Fatalf("the writer ran %d time(s), want 1: only the part that was in flight", tool.ran)
	}
	msgs := a.History()
	if len(msgs) != 4 {
		t.Fatalf("history = %d messages, want 2 assistant/tool pairs: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "assistant" || len(msgs[0].ToolCalls) != 1 || msgs[0].ToolCalls[0].ID != "c_part2" {
		t.Fatalf("msgs[0] = %+v, want the assistant message of the part that ran", msgs[0])
	}
	if msgs[1].Role != "tool" || msgs[1].ToolCallID != "c_part2" || msgs[1].Content != "wrote part" {
		t.Fatalf("msgs[1] = %+v, want the real result of the part that ran", msgs[1])
	}
	if msgs[2].Role != "assistant" || len(msgs[2].ToolCalls) != 1 || msgs[2].ToolCalls[0].ID != "c_part3" {
		t.Fatalf("msgs[2] = %+v, want the assistant message of the pending part", msgs[2])
	}
	if msgs[3].Role != "tool" || msgs[3].ToolCallID != "c_part3" || !strings.Contains(msgs[3].Content, "interrupted") {
		t.Fatalf("msgs[3] = %+v, want the pending part answered as interrupted", msgs[3])
	}
	// The pending part's feedback is announced too (publishing is synchronous, so
	// everything is already queued).
	var sawInterrupted bool
drain:
	for {
		select {
		case ev := <-events:
			if ev.Type == EventToolResult && strings.Contains(ev.Text, "interrupted") {
				sawInterrupted = true
			}
		default:
			break drain
		}
	}
	if !sawInterrupted {
		t.Fatal("the pending part's interrupted answer was not announced")
	}
}

// planWriter stands in for write_file when the split flow itself is under test: it
// plans three writes for any call, so the "multiple writes" batch exists without a
// huge payload, and counts the calls it really executed.
type planWriter struct {
	parts []map[string]any
	ran   int
}

func (w *planWriter) Name() string               { return writeFileToolName }
func (w *planWriter) Description() string        { return "plans a fixed split" }
func (w *planWriter) Parameters() map[string]any { return map[string]any{"type": "object"} }

func (w *planWriter) PlanWriteCalls(map[string]any) ([]map[string]any, bool) {
	return w.parts, true
}

func (w *planWriter) Execute(context.Context, map[string]any) *tools.Result {
	w.ran++
	return &tools.Result{ForLLM: "wrote a part", ForUser: "wrote a part"}
}

// TestInterruptDuringARoundWithWriteParts covers the shape the write-split feature
// really produces: the reply is [write_file(...), block] and the oversized write
// becomes several write calls of that same batch. The interrupt lands while the
// second call runs, so that call is answered with its real result and every call
// that never started — the write parts the first call still owed — is answered as
// interrupted, exactly like any other unstarted call of the batch.
func TestInterruptDuringARoundWithWriteParts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[`+
			`{"id":"c1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"p.txt\",\"content\":\"x\"}"}},`+
			`{"id":"c2","type":"function","function":{"name":"block","arguments":"{}"}}]},`+
			`"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	writer := &planWriter{parts: []map[string]any{
		{"path": "p.txt", "mode": "w", "content": "one"},
		{"path": "p.txt", "mode": "a", "content": "two"},
		{"path": "p.txt", "mode": "a", "content": "three"},
	}}
	block := &blockingTool{started: make(chan struct{}), release: make(chan struct{})}
	defer close(block.release)
	reg := tools.NewRegistry()
	reg.Register(writer)
	reg.Register(block)
	a := New(cfg, llm.NewClient(cfg.OpenAI), reg, bus)

	a.Submit("write it")
	select {
	case <-block.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the second call never started")
	}
	if !a.Interrupt() {
		t.Fatal("Interrupt reported no running turn")
	}
	if !waitForTurnEnd(t, events) {
		t.Fatal("no interrupted event was published")
	}
	if writer.ran != 1 {
		t.Fatalf("the writer ran %d time(s), want 1: only the first part", writer.ran)
	}

	msgs := a.History()
	if len(msgs) != 8 {
		t.Fatalf("history = %d messages, want user + reply + 2 answers + 2 part pairs: %+v", len(msgs), msgs)
	}
	if args := msgs[1].ToolCalls[0].Function.Arguments; !strings.Contains(args, `"content":"one"`) {
		t.Fatalf("the first call's args = %s, want the first part that was executed", args)
	}
	if msgs[2].ToolCallID != "c1" || msgs[2].Content != "wrote a part" {
		t.Fatalf("msgs[2] = %+v, want the real result of the first part", msgs[2])
	}
	if msgs[3].ToolCallID != "c2" || msgs[3].Content != "finished" {
		t.Fatalf("msgs[3] = %+v, want the real result of the call that was running", msgs[3])
	}
	for i, want := range []string{`"content":"two"`, `"content":"three"`} {
		call, answer := msgs[4+i*2], msgs[5+i*2]
		if call.Role != "assistant" || len(call.ToolCalls) != 1 || !strings.Contains(call.ToolCalls[0].Function.Arguments, want) {
			t.Fatalf("msgs[%d] = %+v, want the assistant message of the part with %s", 4+i*2, call, want)
		}
		if answer.Role != "tool" || answer.ToolCallID != call.ToolCalls[0].ID || !strings.Contains(answer.Content, "interrupted") {
			t.Fatalf("msgs[%d] = %+v, want the unstarted part answered as interrupted", 5+i*2, answer)
		}
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

// TestUserEventCarriesTheMessageAttachments verifies the files a message carries
// ride with its user event as descriptions — name, media type and the file they
// came from, never the payload — so a front-end can draw what the message brought
// along.
func TestUserEventCarriesTheMessageAttachments(t *testing.T) {
	a := newTestAgent(t)
	events, cancel := a.Bus().Subscribe()
	defer cancel()

	image := &llm.ContentPart{
		Type:     llm.PartTypeImageURL,
		ImageURL: &llm.ImageURLPart{URL: "data:image/png;base64,AAAA"},
		Path:     filepath.Join("state", "uploads", "shot.png"),
		Mime:     "image/png",
	}
	a.steerCh <- steerMessage{source: "web", text: "look at this", media: []llm.ContentPart{*image}}

	a.drainSteering()

	select {
	case ev := <-events:
		if ev.Type != EventUser || ev.Text != "look at this" || ev.Source != "web" {
			t.Fatalf("event = %+v", ev)
		}
		if len(ev.Attachments) != 1 {
			t.Fatalf("attachments = %+v, want the file the message carried", ev.Attachments)
		}
		got := ev.Attachments[0]
		if got.Name != "shot.png" || got.Type != "image/png" || got.Path == "" {
			t.Errorf("attachment = %+v", got)
		}
	default:
		t.Fatal("no user event was announced")
	}

	// The history keeps the media itself: the model receives the picture.
	history := a.History()
	if len(history) != 1 || len(history[0].Media) != 1 || !history[0].Media[0].HasPayload() {
		t.Fatalf("history = %+v, want the message with its media", history)
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
// sections appended below the base prompt (unlock rule, MCP info, runtime line,
// working directory) included — followed by the messages being compressed
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

// TestCompactionPassExtendsTheLiveRequestPrefix pins the prefill semantics of a
// pass that runs after an ordinary request: the summarizing call re-sends, byte
// for byte, the messages that request carried — its head (system message plus the
// accumulated summary where the configuration puts it, and the same tools)
// included — and only appends at the tail: what the conversation recorded after
// that request (the tool round) and then the summarize instruction. Nothing in
// front of the instruction is rendered, reordered or rewritten, which is what
// lets the provider serve the summarizing call from the prompt prefix it cached
// for the ordinary request instead of prefilling the whole batch again.
func TestCompactionPassExtendsTheLiveRequestPrefix(t *testing.T) {
	type wireRequest struct {
		Messages []json.RawMessage `json:"messages"`
		Tools    json.RawMessage   `json:"tools"`
	}
	// messageOf decodes one wire message, so a request can be told apart by its
	// roles and its last message.
	messageOf := func(raw json.RawMessage) capturedMessage {
		var m capturedMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Errorf("decode message: %v", err)
		}
		return m
	}
	// endsWithInstruction reports whether a request is a compaction pass: its
	// final message is the summarize instruction.
	endsWithInstruction := func(req wireRequest) bool {
		if len(req.Messages) == 0 {
			return false
		}
		return strings.HasPrefix(messageOf(req.Messages[len(req.Messages)-1]).Content, summarizeInstructionIntro)
	}
	rolesOf := func(msgs []json.RawMessage) []string {
		roles := make([]string, 0, len(msgs))
		for _, raw := range msgs {
			roles = append(roles, messageOf(raw).Role)
		}
		return roles
	}

	var (
		mu     sync.Mutex
		lives  int
		bodies []wireRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wireRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		pass := endsWithInstruction(req)

		mu.Lock()
		bodies = append(bodies, req)
		if !pass {
			lives++
		}
		live := lives
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case pass:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"the report"},"finish_reason":"stop"}]}`)
		case live == 1:
			// The turn carries on with a tool round, so the next iteration
			// compresses the grown history and that pass has an ordinary
			// request in front of it. The padded arguments keep the round well
			// over the tiny retention budget, so the whole history is
			// compressed.
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"stub","arguments":%q}}]},"finish_reason":"tool_calls"}]}`,
				`{"pad":"`+strings.Repeat("x", 200)+`"}`)
		default:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	// A tiny window with a 1% trigger compresses on every iteration, and the
	// retention budget leaves no room for the newest turn, so each pass
	// summarizes the whole history.
	cfg.Context.ContextWindow = 100
	cfg.Context.SummarizeTokenPercent = 1
	reg := tools.NewRegistry()
	reg.Register(agentStubTool{name: "stub", desc: "a stub tool"})
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	a := New(cfg, llm.NewClient(cfg.OpenAI), reg, bus)

	a.Load([]llm.Message{
		userRunes("old ", 100),
		{Role: "assistant", Content: "old answer"},
	}, "")
	a.Submit(userRunes("question ", 100).Content)
	drainEvents(t, events)

	mu.Lock()
	captured := append([]wireRequest(nil), bodies...)
	mu.Unlock()

	// Only the pass with an ordinary request in front of it says anything: the
	// first pass of a turn runs before the turn's first call.
	var live, pass *wireRequest
	for i := 1; i < len(captured); i++ {
		if endsWithInstruction(captured[i]) && !endsWithInstruction(captured[i-1]) {
			live, pass = &captured[i-1], &captured[i]
			break
		}
	}
	if live == nil {
		t.Fatalf("no pass followed an ordinary request (%d requests captured)", len(captured))
	}
	if len(pass.Messages) <= len(live.Messages) {
		t.Fatalf("the summarizing call sent %d messages, the ordinary request %d: it has to extend it",
			len(pass.Messages), len(live.Messages))
	}
	// The shared prefix really is the head plus the batch: the system message
	// first, the accumulated summary in the place the default layout puts it
	// (the [engine] message), the messages being compressed behind it.
	if msg := messageOf(live.Messages[0]); msg.Role != "system" {
		t.Fatalf("the ordinary request starts with %q, want the system message", msg.Role)
	}
	if msg := messageOf(pass.Messages[1]); !strings.HasPrefix(msg.Content, summaryUserPrefix) {
		t.Fatalf("the second message of the summarizing call is %q, want the accumulated summary", msg.Content)
	}
	// The shared prefix: head and batch byte for byte, exactly as sent.
	for i, want := range live.Messages {
		if got := pass.Messages[i]; string(got) != string(want) {
			t.Fatalf("message %d of the summarizing call differs from the ordinary request:\ngot:  %s\nwant: %s",
				i, got, want)
		}
	}
	// The only additions are at the tail: what the conversation recorded after
	// that request, with the instruction last.
	appended := pass.Messages[len(live.Messages) : len(pass.Messages)-1]
	if len(appended) == 0 {
		t.Fatal("the summarizing call added nothing but the instruction")
	}
	if got := strings.Join(rolesOf(appended), ","); got != "assistant,tool" {
		t.Fatalf("the summarizing call appended %q, want the tool round behind the shared prefix", got)
	}
	if string(pass.Tools) != string(live.Tools) {
		t.Fatalf("the summarizing call declares different tools:\npass: %s\nlive: %s", pass.Tools, live.Tools)
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

// TestCompactionTriggerUsesTheProviderReport pins what the automatic pass is
// triggered on: the provider-reported prompt-token count plus the lower-bound
// estimate of the messages appended after that report — the same number the
// frontends display. The character count of the whole context is deliberately not
// compared against it, because a lower bound in different units is not a
// competing measurement: comparing them fired a pass at 60% of the window while
// the setting said 80%. A request the provider really does reject for its size is
// recovered from instead (see recoverContextOverflow).
func TestCompactionTriggerUsesTheProviderReport(t *testing.T) {
	cfg := config.Default()
	// The window the report this behaviour came from ran with: 80% of 262144
	// tokens is the 209715-token trigger.
	cfg.Context.ContextWindow = 262144
	cfg.Context.SummarizeTokenPercent = 80
	a := New(cfg, nil, tools.NewRegistry(), NewBus())

	// A history whose character count runs far past the trigger — 840000 runes in
	// one unbroken run is 210000 estimated tokens — while the provider reported
	// 157125 tokens, i.e. 60% of the window.
	a.Load([]llm.Message{{Role: "user", Content: strings.Repeat("x", 840000)}}, "")
	a.setUsage(llm.Usage{PromptTokens: 157125})
	if got := a.Stats().EstimatedTok; got != 157125 {
		t.Fatalf("context estimate = %d, want the reported 157125 tokens", got)
	}
	if a.compactionDue() {
		t.Fatal("the pass fired on the character count of the history instead of the reported usage")
	}

	// What the provider has not seen yet is counted on top of that report.
	a.appendMessage(llm.Message{Role: "tool", Content: strings.Repeat("x", 40000)})
	if got := a.Stats().EstimatedTok; got != 157125+10000 {
		t.Fatalf("context estimate = %d, want the report plus the appended estimate", got)
	}
	if a.compactionDue() {
		t.Fatal("the appended messages alone must not reach the trigger")
	}
	a.appendMessage(llm.Message{Role: "tool", Content: strings.Repeat("x", 240000)})
	if !a.compactionDue() {
		t.Fatal("a context past the trigger must compact")
	}

	// With no report yet (a fresh process, a resumed session) the request itself
	// is estimated — the system prompt and its capability sections included.
	freshCfg := config.Default()
	freshCfg.Agent.SystemPrompt = strings.Repeat("p", 400000)
	fresh := New(freshCfg, nil, tools.NewRegistry(), NewBus())
	if !fresh.compactionDue() {
		t.Fatal("without a report the estimated request must be measured, system prompt included")
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

// TestSystemPromptWorkingDirectory verifies the working-directory line is
// injected by default, suppressed when agent.include_working_dir is off, and
// states the path only — the directory's children are never listed.
func TestSystemPromptWorkingDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	on := New(config.Default(), nil, tools.NewRegistry(), NewBus())
	on.mu.Lock()
	enabled := on.buildMessagesLocked()[0].Content
	on.mu.Unlock()
	if want := "working directory: " + cwd; !strings.HasSuffix(enabled, want) {
		t.Fatalf("system prompt must end with %q:\n%s", want, enabled)
	}
	if n := strings.Count(enabled, "working directory: "); n != 1 {
		t.Fatalf("working-directory line count = %d, want 1:\n%s", n, enabled)
	}

	cfg := config.Default()
	cfg.Agent.IncludeWorkingDir = false
	off := New(cfg, nil, tools.NewRegistry(), NewBus())
	off.mu.Lock()
	disabled := off.buildMessagesLocked()[0].Content
	off.mu.Unlock()
	if strings.Contains(disabled, "working directory: ") {
		t.Fatalf("working-directory line present while disabled:\n%s", disabled)
	}
}

// TestSystemPromptSectionOrder pins the layout of the system prompt: the base
// prompt first, then the tool/machine capability sections (unlock rule, MCP
// global info) and finally the host sections — the runtime line and the working
// directory — at the bottom.
func TestSystemPromptSectionOrder(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.SystemPrompt = "custom base"
	reg := tools.NewRegistry()
	reg.Register(agentStubTool{name: "exec_command", desc: "run a command"})
	reg.RegisterDeferred(agentStubTool{name: "mcp_github_create_issue", desc: "Create a GitHub issue"})
	a := New(cfg, nil, reg, NewBus())
	a.SetMCPServers([]MCPServerInfo{{Server: "playwright", ToolCount: 26}})

	a.mu.Lock()
	content := a.buildMessagesLocked()[0].Content
	a.mu.Unlock()

	prev := -1
	for _, section := range []string{
		"custom base",
		"**Tool Discovery & Unlock**",
		"MCP server `playwright` is connected.",
		RuntimeInfo(),
		"working directory: ",
	} {
		at := strings.Index(content, section)
		if at < 0 {
			t.Fatalf("system prompt is missing %q:\n%s", section, content)
		}
		if at < prev {
			t.Fatalf("system prompt puts %q before the section it must follow:\n%s", section, content)
		}
		prev = at
	}
}

// TestSystemPromptRuntimeLine verifies the runtime line is appended at run time at
// the bottom of the system prompt — also when the base prompt comes from agent.md
// — so the environment is never baked into the prompt file.
func TestSystemPromptRuntimeLine(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.SystemPrompt = "custom base"
	a := New(cfg, nil, tools.NewRegistry(), NewBus())

	a.mu.Lock()
	content := a.buildMessagesLocked()[0].Content
	a.mu.Unlock()

	if !strings.HasPrefix(content, "custom base\n\n") {
		t.Fatalf("system prompt should start with the base prompt:\n%s", content)
	}
	want := RuntimeInfo() + "\n\nworking directory: "
	if !strings.Contains(content, want) {
		t.Fatalf("system prompt should carry %q at the bottom:\n%s", want, content)
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

// TestOverflowRejectionRollsBackTheToolRoundAndRetries covers the usual cause of a
// provider rejecting a request for its size: tool feedback far larger than the
// local estimate counted for it (a whole file, an image read back). The turn rolls
// that round back out of the context and asks the model again with the same user
// message in front of a context that now fits, instead of failing.
func TestOverflowRejectionRollsBackTheToolRoundAndRetries(t *testing.T) {
	const userText = "read the log"
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

		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"echo","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		case 2:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 12000 tokens. Please reduce the length of the messages.","code":"context_length_exceeded"}}`)
		default:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	reg := tools.NewRegistry()
	reg.Register(agentStubTool{name: "echo", desc: "echoes"})
	a := New(cfg, llm.NewClient(cfg.OpenAI), reg, bus)

	a.Submit(userText)
	got := drainEvents(t, events)

	if calls != 3 {
		t.Fatalf("model calls = %d, want the first call, the rejected one and the retry", calls)
	}
	if hasEvent(got, EventError, "") {
		t.Fatalf("the turn failed instead of recovering: %+v", got)
	}
	const rollback = "context length exceeded: rolled back 3 message(s), compressing the context and retrying"
	if !hasEvent(got, EventInfo, rollback) {
		t.Fatalf("no rollback info was published: %+v", got)
	}
	if !hasEvent(got, EventAssistant, "done") {
		t.Fatalf("the retry's reply was not published: %+v", got)
	}

	// The retry is the same user message in front of the history that is left:
	// the rejected tool round is gone (nothing else had to be compressed).
	mu.Lock()
	retry := bodies[len(bodies)-1]
	mu.Unlock()
	if len(retry) != 2 || retry[0].Role != "system" || retry[1].Role != "user" || retry[1].Content != userText {
		t.Fatalf("retry request = %+v, want system + the user message alone", retry)
	}
	hist := a.History()
	if len(hist) != 2 || hist[0].Role != "user" || hist[1].Content != "done" {
		t.Fatalf("history = %+v, want the user message and the retry's reply", hist)
	}
}

// TestOverflowRejectionSummarizesTheContextAndReplaysTheMessage covers the other
// half of the recovery: when the surviving history has something to condense, all
// of it is compressed into the summary (nothing is left raw) and the turn's user
// message is replayed on top, so the retry carries the summary plus that very
// message instead of the history the provider rejected. The replayed message is
// deliberately not part of the summarizing batch: an oversized attachment in it
// would otherwise make that call overflow too.
func TestOverflowRejectionSummarizesTheContextAndReplaysTheMessage(t *testing.T) {
	const userText = "and now?"
	var (
		mu       sync.Mutex
		bodies   [][]capturedMessage
		digests  [][]capturedMessage
		liveCall int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []capturedMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		last := ""
		if len(req.Messages) > 0 {
			last = req.Messages[len(req.Messages)-1].Content
		}
		isDigest := strings.HasPrefix(last, summarizeInstructionIntro)

		mu.Lock()
		bodies = append(bodies, req.Messages)
		if isDigest {
			digests = append(digests, req.Messages)
		} else {
			liveCall++
		}
		n := liveCall
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case isDigest:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"REPORT"},"finish_reason":"stop"}]}`)
		case n == 1:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"echo","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		case n == 2:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 12000 tokens. Please reduce the length of the messages.","code":"context_length_exceeded"}}`)
		default:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	reg := tools.NewRegistry()
	reg.Register(agentStubTool{name: "echo", desc: "echoes"})
	a := New(cfg, llm.NewClient(cfg.OpenAI), reg, bus)

	a.Load([]llm.Message{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "second question"},
		{Role: "assistant", Content: "second answer"},
	}, "")
	a.Submit(userText)
	got := drainEvents(t, events)

	if liveCall != 3 {
		t.Fatalf("live calls = %d, want the first call, the rejected one and the retry", liveCall)
	}
	if hasEvent(got, EventError, "") {
		t.Fatalf("the turn failed instead of recovering: %+v", got)
	}
	if !hasEvent(got, EventInfo, "context length exceeded: rolled back 3 message(s), compressing the context and retrying") {
		t.Fatalf("no rollback info was published: %+v", got)
	}
	if !hasEvent(got, EventCompacted, "context compressed: 4 -> 1 messages") {
		t.Fatalf("the surviving history was not compressed: %+v", got)
	}
	if a.Summary() != "REPORT" {
		t.Fatalf("summary = %q, want the report the model wrote", a.Summary())
	}
	if !hasEvent(got, EventAssistant, "done") {
		t.Fatalf("the retry's reply was not published: %+v", got)
	}

	// The summarizing call condensed the surviving history — and only that: the
	// rolled-back message is what gets replayed afterwards, not summarized (its
	// attachment could be the very thing that overflowed the window).
	mu.Lock()
	defer mu.Unlock()
	if len(digests) != 1 {
		t.Fatalf("summarizing calls = %d, want 1", len(digests))
	}
	digest := digests[0]
	if digest[1].Role != "user" || digest[1].Content != "first question" {
		t.Fatalf("the summarizing batch starts with %+v, want the first recorded turn", digest[1])
	}
	for _, m := range digest {
		if strings.Contains(m.Content, userText) {
			t.Fatalf("the summarizing call carried the rolled-back message: %+v", digest)
		}
	}

	// The retry carries the summary as the first user message and the replayed
	// message right behind it: that is the request the provider accepts.
	retry := bodies[len(bodies)-1]
	if len(retry) != 3 || retry[0].Role != "system" {
		t.Fatalf("retry request = %+v, want system + summary + the replayed message", retry)
	}
	if want := summaryUserPrefix + "REPORT"; retry[1].Content != want {
		t.Fatalf("retry summary message = %q, want %q", retry[1].Content, want)
	}
	if retry[2].Content != userText {
		t.Fatalf("retry message = %q, want the rolled-back %q", retry[2].Content, userText)
	}

	hist := a.History()
	if len(hist) != 2 || hist[0].Content != userText || hist[1].Content != "done" {
		t.Fatalf("history = %+v, want the replayed message and the retry's reply", hist)
	}
}

// TestOverflowRejectionWithoutAnythingToFreeFailsTheTurn covers what the recovery
// cannot help with: the user message alone is over the window (an oversized
// attachment, say), so rolling it back and replaying it would send the very request
// the provider just rejected. The turn reports the provider's error, retries
// nothing, and leaves the history untouched.
func TestOverflowRejectionWithoutAnythingToFreeFailsTheTurn(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","code":"context_length_exceeded"}}`)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	a := New(cfg, llm.NewClient(cfg.OpenAI), tools.NewRegistry(), bus)

	a.Submit("look at this picture")
	got := drainEvents(t, events)

	if calls != 1 {
		t.Fatalf("model calls = %d, want 1 (nothing could be freed, so no retry)", calls)
	}
	if !hasEvent(got, EventError, "context length") {
		t.Fatalf("the provider's error was not reported: %+v", got)
	}
	if hasEvent(got, EventInfo, "context length exceeded:") {
		t.Fatalf("a rollback was announced although nothing could be freed: %+v", got)
	}
	hist := a.History()
	if len(hist) != 1 || hist[0].Role != "user" || hist[0].Content != "look at this picture" {
		t.Fatalf("history = %+v, want the user message left as it was", hist)
	}
}

// TestOverflowRecoveryGivesUpAfterTwoAttempts covers a request that keeps coming
// back too large: the turn recovers at most maxContextOverflowRecoveries times
// (each attempt rolls back, compresses and replays) and then reports the provider's
// error instead of looping.
func TestOverflowRecoveryGivesUpAfterTwoAttempts(t *testing.T) {
	var (
		mu     sync.Mutex
		live   int
		digest int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []capturedMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		last := ""
		hasToolRow := false
		for i, m := range req.Messages {
			if i == len(req.Messages)-1 {
				last = m.Content
			}
			if m.Role == "tool" {
				hasToolRow = true
			}
		}
		isDigest := strings.HasPrefix(last, summarizeInstructionIntro)

		mu.Lock()
		if isDigest {
			digest++
		} else {
			live++
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		// The summarizing call is rejected too (the history it condenses is the
		// oversized one), which the pass survives by dropping that batch.
		if isDigest || hasToolRow {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 8192 tokens.","code":"context_length_exceeded"}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"echo","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	reg := tools.NewRegistry()
	reg.Register(agentStubTool{name: "echo", desc: "echoes"})
	a := New(cfg, llm.NewClient(cfg.OpenAI), reg, bus)

	a.Load([]llm.Message{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "second question"},
		{Role: "assistant", Content: "second answer"},
	}, "")
	a.Submit("keep going")
	got := drainEvents(t, events)

	rollbacks := 0
	for _, ev := range got {
		if ev.Type == EventInfo && strings.HasPrefix(ev.Text, "context length exceeded:") {
			rollbacks++
		}
	}
	if rollbacks != maxContextOverflowRecoveries {
		t.Fatalf("rollbacks = %d, want %d", rollbacks, maxContextOverflowRecoveries)
	}
	if !hasEvent(got, EventError, "context length") {
		t.Fatalf("the provider's error was not reported after the attempts: %+v", got)
	}
	if live != 6 {
		t.Fatalf("live calls = %d, want the two recoveries and their retries", live)
	}
	if digest != 1 {
		t.Fatalf("summarizing calls = %d, want the one the first recovery issued", digest)
	}
	if a.Busy() {
		t.Fatal("the agent is still busy after the failure")
	}
}

// TestOverflowRejectionOnTheFirstCallResendsTheMessage covers the other common
// trigger named in the same breath as tool feedback: a user message the provider
// rejects as too large for the window (an attachment, for instance) when the very
// first call of the turn is made. The message is rolled back, the history before it
// is compressed into the summary, and the message is replayed on top of that
// summary — the same input, sent again with everything else compressed.
func TestOverflowRejectionOnTheFirstCallResendsTheMessage(t *testing.T) {
	const userText = "what do you make of this?"
	var (
		mu     sync.Mutex
		bodies [][]capturedMessage
		live   int
		digest int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []capturedMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		last := ""
		if len(req.Messages) > 0 {
			last = req.Messages[len(req.Messages)-1].Content
		}
		isDigest := strings.HasPrefix(last, summarizeInstructionIntro)

		mu.Lock()
		bodies = append(bodies, req.Messages)
		if isDigest {
			digest++
		} else {
			live++
		}
		n := live
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case isDigest:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"REPORT"},"finish_reason":"stop"}]}`)
		case n == 1:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 20000 tokens.","code":"context_length_exceeded"}}`)
		default:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	a := New(cfg, llm.NewClient(cfg.OpenAI), tools.NewRegistry(), bus)

	a.Load([]llm.Message{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "second question"},
		{Role: "assistant", Content: "second answer"},
	}, "")
	a.Submit(userText)
	got := drainEvents(t, events)

	if live != 2 || digest != 1 {
		t.Fatalf("live/digest calls = %d/%d, want the rejected call, one summarizing call and the retry", live, digest)
	}
	if hasEvent(got, EventError, "") {
		t.Fatalf("the turn failed instead of recovering: %+v", got)
	}
	if !hasEvent(got, EventInfo, "context length exceeded: rolled back 1 message(s), compressing the context and retrying") {
		t.Fatalf("no rollback info was published: %+v", got)
	}

	mu.Lock()
	defer mu.Unlock()
	retry := bodies[len(bodies)-1]
	if len(retry) != 3 || retry[0].Role != "system" {
		t.Fatalf("retry request = %+v, want system + summary + the message", retry)
	}
	if want := summaryUserPrefix + "REPORT"; retry[1].Content != want {
		t.Fatalf("retry summary message = %q, want %q", retry[1].Content, want)
	}
	if retry[2].Content != userText {
		t.Fatalf("retry message = %q, want the resubmitted user message", retry[2].Content)
	}
}
