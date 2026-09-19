package web

import (
	"sync"
	"time"
)

// Outbound queue policy. Every connected browser has its own queue and writer
// goroutine, so a slow, stuck or vanished socket can never stall the mirror's
// lock, the agent's event stream or the other clients; a client whose backlog
// grows past the cap is dropped instead, and its page reconnects with a fresh
// snapshot.
const (
	// maxClientQueueBytes bounds the live frames waiting for one browser. The
	// history snapshot itself is not counted: it is bounded by the scrollback,
	// which the server holds anyway (see enqueueReplay).
	maxClientQueueBytes = 32 << 20
	// clientPingInterval is how often an otherwise idle connection is pinged.
	// The browser answers with a pong (see wsConn.readMessage), so a socket
	// whose page went away without a close frame is noticed instead of being
	// kept open forever.
	clientPingInterval = 30 * time.Second
)

// client is one connected browser: the socket plus a bounded outbound queue
// drained by its own goroutine.
//
// Writing on the client's own goroutine is what keeps a large history replay
// out of the mirror's critical section. Registering a client and snapshotting
// the scrollback happen under s.mu (so an event published concurrently is
// delivered exactly once — either inside the snapshot or live after it), while
// marshalling and socket writes happen here, after the lock is released.
type client struct {
	id   int
	conn *wsConn

	mu sync.Mutex
	// queue holds the frames waiting for the socket, oldest first; queued is
	// their total size.
	queue  [][]byte
	queued int
	// deferred holds the live frames published while the snapshot was still
	// being queued (defBytes is their total size). They are appended behind the
	// snapshot by finishReplay, so the page always receives the snapshot first.
	deferred [][]byte
	defBytes int
	// replaying is true from registration until the snapshot has been queued.
	replaying bool
	closed    bool

	// signal wakes the writer after a frame was queued (buffer 1, so queueing
	// never blocks on the writer).
	signal chan struct{}
	// done is closed once, by close, to stop the writer.
	done chan struct{}
	once sync.Once
}

// newClient wraps a socket that is about to be registered. The client starts in
// the replaying state; addClient queues the history snapshot and ends the phase
// with finishReplay.
func newClient(id int, conn *wsConn) *client {
	return &client{
		id:        id,
		conn:      conn,
		replaying: true,
		signal:    make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

// enqueue accepts one live frame. It never blocks and never touches the socket:
// a closed client, or one more than maxClientQueueBytes behind, is refused and
// the caller drops it (its page reconnects). Frames that arrive while the
// snapshot is still being queued are held back so they cannot overtake it.
func (c *client) enqueue(data []byte) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	if c.replaying {
		if c.defBytes+len(data) > maxClientQueueBytes {
			c.mu.Unlock()
			return false
		}
		c.deferred = append(c.deferred, data)
		c.defBytes += len(data)
	} else {
		if c.queued+len(data) > maxClientQueueBytes {
			c.mu.Unlock()
			return false
		}
		c.queue = append(c.queue, data)
		c.queued += len(data)
	}
	c.mu.Unlock()
	c.wake()
	return true
}

// enqueueReplay queues one frame of the history snapshot itself. The snapshot
// is bounded by the scrollback (which the server already holds), so it is not
// subject to the backlog cap; a refused frame only means the client is gone.
func (c *client) enqueueReplay(data []byte) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	c.queue = append(c.queue, data)
	c.queued += len(data)
	c.mu.Unlock()
	c.wake()
	return true
}

// finishReplay ends the snapshot phase and queues whatever the agent published
// meanwhile, in arrival order, behind the snapshot.
func (c *client) finishReplay() {
	c.mu.Lock()
	if c.closed {
		c.deferred, c.defBytes = nil, 0
		c.mu.Unlock()
		return
	}
	if c.replaying {
		c.replaying = false
		for _, data := range c.deferred {
			c.queue = append(c.queue, data)
			c.queued += len(data)
		}
		c.deferred, c.defBytes = nil, 0
	}
	c.mu.Unlock()
	c.wake()
}

// wake nudges the writer; it never blocks.
func (c *client) wake() {
	select {
	case c.signal <- struct{}{}:
	default:
	}
}

// close stops the writer and closes the socket. It is safe to call from any
// goroutine and repeatedly: the read loop's teardown, the backlog cap and the
// server shutdown all use it.
func (c *client) close() {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.queue, c.queued = nil, 0
		c.deferred, c.defBytes = nil, 0
		c.mu.Unlock()
		close(c.done)
		_ = c.conn.Close()
	})
}

// writeLoop drains the queue on the client's own goroutine and pings the socket
// while it is idle. A failed write closes the connection, which ends the read
// loop of its handler and unregisters the client.
func (c *client) writeLoop() {
	ticker := time.NewTicker(clientPingInterval)
	defer ticker.Stop()
	for {
		if data, ok := c.take(); ok {
			if err := c.conn.writeText(data); err != nil {
				c.close()
				return
			}
			continue
		}
		select {
		case <-c.done:
			return
		case <-c.signal:
		case <-ticker.C:
			if err := c.conn.writeFrame(opPing, nil); err != nil {
				c.close()
				return
			}
		}
	}
}

// take removes the oldest queued frame, if there is one.
func (c *client) take() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) == 0 {
		return nil, false
	}
	data := c.queue[0]
	c.queue = c.queue[1:]
	c.queued -= len(data)
	return data, true
}
