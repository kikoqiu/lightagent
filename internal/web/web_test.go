package web

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"lightagent/internal/agent"
	"lightagent/internal/config"
	"lightagent/internal/llm"
	"lightagent/internal/passwd"
	"lightagent/internal/tools"
)

// newTestServer builds a web server bound to a free loopback port.
func newTestServer(t *testing.T, password string) *Server {
	t.Helper()
	return newTestServerWithMarkdown(t, password, true)
}

// newTestServerWithMarkdown builds a web server with an explicit markdown flag.
// A non-empty password installs a login credential with a fresh salt, the way
// the program does at startup.
func newTestServerWithMarkdown(t *testing.T, password string, markdown bool) *Server {
	t.Helper()

	cfg := config.Default()
	client := llm.NewClient(cfg.OpenAI)
	reg := tools.NewRegistry()
	bus := agent.NewBus()
	ag := agent.New(cfg, client, reg, bus)

	// Reserve a free port, then release it for the server to bind.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	srv, err := New(ag, "127.0.0.1", port, markdown)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	if password != "" {
		salt, saltErr := passwd.NewSalt()
		if saltErr != nil {
			t.Fatalf("passwd.NewSalt: %v", saltErr)
		}
		srv.SetPassword(password, salt)
	}
	srv.Start()
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// baseURL is the mirror's origin.
func baseURL(srv *Server) string { return "http://127.0.0.1:" + itoa(srv.Port()) }

// signIn signs a client in: it computes the digest the page would compute and
// keeps the session cookie in a jar.
func signIn(t *testing.T, srv *Server, password string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{Jar: jar}
	body := fmt.Sprintf(`{"digest":%q,"remember":false}`, passwd.Digest(srv.auth.saltValue(), password))
	resp, err := client.Post(baseURL(srv)+"/api/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	return client
}

// signedInClient returns a client with a session for a password-protected
// mirror, so a test reads as if auth were off.
func signedInClient(t *testing.T, srv *Server, password string) *http.Client {
	t.Helper()
	return signIn(t, srv, password)
}

// pageSource concatenates the page's embedded sources so the assertions below do
// not need to know which file carries a given marker now that the HTML, CSS and
// JS live in separate embedded files.
func pageSource() string {
	return indexHTML + "\n" + appCSS + "\n" + appJS + "\n" + configJS + "\n" + authJS
}

// TestPageIsPublic checks the shell is served without a session: the sign-in
// dialog has to be reachable before there is one.
func TestPageIsPublic(t *testing.T) {
	srv := newTestServer(t, "secret")
	resp, err := http.Get(baseURL(srv) + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), `id="loginForm"`) {
		t.Fatalf("the page does not carry the sign-in form: %s", body)
	}
}

// TestMobileLayout guards the responsive mirror layout: the log must flex
// inside the dynamic viewport instead of the old fixed-height guess that let
// the input bar cover content on phones.
func TestMobileLayout(t *testing.T) {
	src := pageSource()
	if !strings.Contains(src, "100dvh") {
		t.Error("index page should size the body with 100dvh")
	}
	if strings.Contains(src, "position:fixed") {
		t.Error("header/footer must not be position:fixed")
	}
	if !strings.Contains(src, "env(safe-area-inset-bottom)") {
		t.Error("footer should respect the safe-area inset")
	}
	if !strings.Contains(src, "resizes-content") {
		t.Error("viewport should opt into interactive-widget=resizes-content")
	}
}

// TestResponsiveAffordances guards the polished mirror chrome: the connection
// status label, the auto-growing composer, disabled-while-offline send button
// and the mobile-safe padding/tap targets must all stay wired.
func TestResponsiveAffordances(t *testing.T) {
	for _, want := range []string{
		`id="status"`,               // connection state label
		"sendEl.disabled",           // send is disabled while disconnected
		"function autoGrow",         // composer grows with the message
		"env(safe-area-inset-top)",  // notch/status-bar padding
		"env(safe-area-inset-left)", // landscape notch padding
		"overscroll-behavior:contain",
		"min-height:44px", // mobile tap targets
		"prefers-reduced-motion",
	} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("index page is missing %q", want)
		}
	}
}

// TestRunningIndicator guards the busy indicator: the page renders it and the
// history frame reports whether a turn is already running. The pill also carries
// the elapsed turn time, which is started with the spinner and cleared by the
// matching turn_done.
func TestRunningIndicator(t *testing.T) {
	for _, want := range []string{
		`id="run"`,       // busy pill in the header
		`id="elapsed"`,   // elapsed turn time next to the spinner
		"setRunning",     // the pill follows user/turn_done events
		"startTurnTimer", // the clock starts with the spinner...
		"stopTurnTimer",  // ...and stops with it
		"elapsedText",    // the CLI's elapsed formatting
		"turn_done",
	} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("index page is missing %q", want)
		}
	}

	srv := newTestServer(t, "")
	var frame struct {
		Type string `json:"type"`
		Busy bool   `json:"busy"`
	}
	if err := json.Unmarshal(srv.historyFrame(), &frame); err != nil {
		t.Fatalf("historyFrame: %v", err)
	}
	if frame.Type != "history" {
		t.Fatalf("frame type = %q, want history", frame.Type)
	}
	if frame.Busy {
		t.Fatal("busy should be false while idle")
	}
}

// historyRow is one row of the history frame as the page consumes it.
type historyRow struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name"`
	Args    string `json:"args"`
	IsError bool   `json:"is_error"`
}

// decodeHistory unmarshals the rows of the current history frame.
func decodeHistory(t *testing.T, srv *Server) []historyRow {
	t.Helper()
	var frame struct {
		Messages []historyRow `json:"messages"`
	}
	if err := json.Unmarshal(srv.historyFrame(), &frame); err != nil {
		t.Fatalf("historyFrame: %v", err)
	}
	return frame.Messages
}

// waitForHistory polls the history frame until want accepts it or the deadline
// elapses; bus events reach the scrollback asynchronously.
func waitForHistory(t *testing.T, srv *Server, want func([]historyRow) bool) []historyRow {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows := decodeHistory(t, srv)
		if want(rows) {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("history never matched; last = %+v", rows)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// rolesOf joins the row roles for readable assertions.
func rolesOf(rows []historyRow) string {
	got := make([]string, 0, len(rows))
	for _, m := range rows {
		got = append(got, m.Role)
	}
	return strings.Join(got, ",")
}

// TestHistoryFrameKeepsToolRows guards the refresh path: the in-memory
// scrollback must rebuild the [tool] call rows and the user-visible results, so
// a page reload no longer drops tool activity.
func TestHistoryFrameKeepsToolRows(t *testing.T) {
	srv := newTestServer(t, "")
	// A command result is stored as its structured JSON contract; an MCP result
	// is stored as plain text and has no user-facing rendering.
	srv.agent.Load([]llm.Message{
		{Role: "user", Content: "run it"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID:       "1",
			Type:     "function",
			Function: llm.ToolCallFunction{Name: "exec_command", Arguments: `{"command":"ls"}`},
		}}},
		{Role: "tool", ToolCallID: "1", Name: "exec_command", Content: `{"status":"completed","exit_code":0,"session_id":null,"output":"file1","truncated":false,"total_lines":1,"total_bytes":5,"elapsed_seconds":0.1,"warning":null}`},
		{Role: "tool", ToolCallID: "2", Name: "mcp_echo", Content: "plain text"},
		{Role: "assistant", Content: "done"},
	}, "")

	// The scrollback is seeded from the agent when the mirror starts; the test
	// re-seeds it after injecting a conversation.
	srv.seedHistory()
	rows := decodeHistory(t, srv)
	if got := rolesOf(rows); got != "user,tool_call,tool_result,assistant" {
		t.Fatalf("roles = %q, want user,tool_call,tool_result,assistant", got)
	}
	if rows[1].Name != "exec_command" || rows[1].Args != `{"command":"ls"}` {
		t.Fatalf("tool_call row = %+v", rows[1])
	}
	if !strings.Contains(rows[2].Content, "Command completed.") || !strings.Contains(rows[2].Content, "file1") {
		t.Fatalf("tool_result row = %q", rows[2].Content)
	}

	// /result off hides tool results the next time the scrollback is seeded.
	srv.agent.SetToolResultsVisible(false)
	srv.seedHistory()
	if got := rolesOf(decodeHistory(t, srv)); got != "user,tool_call,assistant" {
		t.Fatalf("roles with results hidden = %q", got)
	}
}

// TestHistoryFrameKeepsReasoning pins that the mirror's scrollback carries the
// model's thinking: restored from a stored assistant message when the mirror
// starts, and merged from streamed reasoning_delta events while it runs.
func TestHistoryFrameKeepsReasoning(t *testing.T) {
	srv := newTestServer(t, "")
	srv.agent.Load([]llm.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "answer", ReasoningContent: "thought about it"},
	}, "")
	srv.seedHistory()

	rows := decodeHistory(t, srv)
	if got := rolesOf(rows); got != "user,reasoning,assistant" {
		t.Fatalf("roles = %q, want user,reasoning,assistant", got)
	}
	if rows[1].Content != "thought about it" {
		t.Fatalf("reasoning row = %+v", rows[1])
	}

	// Streamed chunks merge into one thinking row, closed by the next
	// non-thinking event.
	bus := srv.agent.Bus()
	bus.Publish(agent.Event{Type: agent.EventReasoningDelta, Text: "let me "})
	bus.Publish(agent.Event{Type: agent.EventReasoningDelta, Text: "think"})
	bus.Publish(agent.Event{Type: agent.EventAssistant, Text: "done"})
	rows = waitForHistory(t, srv, func(rows []historyRow) bool { return len(rows) == 5 })
	if got := rolesOf(rows); got != "user,reasoning,assistant,reasoning,assistant" {
		t.Fatalf("roles = %q", got)
	}
	if rows[3].Content != "let me think" {
		t.Fatalf("streamed reasoning row = %+v, want the merged chunks", rows[3])
	}

	if !strings.Contains(pageSource(), "m.role === 'reasoning'") {
		t.Error("the page should replay reasoning rows from the history frame")
	}
}

// TestReasoningRenderedAsMarkdown pins that the thinking row goes through the
// same markdown path as the visible answer, while the italic fallback stays
// scoped to plain (non-rendered) text so it cannot fight markdown structure.
func TestReasoningRenderedAsMarkdown(t *testing.T) {
	if !strings.Contains(pageSource(), "setRow(reasoningRow, reasoningText, MARKDOWN)") {
		t.Error("the page should render reasoning rows as markdown")
	}
	if !strings.Contains(pageSource(), ".reasoning .text:not(.md)") {
		t.Error("the italic thinking style should not override rendered markdown")
	}
}

// TestScrollbackTracksLiveEvents pins that live events are recorded in the
// mirror's scrollback (thinking, tool rows and markers included), so a browser
// connecting afterwards sees them.
func TestScrollbackTracksLiveEvents(t *testing.T) {
	srv := newTestServer(t, "")
	bus := srv.agent.Bus()

	bus.Publish(agent.Event{Type: agent.EventUser, Text: "hi"})
	bus.Publish(agent.Event{Type: agent.EventReasoningDelta, Text: "think"})
	bus.Publish(agent.Event{Type: agent.EventAssistant, Text: "answer"})
	bus.Publish(agent.Event{Type: agent.EventToolCall, Name: "exec_command", Args: `{"command":"ls"}`})
	bus.Publish(agent.Event{Type: agent.EventToolResult, Name: "exec_command", Text: "Command completed."})
	bus.Publish(agent.Event{Type: agent.EventInfo, Text: "note"})
	bus.Publish(agent.Event{Type: agent.EventError, Text: "boom"})
	bus.Publish(agent.Event{Type: agent.EventInterrupted, Text: "stopped"})

	rows := waitForHistory(t, srv, func(rows []historyRow) bool { return len(rows) == 8 })
	want := "user,reasoning,assistant,tool_call,tool_result,info,error,interrupted"
	if got := rolesOf(rows); got != want {
		t.Fatalf("roles = %q, want %q", got, want)
	}
	if rows[1].Content != "think" || rows[2].Content != "answer" {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestScrollbackSkipsEmptyToolResult pins that an empty, non-error tool result
// produces no row, matching the live view.
func TestScrollbackSkipsEmptyToolResult(t *testing.T) {
	srv := newTestServer(t, "")
	srv.mu.Lock()
	srv.recordLocked(agent.Event{Type: agent.EventToolResult, Name: "x", Text: "  "})
	srv.recordLocked(agent.Event{Type: agent.EventToolResult, Name: "x", IsError: true})
	n := len(srv.history)
	lastErr := srv.history[n-1].IsError
	srv.mu.Unlock()

	if n != 1 || !lastErr {
		t.Fatalf("rows = %d (last is_error=%v), want the single error row", n, lastErr)
	}
}

// TestInterruptAffordances guards the mirror's interrupt path: the page carries a
// Stop button and an interrupted marker, and /stop is consumed by the server.
func TestInterruptAffordances(t *testing.T) {
	for _, want := range []string{`id="stop"`, "stopEl", "interrupted"} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("index page is missing %q", want)
		}
	}

	srv := newTestServer(t, "")
	events, cancel := srv.agent.Bus().Subscribe()
	defer cancel()
	if !srv.handleCommand("/stop") {
		t.Fatal("/stop should be consumed by the mirror")
	}
	select {
	case ev := <-events:
		if ev.Type != agent.EventInfo || !strings.Contains(ev.Text, "nothing to interrupt") {
			t.Fatalf("event = %+v, want an info about nothing to interrupt", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no feedback for /stop while idle")
	}
}

func TestWebSocketHandshakeAndPing(t *testing.T) {
	srv := newTestServer(t, "")
	addr := "127.0.0.1:" + itoa(srv.Port())

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Handshake.
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	req := "GET /ws HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("handshake status = %q, want 101", strings.TrimSpace(status))
	}
	// Drain the remaining response headers.
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}

	// The server pushes the current conversation first (a text frame) with the
	// context-usage numbers the page needs for its header badge.
	opcode, historyPayload, err := readServerFrame(reader)
	if err != nil {
		t.Fatalf("read history frame: %v", err)
	}
	if opcode != opText {
		t.Fatalf("first frame opcode = %d, want text", opcode)
	}
	if !strings.Contains(string(historyPayload), `"type":"history"`) ||
		!strings.Contains(string(historyPayload), `"window"`) {
		t.Fatalf("history frame = %s", string(historyPayload))
	}

	// A masked ping should be answered with a pong.
	if err := writeMaskedFrame(conn, opPing, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	opcode, payload, err := readServerFrame(reader)
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if opcode != opPong {
		t.Fatalf("opcode = %d, want pong", opcode)
	}
	if string(payload) != "hi" {
		t.Fatalf("pong payload = %q", string(payload))
	}
}

// TestIndexAndAssets verifies the page wires the vendored markdown libraries and
// its own stylesheet/scripts. The UI is public (it carries no data and the sign-in
// dialog has to load before there is a session), and only the markdown flag is
// injected.
func TestIndexAndAssets(t *testing.T) {
	srv := newTestServer(t, "secret")
	base := baseURL(srv)

	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page status = %d, want 200 (the UI is public)", resp.StatusCode)
	}
	page := string(body)
	for _, want := range []string{
		"assets/marked.min.js",
		"assets/dompurify.min.js",
		"app.css",
		"app.js",
		"auth.js",
		`id="usage"`,
		`id="loginModal"`,
		"markdown: true",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page does not carry %q: %s", want, page)
		}
	}
	// Every placeholder must be resolved before the page reaches the browser.
	for _, ph := range []string{"__LIGHTAGENT_TOKEN__", "__LIGHTAGENT_ASSET_QUERY__", "__LIGHTAGENT_MARKDOWN__"} {
		if strings.Contains(page, ph) {
			t.Fatalf("page still carries the unresolved placeholder %s: %s", ph, page)
		}
	}

	// The page's own stylesheet and script are embedded and served with the
	// right content types.
	cssResp, err := http.Get(base + "/app.css")
	if err != nil {
		t.Fatal(err)
	}
	_ = cssResp.Body.Close()
	if cssResp.StatusCode != http.StatusOK {
		t.Fatalf("app.css status = %d, want 200", cssResp.StatusCode)
	}
	if ct := cssResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("app.css content type = %q", ct)
	}

	scriptResp, err := http.Get(base + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	scriptBody, _ := io.ReadAll(scriptResp.Body)
	_ = scriptResp.Body.Close()
	if scriptResp.StatusCode != http.StatusOK {
		t.Fatalf("app.js status = %d, want 200", scriptResp.StatusCode)
	}
	if ct := scriptResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Fatalf("app.js content type = %q", ct)
	}
	if !strings.Contains(string(scriptBody), "e.ctrlKey") {
		t.Fatal("app.js does not wire Ctrl+Enter to send")
	}

	asset, err := http.Get(base + "/assets/marked.min.js")
	if err != nil {
		t.Fatal(err)
	}
	js, _ := io.ReadAll(asset.Body)
	_ = asset.Body.Close()
	if asset.StatusCode != http.StatusOK {
		t.Fatalf("asset status = %d, want 200", asset.StatusCode)
	}
	if !strings.Contains(string(js), "marked") {
		t.Fatalf("asset body does not look like marked: %q", string(js)[:80])
	}
}

// TestIndexMarkdownOff checks the page falls back to plain text when disabled.
func TestIndexMarkdownOff(t *testing.T) {
	srv := newTestServerWithMarkdown(t, "", false)

	resp, err := http.Get("http://127.0.0.1:" + itoa(srv.Port()) + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	page := string(body)
	if !strings.Contains(page, "markdown: false") {
		t.Fatalf("markdown flag not injected: %s", page)
	}
	if !strings.Contains(page, `assets/marked.min.js"></script>`) {
		t.Fatalf("asset URL should carry no token query: %s", page)
	}
	if !strings.Contains(page, `href="app.css">`) {
		t.Fatalf("stylesheet URL should carry no token query: %s", page)
	}
	// With auth off the token is injected as an empty string, not a placeholder.
	for _, ph := range []string{"__LIGHTAGENT_TOKEN__", "__LIGHTAGENT_ASSET_QUERY__", "__LIGHTAGENT_MARKDOWN__"} {
		if strings.Contains(page, ph) {
			t.Fatalf("page still carries the unresolved placeholder %s: %s", ph, page)
		}
	}
	// /assets/ is public when no password is configured.
	asset, err := http.Get("http://127.0.0.1:" + itoa(srv.Port()) + "/assets/dompurify.min.js")
	if err != nil {
		t.Fatal(err)
	}
	_ = asset.Body.Close()
	if asset.StatusCode != http.StatusOK {
		t.Fatalf("asset status = %d, want 200", asset.StatusCode)
	}
}

// TestResultCommandTogglesVisibility checks the web mirror can drive the
// shared /result switch without starting a turn.
func TestResultCommandTogglesVisibility(t *testing.T) {
	srv := newTestServer(t, "")
	if !srv.agent.ToolResultsVisible() {
		t.Fatal("tool results should default to visible")
	}
	srv.handleClientMessage([]byte(`{"text":"/result off"}`))
	if srv.agent.ToolResultsVisible() {
		t.Fatal("expected tool results hidden after /result off")
	}
	srv.handleClientMessage([]byte(`{"text":"/result on"}`))
	if !srv.agent.ToolResultsVisible() {
		t.Fatal("expected tool results visible after /result on")
	}
}

// TestBindHostAllInterfaces checks the configured bind address is honored:
// binding 0.0.0.0 still serves the page over loopback.
func TestBindHostAllInterfaces(t *testing.T) {
	cfg := config.Default()
	ag := agent.New(cfg, llm.NewClient(cfg.OpenAI), tools.NewRegistry(), agent.NewBus())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	srv, err := New(ag, "0.0.0.0", port, true)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	salt, saltErr := passwd.NewSalt()
	if saltErr != nil {
		t.Fatalf("passwd.NewSalt: %v", saltErr)
	}
	srv.SetPassword("secret", salt)
	srv.Start()
	t.Cleanup(func() { _ = srv.Close() })

	resp, err := http.Get("http://127.0.0.1:" + itoa(srv.Port()) + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// readServerFrame reads one unmasked server frame.
func readServerFrame(r *bufio.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	opcode := header[0] & 0x0f
	length := int64(header[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return opcode, payload, nil
}

// writeMaskedFrame writes a client frame with the mask bit set.
func writeMaskedFrame(w io.Writer, opcode byte, payload []byte) error {
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	header := []byte{0x80 | opcode, 0x80 | byte(len(payload))}
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	if _, err := w.Write(mask[:]); err != nil {
		return err
	}
	_, err := w.Write(masked)
	return err
}

// itoa formats an int without importing strconv at the call sites.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
