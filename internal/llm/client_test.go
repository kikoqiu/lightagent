package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lightagent/internal/config"
)

// TestChatRequestBodyIncludesExtraBody verifies that extra_body keys are merged
// into the top-level request body and can override built-in fields.
func TestChatRequestBodyIncludesExtraBody(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	client := NewClient(config.OpenAIConfig{
		APIBase:     srv.URL,
		APIKey:      "sk-test",
		Model:       "test-model",
		Temperature: 0.0,
		MaxTokens:   16,
		ExtraBody: map[string]any{
			"reasoning_effort": "high",
			"temperature":      0.1,
		},
	})

	resp, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, nil)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q, want ok", resp.Content)
	}
	if got["model"] != "test-model" {
		t.Fatalf("model = %v", got["model"])
	}
	if got["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high", got["reasoning_effort"])
	}
	if temp, _ := got["temperature"].(float64); temp != 0.1 {
		t.Fatalf("temperature = %v, want 0.1 (extra_body must override)", got["temperature"])
	}
}

// TestExtraBodyFromConfigFileReachesRequest covers the path the user configures:
// values under openai.extra_body in config.json are read back and merged into
// the /chat/completions request body. The nested chat_template_kwargs
// preserve_thinking switch is the preserve-thinking case.
func TestExtraBodyFromConfigFileReachesRequest(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfgJSON := fmt.Sprintf(`{
	  "openai": {
	    "api_base": %q,
	    "model": "test-model",
	    "stream": false,
	    "extra_body": {
	      "reasoning_effort": "high",
	      "chat_template_kwargs": {"preserve_thinking": true}
	    }
	  }
	}`, srv.URL)
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, _, err := config.LoadFile(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	kwargs, _ := cfg.OpenAI.ExtraBody["chat_template_kwargs"].(map[string]any)
	if kwargs["preserve_thinking"] != true {
		t.Fatalf("config extra_body was not parsed: %+v", cfg.OpenAI.ExtraBody)
	}

	client := NewClient(cfg.OpenAI)
	if _, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, nil); err != nil {
		t.Fatalf("chat: %v", err)
	}

	if got["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high", got["reasoning_effort"])
	}
	sent, _ := got["chat_template_kwargs"].(map[string]any)
	if sent["preserve_thinking"] != true {
		t.Fatalf("chat_template_kwargs = %v, want preserve_thinking=true", got["chat_template_kwargs"])
	}
}

// TestReadStreamAssemblesToolCalls verifies SSE assembly of content and tool
// call fragments split across multiple chunks.
func TestReadStreamAssemblesToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file_lines","arguments":"{\"pa"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.txt\"}"}}]}}]}`,
		`data: {"choices":[{"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	var streamed strings.Builder
	client := &Client{}
	resp, err := client.readStream(strings.NewReader(stream), func(s string) {
		streamed.WriteString(s)
	}, nil)
	if err != nil {
		t.Fatalf("readStream: %v", err)
	}
	if resp.Content != "Hello" {
		t.Fatalf("content = %q, want Hello", resp.Content)
	}
	if streamed.String() != "Hello" {
		t.Fatalf("streamed = %q, want Hello", streamed.String())
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "read_file_lines" {
		t.Fatalf("tool call = %+v", tc)
	}
	if tc.Function.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("arguments = %q", tc.Function.Arguments)
	}
}

// TestReadStreamAcceptsBothArgumentsShapes covers how providers deliver a tool
// call's arguments: OpenAI splits the JSON text over several chunks, while some
// servers send the value itself ({"arguments":{}}) or omit it for a tool that
// takes no parameters. They all assemble into the same wire-level text, so a
// complete call is never mistaken for a truncated one.
func TestReadStreamAcceptsBothArgumentsShapes(t *testing.T) {
	cases := []struct {
		name string
		call string
		want string
	}{
		{
			name: "raw json value",
			call: `{"index":0,"id":"call_1","type":"function","function":{"name":"exec_command","arguments":{"command":"ls","n":2}}}`,
			want: `{"command":"ls","n":2}`,
		},
		{
			name: "null arguments",
			call: `{"index":0,"id":"call_1","type":"function","function":{"name":"ping","arguments":null}}`,
			want: "",
		},
		{
			name: "no arguments key",
			call: `{"index":0,"id":"call_1","type":"function","function":{"name":"ping"}}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := strings.Join([]string{
				`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[` + tc.call + `]}}]}`,
				`data: {"choices":[{"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
				``,
			}, "\n")

			client := &Client{}
			resp, err := client.readStream(strings.NewReader(stream), nil, nil)
			if err != nil {
				t.Fatalf("readStream: %v", err)
			}
			if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Arguments != tc.want {
				t.Fatalf("tool calls = %+v, want arguments %q", resp.ToolCalls, tc.want)
			}
		})
	}
}

// TestIncompleteResponsesAreClassified pins the condition reserved for the
// recovery path (see Agent.runTurn): a half-built reply is recognisable through
// IsIncompleteResponse, while unrelated failures are not.
func TestIncompleteResponsesAreClassified(t *testing.T) {
	cases := []struct {
		name string
		resp *Response
	}{
		{
			name: "truncated arguments",
			resp: &Response{Finish: "stop", ToolCalls: []ToolCall{{Function: ToolCallFunction{Name: "write_file", Arguments: `{"path":"a`}}}},
		},
		{
			name: "nameless call",
			resp: &Response{Finish: "tool_calls", ToolCalls: []ToolCall{{ID: "c1"}}},
		},
		{
			name: "length with calls pending",
			resp: &Response{Finish: "length", ToolCalls: []ToolCall{{Function: ToolCallFunction{Name: "ping", Arguments: "{}"}}}},
		},
		{
			name: "content filter",
			resp: &Response{Finish: "content_filter"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateResponse(tc.resp); !IsIncompleteResponse(err) {
				t.Fatalf("validateResponse = %v, want an incomplete-response error", err)
			}
		})
	}

	complete := &Response{Finish: "stop", ToolCalls: []ToolCall{{Function: ToolCallFunction{Name: "ping", Arguments: "{}"}}}}
	if err := validateResponse(complete); err != nil {
		t.Fatalf("validateResponse(complete reply) = %v", err)
	}
	if IsIncompleteResponse(fmt.Errorf("api error 401: bad key")) {
		t.Fatal("an API error was classified as an incomplete response")
	}
	if IsIncompleteResponse(nil) {
		t.Fatal("nil was classified as an incomplete response")
	}
}

// TestReadStreamIncompleteToolCallFails covers a provider that ends the stream
// with a normal-looking finish_reason after delivering only half of a tool
// call: the accumulated arguments are not valid JSON, so the call never fully
// arrived. Keeping it would execute (and replay) a call with truncated
// arguments, so it is reported as an error instead of ending the turn.
func TestReadStreamIncompleteToolCallFails(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file_lines","arguments":"{\"pa"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	client := &Client{}
	_, err := client.readStream(strings.NewReader(stream), nil, nil)
	if err == nil {
		t.Fatal("expected the incomplete tool call to fail")
	}
	if !strings.Contains(err.Error(), "read_file_lines") || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("error = %v, want an incomplete-tool-call message naming the tool", err)
	}
}

// TestReadStreamToolCallWithoutNameFails covers the other half-call shape: the
// stream ends after the call's id was sent but before its function name
// arrived, so there is no tool to run.
func TestReadStreamToolCallWithoutNameFails(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function"}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	client := &Client{}
	_, err := client.readStream(strings.NewReader(stream), nil, nil)
	if err == nil {
		t.Fatal("expected the nameless tool call to fail")
	}
	if !strings.Contains(err.Error(), "function name") {
		t.Fatalf("error = %v, want a missing-function-name message", err)
	}
}

// TestReadStreamLengthWithToolCallsFails covers a reply cut off at max_tokens
// while tool calls were being emitted: even when the accumulated arguments
// happen to parse, the call list may be short, so the turn fails with the
// setting to raise instead of running a possibly incomplete call.
func TestReadStreamLengthWithToolCallsFails(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{\"command\":\"ls\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	client := &Client{}
	_, err := client.readStream(strings.NewReader(stream), nil, nil)
	if err == nil {
		t.Fatal("expected a truncated tool-call list to fail")
	}
	if !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("error = %v, want a max_tokens hint", err)
	}
}

// TestReadStreamLengthWithoutToolCallsIsReturned pins that a plain truncation
// (no tool calls) is not an error here: the agent picks it up and continues the
// turn from the assistant message it recorded.
func TestReadStreamLengthWithoutToolCallsIsReturned(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"think"},"finish_reason":"length"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	client := &Client{}
	resp, err := client.readStream(strings.NewReader(stream), nil, nil)
	if err != nil {
		t.Fatalf("readStream: %v", err)
	}
	if resp.Finish != "length" || resp.Reasoning != "think" {
		t.Fatalf("response = %+v, want the truncation reported to the caller", resp)
	}
}

// TestReadStreamContentFilterFails covers a provider that stops the reply with
// content_filter: the answer is incomplete, so it must not be presented as one.
func TestReadStreamContentFilterFails(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"par"},"finish_reason":"content_filter"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	client := &Client{}
	_, err := client.readStream(strings.NewReader(stream), nil, nil)
	if err == nil {
		t.Fatal("expected a filtered reply to fail")
	}
	if !strings.Contains(err.Error(), "content_filter") {
		t.Fatalf("error = %v, want the finish_reason named", err)
	}
}

// TestReadJSONIncompleteToolCallFails checks the non-streaming path applies the
// same completeness rules as the streamed one.
func TestReadJSONIncompleteToolCallFails(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"exec_command","arguments":"{\"command\":"}}]},"finish_reason":"stop"}]}`

	client := &Client{}
	if _, err := client.readJSON(strings.NewReader(body), nil); err == nil {
		t.Fatal("expected the incomplete tool call to fail")
	} else if !strings.Contains(err.Error(), "exec_command") {
		t.Fatalf("error = %v, want the tool name in the message", err)
	}
}

// TestReadStreamCorruptFrameFails covers a provider (or proxy) that flushes a
// chat chunk cut in half: the frame looks like JSON but does not parse, so the
// reply cannot be assembled and fails instead of silently dropping the fragment
// (which is how a tool call ends up half delivered while the turn looks fine).
func TestReadStreamCorruptFrameFails(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	client := &Client{}
	_, err := client.readStream(strings.NewReader(stream), nil, nil)
	if err == nil {
		t.Fatal("expected the corrupt frame to fail the stream")
	}
	if !strings.Contains(err.Error(), "unparsable SSE frame") {
		t.Fatalf("error = %v, want an unparsable-frame message", err)
	}
	// A cut frame is an incomplete reply too: the reserved recovery path
	// must see it as such.
	if !IsIncompleteResponse(err) {
		t.Fatalf("error = %v, want an incomplete-response error", err)
	}
}

// TestReadStreamSkipsHeartbeatFrames pins that non-JSON keep-alive payloads are
// still tolerated: only frames that start like JSON but do not parse fail.
func TestReadStreamSkipsHeartbeatFrames(t *testing.T) {
	stream := strings.Join([]string{
		`data: ping`,
		`data: {"choices":[{"delta":{"content":"Hi"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	client := &Client{}
	resp, err := client.readStream(strings.NewReader(stream), nil, nil)
	if err != nil {
		t.Fatalf("readStream: %v", err)
	}
	if resp.Content != "Hi" {
		t.Fatalf("content = %q, want Hi", resp.Content)
	}
}

// TestChatStreamsIncrementally verifies each SSE chunk reaches onDelta before
// the stream finishes: the server holds its last chunk until the test has seen
// the first one, so buffering the whole body would deadlock the assertion.
func TestChatStreamsIncrementally(t *testing.T) {
	gate := make(chan struct{})
	timeout := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		flusher.Flush()
		select {
		case <-gate:
		case <-time.After(2 * time.Second):
			close(timeout)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\ndata: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	client := NewClient(config.OpenAIConfig{APIBase: srv.URL, Model: "m", Stream: true, TimeoutSec: 10})

	var streamed strings.Builder
	resp, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, func(s string) {
		streamed.WriteString(s)
		once.Do(func() { close(gate) })
	}, nil)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	select {
	case <-timeout:
		t.Fatal("onDelta was not called until after the stream finished")
	default:
	}
	if resp.Content != "Hello" || streamed.String() != "Hello" {
		t.Fatalf("content = %q, streamed = %q, want Hello", resp.Content, streamed.String())
	}
}

// TestChatAbortsStalledStream checks the inactivity timeout: a stream that sends
// one frame and then goes quiet is aborted with an actionable error instead of
// hanging (or being cut off mid-generation).
func TestChatAbortsStalledStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		flusher.Flush()
		// Keep the connection open but silent until the client gives up.
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := NewClient(config.OpenAIConfig{APIBase: srv.URL, Model: "m", Stream: true, TimeoutSec: 1})

	start := time.Now()
	if _, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil, nil); err == nil {
		t.Fatal("expected the stalled stream to fail")
	} else if !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("error = %v, want a stall message", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("abort took %s, want roughly the idle budget", elapsed)
	}
}

// TestReadJSONParsesResponse verifies non-streaming parsing of content, tool
// calls and usage.
func TestReadJSONParsesResponse(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"hi","tool_calls":[{"id":"c1","type":"function","function":{"name":"exec_command","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":42}}`

	client := &Client{}
	resp, err := client.readJSON(strings.NewReader(body), nil)
	if err != nil {
		t.Fatalf("readJSON: %v", err)
	}
	if resp.Content != "hi" {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.Finish != "tool_calls" {
		t.Fatalf("finish = %q", resp.Finish)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "exec_command" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.Usage.TotalTokens != 42 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

// TestReadStreamAssemblesReasoning verifies that streamed reasoning
// (reasoning_content / reasoning) is delivered separately from the visible
// content and never mixed into it.
func TestReadStreamAssemblesReasoning(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","reasoning_content":"Let me "}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"think."}}]}`,
		`data: {"choices":[{"delta":{"reasoning":" More."}}]}`,
		`data: {"choices":[{"delta":{"content":"Answer"}}]}`,
		`data: {"choices":[{"delta":{"content":"!"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	var content, reasoning strings.Builder
	client := &Client{}
	resp, err := client.readStream(strings.NewReader(stream), func(s string) {
		content.WriteString(s)
	}, func(s string) {
		reasoning.WriteString(s)
	})
	if err != nil {
		t.Fatalf("readStream: %v", err)
	}
	if resp.Content != "Answer!" || content.String() != "Answer!" {
		t.Fatalf("content = %q, streamed = %q, want Answer!", resp.Content, content.String())
	}
	const wantReasoning = "Let me think. More."
	if resp.Reasoning != wantReasoning || reasoning.String() != wantReasoning {
		t.Fatalf("reasoning = %q, streamed = %q, want %q", resp.Reasoning, reasoning.String(), wantReasoning)
	}
}

// TestReadJSONParsesReasoning verifies reasoning parsing for non-streaming
// responses, where the provider returns thinking as one block.
func TestReadJSONParsesReasoning(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","reasoning_content":"pondering","content":"done"},"finish_reason":"stop"}]}`

	var reasoning string
	client := &Client{}
	resp, err := client.readJSON(strings.NewReader(body), func(s string) {
		reasoning += s
	})
	if err != nil {
		t.Fatalf("readJSON: %v", err)
	}
	if resp.Content != "done" {
		t.Fatalf("content = %q, want done", resp.Content)
	}
	if resp.Reasoning != "pondering" || reasoning != "pondering" {
		t.Fatalf("reasoning = %q, streamed = %q, want pondering", resp.Reasoning, reasoning)
	}
}

// TestMessageMarshalsReasoningContent verifies the reasoning field survives the
// wire format: it is emitted as reasoning_content when present (so a
// preserve-thinking provider sees it on a later request) and omitted when empty
// (so other providers are unaffected).
func TestMessageMarshalsReasoningContent(t *testing.T) {
	withReasoning, err := json.Marshal(Message{Role: "assistant", Content: "answer", ReasoningContent: "thinking"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(withReasoning), `"reasoning_content":"thinking"`) {
		t.Fatalf("reasoning_content missing: %s", withReasoning)
	}

	without, err := json.Marshal(Message{Role: "assistant", Content: "answer"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(without), "reasoning_content") {
		t.Fatalf("empty reasoning must be omitted: %s", without)
	}
}
