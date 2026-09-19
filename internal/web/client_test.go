package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"lightagent/internal/agent"
)

// seedRows appends n assistant rows to the mirror's scrollback directly: these
// tests are about replaying many rows, not about how they got there.
func seedRows(t *testing.T, srv *Server, n int) {
	t.Helper()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for i := 0; i < n; i++ {
		srv.history = append(srv.history, historyMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("row %d", i),
		})
	}
}

// TestHistoryFramesAreChunked pins the snapshot protocol: a header, one frame
// per batch of rows (bounded by count and by size) and a terminator. One frame
// carrying the whole conversation is what used to freeze the page, because a
// WebSocket message is atomic and cannot be rendered in pieces.
func TestHistoryFramesAreChunked(t *testing.T) {
	srv := newTestServer(t, "")
	const rows = historyBatchRows*2 + 7
	seedRows(t, srv, rows)

	frames := decodeHistoryFrames(t, srv)
	if frames[0].Type != "history_start" {
		t.Fatalf("first frame type = %q, want history_start", frames[0].Type)
	}
	if frames[0].Count != rows {
		t.Fatalf("header count = %d, want %d", frames[0].Count, rows)
	}
	if last := frames[len(frames)-1].Type; last != "history_end" {
		t.Fatalf("last frame type = %q, want history_end", last)
	}
	batches := frames[1 : len(frames)-1]
	if len(batches) != 3 {
		t.Fatalf("batches = %d, want 3", len(batches))
	}
	for i, batch := range batches {
		if batch.Type != "history_rows" {
			t.Fatalf("batch %d type = %q, want history_rows", i, batch.Type)
		}
		if len(batch.Messages) == 0 || len(batch.Messages) > historyBatchRows {
			t.Fatalf("batch %d carries %d rows, want 1..%d", i, len(batch.Messages), historyBatchRows)
		}
	}

	// The rows still arrive whole and in order.
	decoded := decodeHistory(t, srv)
	if len(decoded) != rows {
		t.Fatalf("rows = %d, want %d", len(decoded), rows)
	}
	lastRow := fmt.Sprintf("row %d", rows-1)
	if decoded[0].Content != "row 0" || decoded[len(decoded)-1].Content != lastRow {
		t.Fatalf("rows lost their order: first = %q, last = %q", decoded[0].Content, decoded[len(decoded)-1].Content)
	}
}

// TestHistoryFrameSplitsOversizedRows pins the size cap: a single huge row (a
// large tool result, say) must not be batched with its neighbours, or the frame
// would still be huge.
func TestHistoryFrameSplitsOversizedRows(t *testing.T) {
	srv := newTestServer(t, "")
	srv.mu.Lock()
	srv.history = []historyMessage{
		{Role: "assistant", Content: "small"},
		{Role: "user", Content: "before the big one"},
		{Role: "assistant", Content: strings.Repeat("x", historyBatchBytes+1)},
		{Role: "assistant", Content: "small"},
	}
	srv.mu.Unlock()

	frames := decodeHistoryFrames(t, srv)
	batches := frames[1 : len(frames)-1]
	if len(batches) < 3 {
		t.Fatalf("batches = %d, want the big row split off", len(batches))
	}
	// The big row is alone; the rows around it keep their order.
	bigAt := 0
	for i, batch := range batches {
		for _, m := range batch.Messages {
			if len(m.Content) > historyBatchBytes {
				bigAt = i
				if len(batch.Messages) != 1 {
					t.Fatalf("the oversized row shares batch %d with %d other row(s)", i, len(batch.Messages)-1)
				}
			}
		}
	}
	if bigAt == 0 || bigAt == len(batches)-1 {
		t.Fatalf("the oversized row landed in batch %d of %d, want a batch of its own in the middle", bigAt, len(batches))
	}

	// No frame may hold the whole scrollback: the size cap bounds every batch,
	// which is what keeps an atomic WebSocket message renderable.
	for i, raw := range srv.historyFrames() {
		if len(raw) > historyBatchBytes*2 {
			t.Fatalf("frame %d is %d bytes, want batches bounded by the size cap", i, len(raw))
		}
	}
}

// TestLiveFramesWaitForTheSnapshot pins the queue rule that keeps the replay
// exactly-once: a frame published while the snapshot is still being queued waits
// behind it, because the page wipes the log when the snapshot starts.
func TestLiveFramesWaitForTheSnapshot(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	c := newClient(1, &wsConn{conn: server})

	if !c.enqueue([]byte("live")) {
		t.Fatal("enqueue refused a live frame")
	}
	if !c.enqueueReplay([]byte("start")) || !c.enqueueReplay([]byte("end")) {
		t.Fatal("enqueueReplay refused a snapshot frame")
	}
	c.finishReplay()

	var got []string
	for i := 0; i < 3; i++ {
		data, ok := c.take()
		if !ok {
			t.Fatalf("frame %d is missing", i)
		}
		got = append(got, string(data))
	}
	if join := strings.Join(got, ","); join != "start,end,live" {
		t.Fatalf("frame order = %q, want start,end,live", join)
	}
}

// TestSnapshotPrecedesLiveEvents drives the same ordering through a real
// connection: whatever the agent publishes right after a page connects is
// delivered after the snapshot's terminator.
func TestSnapshotPrecedesLiveEvents(t *testing.T) {
	srv := newTestServer(t, "")
	seedRows(t, srv, historyBatchRows*2+7)

	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	c := srv.addClient(&wsConn{conn: server})
	t.Cleanup(c.close)

	srv.publish(agent.Event{Type: agent.EventUser, Text: "live"})

	reader := bufio.NewReader(client)
	var kinds []string
	for {
		opcode, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read snapshot frame: %v", err)
		}
		if opcode != opText {
			continue
		}
		var frame struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &frame); err != nil {
			t.Fatalf("frame %s: %v", payload, err)
		}
		kinds = append(kinds, frame.Type)
		if frame.Type == "history_end" {
			break
		}
	}
	if len(kinds) < 2 || kinds[0] != "history_start" {
		t.Fatalf("frames = %v, want the header first", kinds)
	}

	opcode, payload, err := readServerFrame(reader)
	if err != nil {
		t.Fatalf("read the live frame: %v", err)
	}
	if opcode != opText || !strings.Contains(string(payload), `"type":"user"`) {
		t.Fatalf("live frame = %s (opcode %d), want the user event after history_end", payload, opcode)
	}
}


// TestStuckClientDoesNotBlockTheMirror pins that a browser which stopped reading
// (a suspended laptop, a paused debugger) cannot stall the mirror: publishing
// and the page's own requests keep working, because the socket is paced by the
// client's own goroutine instead of by the server's lock.
func TestStuckClientDoesNotBlockTheMirror(t *testing.T) {
	srv := newTestServer(t, "")
	seedRows(t, srv, historyBatchRows*3)

	server, client := net.Pipe() // nobody ever reads the browser's end
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	c := srv.addClient(&wsConn{conn: server})
	t.Cleanup(c.close)

	published := make(chan struct{})
	go func() {
		srv.publish(agent.Event{Type: agent.EventInfo, Text: "still here"})
		close(published)
	}()
	select {
	case <-published:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked behind a client that stopped reading")
	}

	// The mirror stays available: the index answers and a fresh page still gets
	// its snapshot.
	httpClient := &http.Client{Timeout: 2 * time.Second}
	resp, err := httpClient.Get(baseURL(srv) + "/")
	if err != nil {
		t.Fatalf("GET / while a client is stuck: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	_, reader := dialWS(t, srv)
	if _, payload, err := readServerFrame(reader); err != nil {
		t.Fatalf("second client snapshot: %v", err)
	} else if !strings.Contains(string(payload), `"type":"history_start"`) {
		t.Fatalf("second client frame = %s, want a history header", payload)
	}
}

// TestWriterDropsADeadSocket pins that a failed write closes the connection
// (its handler's read loop then unregisters the client) instead of leaving a
// frame in a queue nobody drains.
func TestWriterDropsADeadSocket(t *testing.T) {
	srv := newTestServer(t, "")
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	t.Cleanup(func() { _ = client.Close() })
	c := srv.addClient(&wsConn{conn: server})

	// The browser goes away without a close frame.
	_ = client.Close()

	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the writer did not notice the dead socket")
	}
	if c.enqueue([]byte("x")) {
		t.Fatal("enqueue accepted a frame for a closed client")
	}
}


// TestPageReplaysSnapshotsInBatches guards the page side of the fix: the
// snapshot is consumed in batches, inserted as a whole, markdown is upgraded in
// idle slices, and the replay skips the entrance animation.
func TestPageReplaysSnapshotsInBatches(t *testing.T) {
	for _, want := range []string{
		"history_start", // the header starts a replay
		"history_rows",  // row batches
		"history_end",   // the terminator
		"function beginHistory",
		"function appendHistoryRows",
		"function endHistory",
		"function flushReplayBatch",
		"replayBatch.appendChild(row)", // rows are collected in one fragment
		"requestIdleCallback",          // markdown upgrades are sliced
		"queueMarkdown",
		"MAX_MD_CHARS",
		"if (renderMD && replaying)", // replayed rows render plain text first
		"if (replaying) { setSpan(el.lastChild, text, renderMD); return; }",
		"#log.replaying .row { animation:none; }",
	} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	if strings.Contains(pageSource(), "function renderHistory") {
		t.Error("the single-frame renderHistory replay should be gone")
	}
}

