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
