package web

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lightagent/internal/agent"
	"lightagent/internal/config"
	"lightagent/internal/llm"
	"lightagent/internal/tools"
)

// restartReply is one /api/restart response as the panel consumes it.
type restartReply struct {
	Restarting bool   `json:"restarting"`
	Saved      string `json:"saved"`
	Error      string `json:"error"`
}

// callRestart performs one POST /api/restart with an unauthenticated client.
func callRestart(t *testing.T, srv *Server) (int, restartReply) {
	t.Helper()
	return callRestartAs(t, srv, http.DefaultClient)
}

// callRestartAs performs one POST /api/restart with a specific client (which may
// carry a session) and decodes the JSON reply.
func callRestartAs(t *testing.T, srv *Server, client *http.Client) (int, restartReply) {
	t.Helper()
	resp, err := client.Post(baseURL(srv)+"/api/restart", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/restart: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	var reply restartReply
	if len(data) > 0 {
		if err := json.Unmarshal(data, &reply); err != nil {
			t.Fatalf("POST /api/restart returned %q: %v", data, err)
		}
	}
	return resp.StatusCode, reply
}

// getPage fetches the served index and returns its body.
func getPage(t *testing.T, srv *Server) string {
	t.Helper()
	resp, err := http.Get(baseURL(srv) + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the page: %v", err)
	}
	return string(body)
}

// newBusyTestServer builds a mirror whose agent is in the middle of a turn: the
// endpoint it talks to never answers, so the turn stays open until the test ends.
func newBusyTestServer(t *testing.T) *Server {
	t.Helper()

	release := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	t.Cleanup(func() {
		// The blocked handler has to finish before the test server can close.
		close(release)
		api.Close()
	})

	cfg := config.Default()
	cfg.LLMs[0].APIBase = api.URL
	ag := agent.New(cfg, llm.NewClient(cfg.LLMs[0].OpenAIConfig), tools.NewRegistry(), agent.NewBus())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	srv, err := New(ag, "127.0.0.1", port, true)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	srv.Start()
	t.Cleanup(func() { _ = srv.Close() })

	ag.SubmitFrom("test", "hello")
	deadline := time.Now().Add(3 * time.Second)
	for !ag.Busy() {
		if time.Now().After(deadline) {
			t.Fatal("the turn never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return srv
}

// TestRestartSavesThenHandsOver covers the endpoint's contract: the conversation
// is written first, the replacement is asked for second, and the reply names the
// file the session went to.
func TestRestartSavesThenHandsOver(t *testing.T) {
	srv := newTestServer(t, "")
	path := filepath.Join(t.TempDir(), "session.json")
	var calls []string
	srv.SetSessionSaver(func() (string, error) {
		calls = append(calls, "save")
		return path, nil
	})
	srv.SetRestarter(func() error {
		calls = append(calls, "restart")
		return nil
	})

	status, reply := callRestart(t, srv)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, reply.Error)
	}
	if !reply.Restarting {
		t.Error("the reply must report the restart")
	}
	if reply.Saved != path {
		t.Errorf("saved = %q, want %q", reply.Saved, path)
	}
	if got := strings.Join(calls, ","); got != "save,restart" {
		t.Errorf("call order = %q, want the session written before the handover", got)
	}
}

// TestRestartReleasesTheListenerAndStillReplies pins the two things the endpoint
// and the handover have to get right together: the reply is written on a
// connection that was accepted long before (so releasing the listener does not
// eat it), and the address is free for the process that replaces this one.
func TestRestartReleasesTheListenerAndStillReplies(t *testing.T) {
	srv := newTestServer(t, "")
	srv.SetSessionSaver(func() (string, error) { return "session.json", nil })
	srv.SetRestarter(func() error { return srv.ReleaseListener() })

	status, reply := callRestart(t, srv)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, reply.Error)
	}
	if !reply.Restarting || reply.Saved != "session.json" {
		t.Fatalf("reply = %+v, want the accepted restart with its file", reply)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+itoa(srv.Port()))
	if err != nil {
		t.Fatalf("the address must be free after ReleaseListener: %v", err)
	}
	_ = ln.Close()
}

// TestReleaseListenerKeepsConnectedPages covers the other half of what a restart
// needs: the address goes, the pages that are already connected stay (their
// sockets were accepted long before), so only a new tab notices the handover.
func TestReleaseListenerKeepsConnectedPages(t *testing.T) {
	srv := newTestServer(t, "")
	_, reader := dialWS(t, srv)
	// The connection is registered with its snapshot: the header and, for an
	// empty conversation, the terminator.
	if _, _, err := readServerFrame(reader); err != nil {
		t.Fatalf("read the snapshot header: %v", err)
	}
	if _, _, err := readServerFrame(reader); err != nil {
		t.Fatalf("read the snapshot terminator: %v", err)
	}

	if err := srv.ReleaseListener(); err != nil {
		t.Fatalf("ReleaseListener: %v", err)
	}
	srv.publish(agent.Event{Type: agent.EventInfo, Text: "handover"})
	for {
		_, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read the live frame: %v", err)
		}
		if strings.Contains(string(payload), "handover") {
			break
		}
	}
}

// TestRestartRefusesWhileATurnRuns: a turn in flight is refused before anything
// happens, because its text is not yet in the conversation a session would keep.
func TestRestartRefusesWhileATurnRuns(t *testing.T) {
	srv := newBusyTestServer(t)
	var touched bool
	srv.SetSessionSaver(func() (string, error) { touched = true; return "", nil })
	srv.SetRestarter(func() error { touched = true; return nil })

	status, reply := callRestart(t, srv)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", status, reply.Error)
	}
	if touched {
		t.Error("a running turn must be refused before the session or the process is touched")
	}
}

// TestRestartRefusedWithoutARestarter covers the embedder: a mirror that cannot
// restart the program says so and saves nothing (there is no next process to
// resume).
func TestRestartRefusedWithoutARestarter(t *testing.T) {
	srv := newTestServer(t, "")
	var saved bool
	srv.SetSessionSaver(func() (string, error) { saved = true; return "", nil })

	status, reply := callRestart(t, srv)
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (%s)", status, reply.Error)
	}
	if saved {
		t.Error("nothing can be resumed without a restarter, so nothing must be saved")
	}
}

// TestRestartRefusedWhenSavingFails keeps the promise the endpoint makes: a
// restart never loses the conversation it was asked to keep, so a failed save is
// reported and the process is left alone.
func TestRestartRefusedWhenSavingFails(t *testing.T) {
	srv := newTestServer(t, "")
	srv.SetSessionSaver(func() (string, error) { return "", io.ErrClosedPipe })
	var restarted bool
	srv.SetRestarter(func() error { restarted = true; return nil })

	status, reply := callRestart(t, srv)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", status, reply.Error)
	}
	if !strings.Contains(reply.Error, "session") {
		t.Errorf("error = %q, want the failed save named", reply.Error)
	}
	if restarted {
		t.Error("a restart must not happen when the session could not be written")
	}
}

// TestRestartReportsARestarterFailure pins the other side of the same promise: a
// replacement that cannot start is answered with the reason, and the run — the
// browser included — continues.
func TestRestartReportsARestarterFailure(t *testing.T) {
	srv := newTestServer(t, "")
	srv.SetSessionSaver(func() (string, error) { return "session.json", nil })
	srv.SetRestarter(func() error { return io.ErrUnexpectedEOF })

	status, reply := callRestart(t, srv)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (%s)", status, reply.Error)
	}
	if !strings.Contains(reply.Error, "restart") {
		t.Errorf("error = %q, want the failed restart named", reply.Error)
	}
}

// TestRestartIsPostOnly pins the method the page uses (a restart changes state,
// so a GET cannot reach it).
func TestRestartIsPostOnly(t *testing.T) {
	srv := newTestServer(t, "")
	srv.SetRestarter(func() error { return nil })

	resp, err := http.Get(baseURL(srv) + "/api/restart")
	if err != nil {
		t.Fatalf("GET /api/restart: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// TestRestartEndpointRequiresAuth keeps the restart behind the session cookie: it
// ends the run, so a page without a session must not reach it.
func TestRestartEndpointRequiresAuth(t *testing.T) {
	srv := newTestServer(t, "secret")
	srv.SetRestarter(func() error { return nil })

	if status, _ := callRestart(t, srv); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a session", status)
	}
	client := signIn(t, srv, "secret")
	srv.SetSessionSaver(func() (string, error) { return "session.json", nil })
	if status, reply := callRestartAs(t, srv, client); status != http.StatusOK {
		t.Fatalf("status = %d, want 200 after signing in (%s)", status, reply.Error)
	}
}

// TestPageOffersRestartWhenTheMirrorCan covers the page side: the control, the
// endpoint it calls and the header hook that reports the restart are wired, and
// the served page tells the panel whether the mirror can restart at all.
func TestPageOffersRestartWhenTheMirrorCan(t *testing.T) {
	for _, want := range []string{
		`id="configRestart"`,       // the control in the config panel
		"/api/restart",             // the endpoint it calls
		"__LIGHTAGENT_RESTART__",   // the server fills the capability in per request
		"restartPending",           // the control asks before it restarts
		"window.MIRROR.restarting", // the header is told to say "restarting…"
		"restarting: function",     // ... which exists in app.js
		"onReconnect",              // the panel is told when the page is back
	} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("the page is missing %q", want)
		}
	}

	srv := newTestServer(t, "")
	if body := getPage(t, srv); !strings.Contains(body, "restart: false") {
		t.Error("a mirror without a restarter must tell the page so")
	}
	srv.SetRestarter(func() error { return nil })
	body := getPage(t, srv)
	if !strings.Contains(body, "restart: true") {
		t.Error("a mirror that can restart must tell the page so")
	}
	if strings.Contains(body, "__LIGHTAGENT_RESTART__") {
		t.Error("the capability placeholder must be replaced before the page is served")
	}
}
