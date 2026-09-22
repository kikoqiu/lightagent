// Package web implements an optional HTTP mirror of the CLI: it exposes the
// same agent over Server-Sent Events and accepts user messages, so a browser
// can observe and drive the same conversation.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"lightagent/internal/agent"
	"lightagent/internal/llm"
	"lightagent/internal/slash"
	"lightagent/internal/tools"
)

// assetsFS holds the vendored browser libraries (marked for markdown, DOMPurify
// for sanitizing the rendered HTML) so the mirror page stays self-contained and
// works without internet access.
//
//go:embed assets
var assetsFS embed.FS

// The mirror UI lives in ordinary files so it can be edited with normal tooling
// (syntax highlighting, formatters, linters) and still ships inside the binary:
// indexHTML is the page skeleton, appCSS its stylesheet and appJS its behaviour.
// Only indexHTML is templated - the server injects the auth token, the asset
// query string and the markdown flag before serving it.
//
//go:embed index.html
var indexHTML string

//go:embed app.css
var appCSS string

//go:embed app.js
var appJS string

// config.js is the config editor (form + raw JSON modes), also embedded as an
// ordinary file so it can be edited with normal tooling. It talks to
// /api/config and is kept separate from app.js, which is only about the
// conversation mirror.
//
//go:embed config.js
var configJS string

// auth.js is the sign-in dialog: it asks /api/session for the public salt,
// digests the password in the browser and keeps the digest when the user asks to
// stay signed in. It is embedded like the other page files.
//
//go:embed auth.js
var authJS string

// tts.js is the read-aloud panel: the agent's output is spoken by the browser's
// own speech synthesis (the Web Speech API, i.e. Chrome's built-in TTS) and its
// switches live in localStorage, so nothing about it reaches the server. It is
// embedded like the other page files.
//
//go:embed tts.js
var ttsJS string

// maxPortTries is how many ports to attempt when the configured one is busy.
const maxPortTries = 50

// Server is the real-time web mirror. It pushes agent events to every
// connected WebSocket client and accepts user messages from any of them, so
// the CLI and one or more browsers can drive the same conversation at once.
type Server struct {
	agent    *agent.Agent
	host     string
	port     int
	listener net.Listener
	srv      *http.Server
	// markdown is the page's markdown switch. It starts at the configured
	// ui.markdown value and the browser's /markdown flips it at runtime; mu
	// guards the field (the terminal has its own switch, see internal/cli).
	markdown bool
	// save writes the conversation to the session file and returns the path. It
	// is registered by the program (SetSessionSaver) and backs /save.
	save func() (string, error)
	// auth holds the login credential and the live sessions; the zero value (no
	// password) means the mirror runs without a login.
	auth *auth
	// configPath is the config.json the /api/config editor reads and rewrites
	// (empty when the mirror runs without one). configPendingRestart records
	// that a document saved through the editor is waiting for the next start.
	configPath           string
	configPendingRestart bool
	// media is the multimedia capability the composer's attach control offers
	// (the types the model accepts) and uploadsDir is where an upload is stored
	// (.lightagent/uploads). The zero value (no types) disables attaching; both
	// are set once by SetMedia before Start.
	media      tools.MediaConfig
	uploadsDir string

	mu       sync.Mutex
	clients  map[int]*client
	nextID   int
	stop     chan struct{}
	stopOnce sync.Once
	// history is the mirror's own in-memory scrollback: the rows the page
	// draws. It is seeded from the agent when the mirror starts and then grown
	// from the event bus (thinking rows and info/error markers included), so a
	// browser that connects later replays the whole conversation.
	history []historyMessage
	// reasoningOpen reports whether the newest row is a thinking row whose
	// stream is still open; further reasoning_delta events append to it.
	reasoningOpen bool
	// version counts the changes of the visible transcript. A page reports the
	// version its log was built from when it reconnects (?since=, see
	// addClientSince); a match means nothing has to be replayed, which is what
	// keeps a phone coming back from the background from rebuilding a long
	// conversation. Every mutation of the rows bumps it, so a skipped replay can
	// never hide a change.
	version uint64
	// pendingDelta/pendingText hold the merged streamed delta waiting for its
	// coalescing window and pendingTimer flushes it once the window expires (see
	// coalesceLocked). All three are guarded by mu.
	pendingDelta agent.Event
	pendingText  string
	pendingTimer *time.Timer
}

// New binds the first available port at or after port (up to +maxPortTries).
// When markdownEnabled is true the mirror page renders replies as markdown in
// the browser (marked + DOMPurify, both embedded). The mirror starts without a
// login; SetPassword adds one.
func New(a *agent.Agent, host string, port int, markdownEnabled bool) (*Server, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		host = "127.0.0.1"
	}
	ln, actual, err := listen(host, port)
	if err != nil {
		return nil, err
	}
	s := &Server{
		agent:    a,
		host:     host,
		port:     actual,
		listener: ln,
		markdown: markdownEnabled,
		auth:     newAuth(),
		clients:  make(map[int]*client),
		stop:     make(chan struct{}),
	}
	s.srv = &http.Server{Handler: s.routes()}
	return s, nil
}

// listen binds host:port, incrementing the port until one is free.
func listen(host string, port int) (net.Listener, int, error) {
	var lastErr error
	for i := 0; i < maxPortTries; i++ {
		candidate := port + i
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, candidate))
		if err == nil {
			return ln, candidate, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf("no free port in [%d, %d]: %w", port, port+maxPortTries-1, lastErr)
}

// Port returns the actual bound port.
func (s *Server) Port() int { return s.port }

// SetSessionSaver registers the callback behind the browser's /save: it writes
// the current conversation to the session file and returns the path it was
// written to. The program wires it to the CLI (cli.CLI.SaveSession), so a save
// driven from the page persists exactly what a terminal /save would; without it
// /save reports that saving is unavailable.
func (s *Server) SetSessionSaver(fn func() (string, error)) { s.save = fn }

// Start seeds the in-memory scrollback, serves in the background and subscribes
// to the agent event bus.
func (s *Server) Start() {
	s.seedHistory()
	go func() {
		_ = s.srv.Serve(s.listener)
	}()
	s.subscribe()
}

// Close shuts the server down and disconnects every client.
func (s *Server) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	s.mu.Lock()
	// Whatever was still waiting for its window goes away with the clients.
	s.dropDeltaLocked()
	clients := make([]*client, 0, len(s.clients))
	for id, c := range s.clients {
		clients = append(clients, c)
		delete(s.clients, id)
	}
	s.mu.Unlock()
	for _, c := range clients {
		c.close()
	}
	return s.srv.Close()
}

// subscribe forwards agent events to the scrollback and to every connected
// client.
func (s *Server) subscribe() {
	events, cancel := s.agent.Bus().Subscribe()
	go func() {
		defer cancel()
		for {
			select {
			case <-s.stop:
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				s.publish(ev)
			}
		}
	}()
}

// Streamed deltas are the mirror's only high-frequency traffic: a fast model
// publishes dozens of reasoning/answer chunks per second, and every frame costs
// a socket write plus, on a phone, a radio wake-up that outlives the bytes. Two
// consecutive chunks of the same stream therefore travel as one frame (see
// coalesceLocked). The scrollback is not affected: every chunk is recorded
// exactly as published, so a page connecting later replays the same rows.
const (
	// deltaCoalesceWindow is how long a streamed delta waits for companions.
	deltaCoalesceWindow = 50 * time.Millisecond
	// deltaCoalesceMaxBytes bounds a merged frame: a very fast stream cannot grow
	// one message without bound, and the page redraws per frame anyway.
	deltaCoalesceMaxBytes = 64 << 10
)

// isStreamedDelta reports whether an event is a high-frequency stream chunk that
// may be merged with its neighbours. Every other event flushes the merge first,
// which is what keeps the order the scrollback recorded.
func isStreamedDelta(t agent.EventType) bool {
	return t == agent.EventReasoningDelta || t == agent.EventAssistantDelta
}

// publish records an agent event in the scrollback and queues it for every
// client, dropping the connections that cannot take it. Recording and queueing
// share one critical section with the connection handshake (see addClient), so a
// client connecting concurrently still receives each event exactly once: either
// inside its history frames or live. Nothing here touches a socket, so holding
// the lock costs nothing even when a browser is slow.
//
// Streamed deltas take the detour through the coalescing window: they are
// recorded right away (a snapshot has to describe them exactly as published) and
// sent as one merged frame a moment later.
func (s *Server) publish(ev agent.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordLocked(ev)
	if isStreamedDelta(ev.Type) {
		s.coalesceLocked(ev)
		return
	}
	// Any other event ends or interrupts the streamed row: the merged chunks go
	// out ahead of it, in the order the scrollback recorded them.
	s.flushDeltaLocked()
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	s.broadcastLocked(data)
}

// coalesceLocked merges one streamed delta into the pending frame. A chunk of
// the other stream flushes first, so a page never draws text out of order. With
// no client connected there is nothing to send it to — a page that connects
// later replays the row from the scrollback — so the merge is dropped instead of
// arming a timer. The caller must hold s.mu.
func (s *Server) coalesceLocked(ev agent.Event) {
	if len(s.clients) == 0 {
		s.dropDeltaLocked()
		return
	}
	if s.pendingDelta.Type != "" && s.pendingDelta.Type != ev.Type {
		s.flushDeltaLocked()
	}
	s.pendingDelta = ev
	s.pendingText += ev.Text
	if len(s.pendingText) >= deltaCoalesceMaxBytes {
		s.flushDeltaLocked()
		return
	}
	if s.pendingTimer == nil {
		s.pendingTimer = time.AfterFunc(deltaCoalesceWindow, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.flushDeltaLocked()
		})
	}
}

// flushDeltaLocked sends the merged streamed delta, if there is one. The caller
// must hold s.mu.
func (s *Server) flushDeltaLocked() {
	if s.pendingTimer != nil {
		s.pendingTimer.Stop()
		s.pendingTimer = nil
	}
	if s.pendingDelta.Type == "" {
		return
	}
	ev := s.pendingDelta
	ev.Text = s.pendingText
	s.pendingDelta = agent.Event{}
	s.pendingText = ""
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	s.broadcastLocked(data)
}

// dropDeltaLocked discards the merged delta without sending it, for a log that is
// about to be replaced. The caller must hold s.mu.
func (s *Server) dropDeltaLocked() {
	if s.pendingTimer != nil {
		s.pendingTimer.Stop()
		s.pendingTimer = nil
	}
	s.pendingDelta = agent.Event{}
	s.pendingText = ""
}

// broadcastLocked queues one payload for every connected client and closes the
// ones that refuse it (a full backlog or a socket that is already gone). The
// caller must hold s.mu. Queueing is a slice append on the client's own queue,
// so a page that stopped reading can never block the mirror: its writer
// goroutine paces the socket, and the read loop unregisters the client once the
// close lands.
func (s *Server) broadcastLocked(data []byte) {
	for _, c := range s.clients {
		if !c.enqueue(data) {
			c.close()
		}
	}
}

// broadcastFramesLocked queues a whole sequence of frames (one history
// snapshot) in order. The caller must hold s.mu, so no event can slip between
// the frames of the snapshot.
func (s *Server) broadcastFramesLocked(frames [][]byte) {
	for _, frame := range frames {
		s.broadcastLocked(frame)
	}
}

// addClient registers a connection and hands it the current scrollback; it is
// addClientSince with an unknown transcript version, so the page always gets the
// rows.
func (s *Server) addClient(conn *wsConn) *client { return s.addClientSince(conn, 0) }

// addClientSince registers a connection and hands it the current scrollback: the
// history frames are queued under the same lock hold that registers the client,
// so an event published concurrently is delivered exactly once — either in the
// snapshot or live after it (see client.replaying). Marshalling and the socket
// writes happen outside the lock, on the client's own goroutine, so replaying a
// long conversation never blocks the CLI, the other browsers or the page's own
// requests.
//
// since is the transcript version the page's log was built from (0 when it has
// none) and is reported as ?since=. When it matches the mirror's own version,
// nothing has been recorded since, and the reply is a single history_same frame
// instead of the rows: the page keeps the log it shows rather than rebuilding it,
// which is what a phone coming back from the background needs (it stops its
// mirror after a while, see app.js). The comparison and the registration share one
// critical section, so a live event is either already in the transcript (and the
// version differs) or arrives as a live frame — never dropped by the shortcut.
func (s *Server) addClientSince(conn *wsConn, since uint64) *client {
	s.mu.Lock()
	// A merged streamed delta goes out before this client is registered: the
	// scrollback already holds those chunks (recordLocked runs at publish time),
	// so they are part of the snapshot below, and sending the frame as well would
	// draw the same text a second time on the new page.
	s.flushDeltaLocked()
	s.nextID++
	c := newClient(s.nextID, conn)
	s.clients[c.id] = c
	upToDate := since != 0 && since == s.version
	var rows []historyMessage
	var header historyHeader
	if upToDate {
		header = s.historyHeaderLocked(len(s.history))
	} else {
		rows, header = s.historySnapshotLocked()
	}
	s.mu.Unlock()

	go c.writeLoop()
	if upToDate {
		if frame := sameHistoryFrame(header); frame != nil {
			c.enqueueReplay(frame)
		}
		c.finishReplay()
		return c
	}
	for _, frame := range chunkHistory(rows, header) {
		if !c.enqueueReplay(frame) {
			break
		}
	}
	c.finishReplay()
	return c
}

// dropClient unregisters a connection and closes it. Both steps are idempotent,
// so the read loop's teardown, the client's own writer and the backlog cap can
// all call it for the same client.
func (s *Server) dropClient(c *client) {
	s.mu.Lock()
	if current, ok := s.clients[c.id]; ok && current == c {
		delete(s.clients, c.id)
	}
	s.mu.Unlock()
	c.close()
}

// routes builds the HTTP handler tree.
//
// The page and its assets are public: they carry no data, and a browser has to
// be able to load the sign-in dialog before it has a session. Everything that can
// see or steer the agent — the WebSocket, the config editor, the password
// control — sits behind requireSession, so a configured password actually
// protects them.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/app.css", staticAsset(appCSS, "text/css; charset=utf-8"))
	mux.HandleFunc("/app.js", staticAsset(appJS, "application/javascript; charset=utf-8"))
	mux.HandleFunc("/config.js", staticAsset(configJS, "application/javascript; charset=utf-8"))
	mux.HandleFunc("/auth.js", staticAsset(authJS, "application/javascript; charset=utf-8"))
	mux.HandleFunc("/tts.js", staticAsset(ttsJS, "application/javascript; charset=utf-8"))
	if sub, err := fs.Sub(assetsFS, "assets"); err == nil {
		mux.Handle("/assets/", noCache(http.StripPrefix("/assets/", http.FileServer(http.FS(sub)))))
	}
	// Sign-in plumbing: /api/session answers without a session (the page needs
	// it first), and /api/login is protected by the login throttle instead.
	mux.HandleFunc("/api/session", s.handleSession)
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.Handle("/api/logout", s.requireSession(http.HandlerFunc(s.handleLogout)))
	// The agent's own endpoints.
	mux.Handle("/ws", s.requireSession(http.HandlerFunc(s.handleWS)))
	mux.Handle("/api/config", s.requireSession(http.HandlerFunc(s.handleConfig)))
	mux.Handle("/api/password", s.requireSession(http.HandlerFunc(s.handlePassword)))
	// Attachments: the composer uploads a file here before sending, and drops
	// it again when the user cancels it.
	mux.Handle("/api/upload", s.requireSession(http.HandlerFunc(s.handleUpload)))
	return mux
}

// noCache forces the browser to revalidate before reusing a response. The UI
// files are embedded in the binary, so a rebuilt binary must be able to replace
// whatever an earlier build left in the browser cache.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// staticAsset serves an embedded file that never changes for the lifetime of
// the process, under the same auth middleware as the rest of the UI.
func staticAsset(body, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = io.WriteString(w, body)
	}
}

// handleWS upgrades the request to a WebSocket and drives one client: it sends
// the current conversation, then forwards inbound messages to the agent while
// the client's own writer goroutine pushes events back.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgradeWebSocket(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c := s.addClientSince(conn, transcriptVersion(r))
	defer s.dropClient(c)

	for {
		opcode, data, err := conn.readMessage()
		if err != nil {
			return
		}
		if opcode != opText {
			continue
		}
		s.handleClientMessage(data)
	}
}

// transcriptVersion reads the transcript version a reconnecting page reported as
// the ?since= query of its WebSocket URL. An absent or unparsable value is 0,
// which means "unknown" and always gets the full snapshot.
func transcriptVersion(r *http.Request) uint64 {
	v, err := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// historyMessage is one conversation row sent to a browser. It mirrors the rows
// the live view draws: user/assistant/thinking text, info and error markers,
// plus one entry per tool call and per user-visible tool result. The "summary"
// role is the compressed-context summary: it marks the point where the older
// messages were cut out of the model context.
type historyMessage struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	Name    string `json:"name,omitempty"`
	Args    string `json:"args,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

// seedHistory fills the in-memory scrollback from the agent's current
// conversation. It runs when the mirror starts, so a resumed session (loaded
// before the server was created) is replayed to the first browser that
// connects, thinking included.
func (s *Server) seedHistory() {
	rows := messageRows(s.agent.History(), s.agent.ToolResultsVisible())
	// A resumed conversation carries the summary of everything that was
	// compressed away before it, so it becomes the first row: the page then
	// starts exactly where the agent's context does.
	if sum := strings.TrimSpace(s.agent.Summary()); sum != "" {
		rows = append([]historyMessage{{Role: "summary", Content: sum}}, rows...)
	}
	s.mu.Lock()
	s.history = rows
	s.reasoningOpen = false
	// The seeded rows are this process's starting transcript: a page from an
	// earlier process carries a version of its own, so it replays.
	s.touchHistoryLocked()
	s.mu.Unlock()
}

// messageRows converts stored conversation messages into the display rows the
// page draws: user and assistant text, a thinking row for each assistant
// message that carries reasoning, and one [tool] row per call plus its shown
// result.
func messageRows(msgs []llm.Message, showResults bool) []historyMessage {
	out := make([]historyMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			if strings.TrimSpace(m.ReasoningContent) != "" {
				out = append(out, historyMessage{Role: "reasoning", Content: m.ReasoningContent})
			}
			if strings.TrimSpace(m.Content) != "" {
				out = append(out, historyMessage{Role: "assistant", Content: m.Content})
			}
			// The live view prints one [tool] row per requested call.
			for _, tc := range m.ToolCalls {
				out = append(out, historyMessage{
					Role: "tool_call",
					Name: tc.Function.Name,
					Args: tc.Function.Arguments,
				})
			}
		case "tool":
			if !showResults {
				continue
			}
			// Only results that have a user-facing rendering are restored;
			// the rest were never shown live either.
			text, isErr, ok := tools.RenderStoredResult(m.Content)
			if !ok {
				continue
			}
			out = append(out, historyMessage{Role: "tool_result", Content: text, IsError: isErr})
		default:
			if strings.TrimSpace(m.Content) != "" {
				out = append(out, historyMessage{Role: m.Role, Content: m.Content})
			}
		}
	}
	return out
}

// recordLocked folds one agent event into the scrollback, mirroring what the
// page's render() draws so a client connecting later replays the same rows. The
// caller must hold s.mu.
func (s *Server) recordLocked(ev agent.Event) {
	// Any event other than a further reasoning chunk closes the open thinking
	// row, exactly like the live view.
	if ev.Type != agent.EventReasoningDelta {
		s.closeReasoningLocked()
	}
	before := len(s.history)
	switch ev.Type {
	case agent.EventReasoningDelta:
		if !s.reasoningOpen {
			s.history = append(s.history, historyMessage{Role: "reasoning"})
			s.reasoningOpen = true
		}
		if ev.Text != "" {
			s.history[len(s.history)-1].Content += ev.Text
			// The row grew although no row was added: the transcript changed, and
			// a page holding the previous version has to replay it.
			s.touchHistoryLocked()
		}
	case agent.EventUser:
		s.history = append(s.history, historyMessage{Role: "user", Content: ev.Text})
	case agent.EventAssistant:
		if strings.TrimSpace(ev.Text) != "" {
			s.history = append(s.history, historyMessage{Role: "assistant", Content: ev.Text})
		}
	case agent.EventToolCall:
		s.history = append(s.history, historyMessage{Role: "tool_call", Name: ev.Name, Args: ev.Args})
	case agent.EventToolResult:
		// Match the live view: an empty result is only surfaced when it failed.
		if strings.TrimSpace(ev.Text) == "" && !ev.IsError {
			return
		}
		s.history = append(s.history, historyMessage{Role: "tool_result", Content: ev.Text, IsError: ev.IsError})
	case agent.EventInfo:
		s.history = append(s.history, historyMessage{Role: "info", Content: ev.Text})
	case agent.EventCompacted:
		s.history = append(s.history, historyMessage{Role: "info", Content: ev.Text})
		// The summary is what replaced the messages that were cut out of the
		// context, so its row is recorded right at the cut: a page connecting
		// later replays the marker in the same place.
		if sum := strings.TrimSpace(ev.Summary); sum != "" {
			s.history = append(s.history, historyMessage{Role: "summary", Content: sum})
		}
	case agent.EventInterrupted:
		s.history = append(s.history, historyMessage{Role: "interrupted", Content: ev.Text})
	case agent.EventError:
		s.history = append(s.history, historyMessage{Role: "error", Content: ev.Text})
	}
	// A row was added (or an empty thinking row was dropped above): the transcript
	// a reconnecting page has to replay changed.
	if len(s.history) != before {
		s.touchHistoryLocked()
	}
}

// touchHistoryLocked records that the visible transcript changed. The caller must
// hold s.mu.
func (s *Server) touchHistoryLocked() { s.version++ }

// closeReasoningLocked finalizes the open thinking row, dropping it when it
// ended up empty. The caller must hold s.mu.
func (s *Server) closeReasoningLocked() {
	if !s.reasoningOpen {
		return
	}
	s.reasoningOpen = false
	if last := len(s.history) - 1; strings.TrimSpace(s.history[last].Content) == "" {
		s.history = s.history[:last]
		// The row never reached a page, so the transcript changed.
		s.touchHistoryLocked()
	}
}

// A history snapshot travels as three kinds of frame, so a page can start
// painting immediately and no single WebSocket message is huge. That matters
// because a WebSocket message is atomic: the browser cannot render (or even
// hand to the page) one huge frame in pieces, which is exactly what left a long
// conversation stuck on the empty-log placeholder while the main thread ground
// through the whole replay.
//
//	history_start   the header: usage numbers, the switches and the row count
//	history_rows    one batch of rows, repeated until the scrollback is sent
//	history_end     the terminator, after which live events follow
const (
	// historyBatchRows is how many rows one history_rows frame carries.
	historyBatchRows = 200
	// historyBatchBytes stops a batch early once its rows add up to this much
	// text: rows vary wildly in size (a long tool result, a paragraph of
	// thinking), so the row count alone does not keep a frame small.
	historyBatchBytes = 256 << 10
)

// historyHeader is the payload of the first snapshot frame. Everything a page
// needs besides the rows rides here, including the switch states, so a
// reconnecting tab is back in sync.
type historyHeader struct {
	Type string `json:"type"`
	// Version identifies the transcript the rows belong to: a page sends it back
	// as ?since= on its next connection (see addClientSince).
	Version  uint64 `json:"version"`
	Tokens   int    `json:"tokens"`
	Window   int    `json:"window"`
	Busy     bool   `json:"busy"`
	Markdown bool   `json:"markdown"`
	Result   bool   `json:"result"`
	Count    int    `json:"count"`
}

// historyRowsFrame is one batch of replayed rows.
type historyRowsFrame struct {
	Type     string           `json:"type"`
	Messages []historyMessage `json:"messages"`
}

// historySameFrame is the reply to a page whose transcript is already current: it
// carries the state that can change without the rows changing (usage numbers, the
// running flag, the switches) and, by its type alone, says that no rows follow.
// The page keeps the log it has, so a phone coming back from the background does
// not rebuild (or even repaint) a long conversation that did not change.
type historySameFrame struct {
	Type     string `json:"type"`
	Version  uint64 `json:"version"`
	Tokens   int    `json:"tokens"`
	Window   int    `json:"window"`
	Busy     bool   `json:"busy"`
	Markdown bool   `json:"markdown"`
	Result   bool   `json:"result"`
}

// sameHistoryFrame renders the "nothing new" reply from the same header the
// snapshot would have carried. It returns nil when the header cannot be
// marshalled, and the caller then sends the full snapshot instead.
func sameHistoryFrame(header historyHeader) []byte {
	data, err := json.Marshal(historySameFrame{
		Type:     "history_same",
		Version:  header.Version,
		Tokens:   header.Tokens,
		Window:   header.Window,
		Busy:     header.Busy,
		Markdown: header.Markdown,
		Result:   header.Result,
	})
	if err != nil {
		return nil
	}
	return data
}

// historyHeaderLocked builds the state the first snapshot frame carries. The
// caller must hold s.mu.
func (s *Server) historyHeaderLocked(rows int) historyHeader {
	stats := s.agent.Stats()
	return historyHeader{
		Type:     "history_start",
		Version:  s.version,
		Tokens:   stats.EstimatedTok,
		Window:   stats.ContextWindow,
		Busy:     stats.Busy,
		Markdown: s.markdown,
		Result:   s.agent.ToolResultsVisible(),
		Count:    rows,
	}
}

// historySnapshotLocked copies the scrollback and the state the header carries.
// The copy shares the row strings (they are immutable) but owns its slice, so
// the frames can be marshalled and queued outside the lock while the agent
// keeps appending. The caller must hold s.mu.
func (s *Server) historySnapshotLocked() ([]historyMessage, historyHeader) {
	rows := make([]historyMessage, len(s.history))
	copy(rows, s.history)
	return rows, s.historyHeaderLocked(len(s.history))
}

// historyFrames returns the frames a newly connected page receives, in order.
// It is the path the tests read (the live path calls chunkHistory per client,
// see addClient).
func (s *Server) historyFrames() [][]byte {
	s.mu.Lock()
	rows, header := s.historySnapshotLocked()
	s.mu.Unlock()
	return chunkHistory(rows, header)
}

// chunkHistory turns a snapshot into the frames a page consumes: a header, one
// frame per batch of rows and a terminator. Marshalling happens here, without
// any lock held.
func chunkHistory(rows []historyMessage, header historyHeader) [][]byte {
	frames := make([][]byte, 0, len(rows)/historyBatchRows+2)
	if data, err := json.Marshal(header); err == nil {
		frames = append(frames, data)
	}
	batch := make([]historyMessage, 0, historyBatchRows)
	size := 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if data, err := json.Marshal(historyRowsFrame{Type: "history_rows", Messages: batch}); err == nil {
			frames = append(frames, data)
		}
		batch = batch[:0]
		size = 0
	}
	for _, row := range rows {
		rowBytes := historyRowBytes(row)
		// A row that is bigger than the cap on its own travels alone: flush what
		// is pending first, so it is never batched with its neighbours.
		if rowBytes >= historyBatchBytes && len(batch) > 0 {
			flush()
		}
		batch = append(batch, row)
		size += rowBytes
		if len(batch) >= historyBatchRows || size >= historyBatchBytes {
			flush()
		}
	}
	flush()
	return append(frames, []byte(`{"type":"history_end"}`))
}

// historyRowBytes approximates the JSON size of one row. It only has to keep a
// batch near historyBatchBytes, so counting the text is precise enough.
func historyRowBytes(row historyMessage) int {
	return len(row.Role) + len(row.Content) + len(row.Name) + len(row.Args) + 48
}

// handleClientMessage submits an inbound client message. Slash commands are
// handled here, so the mirror offers the same command set as the terminal REPL;
// every other line goes to the agent. Multiple clients may call this
// concurrently; steering is handled by the agent.
//
// A message may carry attachments: the ids of files the page uploaded earlier
// (see handleUpload). They are read from the upload directory and travel with
// the text as the media of the user message, so the model receives them with
// the prompt they belong to. An attachment that cannot be read is reported as
// an error row and skipped; the message itself is still sent.
func (s *Server) handleClientMessage(data []byte) {
	var msg struct {
		Text        string   `json:"text"`
		Attachments []string `json:"attachments"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	text := strings.TrimSpace(msg.Text)
	if slash.IsCommandLine(text) {
		// A command is not a prompt: its attachments stay with the page, which
		// keeps the chips until a message actually carries them.
		s.handleCommand(text)
		return
	}
	media, names := s.readAttachments(msg.Attachments)
	if text == "" && len(media) == 0 {
		return
	}
	if text == "" {
		text = "(attached " + strings.Join(names, ", ") + ")"
	}
	s.agent.SubmitMedia("web", text, media)
}

// readAttachments reads the uploaded files a message carries into content parts.
// Every problem (an unknown id, a file that vanished, one the model no longer
// accepts) is reported to the pages as an error row, so the user sees why an
// attachment did not make it instead of quietly losing it.
func (s *Server) readAttachments(ids []string) ([]llm.ContentPart, []string) {
	if len(ids) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	media, dir := s.mediaLocked()
	s.mu.Unlock()
	if !media.Enabled() {
		s.localError("attachments are not enabled in this run")
		return nil, nil
	}
	parts := make([]llm.ContentPart, 0, len(ids))
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		path, err := uploadPath(dir, id)
		if err != nil {
			s.localError("attachment: " + err.Error())
			continue
		}
		file, err := media.ReadMedia(path)
		if err != nil {
			s.localError("attachment " + id + ": " + err.Error())
			continue
		}
		parts = append(parts, file.Parts...)
		names = append(names, file.Name)
	}
	return parts, names
}

// handleCommand runs one slash command from a browser; the set is the terminal
// REPL's (see internal/slash).
//
// Commands that touch state both front-ends share — the conversation, the
// session file, tool-result visibility — answer on the agent bus, so the
// terminal shows the same feedback as the browser. The commands that only
// concern this page (its /help listing, its markdown switch, a mistyped
// command) are recorded in the mirror's scrollback and sent to the browsers
// alone: the terminal has its own /help and its own rendering. An unknown
// command is consumed, exactly like the terminal does, instead of being sent to
// the model.
func (s *Server) handleCommand(text string) {
	cmd, args, known := slash.Split(text)
	if !known {
		s.localError("unknown command " + cmd + "; try /help")
		return
	}
	switch cmd {
	case "/help":
		s.localInfo(slash.Table(nil) + s.helpNotes())
	case "/new":
		if s.agent.Busy() {
			// The side rail runs commands with one click, so never discard a
			// running conversation under the user's feet.
			s.localError("a turn is running; try again when idle")
			return
		}
		s.agent.Reset()
		// The page has to forget the rows of the conversation /new dropped.
		s.clearScrollback()
		s.info("started a new conversation (in memory; /save to persist)")
	case "/save":
		if s.save == nil {
			s.fail("saving is not available in this run")
			return
		}
		path, err := s.save()
		if err != nil {
			s.fail("failed to save session: " + err.Error())
			return
		}
		s.info("session saved to " + path)
	case "/compact":
		if s.agent.Busy() {
			s.fail("a turn is running; try again when idle")
			return
		}
		// A pass with nothing to condense is the only case that needs a row
		// here: one that did compress reports itself on the bus (the
		// "compacting" info, then the compacted event with the summary).
		if msg := s.agent.CompactNow(context.Background()); msg != "" {
			s.info(msg)
		}
	case "/stop":
		if !s.agent.Interrupt() {
			s.info("nothing to interrupt")
		}
		// The interrupted marker and the end of the turn are broadcast to every
		// client by the agent.
	case "/history":
		s.info(slash.UsageText(s.agent.Stats()))
	case "/result":
		on, ok := slash.ToggleArg(argAt(args, 0), s.agent.ToolResultsVisible())
		if !ok {
			s.fail("usage: /result [on|off]")
			return
		}
		s.agent.SetToolResultsVisible(on)
		state := "hiding"
		if on {
			state = "showing"
		}
		s.info(state + " tool/exec results")
		s.broadcastSettings()
	case "/markdown":
		on, ok := slash.ToggleArg(argAt(args, 0), s.markdownEnabled())
		if !ok {
			s.fail("usage: /markdown [on|off]")
			return
		}
		s.setMarkdown(on)
		state := "markdown rendering off (raw output)"
		if on {
			state = "markdown rendering on"
		}
		s.localInfo(state)
	case "/exit":
		// /exit ends the whole session — terminal included — after asking whether
		// to save, so the page points at the terminal instead.
		s.localInfo("/exit quits the terminal; just close the tab to leave the mirror")
	}
}

// helpNotes appends the page-specific hints to the shared command table.
func (s *Server) helpNotes() string {
	lines := []string{
		"",
		"run a command by clicking it in the side rail, or type it here.",
	}
	var terminal []string
	for _, c := range slash.TerminalOnly() {
		terminal = append(terminal, fmt.Sprintf("%s — %s", c.Usage(), c.Summary))
	}
	if len(terminal) > 0 {
		lines = append(lines, "terminal only: "+strings.Join(terminal, "; "))
	}
	return strings.Join(append(lines,
		"input: Enter inserts a newline, Ctrl+Enter sends.",
		"while a turn runs, type a message to insert it into the loop (steering).",
	), "\n")
}

// argAt returns args[i], or "" when the command carried fewer arguments.
func argAt(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

// info publishes shared feedback on the agent bus, so the terminal and every
// browser render the same line.
func (s *Server) info(text string) {
	s.agent.Bus().Publish(agent.Event{Type: agent.EventInfo, Text: text})
}

// fail publishes a shared failure the same way; the front-ends render it as an
// error row.
func (s *Server) fail(text string) {
	s.agent.Bus().Publish(agent.Event{Type: agent.EventError, Text: text})
}

// localRow records a mirror-only row: it joins the mirror's scrollback and is
// pushed to the connected browsers, but never reaches the agent bus, so it
// cannot leak into the terminal's transcript.
func (s *Server) localRow(row historyMessage, ev agent.Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeReasoningLocked()
	s.history = append(s.history, row)
	// This path does not go through recordLocked, so it bumps the transcript version
	// itself: a page holding the previous version has to replay this row.
	s.touchHistoryLocked()
	s.broadcastLocked(data)
}

// localInfo and localError are localRow for the two page-only markers.
func (s *Server) localInfo(text string) {
	s.localRow(historyMessage{Role: "info", Content: text}, agent.Event{Type: agent.EventInfo, Text: text})
}

func (s *Server) localError(text string) {
	s.localRow(historyMessage{Role: "error", Content: text}, agent.Event{Type: agent.EventError, Text: text})
}

// clearScrollback drops the mirror's rows and pushes the now empty snapshot, so
// every open page forgets the conversation that was just discarded. The frames
// are built under the lock: the snapshot is empty here, so it is cheap, and
// nothing can slip between its frames.
func (s *Server) clearScrollback() {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The cleared log has no rows: a merged chunk of the discarded conversation
	// must not land in it after the empty snapshot below.
	s.dropDeltaLocked()
	s.history = nil
	s.reasoningOpen = false
	// The log was replaced, so a page holding the old version has to replay this
	// (empty) transcript instead of keeping what it shows.
	s.touchHistoryLocked()
	rows, header := s.historySnapshotLocked()
	s.broadcastFramesLocked(chunkHistory(rows, header))
}

// markdownEnabled reports the page's markdown switch.
func (s *Server) markdownEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.markdown
}

// setMarkdown flips the page's markdown switch and tells the browsers; the
// command that drove it records the human-readable row itself.
func (s *Server) setMarkdown(on bool) {
	s.mu.Lock()
	s.markdown = on
	s.mu.Unlock()
	s.broadcastSettings()
}

// settingsFrame builds the transient payload carrying the switches the side
// rail mirrors. A fresh page gets the same values injected into its document
// and a reconnecting one gets them in its history frame.
func (s *Server) settingsFrame() []byte {
	data, err := json.Marshal(map[string]any{
		"type":     "settings",
		"markdown": s.markdownEnabled(),
		"result":   s.agent.ToolResultsVisible(),
	})
	if err != nil {
		return nil
	}
	return data
}

// broadcastSettings pushes the current switches to the connected browsers.
func (s *Server) broadcastSettings() {
	data := s.settingsFrame()
	if data == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broadcastLocked(data)
}

// handleIndex serves the single-page UI. The page is public (it carries no data
// and its sign-in dialog has to be reachable before there is a session); the
// runtime switches, the command rail and the media capability are injected, and
// every endpoint that touches the agent asks for a session of its own.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page embeds the script/style URLs, so it must never be reused from
	// cache: a rebuilt binary has to be able to replace the whole UI at once.
	w.Header().Set("Cache-Control", "no-cache")
	body := indexHTML
	body = strings.ReplaceAll(body, "__LIGHTAGENT_MARKDOWN__", strconv.FormatBool(s.markdownEnabled()))
	body = strings.ReplaceAll(body, "__LIGHTAGENT_RESULT__", strconv.FormatBool(s.agent.ToolResultsVisible()))
	body = strings.ReplaceAll(body, "__LIGHTAGENT_COMMANDS__", commandsJSON())
	body = strings.ReplaceAll(body, "__LIGHTAGENT_MEDIA__", s.mediaJSON())
	fmt.Fprint(w, body)
}

// mediaJSON is the media capability handed to the page: whether the composer
// offers an attach control at all, which types it accepts (the file picker's
// accept attribute) and how large one file may be (checked before uploading, so
// an oversized pick is refused without a round trip).
func (s *Server) mediaJSON() string {
	s.mu.Lock()
	media := s.media
	s.mu.Unlock()
	payload := map[string]any{
		"enabled":   media.Enabled(),
		"types":     media.Types,
		"accept":    media.Accept(),
		"max_bytes": media.Limit(),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return `{"enabled":false,"types":[],"accept":"","max_bytes":0}`
	}
	return string(data)
}

// commandsJSON is the command rail handed to the page. It is built from the
// shared catalogue (internal/slash), so the rail can never drift from the /help
// text of either front-end; the page folds everything the catalogue does not
// mark primary.
func commandsJSON() string {
	data, err := json.Marshal(slash.WebCommands())
	if err != nil {
		return "[]"
	}
	return string(data)
}
