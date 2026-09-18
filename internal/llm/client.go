package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	resp := &Response{
		Content:   choice.Message.Content,
		Reasoning: reasoning,
		ToolCalls: choice.Message.ToolCalls,
		Usage:     parsed.Usage,
		Finish:    choice.FinishReason,
	}
	if err := validateResponse(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// IncompleteResponseError reports a completion the client refused to accept
// because the provider did not deliver it whole: a tool call whose arguments
// never closed (or whose function name never arrived), a stream frame cut in
// half, a reply truncated at max_tokens with tool calls pending, or a reply
// stopped by the provider's content filter.
//
// The type is exported (with IsIncompleteResponse) so a caller can single this
// out from transport and API failures: unlike a 401 or a dropped connection, an
// incomplete reply is a condition the model itself could be asked to repair. No
// caller branches on it yet: it is the condition reserved for a recovery path
// that would hand the parse failure back to the model instead of ending the
// turn (see the hook in Agent.runTurn; the policy is not decided).
type IncompleteResponseError struct {
	// Reason is the user-facing description of what was incomplete.
	Reason string
}

// Error implements error.
func (e *IncompleteResponseError) Error() string { return e.Reason }

// IsIncompleteResponse reports whether err is a reply the provider did not
// deliver whole. Nothing recovers from it today; the predicate is the condition
// reserved for that recovery path.
func IsIncompleteResponse(err error) bool {
	var incomplete *IncompleteResponseError
	return errors.As(err, &incomplete)
}

// incompleteResponsef builds the error for a reply the provider did not deliver
// whole.
func incompleteResponsef(format string, args ...any) error {
	return &IncompleteResponseError{Reason: fmt.Sprintf(format, args...)}
}

// validateResponse rejects a completion the provider did not deliver whole. A
// stream can end, with finish_reason "stop" even, after only part of a tool
// call was sent: keeping it would execute (and send back) a call whose
// arguments never fully arrived, and the turn would look like a normal finish.
// The cases that indicate a truncated delivery are therefore turned into an
// error naming what is wrong and which setting can help.
func validateResponse(resp *Response) error {
	if resp.Finish == "content_filter" {
		return incompleteResponsef("the provider stopped the reply with finish_reason=content_filter; the answer is incomplete")
	}
	if resp.Finish == "length" && len(resp.ToolCalls) > 0 {
		return incompleteResponsef(
			"the provider cut the reply off at max_tokens (finish_reason=length) while sending %d tool call(s); the calls may be incomplete; raise openai.max_tokens",
			len(resp.ToolCalls))
	}
	for _, tc := range resp.ToolCalls {
		if tc.Function.Name == "" {
			return incompleteResponsef(
				"incomplete tool call from the provider (id=%q, finish_reason=%q): the stream ended before the function name arrived",
				tc.ID, resp.Finish)
		}
		// An empty argument payload is legal (a tool that takes no
		// parameters); anything else must be a complete JSON value.
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" || json.Valid([]byte(args)) {
			continue
		}
		if resp.Finish == "length" {
			return incompleteResponsef(
				"the provider cut the reply off at max_tokens (finish_reason=length) inside tool call %q: its arguments are incomplete; raise openai.max_tokens",
				tc.Function.Name)
		}
		return incompleteResponsef(
			"incomplete tool call %q from the provider (finish_reason=%q): its arguments are not valid JSON: %s",
			tc.Function.Name, resp.Finish, errorSnippet(args))
	}
	return nil
}

// decodeArguments renders one streamed arguments fragment as JSON text. A JSON
// string (the OpenAI shape, delivered in pieces) contributes its contents, so
// the pieces concatenate into the complete document; a raw JSON value (some
// servers send {"arguments":{}}) contributes its compact literal. An absent or
// null value contributes nothing.
func decodeArguments(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return string(raw)
	}
	return compact.String()
}

// looksLikeJSON reports whether a stream payload starts a JSON value, i.e. it
// was meant to be a chat chunk rather than a heartbeat or vendor marker.
func looksLikeJSON(data string) bool {
	return strings.HasPrefix(data, "{") || strings.HasPrefix(data, "[")
}

// errorSnippet shortens a payload so it stays readable in an error message.
func errorSnippet(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) > max {
		return string(runes[:max]) + "..."
	}
	return s
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
					Name string `json:"name"`
					// OpenAI streams the arguments as JSON text split
					// over several chunks; some servers send the value
					// itself ({"arguments":{}}). Kept raw so both are
					// assembled into the same text (see decodeArguments).
					Arguments json.RawMessage `json:"arguments"`
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
			// Frames that are not JSON at all are keep-alive or
			// vendor-specific payloads and are skipped. A frame that
			// starts like JSON but does not parse is a chunk the
			// provider cut short: skipping it would silently drop
			// content or tool-call fragments, so the stream fails.
			if looksLikeJSON(data) {
				return nil, incompleteResponsef("unparsable SSE frame from the provider: %v (frame: %s)", err, errorSnippet(data))
			}
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
			acc.Function.Arguments += decodeArguments(tc.Function.Arguments)
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
	if err := validateResponse(result); err != nil {
		return nil, err
	}
	return result, nil
}
