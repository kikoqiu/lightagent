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
// snapshot is consumed in batches, inserted as a whole, each row is drawn with its
// final markup as it is built (no plain text first, no idle pass afterwards), and the
// replay skips the entrance animation.
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
		"MAX_MD_CHARS",                // one gigantic row stays plain text
		"if (replaying) { setSpan(rowText(el), text, renderMD); return; }",
		"#log.replaying .row { animation:none; }",
	} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	if strings.Contains(pageSource(), "function renderHistory") {
		t.Error("the single-frame renderHistory replay should be gone")
	}
	// A snapshot that rebuilds a transcript already on screen holds its rows back
	// until the whole snapshot is in: wiping the log first would drop the view to the
	// top of an empty box, and a long conversation takes a visible while to arrive —
	// the reader would watch the top of the new transcript for all of it and then see
	// it snap to the bottom. The first fill has no old transcript to hold on to, so
	// its batches reach the log as they arrive, and a message sent while the snapshot
	// was arriving stays behind the last replayed row.
	for _, want := range []string{
		"swapInAtEnd = log.childNodes.length > 0;",
		"if (!replayBatch || swapInAtEnd) { return; }",
		"if (swapped) {",
		"var merged = keepRenderedRows();",
		"log.appendChild(merged);",
		"if (swapped && following) { writeBottom(); return; }",
		"if (replayBatch) { replayBatch.appendChild(row); return; }",
		"if (replayBatch && pendingRows[i].el.parentNode === replayBatch) { replayBatch.appendChild(pendingRows[i].el); }",
		// The version of a snapshot is remembered when its header arrives and
		// committed only once the snapshot reached the log: a snapshot that died on
		// the way must not be reported as the transcript the page shows.
		"if (typeof ev.version === 'number') { replayVersion = ev.version; }",
		"if (replayVersion) { historyVersion = replayVersion; replayVersion = 0; }",
	} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	if strings.Contains(functionBody(t, "beginHistory"), "log.innerHTML") {
		t.Error("beginHistory must not wipe the log before the new snapshot is complete")
	}
	// A reconnect that has nothing to report keeps the log instead of rebuilding
	// it: the page reports the transcript version its log was built from and the
	// mirror answers history_same when its own version still matches.
	for _, want := range []string{
		"history_same",
		"function sameHistory",
		"function recordHistoryVersion",
		"historyVersion",
		"if (historyVersion > 0) { url += '?since=' + historyVersion; }",
	} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("the page is missing %q", want)
		}
	}
}

// TestResumeKeepsRenderedRows pins the reconciliation a resumed page runs. A
// phone that stopped its page (goIdle) comes back to a transcript that grew while
// the screen was off, and the snapshot it receives is mostly the rows it already
// shows. Rebuilding them is what the reader feels — every row parsed as markdown
// again and the whole log repainted — so the snapshot is compared with the log,
// row by row, and the rows that match are the rows already rendered: they stay,
// and only the rest of the snapshot is built and inserted after them.
func TestResumeKeepsRenderedRows(t *testing.T) {
	src := pageSource()
	for _, want := range []string{
		// The comparison, and what a row is identified by.
		"function keepRenderedRows()",
		"function rowSignature(row)",
		"function rowKind(row)",
		"function rowStamp(row)",
		"function filesSignature(attachments)",
		"rowSignature(old[same]) === rowSignature(fresh[same])",
		// The head of the snapshot stands in for the rows on screen; the first row
		// that differs ends the comparison, so the snapshot takes over from there.
		"for (var k = 0; k < same; k++) { kept.appendChild(old[k]); }",
		// A queued message's row is the page's own (the snapshot never carries it).
		"if (log.children[i].classList.contains('pending')) { continue; }",
		// A replayed row is already finished when it enters its batch, so a kept row
		// keeps its markup and the dropped copy is simply removed.
		"replayBatch.removeChild(fresh[d]);",
		// The page's own state is not part of a row's identity.
		"var ROW_STATE_CLASSES = ['foldable', 'folded', 'show-actions', 'pending'];",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	// The last replayed thinking row is paced like a live one, so its text has to
	// be drawn before the comparison reads it.
	if end := functionBody(t, "endHistory"); !strings.Contains(end, "setRow(reasoningRow, reasoningText, MARKDOWN);") {
		t.Error("endHistory compares the pending thinking row before drawing it")
	}
}

// TestMarkdownCacheSurvivesReload pins what browser storage buys a reloaded page:
// the markdown conversions the last page made, keyed by a hash of the text they were
// made from, written to one slot on the way out (pagehide/freeze, and the phone's
// grace period) and read back at load. The rows themselves, their order, their text,
// their timestamps and their files still come from the server — the cache answers
// one question only ("this text converts to what?"), so a reloaded page draws its
// first snapshot without parsing markdown and nothing about the transcript is stored.
func TestMarkdownCacheSurvivesReload(t *testing.T) {
	src := pageSource()
	for _, want := range []string{
		"function saveMarkdownCache()",
		"function loadMarkdownCache()",
		"function cachedMarkdown(text)",
		"function mdKey(text)",
		"function mdHash(text)",
		"function storageArea()",
		"window.localStorage", // not IndexedDB: a frozen page cannot wait for a commit
		"var MD_CACHE_KEY = 'lightagent.markdown';",
		"var STORAGE_MAX_BYTES = 10 * 1024 * 1024;",
		// The key is the text's length and its hash, and nothing else: no version to
		// keep in step with a snapshot, a session or a marked upgrade.
		"return 'm' + text.length + ':' + mdHash(text);",
		// What is written down is the conversion: the rendered markup of the row, and
		// the text it was made from.
		"var piece = JSON.stringify(mdKey(displayText(src))) + ':' + JSON.stringify(span.innerHTML);",
		"if (!span || !span.classList.contains('md')) { continue; }",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	// Drawing a row consults the map before parsing anything.
	apply := functionBody(t, "applyMarkdown")
	lookup := strings.Index(apply, "var known = cachedMarkdown(text);")
	fresh := strings.Index(apply, "var fresh = newStreamState();")
	if lookup < 0 || fresh < 0 || lookup > fresh {
		t.Error("applyMarkdown parses markdown without asking the cache first")
	}
	if !strings.Contains(apply[lookup:], "span.innerHTML = known;") {
		t.Error("a cached conversion is not written back into the row")
	}
	// One slot, always replaced; the browser's own quota is answered with a shorter
	// dump of the same prefix, which is what the cap keeps as well.
	save := functionBody(t, "saveMarkdownCache")
	if n := strings.Count(save, "area.setItem("); n != 1 {
		t.Errorf("saveMarkdownCache writes %d slots, want exactly one", n)
	}
	if !strings.Contains(save, "if (bytes + piece.length > STORAGE_MAX_BYTES) { break; }") {
		t.Error("saveMarkdownCache does not bound the entry it writes")
	}
	if !strings.Contains(save, "for (var i = 0; i < log.children.length; i++)") {
		t.Error("the dump must follow the log, so that a capped entry holds the older rows")
	}
	if !strings.Contains(save, "n = n >> 1;") {
		t.Error("a refused write (the browser's quota is the harder limit) must retry shorter")
	}
	if !strings.Contains(save, "if (replaying) { return; }") {
		t.Error("saveMarkdownCache would write a log a snapshot is in the middle of rebuilding")
	}
	// It is written on the way out whatever state the page is in — a page stopped by
	// its grace period keeps drawing until it is frozen.
	idle := functionBody(t, "goIdle")
	if !strings.Contains(idle, "saveMarkdownCache();") {
		t.Error("a page that stops does not write its conversions down")
	}
	saveAt := strings.Index(idle, "saveMarkdownCache();")
	guardAt := strings.Index(idle, "if (stopped) { return; }")
	if guardAt < 0 || saveAt > guardAt {
		t.Error("saveMarkdownCache must run before the stop guard, or a stopped page never saves again")
	}
	// The map is filled once, at load, and dropped once the load is drawn: the page's
	// own conversions stay in the log, so nothing is kept in step with the transcript
	// while it runs.
	if n := strings.Count(src, "mdCache.set("); n != 1 {
		t.Errorf("the conversion map is written from %d places, want only the load", n)
	}
	if !strings.Contains(functionBody(t, "endHistory"), "mdCache.clear();") {
		t.Error("the map is kept after the snapshot it was read for is drawn")
	}
	// A row whose conversion is known gets the markup straight away, in the batch it
	// is built into — no plain text, no later pass (see TestReplayDrawsRowsOnce).
	if strings.Contains(functionBody(t, "setSpan"), "queueMarkdown") {
		t.Error("a replayed row is still drawn as plain text first")
	}
	// The key is the text as it was drawn, not as it arrived (a file read verbatim
	// may carry CRLF, see displayText).
	if !strings.Contains(save, "JSON.stringify(mdKey(displayText(src)))") {
		t.Error("the stored key is not the text the screen rendered")
	}
	// Nothing on screen needs converting leaves the previous entry alone: a page that
	// never connected must not wipe an entry it knows nothing about.
	if !strings.Contains(save, "if (!parts.length) { return; }") {
		t.Error("a page with nothing to convert would clear the entry")
	}
	// It is read at load, before the socket is dialed, and the snapshot path neither
	// reads nor writes it: what the server sends is drawn as always.
	if !strings.Contains(src, "\n  loadMarkdownCache();") {
		t.Error("the stored conversions are not read at load")
	}
	if strings.Contains(functionBody(t, "beginHistory"), "mdCache") {
		t.Error("beginHistory must not touch the cache: a snapshot replaces the log, whatever session it belongs to")
	}
	// Only the conversion is kept: no rows, no session, no version.
	for _, unwanted := range []string{"function restoreRow(", "function transcriptRecord(", "restoredRows", "restoredSession"} {
		if strings.Contains(src, unwanted) {
			t.Errorf("the page still restores rows from storage (%q)", unwanted)
		}
	}
}

// TestReplayDrawsRowsOnce pins what a snapshot replay does with the DOM: each row is
// drawn with its final markup while its batch is built, and the batch reaches the log
// in one insert — no plain text waiting for a later pass, and no idle-scheduled
// upgrade queue (so nothing is parsed twice and nothing is rewritten after
// insertion). A reply that is still streaming keeps its own pacing, which is a
// different path (see TestPageStreamsMarkdownIncrementally).
func TestReplayDrawsRowsOnce(t *testing.T) {
	src := pageSource()
	for _, gone := range []string{"queueMarkdown", "upgradeMarkdown", "flushMarkdown", "requestIdleCallback", "mdPending"} {
		if strings.Contains(src, gone) {
			t.Errorf("the page still carries the idle upgrade queue (%q)", gone)
		}
	}
	span := functionBody(t, "setSpan")
	if !strings.Contains(span, "if (renderMD) { applyMarkdown(span, text); return; }") {
		t.Error("a replayed row is not rendered where it is built")
	}
	// A gigantic row is the one exception, and it is decided before anything is
	// parsed.
	apply := functionBody(t, "applyMarkdown")
	guard := strings.Index(apply, "if (replaying && text.length > MAX_MD_CHARS) {")
	lookup := strings.Index(apply, "var known = cachedMarkdown(text);")
	if guard < 0 || lookup < 0 || guard > lookup {
		t.Error("a replayed row past MAX_MD_CHARS must stay plain text, before anything else is tried")
	}
	// The live path keeps its own pacing: a streaming row is redrawn at the interval
	// the device asks for.
	for _, want := range []string{"function renderIntervalMs", "function streamDelay", "RENDER_LIVE_PHONE_MS", "RENDER_HIDDEN_MS"} {
		if !strings.Contains(src, want) {
			t.Errorf("the live pacing is gone (%q)", want)
		}
	}
	// A change of the rows' heights keeps the reader's own row in place.
	for _, want := range []string{
		"function holdView",
		"function topVisibleRow",
		"if (following) { return null; }",
		"if (release) { release(); }",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
}

// TestPageStreamsMarkdownIncrementally pins the incremental streaming renderer: a
// row that only grew re-parses its trailing block alone instead of the whole reply
// (which made one answer cost O(length^2) — marked, math.js, DOMPurify and the
// browser's own parse of the new innerHTML all ran over the entire text on every
// chunk). The prefix is committed once into md-stable, the tail re-renders into
// md-live, and the two containers are display:contents so the blocks lay out and
// their margins collapse as if they were the row's own children.
func TestPageStreamsMarkdownIncrementally(t *testing.T) {
	for _, want := range []string{
		"function applyMarkdown",
		"function streamMarkdown",
		"function advanceStreamBoundary",
		"function isSafeBlockStart",
		"function ensureStreamContainers",
		"function mdRowHTML",
		"var mdStream = new WeakMap();",
		"var LINK_DEF = ", // a link reference definition freezes the boundary
		"className = 'md-stable';",
		"className = 'md-live';",
		"st.stableEl.innerHTML = st.committed;",     // the committed prefix is written once
		"st.liveEl.innerHTML = tailHTML;",           // only the tail is re-parsed per redraw
		"text.slice(0, st.text.length) === st.text", // only an appended tail is incremental
		"if (st && st.text === text) { return; }",   // an unchanged redraw is skipped
		"classList.contains('md')) { return mdRowHTML(span); }",
		"marked.setOptions({ gfm: true, breaks: true })",
		".text.md > .md-stable, .text.md > .md-live { display:contents; }",
	} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	// A block still being typed is never committed (only complete lines are read),
	// nothing inside a code fence is ever split, and a link reference definition
	// stops the split for good.
	adv := functionBody(t, "advanceStreamBoundary")
	for _, want := range []string{
		"text.indexOf('\\n', i)",
		"if (nl === -1) { break; }",
		"fence === ''",
		"st.noSplit = true",
	} {
		if !strings.Contains(adv, want) {
			t.Errorf("advanceStreamBoundary is missing %q", want)
		}
	}
	// A list item, a blockquote continuation or an indented line may still belong
	// to the block above (a loose list, a quoted paragraph), so none may open a
	// boundary.
	safe := functionBody(t, "isSafeBlockStart")
	if !strings.Contains(safe, "c === ' ' || c === '\\t' || c === '>'") {
		t.Error("isSafeBlockStart must reject an indented line and a blockquote as a boundary")
	}
	if !strings.Contains(safe, "[-*+]") || !strings.Contains(safe, "\\d{1,9}[.)]") {
		t.Error("isSafeBlockStart must reject a list marker as a boundary")
	}
}

// attachQueuedClient registers a client whose frames the test reads straight from
// its queue instead of a socket: no writer goroutine runs, so the assertions are
// deterministic. The connection is a pipe that is only ever closed.
func attachQueuedClient(t *testing.T, srv *Server) *client {
	t.Helper()
	server, browser := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = browser.Close() })
	srv.mu.Lock()
	// The id comes from the server the way addClient allocates it, so a test that
	// mixes both paths cannot collide with a registered client.
	srv.nextID++
	c := newClient(srv.nextID, &wsConn{conn: server})
	// A client starts out replaying (its registration queues a snapshot); there is
	// no snapshot here, so the phase ends right away and frames go to the queue.
	c.finishReplay()
	srv.clients[c.id] = c
	srv.mu.Unlock()
	t.Cleanup(c.close)
	return c
}

// queuedFrames copies the frames waiting for a queued client, oldest first.
func queuedFrames(c *client) [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.queue))
	copy(out, c.queue)
	return out
}

// TestStreamedDeltasShareAFrame pins the coalescing of streamed chunks: a fast
// model publishes dozens of reasoning/answer deltas per second and every frame
// costs a socket write plus, on a phone, a radio wake-up, so consecutive chunks
// of one stream travel as a single frame. The scrollback still records every
// chunk exactly as published, so a page connecting later replays the same text.
func TestStreamedDeltasShareAFrame(t *testing.T) {
	srv := newTestServer(t, "")
	c := attachQueuedClient(t, srv)

	for _, chunk := range []string{"let me ", "think about ", "it"} {
		srv.publish(agent.Event{Type: agent.EventReasoningDelta, Text: chunk})
	}
	// The window has not expired, so nothing has been queued yet.
	if frames := queuedFrames(c); len(frames) != 0 {
		t.Fatalf("the merge left the window early: %d frame(s) queued", len(frames))
	}

	// Any other event flushes it first, so the page draws the streamed row exactly
	// where the scrollback recorded it.
	srv.publish(agent.Event{Type: agent.EventAssistant, Text: "done"})
	frames := queuedFrames(c)
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want the merged delta and the answer", len(frames))
	}
	if !strings.Contains(string(frames[0]), `"type":"reasoning_delta"`) ||
		!strings.Contains(string(frames[0]), "let me think about it") {
		t.Fatalf("first frame = %s, want one merged reasoning_delta", frames[0])
	}
	if !strings.Contains(string(frames[1]), `"type":"assistant"`) {
		t.Fatalf("second frame = %s, want the answer", frames[1])
	}

	// A stream that pauses mid-turn still reaches the page: the window closes on
	// its own.
	srv.publish(agent.Event{Type: agent.EventAssistantDelta, Text: "partial"})
	deadline := time.Now().Add(2 * time.Second)
	for len(queuedFrames(c)) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	frames = queuedFrames(c)
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want the chunk of the closed window as well", len(frames))
	}
	if last := string(frames[2]); !strings.Contains(last, `"type":"assistant_delta"`) ||
		!strings.Contains(last, "partial") {
		t.Fatalf("last frame = %s, want the streamed answer chunk", last)
	}

	// One row per stream, carrying every chunk.
	srv.mu.Lock()
	rows := append([]historyMessage(nil), srv.history...)
	srv.mu.Unlock()
	if len(rows) != 2 || rows[0].Role != "reasoning" || rows[0].Content != "let me think about it" ||
		rows[1].Role != "assistant" || rows[1].Content != "done" {
		t.Fatalf("scrollback = %+v, want the streamed thinking and the answer", rows)
	}
}

// TestConnectingMidStreamDoesNotDuplicateChunks pins the hook that keeps a merged
// frame out of a snapshot: the scrollback already carries the chunks (they are
// recorded at publish time), so the snapshot alone describes the streamed row. A
// page that connects in the middle of a stream must therefore not receive the
// pending frame on top of it, or it would draw the same text twice.
func TestConnectingMidStreamDoesNotDuplicateChunks(t *testing.T) {
	srv := newTestServer(t, "")
	c1 := attachQueuedClient(t, srv)

	srv.publish(agent.Event{Type: agent.EventReasoningDelta, Text: "thinking "})
	srv.publish(agent.Event{Type: agent.EventReasoningDelta, Text: "hard"})

	server, browser := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = browser.Close() })
	c2 := srv.addClient(&wsConn{conn: server})
	t.Cleanup(c2.close)

	// The client that was already connected gets the merged chunk...
	frames := queuedFrames(c1)
	if len(frames) != 1 || !strings.Contains(string(frames[0]), "thinking hard") {
		t.Fatalf("connected client frames = %v, want one merged reasoning_delta", frames)
	}
	// ...and nothing of it is left waiting for the client that just connected.
	srv.mu.Lock()
	pending := srv.pendingDelta.Type
	rows := append([]historyMessage(nil), srv.history...)
	srv.mu.Unlock()
	if pending != "" {
		t.Fatalf("pending merge survived the handshake: %s", pending)
	}
	if len(rows) != 1 || rows[0].Content != "thinking hard" {
		t.Fatalf("scrollback = %+v, want the whole streamed thinking", rows)
	}
}

// readHistoryVersion returns the mirror's current transcript version: the value a
// page reports back as ?since=.
func readHistoryVersion(srv *Server) uint64 {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.version
}

// versionedClient is a connection registered the way a reconnecting page does,
// with the transcript version its log was built from.
type versionedClient struct {
	c      *client
	reader *bufio.Reader
}

// dialVersionedClient registers a client for the given transcript version and
// hands back a reader for the frames the mirror sends it.
func dialVersionedClient(t *testing.T, srv *Server, since uint64) versionedClient {
	t.Helper()
	server, browser := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = browser.Close() })
	c := srv.addClientSince(&wsConn{conn: server}, since)
	t.Cleanup(c.close)
	return versionedClient{c: c, reader: bufio.NewReader(browser)}
}

// TestReconnectSkipsAnUnchangedTranscript pins the "nothing new" reply and the
// version that decides it: a page whose log already matches the mirror (it reports
// ?since=) gets a single history_same frame — no header of rows, no batches, no
// terminator — so a phone coming back from the background does not rebuild a long
// conversation. Any change to the rows bumps the version instead, so a page
// holding an older one is still replayed in full.
func TestReconnectSkipsAnUnchangedTranscript(t *testing.T) {
	srv := newTestServer(t, "")
	srv.publish(agent.Event{Type: agent.EventAssistant, Text: "hello"})
	version := readHistoryVersion(srv)
	if version == 0 {
		t.Fatal("a recorded row did not bump the transcript version")
	}

	// The page that reports the version it shows is not replayed...
	upToDate := dialVersionedClient(t, srv, version)
	if _, payload, err := readServerFrame(upToDate.reader); err != nil {
		t.Fatalf("read the nothing-new frame: %v", err)
	} else {
		var same struct {
			Type     string `json:"type"`
			Version  uint64 `json:"version"`
			Markdown bool   `json:"markdown"`
		}
		if err := json.Unmarshal(payload, &same); err != nil {
			t.Fatalf("decode %s: %v", payload, err)
		}
		if same.Type != "history_same" {
			t.Fatalf("frame = %s, want history_same", payload)
		}
		if same.Version != version {
			t.Fatalf("reply version = %d, want %d", same.Version, version)
		}
		// The state that can change without the rows still travels.
		if !same.Markdown {
			t.Fatalf("the reply must carry the switches: %s", payload)
		}
	}
	// Nothing follows it: the page keeps the log it has.
	if frames := queuedFrames(upToDate.c); len(frames) != 0 {
		t.Fatalf("frames queued after history_same = %d, want none", len(frames))
	}
	// It is a live client all the same.
	srv.publish(agent.Event{Type: agent.EventInfo, Text: "still here"})
	if _, next, err := readServerFrame(upToDate.reader); err != nil {
		t.Fatalf("read the live frame: %v", err)
	} else if !strings.Contains(string(next), `"type":"info"`) {
		t.Fatalf("live frame = %s, want the info row", next)
	}

	// ...while a page holding an older version is.
	srv.publish(agent.Event{Type: agent.EventAssistant, Text: "again"})
	if now := readHistoryVersion(srv); now == version {
		t.Fatal("a new row did not bump the transcript version")
	}
	stale := dialVersionedClient(t, srv, version)
	if _, first, err := readServerFrame(stale.reader); err != nil {
		t.Fatalf("read the snapshot header: %v", err)
	} else if !strings.Contains(string(first), `"type":"history_start"`) {
		t.Fatalf("stale page frame = %s, want a full snapshot", first)
	}

	// A thinking row that keeps growing changes the transcript without adding a
	// row, and a cleared log replaces it: neither may pass as "nothing new".
	srv.publish(agent.Event{Type: agent.EventReasoningDelta, Text: "think"})
	thinking := readHistoryVersion(srv)
	srv.publish(agent.Event{Type: agent.EventReasoningDelta, Text: "ing"})
	if readHistoryVersion(srv) == thinking {
		t.Fatal("a growing thinking row did not bump the transcript version")
	}
	srv.clearScrollback()
	if readHistoryVersion(srv) == thinking {
		t.Fatal("clearing the log did not bump the transcript version")
	}

	// A mirror-only row (the page's /help answer, say) joins the transcript too,
	// although it never goes through the agent bus.
	before := readHistoryVersion(srv)
	srv.localInfo("page-only note")
	if readHistoryVersion(srv) == before {
		t.Fatal("a mirror-only row did not bump the transcript version")
	}
}

