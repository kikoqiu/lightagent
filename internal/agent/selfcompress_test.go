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
	"lightagent/internal/utils"
)

// condensingTool is a tool whose answer is a long body and that asks for the
// self-compression pass, the way webfetch does. bodies maps the url argument of a
// call to the answer it returns, so a round with several calls can be told apart;
// body is the answer of every call the map does not mention.
type condensingTool struct {
	body     string
	bodies   map[string]string
	compress bool
	retries  int
}

func (t condensingTool) Name() string               { return "webfetch" }
func (t condensingTool) Description() string        { return "fetch a page" }
func (t condensingTool) Parameters() map[string]any { return map[string]any{"type": "object"} }

func (t condensingTool) Execute(_ context.Context, args map[string]any) *tools.Result {
	body := t.body
	if url, ok := args["url"].(string); ok {
		if specific, ok := t.bodies[url]; ok {
			body = specific
		}
	}
	return &tools.Result{
		// The tool returns its own text, which the agent records as the answer of
		// the call like any other tool's return value.
		ForLLM:          "Conversion succeeded. Converter warnings (if any): none\n---\n\n" + body,
		ForUser:         "Fetched the page",
		Compress:        t.compress,
		CompressRetries: t.retries,
	}
}

// scriptedCall is one tool call the first model reply of a scripted provider
// asks for.
type scriptedCall struct {
	id   string
	name string
	args string
}

// condensingProvider answers the first model call with the given tool calls and
// every call after it with the next scripted reply ("done" once the script is
// spent). It keeps the messages of every call for the assertions.
func condensingProvider(t *testing.T, calls []scriptedCall, replies []string) (*httptest.Server, func() [][]capturedMessage) {
	t.Helper()
	var (
		mu     sync.Mutex
		count  int
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
		count++
		n := count
		bodies = append(bodies, req.Messages)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			toolCalls := make([]map[string]any, 0, len(calls))
			for _, call := range calls {
				toolCalls = append(toolCalls, map[string]any{
					"id":       call.id,
					"type":     "function",
					"function": map[string]any{"name": call.name, "arguments": call.args},
				})
			}
			reply, err := json.Marshal(map[string]any{
				"choices": []map[string]any{{
					"message":       map[string]any{"role": "assistant", "tool_calls": toolCalls},
					"finish_reason": "tool_calls",
				}},
			})
			if err != nil {
				t.Errorf("encode tool calls: %v", err)
				return
			}
			_, _ = w.Write(reply)
			return
		}
		content := "done"
		if n-2 < len(replies) {
			content = replies[n-2]
		}
		body, err := json.Marshal(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": "stop",
			}},
		})
		if err != nil {
			t.Errorf("encode reply: %v", err)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	return srv, func() [][]capturedMessage {
		mu.Lock()
		defer mu.Unlock()
		return append([][]capturedMessage(nil), bodies...)
	}
}

// condensingCallArgs is the argument JSON of the tool call the single-call tests
// use.
const condensingCallArgs = `{"url":"https://example.com"}`

// condensingCall mirrors that call, so a test can render the instructions the
// pass sends for it.
func condensingCall() llm.ToolCall {
	return llm.ToolCall{
		ID:       "c1",
		Type:     "function",
		Function: llm.ToolCallFunction{Name: "webfetch", Arguments: condensingCallArgs},
	}
}

// condensingRequests is that call as the round a scripted provider asks for.
func condensingRequests() []scriptedCall {
	return []scriptedCall{{id: "c1", name: "webfetch", args: condensingCallArgs}}
}

// plainTool is an ordinary tool that carries no compression request, so a round
// mixing it with webfetch shows that only the answers that asked for the pass are
// condensed.
type plainTool struct{ name, answer string }

func (t plainTool) Name() string               { return t.name }
func (t plainTool) Description() string        { return "a tool with a plain answer" }
func (t plainTool) Parameters() map[string]any { return map[string]any{"type": "object"} }

func (t plainTool) Execute(context.Context, map[string]any) *tools.Result {
	return tools.OK(t.answer)
}

// newCondensingAgent builds an agent whose registry holds the given tools.
func newCondensingAgent(t *testing.T, apiBase string, registered ...tools.Tool) (*Agent, <-chan Event) {
	t.Helper()
	cfg := config.Default()
	cfg.OpenAI.APIBase = apiBase
	cfg.OpenAI.Stream = false
	reg := tools.NewRegistry()
	for _, tool := range registered {
		reg.Register(tool)
	}
	bus := NewBus()
	events, cancel := bus.Subscribe()
	t.Cleanup(cancel)
	return New(cfg, llm.NewClient(cfg.OpenAI), reg, bus), events
}

// lastMessage returns the newest message of a captured call.
func lastMessage(msgs []capturedMessage) capturedMessage {
	if len(msgs) == 0 {
		return capturedMessage{}
	}
	return msgs[len(msgs)-1]
}

// containsMessage reports whether the call carried want as the content of one
// of its messages.
func containsMessage(msgs []capturedMessage, want string) bool {
	for _, m := range msgs {
		if m.Content == want {
			return true
		}
	}
	return false
}

// TestSelfCompressionReplacesTheToolResult covers the happy path: the first
// compression reply ignores the format and is asked again, the second one is
// wrapped, and the tool answer is rolled back and recorded as the condensed
// content — the instruction and the replies of the pass are gone.
func TestSelfCompressionReplacesTheToolResult(t *testing.T) {
	body := strings.Repeat("long page text. ", 200)
	srv, calls := condensingProvider(t, condensingRequests(), []string{
		"Here is the summary, without any tags.",
		"<compressed-content>\n# Page\n\nshort version\n</compressed-content>",
	})
	a, events := newCondensingAgent(t, srv.URL, condensingTool{body: body, compress: true, retries: 2})

	a.Submit("read the page")
	got := drainEvents(t, events)

	hist := a.History()
	if len(hist) != 4 {
		t.Fatalf("history = %d messages, want user + assistant + tool + answer: %+v", len(hist), hist)
	}
	if hist[3].Role != "assistant" || hist[3].Content != "done" {
		t.Fatalf("history = %+v, want the turn to end on the model's answer", hist)
	}
	tool := hist[2]
	if tool.Role != "tool" || tool.ToolCallID != "c1" || tool.Name != "webfetch" {
		t.Fatalf("history[2] = %+v, want the answer of the webfetch call c1", tool)
	}
	if tool.Content != "# Page\n\nshort version" {
		t.Errorf("the tool answer = %q, want exactly the condensed content", tool.Content)
	}
	if strings.Contains(tool.Content, "long page text.") {
		t.Errorf("the tool answer still carries the full page:\n%s", tool.Content)
	}

	// The pass announced itself, and the aborted attempt was reported.
	if !hasEvent(got, EventInfo, "condensing the answer of") ||
		!hasEvent(got, EventInfo, "did not follow the required format") ||
		!hasEvent(got, EventInfo, "was condensed to") {
		t.Errorf("the pass did not report its progress: %+v", got)
	}

	// The instruction names the call it is about, so it stays unambiguous when a
	// round answers several calls.
	instruction := selfCompressInstruction(condensingCall())
	retryInstruction := selfCompressRetryInstruction(condensingCall())
	for _, want := range []string{"webfetch", condensingCallArgs, "call id c1"} {
		if !strings.Contains(instruction, want) {
			t.Errorf("the instruction does not name %q:\n%s", want, instruction)
		}
		if !strings.Contains(retryInstruction, want) {
			t.Errorf("the retry instruction does not name %q:\n%s", want, retryInstruction)
		}
	}

	// Four calls: the tool call, the attempt, the retry, the ordinary call that
	// follows the pass. Only the pass carried engine text.
	captured := calls()
	if len(captured) != 4 {
		t.Fatalf("model calls = %d, want 4", len(captured))
	}
	if got := lastMessage(captured[1]); got.Content != instruction {
		t.Errorf("the first compression call ends with %q, want the instruction", got.Content)
	}
	if !containsMessage(captured[2], instruction) ||
		!containsMessage(captured[2], "Here is the summary, without any tags.") {
		t.Errorf("the retry call does not carry the instruction and the malformed reply: %+v", captured[2])
	}
	if got := lastMessage(captured[2]); got.Content != retryInstruction {
		t.Errorf("the retry call ends with %q, want the retry instruction", got.Content)
	}
	last := lastMessage(captured[3])
	if last.Role != "tool" || last.Content != "# Page\n\nshort version" {
		t.Errorf("the last call's newest message = %+v, want the condensed tool answer", last)
	}
	// The ordinary calls — before and after the pass — never carry engine text:
	// the instruction lives only for the calls the pass itself makes.
	for _, index := range []int{0, 3} {
		if containsMessage(captured[index], instruction) ||
			containsMessage(captured[index], retryInstruction) {
			t.Errorf("call %d carries an engine instruction of the pass", index+1)
		}
	}
}

// TestSelfCompressionCondensesEachAnswerOfARound covers a round that answers two
// calls: each answer gets its own instruction — each naming its own call — the
// pass runs them one at a time, and both tool messages are recorded again with
// their condensed content once every answer came back.
func TestSelfCompressionCondensesEachAnswerOfARound(t *testing.T) {
	firstArgs := `{"url":"https://one.example"}`
	secondArgs := `{"url":"https://two.example"}`
	firstBody := strings.Repeat("first page text. ", 100)
	secondBody := strings.Repeat("second page text. ", 100)
	round := []scriptedCall{
		{id: "c1", name: "webfetch", args: firstArgs},
		{id: "c2", name: "webfetch", args: secondArgs},
	}
	srv, calls := condensingProvider(t, round, []string{
		"<compressed-content>first condensed</compressed-content>",
		"<compressed-content>second condensed</compressed-content>",
	})
	a, events := newCondensingAgent(t, srv.URL, condensingTool{
		bodies:   map[string]string{"https://one.example": firstBody, "https://two.example": secondBody},
		compress: true,
		retries:  2,
	})

	a.Submit("read both pages")
	drainEvents(t, events)

	// Both tool messages keep their place in the round and carry the condensed
	// content in the tool frame.
	hist := a.History()
	if len(hist) != 5 {
		t.Fatalf("history = %d messages, want user + assistant + two tool + answer: %+v", len(hist), hist)
	}
	if hist[2].Content != "first condensed" || hist[3].Content != "second condensed" {
		t.Fatalf("tool answers = %q / %q, want the two condensed contents",
			hist[2].Content, hist[3].Content)
	}
	for _, m := range hist {
		if strings.Contains(m.Content, "page text.") {
			t.Errorf("a message still carries a full page:\n%s", m.Content)
		}
	}

	// One instruction per answer, each naming its own call, and the ordinary
	// call after the pass carries neither of them.
	first := selfCompressInstruction(llm.ToolCall{
		ID: "c1", Function: llm.ToolCallFunction{Name: "webfetch", Arguments: firstArgs}})
	second := selfCompressInstruction(llm.ToolCall{
		ID: "c2", Function: llm.ToolCallFunction{Name: "webfetch", Arguments: secondArgs}})
	if first == second {
		t.Fatal("the two instructions are identical, so neither names its call")
	}

	captured := calls()
	if len(captured) != 4 {
		t.Fatalf("model calls = %d, want the round, two passes and the answer", len(captured))
	}
	if got := lastMessage(captured[1]); got.Content != first {
		t.Errorf("the first pass ends with %q, want the instruction for c1", got.Content)
	}
	if got := lastMessage(captured[2]); got.Content != second {
		t.Errorf("the second pass ends with %q, want the instruction for c2", got.Content)
	}
	// The second pass still sees the first instruction and the reply it got:
	// every answer is condensed before the pass is rolled back.
	if !containsMessage(captured[2], first) ||
		!containsMessage(captured[2], "<compressed-content>first condensed</compressed-content>") {
		t.Errorf("the second pass does not see the first instruction and reply: %+v", captured[2])
	}
	last := lastMessage(captured[3])
	if last.Role != "tool" || last.Content != "second condensed" {
		t.Errorf("the last call's newest message = %+v, want the condensed tool answer", last)
	}
	if containsMessage(captured[3], first) || containsMessage(captured[3], second) {
		t.Errorf("the ordinary call still carries an instruction of the pass")
	}
}

// TestSelfCompressionSkipsTheCallsThatDidNotAskForIt covers a round of three
// calls where two are webfetch: the two webfetch answers are condensed in the
// order their calls came in, and the third answer is left exactly as it is.
func TestSelfCompressionSkipsTheCallsThatDidNotAskForIt(t *testing.T) {
	firstArgs := `{"url":"https://one.example"}`
	secondArgs := `{"url":"https://two.example"}`
	round := []scriptedCall{
		{id: "c1", name: "webfetch", args: firstArgs},
		{id: "c2", name: "exec_command", args: `{"command":"ls"}`},
		{id: "c3", name: "webfetch", args: secondArgs},
	}
	srv, calls := condensingProvider(t, round, []string{
		"<compressed-content>first condensed</compressed-content>",
		"<compressed-content>second condensed</compressed-content>",
	})
	a, events := newCondensingAgent(t, srv.URL,
		condensingTool{
			bodies: map[string]string{
				"https://one.example": "first page",
				"https://two.example": "second page",
			},
			compress: true,
			retries:  2,
		},
		plainTool{name: "exec_command", answer: "Command completed."},
	)

	a.Submit("do three things")
	got := drainEvents(t, events)

	// The three answers keep their places; only the two webfetch ones changed.
	hist := a.History()
	if len(hist) != 6 {
		t.Fatalf("history = %d messages, want user + assistant + three tool + answer: %+v", len(hist), hist)
	}
	if hist[2].Content != "first condensed" {
		t.Errorf("the first answer = %q, want its condensed content", hist[2].Content)
	}
	if hist[3].Content != "Command completed." {
		t.Errorf("the exec_command answer = %q, want it untouched", hist[3].Content)
	}
	if hist[4].Content != "second condensed" {
		t.Errorf("the last answer = %q, want its condensed content", hist[4].Content)
	}

	// The pass visits the two webfetch answers in call order and stops there:
	// the round, two passes and the ordinary call that follows.
	first := selfCompressInstruction(llm.ToolCall{
		ID: "c1", Function: llm.ToolCallFunction{Name: "webfetch", Arguments: firstArgs}})
	second := selfCompressInstruction(llm.ToolCall{
		ID: "c3", Function: llm.ToolCallFunction{Name: "webfetch", Arguments: secondArgs}})
	skipped := selfCompressInstruction(llm.ToolCall{
		ID: "c2", Function: llm.ToolCallFunction{Name: "exec_command", Arguments: `{"command":"ls"}`}})

	captured := calls()
	if len(captured) != 4 {
		t.Fatalf("model calls = %d, want the round, two passes and the answer", len(captured))
	}
	if got := lastMessage(captured[1]); got.Content != first {
		t.Errorf("the first pass ends with %q, want the instruction for c1", got.Content)
	}
	if got := lastMessage(captured[2]); got.Content != second {
		t.Errorf("the second pass ends with %q, want the instruction for c3", got.Content)
	}
	for i, msgs := range captured {
		if containsMessage(msgs, skipped) {
			t.Errorf("call %d was asked to condense the exec_command answer", i+1)
		}
	}
	if containsMessage(captured[3], first) || containsMessage(captured[3], second) {
		t.Errorf("the ordinary call still carries an instruction of the pass")
	}

	// The two passes announce themselves in the same order.
	var condensed []string
	for _, ev := range got {
		if ev.Type == EventInfo && strings.HasPrefix(ev.Text, "condensing the answer of") {
			condensed = append(condensed, ev.Text)
		}
	}
	if len(condensed) != 2 || !strings.Contains(condensed[0], "one.example") ||
		!strings.Contains(condensed[1], "two.example") {
		t.Errorf("the passes did not run in call order: %q", condensed)
	}
}

// TestSelfCompressionKeepsTheFullResultAfterRetryLimit covers a model that
// never wraps its reply: the pass is abandoned after the configured retries and
// the tool answer keeps the whole page.
func TestSelfCompressionKeepsTheFullResultAfterRetryLimit(t *testing.T) {
	body := strings.Repeat("long page text. ", 200)
	srv, calls := condensingProvider(t, condensingRequests(),
		[]string{"no tags", "still no tags", "and none here either"})
	a, events := newCondensingAgent(t, srv.URL, condensingTool{body: body, compress: true, retries: 2})

	a.Submit("read the page")
	got := drainEvents(t, events)

	hist := a.History()
	if len(hist) != 4 {
		t.Fatalf("history = %d messages, want user + assistant + tool + answer", len(hist))
	}
	if !strings.Contains(hist[2].Content, body) {
		t.Errorf("the tool answer should keep the whole page after the pass gave up:\n%s", hist[2].Content)
	}
	// One call per attempt (the first plus two retries) on top of the tool call
	// and the ordinary call that follows the abandoned pass.
	if n := len(calls()); n != 5 {
		t.Errorf("model calls = %d, want 5", n)
	}
	if !hasEvent(got, EventInfo, "self-compression got 3 reply/replies") {
		t.Errorf("the abandoned pass was not reported: %+v", got)
	}
}

// TestSelfCompressionSkippedWhenNotRequested pins that a tool which does not ask
// for the pass costs no extra model call and keeps its answer as it is.
func TestSelfCompressionSkippedWhenNotRequested(t *testing.T) {
	body := strings.Repeat("long page text. ", 20)
	srv, calls := condensingProvider(t, condensingRequests(), nil)
	a, events := newCondensingAgent(t, srv.URL, condensingTool{body: body, compress: false, retries: 2})

	a.Submit("read the page")
	drainEvents(t, events)

	hist := a.History()
	if len(hist) != 4 {
		t.Fatalf("history = %d messages, want user + assistant + tool + answer", len(hist))
	}
	if !strings.Contains(hist[2].Content, body) {
		t.Errorf("the tool answer should be untouched without a compression request:\n%s", hist[2].Content)
	}
	if n := len(calls()); n != 2 {
		t.Errorf("model calls = %d, want the tool call and the answer", n)
	}
}

// TestSelfCompressionWorksWithTheRealWebFetchTool runs the handshake of the two
// sides end to end: the real webfetch tool fetches and converts a page, asks for
// the pass, and the pass records the condensed content in the tool answer.
func TestSelfCompressionWorksWithTheRealWebFetchTool(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><head><title>Doc</title></head><body><h1>Heading</h1><p>%s</p></body></html>",
			strings.Repeat("long body text. ", 100))
	}))
	defer page.Close()

	srv, _ := condensingProvider(t,
		[]scriptedCall{{id: "c1", name: "webfetch", args: fmt.Sprintf("{\"url\":%q}", page.URL)}},
		[]string{"<compressed-content>condensed page</compressed-content>"})

	cfg := config.Default()
	cfg.OpenAI.APIBase = srv.URL
	cfg.OpenAI.Stream = false
	reg := tools.NewRegistry()
	reg.Register(tools.NewWebFetchTool(tools.WebFetchConfig{
		Timeout:         10 * time.Second,
		Mode:            utils.FetchModeHTTP,
		Compress:        true,
		CompressRetries: 2,
	}))
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	a := New(cfg, llm.NewClient(cfg.OpenAI), reg, bus)

	a.Submit("read the page")
	drainEvents(t, events)

	hist := a.History()
	if len(hist) != 4 {
		t.Fatalf("history = %d messages, want user + assistant + tool + answer", len(hist))
	}
	tool := hist[2]
	if tool.Content != "condensed page" {
		t.Errorf("the tool answer = %q, want exactly the condensed content", tool.Content)
	}
	if strings.Contains(tool.Content, "long body text.") {
		t.Errorf("the tool answer still carries the page markdown (%d chars)", len(tool.Content))
	}
}

// TestCallReference pins how an instruction names the call it is about: several
// answers can arrive in one round, so the function name, the call id and the
// arguments have to be there — without letting a huge payload into the prompt.
func TestCallReference(t *testing.T) {
	cases := []struct {
		name string
		call llm.ToolCall
		want string
	}{
		{"name, id and arguments",
			llm.ToolCall{ID: "c1", Function: llm.ToolCallFunction{
				Name: "webfetch", Arguments: `{"url":"https://example.com"}`}},
			`webfetch {"url":"https://example.com"} (call id c1)`},
		{"name only", llm.ToolCall{Function: llm.ToolCallFunction{Name: "webfetch"}}, "webfetch"},
		{"no name", llm.ToolCall{ID: "c2"}, "an unnamed tool (call id c2)"},
		{"no id", llm.ToolCall{Function: llm.ToolCallFunction{Name: "x", Arguments: "{}"}}, "x {}"},
	}
	for _, testCase := range cases {
		if got := callReference(testCase.call); got != testCase.want {
			t.Errorf("%s: callReference = %q, want %q", testCase.name, got, testCase.want)
		}
	}

	// A long payload is cut: the reference identifies the call, it does not
	// restate it. Arguments that arrive with newlines still take one line.
	long := callReference(llm.ToolCall{ID: "c3", Function: llm.ToolCallFunction{
		Name: "write_file", Arguments: `{"content":"` + strings.Repeat("x", 500) + `"}`,
	}})
	if !strings.Contains(long, "…") || len(long) > maxCallArgumentsChars+40 {
		t.Errorf("callReference did not abbreviate a long argument list (%d chars): %s", len(long), long)
	}
	if multiline := callReference(llm.ToolCall{ID: "c4", Function: llm.ToolCallFunction{
		Name: "write_file", Arguments: "{\n  \"path\": \"a.txt\"\n}",
	}}); strings.Contains(multiline, "\n") {
		t.Errorf("callReference kept a newline: %q", multiline)
	}
}

// TestExtractCompressedContent pins the format check the pass applies to every
// compression reply.
func TestExtractCompressedContent(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  string
		ok    bool
	}{
		{"wrapped", "<compressed-content>short</compressed-content>", "short", true},
		{"padded", "\n<compressed-content>\n  short\n</compressed-content>\n", "short", true},
		{"multiline", "<compressed-content># Title\n\n- item</compressed-content>", "# Title\n\n- item", true},
		{"no tags", "short", "", false},
		{"missing close", "<compressed-content>short", "", false},
		{"missing open", "short</compressed-content>", "", false},
		{"empty block", "<compressed-content>   </compressed-content>", "", false},
		{"blank reply", "", "", false},
	}
	for _, testCase := range cases {
		got, ok := extractCompressedContent(testCase.reply)
		if ok != testCase.ok || got != testCase.want {
			t.Errorf("%s: extractCompressedContent(%q) = %q, %v; want %q, %v",
				testCase.name, testCase.reply, got, ok, testCase.want, testCase.ok)
		}
	}
}
