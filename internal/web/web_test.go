package web

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
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
	return indexHTML + "\n" + appCSS + "\n" + appJS + "\n" + configJS + "\n" + authJS + "\n" + ttsJS + "\n" + mathJS
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

// TestMobileChrome pins what the phone-sized layout drops, because the header
// is the tightest spot on the page: the context badge keeps the bare percentage
// (the "ctx" word and the token counts are hidden by CSS, the full tag stays in
// its title) and the way out turns into the icon-only button the gear and the
// read-aloud button already are. The composer asks for a message with a short
// placeholder there too.
func TestMobileChrome(t *testing.T) {
	src := pageSource()
	for _, want := range []string{
		`class="usage-label"`, // the word a phone drops...
		`id="usagePct"`,       // ...so only the percentage is left...
		`id="usageTokens"`,    // ...and the counts go with it
		".usage-label, header .usage .usage-tokens { display:none; }",
		`class="signout-glyph"`, // the icon-only way out...
		`class="signout-label"`, // ...that replaces the word
		".signout .signout-glyph { display:block; }",
		`data-placeholder-short`,           // the composer's short hint
		"setComposerPlaceholder",           // it is swapped by app.js...
		"matchMedia('(max-width: 480px)')", // ...at the stylesheet's phone breakpoint
	} {
		if !strings.Contains(src, want) {
			t.Errorf("phone layout is missing %q", want)
		}
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

// TestFollowLockAndJumpButtons pins the transcript's follow lock and the two
// floating jump buttons. Following is a state of the reader's intent, not a
// distance: an up-gesture (a wheel turning up, a finger dragging the content
// down, a page key) and any upward scroll stop it at once — a pin already queued
// for that frame yields to the gesture — while reaching the exact end of the
// transcript starts it again. "Previous" walks back through the user's own
// messages (and stops the follow), "Latest" returns to the newest row and follows
// again, and that one is hidden while the view follows already.
func TestFollowLockAndJumpButtons(t *testing.T) {
	for _, want := range []string{
		"function followable()",
		"function pinned()",
		"function setFollowing(on)",
		// A pin that is already on its way still yields to a reader's gesture.
		"if (pinQueued || !following) { return; }",
		"      if (!following) { return; }",
		// The probes: wheel, touch drag, page keys, and the scroll direction.
		"log.addEventListener('wheel', function (e) {",
		"if (e.deltaY < 0) { unpinView(); }",
		"log.addEventListener('touchstart', function (e) {",
		"log.addEventListener('touchmove', function (e) {",
		"if (y - touchY > 1) { unpinView(); }",
		"document.addEventListener('keydown', function (e) {",
		"if (isFormField(e.target)) { return; }",
		"log.addEventListener('scroll', onLogScroll, { passive: true });",
		"if (pinned()) { setFollowing(true); }",
		"else if (moved < 0) { setFollowing(false); }",
		// The buttons, and the state that hides the "latest" one.
		"function updateJump()",
		"jumpLatestEl.classList.toggle('off', following);",
		"function prevUserRow()",
		"log.children",
		"var lastJumpRow = null;", // the click after a jump goes one message higher
		"function jumpToPrevMessage()",
		"function jumpToLatest()",
		"if (jumpPrevEl) { jumpPrevEl.onclick = jumpToPrevMessage; }",
		"if (jumpLatestEl) { jumpLatestEl.onclick = jumpToLatest; }",
		"setFollowing(false);", // the jump stops the follow...
		"setFollowing(true);",  // ...and sending / the "latest" button resume it
	} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	for _, want := range []string{
		`id="jumpPrev"`,
		`id="jumpLatest"`,
		`class="jump-btn"`,
		`class="jump-btn off"`, // it starts faded out, before app.js runs
		`class="jump-icon"`,    // the glyph is part of both layouts...
		`class="jump-label"`,   // ...the word is the phone's to drop
		// The transcript and its buttons share the box the rows scroll in.
		".stream { position:relative; display:flex; flex-direction:column; flex:1 1 auto; min-height:0; }",
		"#log:empty ~ .jump { display:none; }",
		// Stacked (up above down), a thin pill at the window's edge, and
		// see-through until a pointer is on it.
		"display:flex; flex-direction:column; align-items:stretch; gap:8px;",
		"right:16px; bottom:20px;",
		"border:1px solid rgba(37,50,74,.6); border-radius:999px;",
		"background:rgba(18,26,38,.4);",
		".jump-btn.off { opacity:0; visibility:hidden; pointer-events:none; }",
		"@media (hover: hover) {",
		"-webkit-tap-highlight-color:transparent;",
		// The glyphs are a bar with a chevron, in both layouts.
		`d="M3.4 3.6h9.2"`,
		`d="M3.4 12.4h9.2"`,
		".jump-btn .jump-icon { flex:0 0 auto; width:15px; height:15px; }",
		// A phone keeps the round glyph buttons (40px, the desktop's opacity) and
		// no tap highlight.
		".jump-btn { width:40px; height:40px; min-width:40px; padding:0; border-radius:50%; }",
		".jump-btn .jump-icon { width:16px; height:16px; }",
		".jump-btn .jump-label { display:none; }",
		".jump-btn:focus, .jump-btn:focus-visible { outline:none; }",
	} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	// The pair reads as one designed control: no bare up/down arrows left, the
	// "latest" button is never shown or hidden abruptly, and a phone does not
	// carry an opacity of its own.
	for _, reject := range []string{
		`d="M8 13V3.4"`, `d="M8 3v9.6"`, "jumpLatestEl.hidden = following;", "rgba(18,26,38,.62)",
	} {
		if strings.Contains(pageSource(), reject) {
			t.Errorf("the page still carries %q", reject)
		}
	}
}

// TestHiddenPageSavesEnergy pins the power behaviour of the page: a desktop keeps
// its mirror and its transcript while it is hidden (it only slows its redraws
// down), a phone gets a grace period before it is stopped, a background page
// redraws once a second, and a visible page is redrawn as soon as text arrives.
// It also pins the reconnect backoff with jitter, so a sleeping phone does not
// wake its radio every 1.5s.
func TestHiddenPageSavesEnergy(t *testing.T) {
	for _, want := range []string{
		"function visible()", // "being looked at" is one predicate
		"document.hidden",
		"document.addEventListener('visibilitychange', onPageStateChange)",
		"window.addEventListener('pagehide', goIdle)", // Safari/bfcache hand-over...
		"window.addEventListener('pageshow', goActive)",
		"window.addEventListener('freeze', goIdle)", // ...and a discarded tab
		"window.addEventListener('resume', goActive)",
		"function goIdle",
		"function goActive",
		"HIDDEN_GRACE_MS_PHONE", // a phone is stopped after a grace period...
		"hiddenStopTimer = setTimeout(goIdle, HIDDEN_GRACE_MS_PHONE);",
		"if (stopped) { return; }", // a stopped page does not dial, a hidden one does
		"stopTurnTimer();",         // the elapsed clock stops with the page
		"data-idle",                // the attribute the stylesheet hangs the pause on
		"[data-idle] header .run .spin { animation-play-state:paused; }",
		"turnTickMs", // whole seconds on a phone...
		"TURN_TICK_MS_PHONE",
		"TURN_TICK_MS_HIDDEN",             // ...and once a second in the background
		"matchMedia('(pointer: coarse)')", // a touch device counts as a phone
		"restartTurnTimer",                // the clock follows the page's state
		"RENDER_LIVE_PHONE_MS",            // a visible phone caps its redraws...
		"RENDER_HIDDEN_MS",                // ...and a background page slows to one per second
		"streamDelay",
		"pinQueued", // one scroll write per animation frame
		"requestAnimationFrame",
		"nextReconnectDelay", // exponential backoff with jitter
		"RECONNECT_MAX_MS",
		"Math.random()",
		"reconnectDelay = 0", // ...cleared by a handshake that succeeds
	} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("the page is missing %q", want)
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
	frames := decodeHistoryFrames(t, srv)
	if len(frames) < 2 {
		t.Fatalf("history frames = %d, want a header and a terminator", len(frames))
	}
	first, last := frames[0], frames[len(frames)-1]
	if first.Type != "history_start" {
		t.Fatalf("frame type = %q, want history_start", first.Type)
	}
	if last.Type != "history_end" {
		t.Fatalf("last frame type = %q, want history_end", last.Type)
	}
	if first.Busy {
		t.Fatal("busy should be false while idle")
	}
}

// historyRow is one row of the history snapshot as the page consumes it.
type historyRow struct {
	Role        string           `json:"role"`
	Content     string           `json:"content"`
	Name        string           `json:"name"`
	Args        string           `json:"args"`
	IsError     bool             `json:"is_error"`
	Attachments []llm.Attachment `json:"attachments"`
}

// historyFrame is one frame of a history snapshot as the page consumes it: the
// header (history_start), a batch of rows (history_rows) or the terminator
// (history_end).
type historyFrame struct {
	Type     string       `json:"type"`
	Messages []historyRow `json:"messages"`
	Tokens   int          `json:"tokens"`
	Window   int          `json:"window"`
	Busy     bool         `json:"busy"`
	Markdown bool         `json:"markdown"`
	Result   bool         `json:"result"`
	Count    int          `json:"count"`
}

// decodeHistoryFrames decodes the frames of the current snapshot, in order.
func decodeHistoryFrames(t *testing.T, srv *Server) []historyFrame {
	t.Helper()
	var frames []historyFrame
	for _, raw := range srv.historyFrames() {
		var frame historyFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatalf("history frame: %v", err)
		}
		frames = append(frames, frame)
	}
	return frames
}

// decodeHistory collects the rows the chunked history frames carry, in order.
func decodeHistory(t *testing.T, srv *Server) []historyRow {
	t.Helper()
	var rows []historyRow
	for _, frame := range decodeHistoryFrames(t, srv) {
		if frame.Type != "history_rows" {
			continue
		}
		rows = append(rows, frame.Messages...)
	}
	return rows
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

// TestSteeringRowOrder pins the page's handling of a message sent while the turn
// is running: it is drawn right away and marked pending, every row the running
// reply produces is inserted *above* it (so the previous round's feedback stays
// first), and the row becomes an ordinary one when the agent sends the message. The
// queued badge counts what is still waiting.
func TestSteeringRowOrder(t *testing.T) {
	for _, want := range []string{
		"function addPendingRow(text)",
		"function settlePendingRow(text, attachments)",
		"addPendingRow(text);",
		"settlePendingRow(ev.text || '', ev.attachments)",
		// The files the settled message carried join its row there.
		"setRowMedia(row, attachments);",
		// Transcript rows are inserted before the pending messages.
		"if (anchor) { log.insertBefore(row, anchor); } else { log.appendChild(row); }",
		"pendingRows = [];",
		// The badge counts the messages this page sent while the turn was running.
		"var queued = 0;",
		"queuedText()",
	} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	for _, want := range []string{".user.pending .role::after", `content:" · pending"`} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("the stylesheet is missing %q", want)
		}
	}
}

// TestHistorySummaryMarksTheTruncationPoint pins the reload path: a resumed
// conversation replays the compressed-context summary as its first row, because
// everything before the cut is gone and the summary is all that is left of it.
func TestHistorySummaryMarksTheTruncationPoint(t *testing.T) {
	srv := newTestServer(t, "")
	srv.agent.Load([]llm.Message{
		{Role: "user", Content: "after the cut"},
		{Role: "assistant", Content: "answer"},
	}, "what happened before")
	srv.seedHistory()

	rows := decodeHistory(t, srv)
	if got := rolesOf(rows); got != "summary,user,assistant" {
		t.Fatalf("roles = %q, want the summary row first", got)
	}
	if rows[0].Content != "what happened before" {
		t.Fatalf("summary row = %+v", rows[0])
	}
	for _, want := range []string{"m.role === 'summary'", "SUMMARY_ROLE", ".summary .role"} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("index page is missing %q", want)
		}
	}
}

// TestScrollbackRecordsTheCompactedSummary pins the live path: a compaction
// records the summary it produced right after the info row that counts it, so a
// page reconnecting later replays the marker at the cut. A compaction that
// produced no summary (the fallback dropped messages without condensing them)
// records the info row alone.
func TestScrollbackRecordsTheCompactedSummary(t *testing.T) {
	srv := newTestServer(t, "")
	bus := srv.agent.Bus()

	bus.Publish(agent.Event{Type: agent.EventAssistant, Text: "early"})
	bus.Publish(agent.Event{
		Type:    agent.EventCompacted,
		Text:    "context compressed: 9 -> 3 messages",
		Summary: "the digest",
	})
	bus.Publish(agent.Event{Type: agent.EventCompacted, Text: "context compressed: 3 -> 2 messages"})
	bus.Publish(agent.Event{Type: agent.EventAssistant, Text: "later"})

	rows := waitForHistory(t, srv, func(rows []historyRow) bool { return len(rows) == 5 })
	if got, want := rolesOf(rows), "assistant,info,summary,info,assistant"; got != want {
		t.Fatalf("roles = %q, want %q", got, want)
	}
	if rows[2].Content != "the digest" {
		t.Fatalf("summary row = %+v", rows[2])
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
	srv.handleCommand("/stop")
	select {
	case ev := <-events:
		if ev.Type != agent.EventInfo || !strings.Contains(ev.Text, "nothing to interrupt") {
			t.Fatalf("event = %+v, want an info about nothing to interrupt", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no feedback for /stop while idle")
	}
}

// dialWS opens a raw WebSocket connection to the mirror and returns the socket
// plus a reader positioned after the handshake headers: it is the client a page
// would be, minus the cookie (the test servers run without a login).
func dialWS(t *testing.T, srv *Server) (net.Conn, *bufio.Reader) {
	t.Helper()
	return dialWSQuery(t, srv, "")
}

// dialWSQuery is dialWS with a query string appended to /ws — the page's
// ?since=<transcript version>.
func dialWSQuery(t *testing.T, srv *Server, query string) (net.Conn, *bufio.Reader) {
	t.Helper()
	addr := "127.0.0.1:" + itoa(srv.Port())

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	req := "GET /ws" + query + " HTTP/1.1\r\n" +
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
	return conn, reader
}

// TestDialWithSinceSkipsTheReplay drives the shortcut through the real handler and
// the page's own dial: the version travels as ?since=, and a matching one means the
// first (and only) frame is history_same instead of the rows.
func TestDialWithSinceSkipsTheReplay(t *testing.T) {
	srv := newTestServer(t, "")
	srv.publish(agent.Event{Type: agent.EventAssistant, Text: "hello"})
	version := readHistoryVersion(srv)

	conn, reader := dialWSQuery(t, srv, fmt.Sprintf("?since=%d", version))
	if _, payload, err := readServerFrame(reader); err != nil {
		t.Fatalf("read the nothing-new frame: %v", err)
	} else if !strings.Contains(string(payload), `"type":"history_same"`) {
		t.Fatalf("frame = %s, want history_same", payload)
	}
	// The connection is a normal live client: a command still works on it.
	if err := writeMaskedFrame(conn, opText, []byte(`{"text":"/help"}`)); err != nil {
		t.Fatal(err)
	}
	for {
		_, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read the /help answer: %v", err)
		}
		if strings.Contains(string(payload), `"type":"info"`) {
			break
		}
		t.Fatalf("frame = %s, want the /help answer", payload)
	}

	// An unparsable value counts as "no version": the page is replayed in full.
	_, stale := dialWSQuery(t, srv, "?since=not-a-number")
	if _, payload, err := readServerFrame(stale); err != nil {
		t.Fatalf("read the snapshot header: %v", err)
	} else if !strings.Contains(string(payload), `"type":"history_start"`) {
		t.Fatalf("frame = %s, want a full snapshot", payload)
	}
}

func TestWebSocketHandshakeAndPing(t *testing.T) {
	srv := newTestServer(t, "")
	conn, reader := dialWS(t, srv)

	// The server pushes the current conversation first: a header frame carrying
	// the context-usage numbers the page needs for its badge, then the
	// terminator (this conversation is empty, so there are no row batches).
	opcode, header, err := readServerFrame(reader)
	if err != nil {
		t.Fatalf("read history header: %v", err)
	}
	if opcode != opText {
		t.Fatalf("first frame opcode = %d, want text", opcode)
	}
	if !strings.Contains(string(header), `"type":"history_start"`) ||
		!strings.Contains(string(header), `"window"`) {
		t.Fatalf("history header = %s", string(header))
	}
	if _, end, err := readServerFrame(reader); err != nil {
		t.Fatalf("read history terminator: %v", err)
	} else if !strings.Contains(string(end), `"type":"history_end"`) {
		t.Fatalf("history terminator = %s", string(end))
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

// TestSlashCommandOverWebSocket drives the browser's own path end to end: the
// page sends {"text":"/help"} and gets the command listing back, with no turn
// started. It is the regression test for "the CLI commands do nothing in the
// page", where such a line was handed to the model instead.
func TestSlashCommandOverWebSocket(t *testing.T) {
	srv := newTestServer(t, "")
	conn, reader := dialWS(t, srv)

	// The handshake is followed by the history snapshot: a header and rows,
	// terminated by history_end. The /help answer below must not be mistaken
	// for one of those frames, so the read loop skips them.
	for {
		_, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read history snapshot: %v", err)
		}
		if !strings.Contains(string(payload), `"type":"history_`) {
			t.Fatalf("frame = %s, want a history frame before the answer", payload)
		}
		if strings.Contains(string(payload), `"type":"history_end"`) {
			break
		}
	}

	if err := writeMaskedFrame(conn, opText, []byte(`{"text":"/help"}`)); err != nil {
		t.Fatal(err)
	}
	for {
		opcode, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read the answer to /help: %v", err)
		}
		if opcode != opText {
			continue
		}
		if strings.Contains(string(payload), `"type":"history_`) {
			// A late snapshot frame (the pump of the previous connection is
			// gone here, but keep the loop honest).
			continue
		}
		if !strings.Contains(string(payload), "/compact") {
			t.Fatalf("frame = %s, want the command listing", payload)
		}
		break
	}
	if srv.agent.Busy() {
		t.Fatal("/help over the socket must not start a turn")
	}
	if len(srv.agent.History()) != 0 {
		t.Fatal("/help over the socket must not reach the model")
	}
}

// TestIndexAndAssets verifies the page wires the vendored markdown libraries and
// its own stylesheet/scripts. The UI is public (it carries no data and the sign-in
// dialog has to load before there is a session), and the runtime switches plus
// the command rail are injected.
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
		"math.js",
		"app.css",
		"app.js",
		"auth.js",
		`id="usage"`,
		`id="loginModal"`,
		"markdown: true",
		// The rail is filled from the injected command list, which the server
		// builds from the shared slash catalogue: primary and folded commands
		// are distinguished there.
		`id="cmds"`,
		`id="cmdToggle"`,
		`"primary":true`,
		`"name":"/compact"`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page does not carry %q: %s", want, page)
		}
	}
	// Every placeholder must be resolved before the page reaches the browser.
	for _, ph := range []string{
		"__LIGHTAGENT_TOKEN__", "__LIGHTAGENT_ASSET_QUERY__",
		"__LIGHTAGENT_MARKDOWN__", "__LIGHTAGENT_RESULT__", "__LIGHTAGENT_COMMANDS__",
	} {
		if strings.Contains(page, ph) {
			t.Fatalf("page still carries the unresolved placeholder %s: %s", ph, page)
		}
	}
	// math.js has to load before app.js: the markdown pipeline app.js installs
	// calls into it (math_test.go checks what it renders).
	mathAt, appAt := strings.Index(page, `src="math.js"`), strings.Index(page, `src="app.js"`)
	if mathAt == -1 || appAt == -1 || mathAt > appAt {
		t.Fatalf("the page must load math.js before app.js: %s", page)
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
	// The rail is drawn by app.js from the injected catalogue, it folds what the
	// catalogue does not mark primary, and a click sends the command over the
	// same socket as the composer.
	for _, want := range []string{"buildCommands", "setCommandsOpen", "row.className = 'folded'", "sendCommand(cmd.name"} {
		if !strings.Contains(string(scriptBody), want) {
			t.Errorf("app.js does not build the command rail (%q is missing)", want)
		}
	}
	// The markdown pipeline lifts formulas out through math.js and puts the
	// rendered MathML back before the result is sanitized.
	for _, want := range []string{"markdownToHTML", "MathTex.protect", "MathTex.inject"} {
		if !strings.Contains(string(scriptBody), want) {
			t.Errorf("app.js does not run the markdown pipeline through math.js (%q is missing)", want)
		}
	}
	// The alignment a formula table asks for has to be added back by hand:
	// DOMPurify's MathML attribute list spells columnalign "columnsalign".
	if !strings.Contains(string(scriptBody), "ADD_ATTR: ['columnalign']") {
		t.Error("app.js does not add columnalign back to the sanitizer")
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

	// The formula renderer is a page script of its own (hand written, not a
	// vendored library), served like the rest of the UI.
	mathResp, err := http.Get(base + "/math.js")
	if err != nil {
		t.Fatal(err)
	}
	mathBody, _ := io.ReadAll(mathResp.Body)
	_ = mathResp.Body.Close()
	if mathResp.StatusCode != http.StatusOK {
		t.Fatalf("/math.js status = %d, want 200 (the UI is public)", mathResp.StatusCode)
	}
	if ct := mathResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Fatalf("/math.js content type = %q", ct)
	}
	for _, want := range []string{"window.MathTex", "protect", "inject", "<math>"} {
		if !strings.Contains(string(mathBody), want) {
			t.Errorf("math.js does not carry %q", want)
		}
	}
	// A display formula is centred and scrolls sideways instead of overflowing
	// the row (see the formulas section of app.css).
	if !strings.Contains(appCSS, ".mtex-block { display:block;") {
		t.Error("app.css does not lay out a display formula")
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
	for _, ph := range []string{
		"__LIGHTAGENT_TOKEN__", "__LIGHTAGENT_ASSET_QUERY__",
		"__LIGHTAGENT_MARKDOWN__", "__LIGHTAGENT_RESULT__", "__LIGHTAGENT_COMMANDS__",
	} {
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

// TestHelpCommandAnswersInThePage pins the fix for "the CLI commands do nothing
// in the browser": /help is answered by the mirror with the shared command table
// instead of being forwarded to the model.
func TestHelpCommandAnswersInThePage(t *testing.T) {
	srv := newTestServer(t, "")
	events, cancel := srv.agent.Bus().Subscribe()
	defer cancel()

	srv.handleClientMessage([]byte(`{"text":"/help"}`))
	if srv.agent.Busy() {
		t.Fatal("/help must not start a turn")
	}
	if len(srv.agent.History()) != 0 {
		t.Fatal("/help must not reach the model")
	}
	rows := decodeHistory(t, srv)
	if len(rows) != 1 || rows[0].Role != "info" {
		t.Fatalf("rows = %+v, want a single info row", rows)
	}
	for _, want := range []string{"/help, /?", "/compact", "/markdown [on|off]", "terminal only: /exit"} {
		if !strings.Contains(rows[0].Content, want) {
			t.Fatalf("help row is missing %q:\n%s", want, rows[0].Content)
		}
	}
	// The listing describes this page, so it stays off the terminal's bus.
	select {
	case ev := <-events:
		t.Fatalf("the page-only /help leaked onto the agent bus: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestUnknownCommandIsRejected pins that a mistyped command is answered instead
// of being sent to the model, exactly like the terminal REPL does.
func TestUnknownCommandIsRejected(t *testing.T) {
	srv := newTestServer(t, "")
	srv.handleClientMessage([]byte(`{"text":"/bogus"}`))
	if srv.agent.Busy() {
		t.Fatal("an unknown command must not start a turn")
	}
	if len(srv.agent.History()) != 0 {
		t.Fatal("an unknown command must not reach the model")
	}
	rows := decodeHistory(t, srv)
	last := rows[len(rows)-1]
	if last.Role != "error" || !strings.Contains(last.Content, "unknown command /bogus") {
		t.Fatalf("row = %+v, want an unknown-command error", last)
	}
}

// TestNewCommandClearsTheMirrorScrollback pins that /new drops the conversation
// and the rows the page was showing with it.
func TestNewCommandClearsTheMirrorScrollback(t *testing.T) {
	srv := newTestServer(t, "")
	bus := srv.agent.Bus()
	bus.Publish(agent.Event{Type: agent.EventUser, Text: "hi"})
	bus.Publish(agent.Event{Type: agent.EventAssistant, Text: "hello"})
	waitForHistory(t, srv, func(rows []historyRow) bool { return len(rows) == 2 })

	srv.handleClientMessage([]byte(`{"text":"/new"}`))
	rows := waitForHistory(t, srv, func(rows []historyRow) bool { return len(rows) == 1 })
	if rows[0].Role != "info" || !strings.Contains(rows[0].Content, "new conversation") {
		t.Fatalf("rows = %+v, want the mirror to forget the old conversation", rows)
	}
	if len(srv.agent.History()) != 0 {
		t.Fatal("the conversation should be empty after /new")
	}
}

// TestMarkdownSwitchIsPageOnly pins that /markdown flips the browser's rendering
// without announcing it on the shared bus: the terminal renders markdown with its
// own switch, so the mirror must not claim a change it did not make.
func TestMarkdownSwitchIsPageOnly(t *testing.T) {
	srv := newTestServer(t, "")
	events, cancel := srv.agent.Bus().Subscribe()
	defer cancel()

	srv.handleClientMessage([]byte(`{"text":"/markdown off"}`))
	if srv.markdownEnabled() {
		t.Fatal("markdown should be off after /markdown off")
	}
	var frame struct {
		Type     string `json:"type"`
		Markdown bool   `json:"markdown"`
		Result   bool   `json:"result"`
	}
	if err := json.Unmarshal(srv.settingsFrame(), &frame); err != nil {
		t.Fatalf("settingsFrame: %v", err)
	}
	if frame.Type != "settings" || frame.Markdown || !frame.Result {
		t.Fatalf("settings frame = %+v", frame)
	}
	rows := decodeHistory(t, srv)
	if last := rows[len(rows)-1]; last.Content != "markdown rendering off (raw output)" {
		t.Fatalf("row = %+v", last)
	}
	select {
	case ev := <-events:
		t.Fatalf("the page-only /markdown leaked onto the agent bus: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestSaveCommandUsesTheRegisteredSaver pins that the browser's /save writes
// through the callback the program registers, and reports failures as an error.
func TestSaveCommandUsesTheRegisteredSaver(t *testing.T) {
	srv := newTestServer(t, "")
	events, cancel := srv.agent.Bus().Subscribe()
	defer cancel()

	srv.SetSessionSaver(func() (string, error) { return `C:\tmp\session.json`, nil })
	srv.handleClientMessage([]byte(`{"text":"/save"}`))
	select {
	case ev := <-events:
		if ev.Type != agent.EventInfo || !strings.Contains(ev.Text, "session saved to") {
			t.Fatalf("event = %+v, want a save confirmation", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no feedback for /save")
	}

	srv.SetSessionSaver(func() (string, error) { return "", errors.New("disk full") })
	srv.handleClientMessage([]byte(`{"text":"/save"}`))
	select {
	case ev := <-events:
		if ev.Type != agent.EventError || !strings.Contains(ev.Text, "disk full") {
			t.Fatalf("event = %+v, want the failure reported", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no feedback for a failed /save")
	}
}

// TestSaveCommandWithoutASaver checks the one-shot-run case: the page is told
// that saving is unavailable instead of failing silently.
func TestSaveCommandWithoutASaver(t *testing.T) {
	srv := newTestServer(t, "")
	events, cancel := srv.agent.Bus().Subscribe()
	defer cancel()

	srv.handleClientMessage([]byte(`{"text":"/save"}`))
	select {
	case ev := <-events:
		if ev.Type != agent.EventError || !strings.Contains(ev.Text, "not available") {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no feedback for /save")
	}
}

// TestSharedCommandsAnswerOnTheBus pins that the commands changing state both
// front-ends share report through the agent bus, so the terminal and every
// browser read the same feedback.
func TestSharedCommandsAnswerOnTheBus(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"/history", "messages, ~"},
		{"/compact", "nothing to compress yet"},
		{"/result off", "hiding tool/exec results"},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			srv := newTestServer(t, "")
			srv.handleClientMessage([]byte(`{"text":"` + tc.text + `"}`))
			rows := waitForHistory(t, srv, func(rows []historyRow) bool { return len(rows) > 0 })
			last := rows[len(rows)-1]
			if last.Role != "info" || !strings.Contains(last.Content, tc.want) {
				t.Fatalf("row = %+v, want an info containing %q", last, tc.want)
			}
			if srv.agent.Busy() {
				t.Fatalf("%s must not start a turn", tc.text)
			}
		})
	}
}

// TestExitCommandStaysPageLocal pins that /exit cannot end the shared session
// from a browser: it explains itself and leaves the agent alone.
func TestExitCommandStaysPageLocal(t *testing.T) {
	srv := newTestServer(t, "")
	events, cancel := srv.agent.Bus().Subscribe()
	defer cancel()

	srv.handleClientMessage([]byte(`{"text":"/exit"}`))
	if srv.agent.Busy() {
		t.Fatal("/exit must not start a turn")
	}
	rows := decodeHistory(t, srv)
	if last := rows[len(rows)-1]; last.Role != "info" || !strings.Contains(last.Content, "close the tab") {
		t.Fatalf("row = %+v, want the terminal-only explanation", last)
	}
	select {
	case ev := <-events:
		t.Fatalf("/exit leaked onto the agent bus: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHistoryFrameCarriesTheSwitches pins that a page reads the current switch
// values from its history snapshot, so a tab reconnecting after another one (or
// the CLI) flipped them is back in sync.
func TestHistoryFrameCarriesTheSwitches(t *testing.T) {
	srv := newTestServerWithMarkdown(t, "", false)
	srv.agent.SetToolResultsVisible(false)

	frames := decodeHistoryFrames(t, srv)
	if len(frames) == 0 {
		t.Fatal("no history frames")
	}
	header := frames[0]
	if header.Markdown || header.Result {
		t.Fatalf("header = %+v, want both switches off", header)
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
