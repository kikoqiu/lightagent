package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lightagent/internal/config"
)

// Client is a minimal OpenAI-compatible chat client.
type Client struct {
	apiBase     string
	apiKey      string
	model       string
	temperature float64
	maxTokens   int
	stream      bool
	extraBody   map[string]any
	// idleTimeout aborts a request whose response stops producing data for too
	// long. It is an inactivity timeout, not a total deadline, so a long
	// generation never fails while a stalled connection does. Zero disables it.
	idleTimeout time.Duration
	http        *http.Client
}

// NewClient builds a client from the OpenAI config block.
func NewClient(cfg config.OpenAIConfig) *Client {
	idle := time.Duration(cfg.TimeoutSec) * time.Second
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Waiting for the response headers is part of the inactivity budget.
		ResponseHeaderTimeout: idle,
	}
	return &Client{
		apiBase:     strings.TrimRight(cfg.APIBase, "/"),
		apiKey:      cfg.APIKey,
		model:       cfg.Model,
		temperature: cfg.Temperature,
		maxTokens:   cfg.MaxTokens,
		stream:      cfg.Stream,
		extraBody:   cfg.ExtraBody,
		idleTimeout: idle,
		// No overall Timeout: a streamed reply can legitimately take minutes.
		// The inactivity guard (see idleGuard) aborts stalled transfers instead.
		http: &http.Client{Transport: transport},
	}
}

// Model returns the configured model name.
func (c *Client) Model() string { return c.model }

// endpoint resolves the chat completions URL.
func (c *Client) endpoint() string {
	base := c.apiBase
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	return base + "/chat/completions"
}

// Chat performs one completion. When onDelta is non-nil and streaming is
// enabled, incremental assistant text is delivered to it as it arrives.
// onReasoning, when non-nil, receives the model's reasoning ("thinking") text:
// providers that expose it (reasoning_content / reasoning) stream it before the
// visible answer, other providers never call the callback.
func (c *Client) Chat(ctx context.Context, messages []Message, tools []ToolDef, onDelta, onReasoning func(string)) (*Response, error) {
	req := ChatRequest{
		Model:       c.model,
		Messages:    messages,
		Tools:       tools,
		Temperature: c.temperature,
		MaxTokens:   c.maxTokens,
	}
	if len(tools) > 0 {
		req.ToolChoice = "auto"
	}
	if c.stream {
		req.Stream = true
	}

	body, err := c.marshalRequest(req)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("api error %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	// Abort the transfer when no data arrives for the configured idle timeout.
	guard := newIdleGuard(resp.Body, c.idleTimeout, cancel)
	defer guard.close()

	if c.stream {
		result, err := c.readStream(guard, onDelta, onReasoning)
		return result, c.stallError(err, guard)
	}
	result, err := c.readJSON(guard, onReasoning)
	return result, c.stallError(err, guard)
}

// stallError turns the transport error caused by the inactivity guard into an
// actionable message naming the setting to raise.
func (c *Client) stallError(err error, guard *idleGuard) error {
	if err == nil || !guard.stalled.Load() {
		return err
	}
	return fmt.Errorf(
		"request stalled: the response sent no data for %s (openai.timeout_seconds=%d); raise it for slow providers",
		c.idleTimeout, int(c.idleTimeout.Seconds()),
	)
}

// idleGuard aborts a request whose response stops producing data: every read
// resets the watchdog, so a long generation never times out while a stalled
// connection does. It is a pass-through reader around the response body.
type idleGuard struct {
	reader  io.Reader
	idle    time.Duration
	cancel  context.CancelFunc
	tick    chan struct{}
	stop    chan struct{}
	stalled atomic.Bool
	once    sync.Once
}

// newIdleGuard wraps body. With idle <= 0 the guard never fires.
func newIdleGuard(body io.Reader, idle time.Duration, cancel context.CancelFunc) *idleGuard {
	g := &idleGuard{
		reader: body,
		idle:   idle,
		cancel: cancel,
		tick:   make(chan struct{}, 1),
		stop:   make(chan struct{}),
	}
	if idle > 0 {
		go g.watch()
	}
	return g
}

// Read forwards to the wrapped reader and reports progress to the watchdog.
func (g *idleGuard) Read(p []byte) (int, error) {
	n, err := g.reader.Read(p)
	if n > 0 {
		select {
		case g.tick <- struct{}{}:
		default:
		}
	}
	return n, err
}

// watch cancels the request once no data has arrived for idle.
func (g *idleGuard) watch() {
	timer := time.NewTimer(g.idle)
	defer timer.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-g.tick:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(g.idle)
		case <-timer.C:
			g.stalled.Store(true)
			g.cancel()
			return
		}
	}
}

// close stops the watchdog (safe to call more than once).
func (g *idleGuard) close() {
	g.once.Do(func() { close(g.stop) })
}

// marshalRequest serializes the chat request and merges the configured
// extra_body keys into the top-level body, so provider-specific parameters can
// be sent (and can override built-in fields such as temperature).
func (c *Client) marshalRequest(req ChatRequest) ([]byte, error) {
	base, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	if len(c.extraBody) == 0 {
		return base, nil
	}
	var body map[string]any
	if err := json.Unmarshal(base, &body); err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	for key, value := range c.extraBody {
		body[key] = value
	}
	merged, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	return merged, nil
}

// readJSON parses a non-streaming chat response. Reasoning is delivered to
// onReasoning as one block: without streaming the provider returns it whole.
func (c *Client) readJSON(r io.Reader, onReasoning func(string)) (*Response, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Message
				// OpenRouter and Ollama name the reasoning field "reasoning";
				// reasoning_content is already covered by Message.
				Reasoning string `json:"reasoning"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
	}
	if err := json.NewDecoder(r).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("empty choices in response")
	}
	choice := parsed.Choices[0]
	reasoning := firstNonEmpty(choice.Message.ReasoningContent, choice.Message.Reasoning)
	if reasoning != "" && onReasoning != nil {
		onReasoning(reasoning)
	}
	return &Response{
		Content:   choice.Message.Content,
		Reasoning: reasoning,
		ToolCalls: choice.Message.ToolCalls,
		Usage:     parsed.Usage,
		Finish:    choice.FinishReason,
	}, nil
}

// firstNonEmpty returns the first non-empty string, or "" when all are empty.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// streamChunk is one SSE delta frame.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			// Providers disagree on the reasoning field name; accept the
			// DeepSeek/Qwen/vLLM style and the OpenRouter/Ollama one.
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

// readStream consumes an SSE stream and assembles the final response.
func (c *Client) readStream(r io.Reader, onDelta, onReasoning func(string)) (*Response, error) {
	result := &Response{}
	// toolAccum preserves insertion order of streamed tool calls by index.
	toolAccum := map[int]*ToolCall{}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Ignore frames we cannot parse; providers occasionally emit
			// keep-alive or vendor-specific payloads.
			continue
		}
		if chunk.Usage != nil {
			result.Usage = *chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			result.Finish = choice.FinishReason
		}
		if reasoning := firstNonEmpty(choice.Delta.ReasoningContent, choice.Delta.Reasoning); reasoning != "" {
			result.Reasoning += reasoning
			if onReasoning != nil {
				onReasoning(reasoning)
			}
		}
		if choice.Delta.Content != "" {
			result.Content += choice.Delta.Content
			if onDelta != nil {
				onDelta(choice.Delta.Content)
			}
		}
		for _, tc := range choice.Delta.ToolCalls {
			acc, ok := toolAccum[tc.Index]
			if !ok {
				acc = &ToolCall{Type: "function"}
				toolAccum[tc.Index] = acc
			}
			if tc.ID != "" {
				acc.ID = tc.ID
			}
			if tc.Type != "" {
				acc.Type = tc.Type
			}
			if tc.Function.Name != "" {
				acc.Function.Name = tc.Function.Name
			}
			acc.Function.Arguments += tc.Function.Arguments
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}

	// Flatten to a stable, index-ordered slice.
	for i := 0; i < len(toolAccum); i++ {
		if tc, ok := toolAccum[i]; ok {
			if tc.Type == "" {
				tc.Type = "function"
			}
			result.ToolCalls = append(result.ToolCalls, *tc)
		}
	}
	return result, nil
}
