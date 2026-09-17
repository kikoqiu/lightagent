package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lightagent/internal/config"
	"lightagent/internal/tools"
)

// fakeMCPServer is a minimal Streamable HTTP MCP endpoint.
type fakeMCPServer struct {
	sessionHeader string
	sawSession    bool
	sseToolsCall  bool // return tools/call as an SSE body
}

func (s *fakeMCPServer) handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
		s.sawSession = true
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if s.sessionHeader != "" {
		w.Header().Set("Mcp-Session-Id", s.sessionHeader)
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := helperHandle(req.Method, req.ID, req.Params)

	if s.sseToolsCall && req.Method == "tools/call" {
		w.Header().Set("Content-Type", "text/event-stream")
		data, _ := json.Marshal(resp)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// TestHTTPTransportClient validates the Streamable HTTP transport, including
// session-id echoing.
func TestHTTPTransportClient(t *testing.T) {
	fake := &fakeMCPServer{sessionHeader: "sess-123"}
	ts := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer ts.Close()

	client := newClient(newHTTPTransport(ts.URL, nil))
	defer client.close()
	ctx := context.Background()

	if err := client.initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	discovered, err := client.listTools(ctx)
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if len(discovered) != 1 || discovered[0].Name != "echo" {
		t.Fatalf("tools = %+v", discovered)
	}
	res, err := client.callTool(ctx, "echo", map[string]any{"message": "over http"})
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if renderCallResult(res) != "over http" {
		t.Fatalf("result = %q", renderCallResult(res))
	}
	if !fake.sawSession {
		t.Fatal("client did not echo the Mcp-Session-Id header")
	}
}

// TestHTTPTransportSSEResponseBody validates decoding a response delivered as
// an SSE body.
func TestHTTPTransportSSEResponseBody(t *testing.T) {
	fake := &fakeMCPServer{sseToolsCall: true}
	ts := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer ts.Close()

	client := newClient(newHTTPTransport(ts.URL, nil))
	defer client.close()
	ctx := context.Background()
	if err := client.initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	res, err := client.callTool(ctx, "echo", map[string]any{"message": "sse body"})
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if renderCallResult(res) != "sse body" {
		t.Fatalf("result = %q", renderCallResult(res))
	}
}

// fakeSSEServer is a minimal legacy HTTP+SSE MCP endpoint.
type fakeSSEServer struct {
	msgs chan string
}

func (s *fakeSSEServer) sseHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "event: endpoint\ndata: /messages\n\n")
	flusher.Flush()

	for {
		select {
		case msg := <-s.msgs:
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *fakeSSEServer) msgHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	if len(req.ID) == 0 {
		return
	}
	data, _ := json.Marshal(helperHandle(req.Method, req.ID, req.Params))
	select {
	case s.msgs <- string(data):
	default:
	}
}

// TestSSETransportClient validates the legacy HTTP+SSE transport.
func TestSSETransportClient(t *testing.T) {
	fake := &fakeSSEServer{msgs: make(chan string, 16)}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", fake.sseHandler)
	mux.HandleFunc("/messages", fake.msgHandler)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx := context.Background()
	tr, err := newSSETransport(ctx, ts.URL+"/sse", nil)
	if err != nil {
		t.Fatalf("newSSETransport: %v", err)
	}
	client := newClient(tr)
	defer client.close()

	if err := client.initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	discovered, err := client.listTools(ctx)
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if len(discovered) != 1 || discovered[0].Name != "echo" {
		t.Fatalf("tools = %+v", discovered)
	}
	res, err := client.callTool(ctx, "echo", map[string]any{"message": "via sse"})
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if renderCallResult(res) != "via sse" {
		t.Fatalf("result = %q", renderCallResult(res))
	}
}

// TestResolveEndpoint verifies relative endpoint resolution.
func TestResolveEndpoint(t *testing.T) {
	if got := resolveEndpoint("http://h/sse", "/messages"); got != "http://h/messages" {
		t.Fatalf("resolveEndpoint = %q", got)
	}
	if got := resolveEndpoint("http://h/sse", "http://other/m"); got != "http://other/m" {
		t.Fatalf("absolute endpoint = %q", got)
	}
}

// MCP server: connect -> register deferred -> search -> unlock -> dynamic_call.
func TestManagerUnlockFlowOverHTTP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc((&fakeMCPServer{}).handler))
	defer ts.Close()

	cfg := config.MCPConfig{
		Enabled: true,
		Servers: map[string]config.MCPServerConfig{
			"fake": {Enabled: true, Type: "http", URL: ts.URL},
		},
	}
	manager, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer manager.Close()
	if len(manager.Tools()) != 1 {
		t.Fatalf("manager tools = %+v", manager.Tools())
	}
	if info, ok := manager.ServerInfo("fake"); !ok || info.Name != "helper-server" || info.Instructions == "" {
		t.Fatalf("manager.ServerInfo = %+v ok=%v", info, ok)
	}

	reg := tools.NewRegistry()
	RegisterTools(reg, manager)

	name := toolID("fake", "echo")
	if !reg.IsDeferred(name) {
		t.Fatalf("tool %q should be deferred", name)
	}
	if len(reg.Definitions()) != 0 {
		t.Fatal("deferred MCP tools must not be declared to the model")
	}

	// Discover.
	search := tools.NewBM25SearchTool(reg, 5, 0.5)
	res := search.Execute(context.Background(), map[string]any{"query": "echo a message"})
	if res.IsError || !strings.Contains(res.ForLLM, name) {
		t.Fatalf("search = %q (error=%v)", res.ForLLM, res.IsError)
	}

	// Locked until unlocked.
	dc := tools.NewDynamicCallTool(reg)
	locked := dc.Execute(context.Background(), map[string]any{"name": name, "arguments": map[string]any{"message": "x"}})
	if !locked.IsError || !strings.Contains(locked.ForLLM, "locked") {
		t.Fatalf("expected locked error, got %q", locked.ForLLM)
	}

	// Unlock then invoke.
	unlock := tools.NewUnlockTool(reg, 2)
	if out := unlock.Execute(context.Background(), map[string]any{"name": name}); out.IsError {
		t.Fatalf("unlock failed: %s", out.ForLLM)
	}
	out := dc.Execute(context.Background(), map[string]any{
		"name":      name,
		"arguments": map[string]any{"message": "hello via dynamic_call"},
	})
	if out.IsError {
		t.Fatalf("dynamic_call failed: %s", out.ForLLM)
	}
	if out.ForLLM != "hello via dynamic_call" {
		t.Fatalf("dynamic_call result = %q", out.ForLLM)
	}
}
