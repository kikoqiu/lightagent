package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Helper environment variables: helperModeEnv selects a variant of the helper
// server, helperMarkerEnv names the file the stubborn variant writes when it
// survived the shutdown, and helperChildEnv names the file the detached variant
// reports its lingering child in (so the test can end it).
const (
	helperModeEnv   = "LIGHTAGENT_MCP_HELPER_MODE"
	helperMarkerEnv = "LIGHTAGENT_MCP_MARKER"
	helperChildEnv  = "LIGHTAGENT_MCP_CHILD_PID"
)

// helperMarkerDelay is how long the stubborn helper waits after seeing EOF on
// stdin before proving it is still alive. It has to exceed the transport's
// shutdown grace, so the marker can only appear when the forced termination
// failed.
const helperMarkerDelay = shutdownGrace + 400*time.Millisecond

// lingerDuration is how long the "linger" helper holds its parent's pipes open
// before it leaves on its own.
const lingerDuration = 10 * time.Second

// TestHelperProcess is not a real test: when LIGHTAGENT_MCP_HELPER=1 it acts as
// a minimal MCP server speaking newline-delimited JSON-RPC over stdin/stdout.
// The stdio transport tests spawn it with `-test.run=^TestHelperProcess$`.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("LIGHTAGENT_MCP_HELPER") != "1" {
		return
	}
	switch os.Getenv(helperModeEnv) {
	case "linger":
		// Not a server at all: it only holds the pipes of the process that
		// spawned it open for a while.
		time.Sleep(lingerDuration)
		os.Exit(0)
	case "detached":
		// Leave such a child behind, then behave like a normal server.
		child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		child.Env = append(os.Environ(), "LIGHTAGENT_MCP_HELPER=1", helperModeEnv+"=linger")
		// Inheriting our stdout is what keeps the stream open for the transport.
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(1)
		}
		if path := os.Getenv(helperChildEnv); path != "" {
			_ = os.WriteFile(path, []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		}
	}
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	for {
		line, err := reader.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal([]byte(trimmed), &req) == nil {
				if resp := helperHandle(req.Method, req.ID, req.Params); resp != nil {
					data, _ := json.Marshal(resp)
					_, _ = writer.Write(data)
					_ = writer.WriteByte('\n')
					_ = writer.Flush()
				}
			}
		}
		if err != nil {
			if os.Getenv(helperModeEnv) == "ignore-eof" {
				// A server that ignores the protocol shutdown (EOF on stdin):
				// only the forced termination can end it. The marker it leaves
				// behind when it is still alive proves the difference.
				time.Sleep(helperMarkerDelay)
				if marker := os.Getenv(helperMarkerEnv); marker != "" {
					_ = os.WriteFile(marker, []byte("still running"), 0o644)
				}
			}
			os.Exit(0)
		}
	}
}

func helperHandle(method string, id json.RawMessage, params json.RawMessage) any {
	if len(id) == 0 {
		return nil // notification
	}
	result := func(v any) any {
		return map[string]any{"jsonrpc": "2.0", "id": id, "result": v}
	}
	switch method {
	case "initialize":
		return result(map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "helper-server", "title": "Helper MCP", "version": "1.2.3"},
			"instructions":    "Use echo to repeat a message.",
		})
	case "tools/list":
		return result(map[string]any{
			"tools": []map[string]any{{
				"name":        "echo",
				"description": "Echo the provided message",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"message": map[string]any{"type": "string"}},
					"required":   []string{"message"},
				},
			}},
		})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Name != "echo" {
			return result(map[string]any{
				"content": []map[string]any{{"type": "text", "text": "unknown tool " + p.Name}},
				"isError": true,
			})
		}
		msg, _ := p.Arguments["message"].(string)
		return result(map[string]any{
			"content": []map[string]any{{"type": "text", "text": msg}},
		})
	}
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32601, "message": "method not found: " + method},
	}
}

// helperTransportEnv returns the environment used to spawn the helper process.
func helperTransportEnv() []string {
	return append(os.Environ(), "LIGHTAGENT_MCP_HELPER=1")
}

// newHelperClient starts the helper process and performs the MCP handshake.
func newHelperClient(t *testing.T) *Client {
	t.Helper()
	tr, err := newStdioTransport(os.Args[0], []string{"-test.run=^TestHelperProcess$"}, helperTransportEnv())
	if err != nil {
		t.Fatalf("newStdioTransport: %v", err)
	}
	client := newClient(tr)
	if err := client.initialize(context.Background()); err != nil {
		_ = client.close()
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() { _ = client.close() })
	return client
}

// TestStdioClientEndToEnd validates the stdio transport against the helper.
func TestStdioClientEndToEnd(t *testing.T) {
	client := newHelperClient(t)
	ctx := context.Background()

	info := client.ServerInfo()
	if info.Name != "helper-server" || info.Title != "Helper MCP" || info.Version != "1.2.3" ||
		info.Instructions != "Use echo to repeat a message." {
		t.Fatalf("server info = %+v", info)
	}

	tools, err := client.listTools(ctx)
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" || tools[0].InputSchema == nil {
		t.Fatalf("tools = %+v, want one echo tool with a schema", tools)
	}

	res, err := client.callTool(ctx, "echo", map[string]any{"message": "hello mcp"})
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if got := renderCallResult(res); got != "hello mcp" || res.IsError {
		t.Fatalf("callTool = %q (isError=%v)", got, res.IsError)
	}

	bad, err := client.callTool(ctx, "nope", nil)
	if err != nil {
		t.Fatalf("callTool(nope): %v", err)
	}
	if !bad.IsError {
		t.Fatal("unknown tool should set isError")
	}
}

// TestStdioTransportCloseTerminatesStubbornServer pins the forced half of the
// shutdown: a server that ignores EOF on stdin is force-terminated once the
// grace period passed, and the close itself costs that grace at most.
func TestStdioTransportCloseTerminatesStubbornServer(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survived.txt")
	env := append(os.Environ(),
		"LIGHTAGENT_MCP_HELPER=1",
		helperModeEnv+"=ignore-eof",
		helperMarkerEnv+"="+marker,
	)
	tr, err := newStdioTransport(os.Args[0], []string{"-test.run=^TestHelperProcess$"}, env)
	if err != nil {
		t.Fatalf("newStdioTransport: %v", err)
	}
	client := newClient(tr)
	if err := client.initialize(context.Background()); err != nil {
		_ = client.close()
		t.Fatalf("initialize: %v", err)
	}

	start := time.Now()
	if err := client.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if elapsed, limit := time.Since(start), shutdownGrace+time.Second; elapsed > limit {
		t.Fatalf("close took %v, want at most %v", elapsed, limit)
	}
	// The marker delay runs from the EOF that close delivered, so waiting a
	// little past it catches a server that was not really terminated.
	time.Sleep(helperMarkerDelay - shutdownGrace + 500*time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the server survived the shutdown")
	}
}

// TestStdioTransportCloseIsBoundedWithADetachedChild pins the "never hang" part
// of the shutdown: a server that exits on EOF but leaves a child holding its
// pipes must not stall the close — the pipes are given up once the grace period
// is over instead of waiting for them forever.
func TestStdioTransportCloseIsBoundedWithADetachedChild(t *testing.T) {
	childPIDFile := filepath.Join(t.TempDir(), "child.pid")
	env := append(os.Environ(),
		"LIGHTAGENT_MCP_HELPER=1",
		helperModeEnv+"=detached",
		helperChildEnv+"="+childPIDFile,
	)
	tr, err := newStdioTransport(os.Args[0], []string{"-test.run=^TestHelperProcess$"}, env)
	if err != nil {
		t.Fatalf("newStdioTransport: %v", err)
	}
	client := newClient(tr)
	if err := client.initialize(context.Background()); err != nil {
		_ = client.close()
		t.Fatalf("initialize: %v", err)
	}
	// The lingering child belongs to the helper, not to the transport, so the
	// test ends it.
	t.Cleanup(func() { killPIDFromFile(t, childPIDFile) })

	start := time.Now()
	if err := client.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Noticing the exit costs the grace period at most (the pipes stay open);
	// anything beyond that would mean a stuck shutdown.
	if elapsed, limit := time.Since(start), shutdownGrace+time.Second; elapsed > limit {
		t.Fatalf("close took %v, want at most %v", elapsed, limit)
	}
}

// killPIDFromFile ends the process whose pid the helper reported, so the test
// does not leave it behind (it outlives the transport by design).
func killPIDFromFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// TestManagerCallToolStdio exercises Manager.CallTool over the stdio helper.
func TestManagerCallToolStdio(t *testing.T) {
	client := newHelperClient(t)
	manager := &Manager{conns: map[string]*Client{"local": client}}
	ctx := context.Background()

	text, isError, err := manager.CallTool(ctx, "local", "echo", map[string]any{"message": "hi"})
	if err != nil || isError || text != "hi" {
		t.Fatalf("CallTool = %q, error=%v isError=%v", text, err, isError)
	}
	if _, _, err := manager.CallTool(ctx, "missing", "echo", nil); err == nil {
		t.Fatal("unknown server must fail")
	}
	if names := manager.ServerNames(); len(names) != 1 || names[0] != "local" {
		t.Fatalf("ServerNames = %v", names)
	}
}

// TestSanitizeComponent verifies model-facing identifier normalization.
func TestSanitizeComponent(t *testing.T) {
	cases := map[string]string{
		"GitHub":         "github",
		"my server!":     "my_server",
		"a__b":           "a_b",
		"__weird__":      "weird",
		"caf\u00e9.tool": "caf_tool",
		"":               "unnamed",
		"!!!":            "unnamed",
	}
	for in, want := range cases {
		if got := sanitizeComponent(in); got != want {
			t.Errorf("sanitizeComponent(%q) = %q, want %q", in, got, want)
		}
	}
	if got := toolID("My Server", "Do.Thing"); got != "mcp_my_server_do_thing" {
		t.Errorf("toolID = %q", got)
	}
}

// TestRenderCallResult verifies content flattening.
func TestRenderCallResult(t *testing.T) {
	res := &CallResult{Content: []ContentItem{
		{Type: "text", Text: "line one"},
		{Type: "text", Text: "line two"},
		{Type: "image", MIMEType: "image/png"},
		{Type: "resource", URI: "file:///x"},
	}}
	got := renderCallResult(res)
	for _, want := range []string{"line one", "line two", "image content", "resource"} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderCallResult missing %q: %s", want, got)
		}
	}

	structured := renderCallResult(&CallResult{StructuredContent: map[string]any{"a": 1}})
	if !strings.Contains(structured, `"a":1`) {
		t.Fatalf("structured content not rendered: %s", structured)
	}
	if renderCallResult(nil) != "" {
		t.Fatal("nil result should render empty")
	}
}

// TestLoadEnvFile verifies .env parsing and environment merging.
func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + string(os.PathSeparator) + "server.env"
	blob := "# comment\n\nFOO=bar\nQUOTED=\"a b\"\nSINGLE='c'\n"
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	vars, err := LoadEnvFile(path)
	if err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if vars["FOO"] != "bar" || vars["QUOTED"] != "a b" || vars["SINGLE"] != "c" {
		t.Fatalf("vars = %#v", vars)
	}

	env := buildEnv(map[string]string{"LIGHTAGENT_TEST_KEY": "v"})
	found := false
	for _, entry := range env {
		if entry == "LIGHTAGENT_TEST_KEY=v" {
			found = true
		}
	}
	if !found {
		t.Fatal("buildEnv did not include the extra variable")
	}
}
