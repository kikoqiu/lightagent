package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// httpTransport implements the MCP Streamable HTTP transport: every JSON-RPC
// message is POSTed to the endpoint, and the response is returned either as a
// JSON body or as an SSE stream. An Mcp-Session-Id returned by the server is
// echoed on subsequent requests.
type httpTransport struct {
	url     string
	headers map[string]string
	client  *http.Client

	mu       sync.Mutex
	session  string
	protocol string
}

func newHTTPTransport(rawURL string, headers map[string]string) *httpTransport {
	return &httpTransport{url: rawURL, headers: headers, client: &http.Client{}}
}

func (t *httpTransport) setProtocolVersion(v string) {
	t.mu.Lock()
	t.protocol = v
	t.mu.Unlock()
}

func (t *httpTransport) roundTrip(ctx context.Context, req *request) (*response, error) {
	return t.post(ctx, req, true)
}

func (t *httpTransport) notify(ctx context.Context, req *request) error {
	_, err := t.post(ctx, req, false)
	return err
}

func (t *httpTransport) post(ctx context.Context, req *request, wantResponse bool) (*response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	for key, value := range t.headers {
		httpReq.Header.Set(key, value)
	}
	t.mu.Lock()
	session, protocol := t.session, t.protocol
	t.mu.Unlock()
	if session != "" {
		httpReq.Header.Set("Mcp-Session-Id", session)
	}
	if protocol != "" {
		httpReq.Header.Set("MCP-Protocol-Version", protocol)
	}

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("mcp: http request: %w", err)
	}
	defer resp.Body.Close()

	if sid := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")); sid != "" {
		t.mu.Lock()
		t.session = sid
		t.mu.Unlock()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("mcp: http %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if !wantResponse {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, nil
	}
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return readSSEResponse(bufio.NewReader(resp.Body), req.ID)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, fmt.Errorf("mcp: http: empty response body")
	}
	var r response
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("mcp: http: decode response: %w", err)
	}
	return &r, nil
}

// close releases the transport. HTTP requests are one-shot, so dropping the
// idle connections is all that is left to do.
func (t *httpTransport) close() error {
	if t.client != nil {
		t.client.CloseIdleConnections()
	}
	return nil
}

// readSSEResponse reads SSE events until it finds the JSON-RPC response whose
// id matches wantID.
func readSSEResponse(r *bufio.Reader, wantID int64) (*response, error) {
	for {
		ev, err := readSSEEvent(r)
		if ev.Data != "" {
			var resp response
			if json.Unmarshal([]byte(ev.Data), &resp) == nil {
				if id, ok := parseMessageID(resp.ID); ok && id == wantID {
					return &resp, nil
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil, fmt.Errorf("mcp: http: sse stream ended without a response")
			}
			return nil, err
		}
	}
}
