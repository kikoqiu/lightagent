// Package web implements an optional HTTP mirror of the CLI: it exposes the
// same agent over Server-Sent Events and accepts user messages, so a browser
// can observe and drive the same conversation.
package web

import (
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

	"lightagent/internal/agent"
	"lightagent/internal/llm"
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
	markdown bool
	// auth holds the login credential and the live sessions; the zero value (no
	// password) means the mirror runs without a login.
	auth *auth
	// configPath is the config.json the /api/config editor reads and rewrites
	// (empty when the mirror runs without one). configPendingRestart records
	// that a document saved through the editor is waiting for the next start.
	configPath           string
	configPendingRestart bool

	mu       sync.Mutex
	clients  map[int]*wsConn
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
		clients:  make(map[int]*wsConn),
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
	for id, c := range s.clients {
		_ = c.Close()
		delete(s.clients, id)
	}
	s.mu.Unlock()
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

// publish records an agent event in the scrollback and broadcasts it to every
// client, dropping dead connections. Recording and broadcasting share one
// critical section with the connection handshake (see addClient), so a client
// connecting concurrently still receives each event exactly once: either inside
// its history frame or live.
func (s *Server) publish(ev agent.Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordLocked(ev)
	for id, c := range s.clients {
		if err := c.writeText(data); err != nil {
			_ = c.Close()
			delete(s.clients, id)
		}
	}
}

// addClient registers a connection, returns its id, and pushes the current
// scrollback. Both steps happen under one lock hold so an event broadcast
// concurrently is delivered exactly once (live or in the frame).
func (s *Server) addClient(c *wsConn) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	s.clients[id] = c
	if payload := s.historyFrameLocked(); payload != nil {
		_ = c.writeText(payload)
	}
	return id
}

// removeClient unregisters and closes a connection.
func (s *Server) removeClient(id int) {
	s.mu.Lock()
	c, ok := s.clients[id]
	delete(s.clients, id)
	s.mu.Unlock()
	if ok {
		_ = c.Close()
	}
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
// the publish loop pushes events back.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgradeWebSocket(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := s.addClient(conn)
	defer s.removeClient(id)

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

// historyMessage is one conversation row sent to a browser. It mirrors the rows
// the live view draws: user/assistant/thinking text, info and error markers,
// plus one entry per tool call and per user-visible tool result.
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
	s.mu.Lock()
	s.history = rows
	s.reasoningOpen = false
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
	switch ev.Type {
	case agent.EventReasoningDelta:
		if !s.reasoningOpen {
			s.history = append(s.history, historyMessage{Role: "reasoning"})
			s.reasoningOpen = true
		}
		s.history[len(s.history)-1].Content += ev.Text
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
	case agent.EventInfo, agent.EventCompacted:
		s.history = append(s.history, historyMessage{Role: "info", Content: ev.Text})
	case agent.EventInterrupted:
		s.history = append(s.history, historyMessage{Role: "interrupted", Content: ev.Text})
	case agent.EventError:
		s.history = append(s.history, historyMessage{Role: "error", Content: ev.Text})
	}
}

// closeReasoningLocked finalizes the open thinking row, dropping it when it
// ended up empty. The caller must hold s.mu.
func (s *Server) closeReasoningLocked() {
	if !s.reasoningOpen {
		return
	}
	s.reasoningOpen = false
	if last := len(s.history) - 1; strings.TrimSpace(s.history[last].Content) == "" {
		s.history = s.history[:last]
	}
}

// historyFrame builds the payload sent to a newly connected client.
func (s *Server) historyFrame() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.historyFrameLocked()
}

// historyFrameLocked serializes the scrollback plus the summary, context-usage
// and busy state the page needs. The busy flag lets a client connecting
// mid-turn show the running indicator. The caller must hold s.mu.
func (s *Server) historyFrameLocked() []byte {
	stats := s.agent.Stats()
	payload, err := json.Marshal(map[string]any{
		"type":     "history",
		"messages": s.history,
		"summary":  s.agent.Summary(),
		"tokens":   stats.EstimatedTok,
		"window":   stats.ContextWindow,
		"busy":     stats.Busy,
	})
	if err != nil {
		return nil
	}
	return payload
}

// handleClientMessage submits an inbound client message to the agent. Multiple
// clients may call this concurrently; steering is handled by the agent. A few
// control commands (handled by the agent itself) are recognized so the mirror
// can drive them too.
func (s *Server) handleClientMessage(data []byte) {
	var msg struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}
	if s.handleCommand(text) {
		return
	}
	s.agent.SubmitFrom("web", text)
}

// handleCommand applies the small set of shared control commands the web UI may
// send. It reports whether text was consumed. Any other "/..." text is passed
// through to the agent.
func (s *Server) handleCommand(text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	switch strings.ToLower(fields[0]) {
	case "/result", "/results":
		on := !s.agent.ToolResultsVisible()
		if len(fields) > 1 {
			switch strings.ToLower(fields[1]) {
			case "on", "true", "1", "yes":
				on = true
			case "off", "false", "0", "no":
				on = false
			default:
				s.agent.Bus().Publish(agent.Event{Type: agent.EventInfo, Text: "usage: /result [on|off]"})
				return true
			}
		}
		s.agent.SetToolResultsVisible(on)
		state := "hiding"
		if on {
			state = "showing"
		}
		s.agent.Bus().Publish(agent.Event{Type: agent.EventInfo, Text: state + " tool/exec results"})
		return true
	case "/stop", "/interrupt":
		if !s.agent.Interrupt() {
			s.agent.Bus().Publish(agent.Event{Type: agent.EventInfo, Text: "nothing to interrupt"})
		}
		// The interrupted marker and the end of the turn are broadcast to every
		// client by the agent.
		return true
	}
	return false
}

// handleIndex serves the single-page UI. The page is public (it carries no data
// and its sign-in dialog has to be reachable before there is a session); only the
// markdown switch is injected, and every endpoint that touches the agent asks for
// a session of its own.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page embeds the script/style URLs, so it must never be reused from
	// cache: a rebuilt binary has to be able to replace the whole UI at once.
	w.Header().Set("Cache-Control", "no-cache")
	body := strings.ReplaceAll(indexHTML, "__LIGHTAGENT_MARKDOWN__", strconv.FormatBool(s.markdown))
	fmt.Fprint(w, body)
}
