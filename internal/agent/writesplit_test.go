package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"lightagent/internal/config"
	"lightagent/internal/llm"
	"lightagent/internal/tools"
)

// capturedCall mirrors one wire message including its tool calls, so a test can
// assert on the exact assistant/tool sequence the model is shown.
type capturedCall struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	ToolCalls []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
	ToolCallID string `json:"tool_call_id"`
}

// scriptedProvider is a stub OpenAI endpoint: the first request is answered with
// the given tool_calls array, every later one with a plain "done" reply. It
// records the request bodies so a test can inspect the messages the model saw.
type scriptedProvider struct {
	t      *testing.T
	first  string
	mu     sync.Mutex
	calls  int
	bodies [][]capturedCall
	srv    *httptest.Server
}

func newScriptedProvider(t *testing.T, first string) *scriptedProvider {
	t.Helper()
	p := &scriptedProvider{t: t, first: first}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *scriptedProvider) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []capturedCall `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		p.t.Errorf("decode request: %v", err)
	}
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.bodies = append(p.bodies, req.Messages)
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if call == 1 {
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":%s},"finish_reason":"tool_calls"}]}`, p.first)
		return
	}
	fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
}

func (p *scriptedProvider) lastMessages() []capturedCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.bodies) == 0 {
		return nil
	}
	return p.bodies[len(p.bodies)-1]
}

func (p *scriptedProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// writeCallJSON renders one write_file tool call the way a provider delivers it.
func writeCallJSON(t *testing.T, id string, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := json.Marshal(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"id":%q,"type":"function","function":{"name":"write_file","arguments":%s}}`, id, quoted)
}

// numberedLines builds a payload of n lines named after their position.
func numberedLines(n int) string {
	lines := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		lines = append(lines, fmt.Sprintf("line%02d", i))
	}
	return strings.Join(lines, "\n")
}

// newAutoSplitAgent wires an agent around a stub provider, with the real
// write_file tool registered under the configured line limit.
func newAutoSplitAgent(t *testing.T, cfg *config.Config, url string) (*Agent, <-chan Event) {
	t.Helper()
	cfg.OpenAI.APIBase = url
	cfg.OpenAI.Stream = false
	reg := tools.NewRegistry()
	reg.Register(tools.NewWriteFileTool(tools.FsConfig{MaxWriteLines: cfg.Tools.WriteFile.MaxLines}))
	bus := NewBus()
	events, cancel := bus.Subscribe()
	t.Cleanup(cancel)
	return New(cfg, llm.NewClient(cfg.OpenAI), reg, bus), events
}

// checkCallPairing pins the invariant every provider enforces: each entry of an
// assistant message's tool_calls is answered by the tool message right after it,
// in order. The follow-up write parts have to keep it.
func checkCallPairing(t *testing.T, msgs []capturedCall) {
	t.Helper()
	for i := 0; i < len(msgs); i++ {
		if msgs[i].Role != "assistant" || len(msgs[i].ToolCalls) == 0 {
			continue
		}
		for j, call := range msgs[i].ToolCalls {
			idx := i + 1 + j
			if idx >= len(msgs) {
				t.Fatalf("call %s has no tool answer", call.ID)
			}
			if msgs[idx].Role != "tool" || msgs[idx].ToolCallID != call.ID {
				t.Fatalf("call %s answered by %+v, want the tool message carrying its id", call.ID, msgs[idx])
			}
		}
	}
}

// readText reads a file the test wrote through the agent.
func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestAutoSplitWriteRunsFollowUpRounds covers the feature end to end: a payload
// of twelve lines under a five-line limit becomes three write calls on the one
// assistant message. The model's own call keeps the first part — in the history
// and in the request the model sees next — and the remaining parts ride along as
// extra calls of that same message, each answered by the tool message that
// follows it.
func TestAutoSplitWriteRunsFollowUpRounds(t *testing.T) {
	payload := numberedLines(12)
	file := filepath.Join(t.TempDir(), "big.txt")
	provider := newScriptedProvider(t, "["+writeCallJSON(t, "c1", map[string]any{"path": file, "content": payload})+"]")

	cfg := config.Default()
	cfg.Tools.WriteFile.MaxLines = 5
	a, events := newAutoSplitAgent(t, cfg, provider.srv.URL)

	a.Submit("write the file")
	drainEvents(t, events)

	hist := a.History()
	if len(hist) != 6 {
		t.Fatalf("history = %d messages, want user + one reply with three calls + three answers + the final reply: %+v", len(hist), hist)
	}
	assistant := hist[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 3 {
		t.Fatalf("history[1] = %+v, want one assistant message carrying the three write calls", assistant)
	}
	for i, want := range []string{"c1", "c1_part2", "c1_part3"} {
		call := assistant.ToolCalls[i]
		if call.ID != want || call.Function.Name != writeFileToolName {
			t.Fatalf("call %d = %s/%s, want %s/write_file", i+1, call.ID, call.Function.Name, want)
		}
		toolMsg := hist[2+i]
		if toolMsg.Role != "tool" || toolMsg.ToolCallID != want || toolMsg.Name != writeFileToolName {
			t.Fatalf("answer %d = %+v, want the answer for %s", i+1, toolMsg, want)
		}
		if strings.Contains(toolMsg.Content, "truncated") {
			t.Fatalf("call %d was truncated instead of split: %s", i+1, toolMsg.Content)
		}
	}
	if hist[5].Role != "assistant" || hist[5].Content != "done" {
		t.Fatalf("history[5] = %+v, want the model's final reply", hist[5])
	}

	// The model's own call was rewritten to the first part, and the follow-ups
	// carry the rest, appending.
	wantParts := []string{
		"line01\nline02\nline03\nline04\n",
		"line05\nline06\nline07\nline08\n",
		"line09\nline10\nline11\nline12",
	}
	for i, want := range wantParts {
		args := map[string]any{}
		if err := json.Unmarshal([]byte(assistant.ToolCalls[i].Function.Arguments), &args); err != nil {
			t.Fatalf("call %d arguments: %v", i+1, err)
		}
		if got, _ := args["content"].(string); got != want {
			t.Fatalf("call %d content = %q, want %q", i+1, got, want)
		}
		if i == 0 && args["mode"] != nil {
			t.Fatalf("the model's own call gained a mode: %v", args["mode"])
		}
		if i > 0 && args["mode"] != "a" {
			t.Fatalf("follow-up %d mode = %v, want append", i, args["mode"])
		}
	}
	if got := readText(t, file); got != payload {
		t.Fatalf("file content = %q, want the payload written in order", got)
	}

	// The request the model saw after the writes carries the whole chain, with
	// every call answered.
	if provider.callCount() != 2 {
		t.Fatalf("model calls = %d, want the reply after the writes", provider.callCount())
	}
	sent := provider.lastMessages()
	checkCallPairing(t, sent)
	var writeCalls int
	for _, m := range sent {
		for _, call := range m.ToolCalls {
			if call.Function.Name == writeFileToolName {
				writeCalls++
			}
		}
	}
	if writeCalls != 3 {
		t.Fatalf("the model saw %d write calls, want 3", writeCalls)
	}
}

// TestAutoSplitWriteKeepsOtherCalls covers one reply that mixes an ordinary call
// with an oversized write: the ordinary call keeps its place, the write is split
// and the extra parts join the same assistant message.
func TestAutoSplitWriteKeepsOtherCalls(t *testing.T) {
	payload := numberedLines(9)
	file := filepath.Join(t.TempDir(), "mixed.txt")
	first := "[" +
		`{"id":"s1","type":"function","function":{"name":"stub","arguments":"{}"}},` +
		writeCallJSON(t, "c1", map[string]any{"path": file, "content": payload}) +
		"]"
	provider := newScriptedProvider(t, first)

	cfg := config.Default()
	cfg.Tools.WriteFile.MaxLines = 5
	cfg.OpenAI.APIBase = provider.srv.URL
	cfg.OpenAI.Stream = false
	reg := tools.NewRegistry()
	reg.Register(agentStubTool{name: "stub", desc: "stub"})
	reg.Register(tools.NewWriteFileTool(tools.FsConfig{MaxWriteLines: 5}))
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	a := New(cfg, llm.NewClient(cfg.OpenAI), reg, bus)

	a.Submit("go")
	drainEvents(t, events)

	hist := a.History()
	if len(hist) != 7 {
		t.Fatalf("history = %d messages, want user + one reply with four calls + four answers + the final reply: %+v", len(hist), hist)
	}
	// The reply carries the ordinary call, the write's first part and the two
	// follow-up parts, all on the one assistant message.
	assistant := hist[1]
	wantIDs := []string{"s1", "c1", "c1_part2", "c1_part3"}
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != len(wantIDs) {
		t.Fatalf("history[1] = %+v, want the four calls on one message", assistant)
	}
	for i, want := range wantIDs {
		if got := assistant.ToolCalls[i].ID; got != want {
			t.Fatalf("call %d = %s, want %s", i, got, want)
		}
		answer := hist[2+i]
		if answer.Role != "tool" || answer.ToolCallID != want {
			t.Fatalf("answer %d = %+v, want the answer for %s", i, answer, want)
		}
	}
	if hist[2].Name != "stub" {
		t.Fatalf("history[2] = %+v, want the stub answer first", hist[2])
	}
	if hist[3].Name != writeFileToolName {
		t.Fatalf("history[3] = %+v, want the first write part", hist[3])
	}
	if hist[6].Role != "assistant" || hist[6].Content != "done" {
		t.Fatalf("history[6] = %+v, want the model's final reply", hist[6])
	}
	if got := readText(t, file); got != payload {
		t.Fatalf("file content = %q, want the payload", got)
	}
	checkCallPairing(t, provider.lastMessages())
}

// TestAutoSplitWriteDisabled pins the previous behaviour: with the switch off the
// tool keeps truncating and no follow-up round is added.
func TestAutoSplitWriteDisabled(t *testing.T) {
	payload := numberedLines(12)
	file := filepath.Join(t.TempDir(), "plain.txt")
	provider := newScriptedProvider(t, "["+writeCallJSON(t, "c1", map[string]any{"path": file, "content": payload})+"]")

	cfg := config.Default()
	cfg.Tools.WriteFile.MaxLines = 5
	cfg.Tools.WriteFile.AutoSplit = false
	a, events := newAutoSplitAgent(t, cfg, provider.srv.URL)

	a.Submit("write the file")
	drainEvents(t, events)

	hist := a.History()
	if len(hist) != 4 {
		t.Fatalf("history = %d messages, want user + one round + the reply", len(hist))
	}
	if !strings.Contains(hist[2].Content, "truncated") {
		t.Fatalf("tool answer = %q, want the truncation note", hist[2].Content)
	}
	if got := readText(t, file); got != "line01\nline02\nline03\nline04\nline05\n" {
		t.Fatalf("file content = %q, want the truncated first five lines", got)
	}
}

// TestAutoSplitWriteStopsWhenFirstPartFails covers a first part that fails (a
// create-only call on an existing file): the remaining parts are dropped, so the
// file is never left holding a payload that starts halfway through.
func TestAutoSplitWriteStopsWhenFirstPartFails(t *testing.T) {
	payload := numberedLines(12)
	file := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(file, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := newScriptedProvider(t, "["+writeCallJSON(t, "c1", map[string]any{
		"path": file, "content": payload, "mode": "c",
	})+"]")

	cfg := config.Default()
	cfg.Tools.WriteFile.MaxLines = 5
	a, events := newAutoSplitAgent(t, cfg, provider.srv.URL)

	a.Submit("write the file")
	got := drainEvents(t, events)

	hist := a.History()
	if len(hist) != 4 {
		t.Fatalf("history = %d messages, want the failed round and the reply only", len(hist))
	}
	if !strings.Contains(hist[2].Content, "already exists") {
		t.Fatalf("tool answer = %q, want the create-only error", hist[2].Content)
	}
	if text := readText(t, file); text != "original" {
		t.Fatalf("file content = %q, want it untouched", text)
	}
	if !hasEvent(got, EventInfo, "2 part(s) were skipped") {
		t.Fatalf("no info about the dropped parts in %+v", got)
	}
}
