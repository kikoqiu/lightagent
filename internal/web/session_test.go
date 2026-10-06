package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"lightagent/internal/agent"
	"lightagent/internal/llm"
	"lightagent/internal/store"
)

// TestSaveAsCommandUsesTheRegisteredSaver covers /saveas from the page: it
// reaches the registered callback with the name and the -f flag, and reports
// the path (or the refusal) on the shared bus.
func TestSaveAsCommandUsesTheRegisteredSaver(t *testing.T) {
	srv := newTestServer(t, "")
	events, cancel := srv.agent.Bus().Subscribe()
	defer cancel()

	var gotName string
	var gotForce bool
	srv.SetSessionSaverAs(func(name string, force bool) (string, error) {
		gotName, gotForce = name, force
		return `C:\tmp\` + name + `.json`, nil
	})
	srv.handleClientMessage([]byte(`{"text":"/saveas -f notes"}`))
	select {
	case ev := <-events:
		if ev.Type != agent.EventInfo || !strings.Contains(ev.Text, "session saved to") {
			t.Fatalf("event = %+v, want a save confirmation", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no feedback for /saveas")
	}
	if gotName != "notes" || !gotForce {
		t.Fatalf("callback got (%q, %v), want (\"notes\", true)", gotName, gotForce)
	}

	// A quoted name with a space stays one argument.
	srv.handleClientMessage([]byte(`{"text":"/saveas \"my notes\""}`))
	select {
	case <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("no feedback for the quoted /saveas")
	}
	if gotName != "my notes" || gotForce {
		t.Fatalf("callback got (%q, %v), want (\"my notes\", false)", gotName, gotForce)
	}

	// Without a name the page is told the usage.
	srv.handleClientMessage([]byte(`{"text":"/saveas"}`))
	select {
	case ev := <-events:
		if ev.Type != agent.EventError || !strings.Contains(ev.Text, "usage") {
			t.Fatalf("event = %+v, want a usage error", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no feedback for a bare /saveas")
	}
}

// TestLoadCommandRebuildsTheScrollback covers /load from the page: the loader
// runs, the page's transcript is replaced with the loaded conversation and the
// feedback is shared on the bus.
func TestLoadCommandRebuildsTheScrollback(t *testing.T) {
	srv := newTestServer(t, "")
	srv.SetSessionLoader(func(name string, force bool) (string, error) {
		srv.agent.Load([]llm.Message{{Role: "user", Content: "loaded:" + name}}, "")
		return name + ".json", nil
	})
	srv.handleClientMessage([]byte(`{"text":"/load alpha"}`))
	// The rebuild runs synchronously inside the command; the "loaded session"
	// info travels the bus and may land just after it, so only the first row is
	// pinned here.
	rows := decodeHistory(t, srv)
	if len(rows) == 0 || rows[0].Role != "user" || rows[0].Content != "loaded:alpha" {
		t.Fatalf("after /load, rows = %+v, want a leading user row", rows)
	}

	// A failing loader is reported as an error.
	srv.SetSessionLoader(func(name string, force bool) (string, error) {
		return "", errors.New("no such session")
	})
	events, cancel := srv.agent.Bus().Subscribe()
	defer cancel()
	srv.handleClientMessage([]byte(`{"text":"/load missing"}`))
	select {
	case ev := <-events:
		if ev.Type != agent.EventError || !strings.Contains(ev.Text, "no such session") {
			t.Fatalf("event = %+v, want the loader failure", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no feedback for a failed /load")
	}
}

// TestListCommandUsesTheLister covers /list from the page: it shows the shared
// listing as a page-only row the browser draws as a grid (not on the terminal's
// bus, and not as padded text a CJK name would misalign).
func TestListCommandUsesTheLister(t *testing.T) {
	srv := newTestServer(t, "")
	stamp := time.Date(2026, 10, 5, 14, 22, 10, 0, time.UTC)
	srv.SetSessionLister(func(n int) ([]store.SessionInfo, error) {
		return []store.SessionInfo{{Name: "session.json", ModTime: stamp, Current: true}}, nil
	})
	events, cancel := srv.agent.Bus().Subscribe()
	defer cancel()

	srv.handleClientMessage([]byte(`{"text":"/list"}`))
	rows := decodeHistory(t, srv)
	if len(rows) == 0 {
		t.Fatal("/list left no page row")
	}
	last := rows[len(rows)-1]
	if last.Role != "sessions" || len(last.Sessions) != 1 {
		t.Fatalf("row = %+v, want a one-entry session table", last)
	}
	if got := last.Sessions[0]; got.Index != 1 || got.Name != "session.json" || !got.Current || got.Modified != "2026-10-05 14:22:10" {
		t.Fatalf("session row = %+v", got)
	}
	select {
	case ev := <-events:
		t.Fatalf("/list leaked onto the agent bus: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestRemoveCommandUsesTheRemover covers /rm from the page.
func TestRemoveCommandUsesTheRemover(t *testing.T) {
	srv := newTestServer(t, "")
	var got string
	srv.SetSessionRemover(func(name string) (string, error) {
		got = name
		return name + ".json", nil
	})
	events, cancel := srv.agent.Bus().Subscribe()
	defer cancel()
	srv.handleClientMessage([]byte(`{"text":"/rm notes"}`))
	select {
	case ev := <-events:
		if ev.Type != agent.EventInfo || !strings.Contains(ev.Text, "removed session") {
			t.Fatalf("event = %+v, want a removal confirmation", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no feedback for /rm")
	}
	if got != "notes" {
		t.Fatalf("remover got %q, want notes", got)
	}
}

// TestSessionsEndpointServesThePickerList covers /api/sessions: the page's load
// picker reads the same listing, with the 1-based index /list prints.
func TestSessionsEndpointServesThePickerList(t *testing.T) {
	srv := newTestServer(t, "")
	stamp := time.Date(2026, 10, 5, 14, 22, 10, 0, time.UTC)
	srv.SetSessionLister(func(n int) ([]store.SessionInfo, error) {
		if n != sessionPickerLimit {
			t.Fatalf("lister asked for %d, want %d", n, sessionPickerLimit)
		}
		return []store.SessionInfo{
			{Name: "session.json", ModTime: stamp, Current: true, Messages: 4},
			{Name: "notes.json", ModTime: stamp.Add(-time.Hour), Messages: 2},
		}, nil
	})

	resp, err := http.Get(baseURL(srv) + "/api/sessions")
	if err != nil {
		t.Fatalf("GET /api/sessions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var got struct {
		Sessions []sessionJSON `json:"sessions"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(got.Sessions) != 2 {
		t.Fatalf("sessions = %+v", got.Sessions)
	}
	if first := got.Sessions[0]; first.Index != 1 || !first.Current || first.Messages != 4 || first.Modified != "2026-10-05 14:22:10" {
		t.Fatalf("first row = %+v", first)
	}
	if got.Sessions[1].Index != 2 {
		t.Fatalf("second row index = %d, want 2", got.Sessions[1].Index)
	}

	// Without a lister the endpoint says so instead of failing opaquely.
	bare := newTestServer(t, "")
	resp2, err := http.Get(baseURL(bare) + "/api/sessions")
	if err != nil {
		t.Fatalf("GET /api/sessions (bare): %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("bare status = %d, want 503", resp2.StatusCode)
	}
}

// TestPageAlignsMarkerRows pins the fix for a multi-line marker row (the /list
// table): the page marks "[info] "/"[error] "/"[interrupted] " rows so their
// continuation lines hang under the label, which is what lines the session
// table's columns up instead of restarting every row at the left edge.
func TestPageAlignsMarkerRows(t *testing.T) {
	for _, want := range []string{
		"addRow('result marked', '', '[info] ' + (ev.text || ''), false)",
		"addRow('interrupted marked', '', '[interrupted] ' + (ev.text || ''), false)",
		"addRow('error marked', '', '[error] ' + (ev.text || ''), false)",
		".result.marked .text {",
		".error.marked .text {",
		".interrupted.marked .text {",
	} {
		if !strings.Contains(pageSource(), want) {
			t.Fatalf("the page does not carry %q", want)
		}
	}
}

// TestListCommandSendsTheTableFrame drives /list over the real socket: the page
// receives a "sessions" frame (the grid it draws) carrying the file name, which
// is what keeps a CJK name aligned.
func TestListCommandSendsTheTableFrame(t *testing.T) {
	srv := newTestServer(t, "")
	stamp := time.Date(2026, 10, 5, 14, 22, 10, 0, time.UTC)
	srv.SetSessionLister(func(n int) ([]store.SessionInfo, error) {
		return []store.SessionInfo{{Name: "会议.json", ModTime: stamp, Current: true}}, nil
	})
	conn, reader := dialWS(t, srv)
	for {
		_, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read the snapshot: %v", err)
		}
		if strings.Contains(string(payload), `"type":"history_end"`) {
			break
		}
	}
	if err := writeMaskedFrame(conn, opText, []byte(`{"text":"/list"}`)); err != nil {
		t.Fatal(err)
	}
	for {
		opcode, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read the /list frame: %v", err)
		}
		if opcode != opText || strings.Contains(string(payload), `"type":"history_`) {
			continue
		}
		if !strings.Contains(string(payload), `"type":"sessions"`) || !strings.Contains(string(payload), "会议.json") {
			t.Fatalf("frame = %s, want the session table", payload)
		}
		break
	}
}

// as a grid (the load picker's rows), so a CJK file name cannot push the
// columns out of line the way padded text can.
func TestPageDrawsTheSessionTable(t *testing.T) {
	for _, want := range []string{
		"function addSessionsRow(sessions)",
		"else if (kind === 'sessions') { addSessionsRow(ev.sessions || []); }",
		"else if (m.role === 'sessions') { render('sessions', { sessions: m.sessions }); }",
		"flag.className = 'session-flag';",
		".row.sessions {",
		".row.sessions .session-list { margin:4px 0 0; display:grid;",
		".row.sessions .session-row { display:contents; }",
		".row.sessions .session-flag {",
	} {
		if !strings.Contains(pageSource(), want) {
			t.Fatalf("the page does not carry %q", want)
		}
	}
}

// TestPageCarriesSessionPanels pins the page side: the three panels and the
// endpoint the load picker reads are present, and the rail rows open a panel
// instead of sending the bare command.
func TestPageCarriesSessionPanels(t *testing.T) {
	page := pageSource()
	for _, want := range []string{
		`id="saveAsModal"`,
		`id="loadModal"`,
		`id="rmModal"`,
		`data-saveas-close`,
		`data-load-close`,
		`data-rm-close`,
		`id="loadList"`,
		"/api/sessions",
		"SESSION_PANELS['/saveas']",
		"SESSION_PANELS['/load']",
		"SESSION_PANELS['/rm']",
		"buildSessionPanels",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("the page does not carry %q", want)
		}
	}
}

// TestHistoryFrameCarriesSessionLine pins that the snapshot header tells the page
// which session the conversation belongs to and when it was last saved, so the
// rail's session line is right as soon as the socket opens.
func TestHistoryFrameCarriesSessionLine(t *testing.T) {
	srv := newTestServer(t, "")
	saved := time.Date(2026, 10, 6, 15, 4, 5, 0, time.UTC)
	srv.SetSessionInfo(func() (string, time.Time) { return "notes.json", saved })
	_, reader := dialWS(t, srv)

	for {
		_, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		s := string(payload)
		if strings.Contains(s, `"type":"history_end"`) {
			t.Fatal("no history_start frame arrived")
		}
		if !strings.Contains(s, `"type":"history_start"`) {
			continue
		}
		if !strings.Contains(s, `"session":"notes.json"`) {
			t.Fatalf("header = %s, want the session name", payload)
		}
		// The moment travels as RFC 3339: the page turns it into a relative label.
		if !strings.Contains(s, `"saved":"2026-10-06T15:04:05Z"`) {
			t.Fatalf("header = %s, want the last-saved time", payload)
		}
		return
	}
}

// TestSessionFrameFollowsSessionChanges pins the live path: a broadcast sends a
// session frame carrying the current file and its last-saved time, which is what
// the rail's session line follows after /save, /saveas and /load. A never-saved
// conversation reports both fields empty, so the rail shows its unsaved state.
func TestSessionFrameFollowsSessionChanges(t *testing.T) {
	srv := newTestServer(t, "")
	saved := time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)
	srv.SetSessionInfo(func() (string, time.Time) { return "notes.json", saved })
	_, reader := dialWS(t, srv)
	drainHistory(t, reader)

	readSessionFrame := func() string {
		t.Helper()
		for {
			_, payload, err := readServerFrame(reader)
			if err != nil {
				t.Fatalf("read session frame: %v", err)
			}
			if strings.Contains(string(payload), `"type":"session"`) {
				return string(payload)
			}
		}
	}

	srv.broadcastSession()
	if frame := readSessionFrame(); !strings.Contains(frame, `"name":"notes.json"`) ||
		!strings.Contains(frame, `"saved":"2026-10-06T09:30:00Z"`) {
		t.Fatalf("session frame = %s, want the name and the last-saved time", frame)
	}

	srv.SetSessionInfo(func() (string, time.Time) { return "", time.Time{} })
	srv.broadcastSession()
	if frame := readSessionFrame(); !strings.Contains(frame, `"name":""`) ||
		!strings.Contains(frame, `"saved":""`) {
		t.Fatalf("session frame = %s, want the unsaved state", frame)
	}
}

