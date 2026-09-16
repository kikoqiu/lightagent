package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// errSSEClosed is returned when the legacy SSE transport has been closed.
var errSSEClosed = errors.New("mcp: sse transport closed")

// sseTransport implements the legacy HTTP+SSE MCP transport: a long-lived GET
// event stream advertises a message endpoint (the first "endpoint" event) and
// carries JSON-RPC responses as "message" events, while requests are POSTed to
// that endpoint.
type sseTransport struct {
	url     string
	headers map[string]string
	client  *http.Client

	mu       sync.Mutex
	endpoint string
	pending  map[int64]chan *response
	closed   bool
	readErr  error

	readyOnce sync.Once
	ready     chan struct{}
	done      chan struct{}
	cancel    context.CancelFunc
}

func newSSETransport(ctx context.Context, rawURL string, headers map[string]string) (*sseTransport, error) {
	streamCtx, cancel := context.WithCancel(context.Background())
	t := &sseTransport{
		url:     rawURL,
		headers: headers,
		client:  &http.Client{},
		pending: make(map[int64]chan *response),
		ready:   make(chan struct{}),
		done:    make(chan struct{}),
		cancel:  cancel,
	}

	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	for key, value := range headers {
		httpReq.Header.Set(key, value)
	}
	resp, err := t.client.Do(httpReq)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mcp: sse connect: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("mcp: sse connect: %s", resp.Status)
	}
	go t.readLoop(resp.Body)

	select {
	case <-t.ready:
		return t, nil
	case <-t.done:
		return nil, t.streamFailure()
	case <-ctx.Done():
		_ = t.close()
		return nil, ctx.Err()
	}
}

func (t *sseTransport) readLoop(body io.ReadCloser) {
	defer close(t.done)
	defer body.Close()
	reader := bufio.NewReader(body)
	for {
		ev, err := readSSEEvent(reader)
		if ev.Event != "" || ev.Data != "" {
			t.handleEvent(ev)
		}
		if err != nil {
			t.finish(err)
			return
		}
	}
}

func (t *sseTransport) handleEvent(ev sseEvent) {
	switch ev.Event {
	case "endpoint":
		endpoint := resolveEndpoint(t.url, ev.Data)
		t.mu.Lock()
		if t.endpoint == "" {
			t.endpoint = endpoint
		}
		t.mu.Unlock()
		t.readyOnce.Do(func() { close(t.ready) })
	case "message", "":
		if ev.Data == "" {
			return
		}
		var resp response
		if json.Unmarshal([]byte(ev.Data), &resp) != nil {
			return
		}
		t.deliver(&resp)
	}
}

func (t *sseTransport) deliver(resp *response) {
	id, ok := parseMessageID(resp.ID)
	if !ok {
		return
	}
	t.mu.Lock()
	ch := t.pending[id]
	delete(t.pending, id)
	t.mu.Unlock()
	if ch != nil {
		ch <- resp
	}
}

func (t *sseTransport) finish(err error) {
	t.mu.Lock()
	if t.readErr == nil {
		t.readErr = err
	}
	t.pending = make(map[int64]chan *response)
	t.mu.Unlock()
	t.readyOnce.Do(func() { close(t.ready) })
}

func (t *sseTransport) streamFailure() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.readErr != nil && !errors.Is(t.readErr, io.EOF) {
		return fmt.Errorf("mcp: sse stream closed: %w", t.readErr)
	}
	return errors.New("mcp: sse stream closed")
}

func (t *sseTransport) roundTrip(ctx context.Context, req *request) (*response, error) {
	ch := make(chan *response, 1)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, errSSEClosed
	}
	t.pending[req.ID] = ch
	endpoint := t.endpoint
	t.mu.Unlock()

	if err := t.post(ctx, endpoint, req); err != nil {
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp == nil {
			return nil, t.streamFailure()
		}
		return resp, nil
	case <-ctx.Done():
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
		return nil, ctx.Err()
	case <-t.done:
		return nil, t.streamFailure()
	}
}

func (t *sseTransport) notify(ctx context.Context, req *request) error {
	t.mu.Lock()
	endpoint := t.endpoint
	t.mu.Unlock()
	return t.post(ctx, endpoint, req)
}

func (t *sseTransport) post(ctx context.Context, endpoint string, req *request) error {
	if strings.TrimSpace(endpoint) == "" {
		return errors.New("mcp: sse endpoint is not known yet")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for key, value := range t.headers {
		httpReq.Header.Set(key, value)
	}
	resp, err := t.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("mcp: sse post: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mcp: sse post: %s", resp.Status)
	}
	return nil
}

func (t *sseTransport) close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	t.cancel()
	return nil
}

// resolveEndpoint resolves the endpoint advertised by an SSE event against the
// stream URL.
func resolveEndpoint(base, endpoint string) string {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return endpoint
	}
	if u.IsAbs() {
		return u.String()
	}
	b, err := url.Parse(base)
	if err != nil {
		return endpoint
	}
	return b.ResolveReference(u).String()
}
