package utils

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Protocol tuning: the handshake and the small JSON-RPC calls are all local, so
// a short deadline is enough, while a single message (a serialized page) can be
// large.
const (
	cdpHandshakeTimeout = 10 * time.Second
	cdpWriteTimeout     = 30 * time.Second
	cdpReadLimit        = 64 << 20
	cdpEventBuffer      = 32
	devToolsHTTPTimeout = 5 * time.Second
)

// cdpEndpoint is a live DevTools endpoint of a browser.
type cdpEndpoint struct {
	Address         string // host:port of the DevTools HTTP server
	WSURL           string // browser level WebSocket URL
	Product         string // "Chrome/153.0.8010.52"
	ProtocolVersion string // "1.3"
}

// normalizeAddress accepts what a caller may pass as the address of a running
// browser: "127.0.0.1:9222", "http://127.0.0.1:9222",
// "ws://127.0.0.1:9222/devtools/browser/<id>", "9222" or a bare host, which
// defaults to port 9222.
func normalizeAddress(raw string) (address, wsURL string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", errors.New("empty browser address")
	}
	if strings.Contains(trimmed, "://") {
		parsed, parseErr := url.Parse(trimmed)
		if parseErr != nil {
			return "", "", fmt.Errorf("invalid browser address %q: %w", raw, parseErr)
		}
		address = parsed.Host
		if parsed.Scheme == "ws" || parsed.Scheme == "wss" {
			wsURL = trimmed
		}
	} else {
		address = trimmed
	}
	if address == "" {
		return "", "", fmt.Errorf("invalid browser address %q: no host", raw)
	}
	if !strings.Contains(address, ":") {
		if isAllDigits(address) {
			address = "127.0.0.1:" + address
		} else {
			address += ":9222"
		}
	}
	return address, wsURL, nil
}

// cdpVersion is the /json/version payload.
type cdpVersion struct {
	Browser              string `json:"Browser"`
	ProtocolVersion      string `json:"Protocol-Version"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// probeEndpoint asks a DevTools HTTP server for its browser socket. It is the
// readiness check used after a launch and the entry point of an attach.
func probeEndpoint(ctx context.Context, address, wsURL string) (cdpEndpoint, error) {
	endpoint := cdpEndpoint{Address: address, WSURL: wsURL}
	if wsURL != "" {
		return endpoint, nil
	}
	client := &http.Client{Timeout: devToolsHTTPTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/json/version", nil)
	if err != nil {
		return endpoint, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return endpoint, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return endpoint, fmt.Errorf("devtools endpoint %s answered %s", address, resp.Status)
	}
	var version cdpVersion
	if err := json.NewDecoder(resp.Body).Decode(&version); err != nil {
		return endpoint, fmt.Errorf("devtools endpoint %s: %w", address, err)
	}
	if version.WebSocketDebuggerURL == "" {
		return endpoint, fmt.Errorf("devtools endpoint %s: no browser WebSocket URL", address)
	}
	endpoint.WSURL = version.WebSocketDebuggerURL
	endpoint.Product = version.Browser
	endpoint.ProtocolVersion = version.ProtocolVersion
	return endpoint, nil
}

// waitEndpoint polls a DevTools HTTP server until it answers or the context
// ends, which is how a fresh launch is awaited.
func waitEndpoint(ctx context.Context, address, wsURL string) (cdpEndpoint, error) {
	interval := 25 * time.Millisecond
	for {
		endpoint, err := probeEndpoint(ctx, address, wsURL)
		if err == nil {
			return endpoint, nil
		}
		if ctx.Err() != nil {
			return cdpEndpoint{}, fmt.Errorf("devtools endpoint %s did not come up: %w", address, ctx.Err())
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
		if interval < 400*time.Millisecond {
			interval *= 2
		}
	}
}

// cdpRPCError is a protocol level error answer.
type cdpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func (e *cdpRPCError) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("cdp error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message)
}

// cdpRequest is the envelope of a call, cdpMessage the envelope of everything
// the browser sends back: an answer (id set) or an event (method set).
type cdpRequest struct {
	ID        int64  `json:"id"`
	Method    string `json:"method"`
	Params    any    `json:"params,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpRPCError    `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

// cdpReply is what a pending call waits for.
type cdpReply struct {
	Result json.RawMessage
	Err    error
}

// cdpEvent is one protocol event, still undecoded.
type cdpEvent struct {
	Method    string
	SessionID string
	Params    json.RawMessage
}

// cdpSubscription receives the events of one method, optionally of one session
// only. Events are dropped, oldest first, when a subscriber cannot keep up, so
// a slow reader can never stall the connection.
type cdpSubscription struct {
	conn    *cdpConn
	method  string
	session string
	ch      chan cdpEvent
}

// cdpConn is a DevTools protocol connection: JSON messages over the browser
// WebSocket, with answers matched to the call that asked for them and events
// routed to the subscribers of their method. Session scoped calls (flattened
// page targets) carry a sessionId and travel over the same socket, so a page
// never needs a connection of its own.
type cdpConn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan cdpReply
	subs    []*cdpSubscription
	closed  bool
	err     error
	done    chan struct{}
}

// dialCDP opens the browser WebSocket of a DevTools endpoint.
func dialCDP(ctx context.Context, wsURL string, handshakeTimeout time.Duration) (*cdpConn, error) {
	if handshakeTimeout <= 0 {
		handshakeTimeout = cdpHandshakeTimeout
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
		Proxy:            http.ProxyFromEnvironment,
	}
	// The request carries no Origin header: a browser rejects a DevTools
	// handshake that looks like a cross-origin request unless it was started
	// with --remote-allow-origins.
	ws, resp, err := dialer.DialContext(ctx, wsURL, http.Header{})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("devtools handshake failed (%s): %w", resp.Status, err)
		}
		return nil, fmt.Errorf("devtools handshake failed: %w", err)
	}
	ws.SetReadLimit(cdpReadLimit)
	conn := &cdpConn{
		ws:      ws,
		pending: make(map[int64]chan cdpReply),
		done:    make(chan struct{}),
	}
	go conn.readLoop()
	return conn, nil
}

// call sends a protocol command and decodes its result into out (which may be
// nil). It returns as soon as the answer arrives, the context ends or the
// connection dies.
func (c *cdpConn) call(ctx context.Context, session, method string, params any, out any) error {
	id, reply, err := c.register()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(cdpRequest{ID: id, Method: method, Params: params, SessionID: session})
	if err != nil {
		c.forget(id)
		return err
	}
	if err := c.write(payload); err != nil {
		c.forget(id)
		return fmt.Errorf("cdp %s: %w", method, err)
	}

	select {
	case answer := <-reply:
		if answer.Err != nil {
			return fmt.Errorf("cdp %s: %w", method, answer.Err)
		}
		if out == nil || len(answer.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(answer.Result, out); err != nil {
			return fmt.Errorf("cdp %s: cannot decode result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		c.forget(id)
		return fmt.Errorf("cdp %s: %w", method, ctx.Err())
	case <-c.done:
		c.forget(id)
		return fmt.Errorf("cdp %s: %w", method, c.closeError())
	}
}

// closeDone reports the channel closed once the connection is gone, which lets
// a wait give up immediately instead of running into its timeout.
func (c *cdpConn) closeDone() <-chan struct{} { return c.done }

// closeError reports why the connection ended; nil while it is alive.
func (c *cdpConn) closeError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErrorLocked()
}

// closeErrorLocked is closeError for a caller that already holds the lock; the
// mutex is not reentrant, so it must not call closeError.
func (c *cdpConn) closeErrorLocked() error {
	if c.err != nil {
		return c.err
	}
	return errors.New("devtools connection closed")
}

// subscribe registers interest in the events of a method of a session; an empty
// session matches every session.
func (c *cdpConn) subscribe(session, method string) *cdpSubscription {
	sub := &cdpSubscription{
		conn:    c,
		method:  method,
		session: session,
		ch:      make(chan cdpEvent, cdpEventBuffer),
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return sub
	}
	c.subs = append(c.subs, sub)
	c.mu.Unlock()
	return sub
}

// cancel drops a subscription.
func (s *cdpSubscription) cancel() {
	if s == nil || s.conn == nil {
		return
	}
	c := s.conn
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, sub := range c.subs {
		if sub == s {
			c.subs = append(c.subs[:i], c.subs[i+1:]...)
			break
		}
	}
}

// register allocates the next id together with the channel its answer arrives
// on.
func (c *cdpConn) register() (int64, chan cdpReply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		// A call on a dead connection fails at once instead of waiting for an
		// answer that will never come.
		return 0, nil, c.closeErrorLocked()
	}
	c.nextID++
	reply := make(chan cdpReply, 1)
	c.pending[c.nextID] = reply
	return c.nextID, reply, nil
}

// forget drops the answer channel of a call that gave up.
func (c *cdpConn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// write sends one frame.
func (c *cdpConn) write(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.SetWriteDeadline(time.Now().Add(cdpWriteTimeout)); err != nil {
		return err
	}
	return c.ws.WriteMessage(websocket.TextMessage, payload)
}

// readLoop dispatches everything the browser sends until the socket fails.
func (c *cdpConn) readLoop() {
	for {
		_, payload, err := c.ws.ReadMessage()
		if err != nil {
			c.shutdown(fmt.Errorf("devtools connection lost: %w", err))
			return
		}
		var message cdpMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			// A frame that cannot be read is not worth breaking the socket for.
			continue
		}
		if message.ID != 0 {
			c.deliver(message)
			continue
		}
		if message.Method != "" {
			c.publish(cdpEvent{
				Method:    message.Method,
				SessionID: message.SessionID,
				Params:    message.Params,
			})
		}
	}
}

// deliver hands an answer to the call waiting for it.
func (c *cdpConn) deliver(message cdpMessage) {
	c.mu.Lock()
	reply, ok := c.pending[message.ID]
	delete(c.pending, message.ID)
	c.mu.Unlock()
	if !ok {
		return
	}
	answer := cdpReply{Result: message.Result}
	if message.Error != nil {
		answer.Err = message.Error
	}
	select {
	case reply <- answer:
	default:
	}
}

// publish routes an event to the subscribers of its method.
func (c *cdpConn) publish(event cdpEvent) {
	c.mu.Lock()
	var subs []*cdpSubscription
	for _, sub := range c.subs {
		if sub.method != event.Method {
			continue
		}
		if sub.session != "" && sub.session != event.SessionID {
			continue
		}
		subs = append(subs, sub)
	}
	c.mu.Unlock()

	for _, sub := range subs {
		select {
		case sub.ch <- event:
			continue
		default:
		}
		// Full buffer: drop the oldest event so that the newest one, which is
		// the one a load wait cares about, still gets through.
		select {
		case <-sub.ch:
		default:
		}
		select {
		case sub.ch <- event:
		default:
		}
	}
}

// shutdown fails every pending call and marks the connection dead. It is
// idempotent, so a close and a read loop failure can both call it.
func (c *cdpConn) shutdown(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.err = err
	pending := c.pending
	c.pending = make(map[int64]chan cdpReply)
	c.mu.Unlock()

	close(c.done)
	for _, reply := range pending {
		select {
		case reply <- cdpReply{Err: err}:
		default:
		}
	}
}

// close shuts the connection down, telling the browser why.
func (c *cdpConn) close() error {
	c.shutdown(errors.New("devtools connection closed by client"))
	_ = c.ws.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second),
	)
	return c.ws.Close()
}

// isAllDigits reports whether s is a plain decimal number.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
