package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// errStdioClosed is returned when the transport has been closed.
var errStdioClosed = errors.New("mcp: transport closed")

// cappedBuffer is a concurrency-safe writer that keeps at most max bytes. It is
// used to capture a bounded slice of a child process's stderr for diagnostics.
type cappedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func newCappedBuffer(max int) *cappedBuffer { return &cappedBuffer{max: max} }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if remaining := b.max - b.buf.Len(); remaining > 0 {
		if len(p) <= remaining {
			b.buf.Write(p)
		} else {
			b.buf.Write(p[:remaining])
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.buf.String())
}

// stdioTransport speaks newline-delimited JSON-RPC over a child process's
// stdin/stdout. A background reader demultiplexes responses by id so
// server-initiated notifications do not block pending requests.
type stdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *cappedBuffer

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[int64]chan *response
	closed  bool
	readErr error
	done    chan struct{}
}

func newStdioTransport(command string, args, env []string) (*stdioTransport, error) {
	cmd := childCommand(command, args)
	cmd.Env = env
	cmd.Stderr = newCappedBuffer(8 * 1024)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: open stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp: open stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp: start %q: %w", command, err)
	}
	t := &stdioTransport{
		cmd:     cmd,
		stdin:   stdin,
		stderr:  cmd.Stderr.(*cappedBuffer),
		pending: make(map[int64]chan *response),
		done:    make(chan struct{}),
	}
	go t.readLoop(stdout)
	return t, nil
}

// childCommand builds the exec.Cmd. On Windows a .cmd/.bat launcher (for
// example npx.cmd) must be run through cmd.exe.
func childCommand(command string, args []string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		if path, err := exec.LookPath(command); err == nil {
			switch strings.ToLower(filepath.Ext(path)) {
			case ".cmd", ".bat":
				return exec.Command("cmd", append([]string{"/c", path}, args...)...)
			}
			return exec.Command(path, args...)
		}
	}
	return exec.Command(command, args...)
}

func (t *stdioTransport) readLoop(stdout io.Reader) {
	defer close(t.done)
	reader := bufio.NewReaderSize(stdout, 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var resp response
			if json.Unmarshal(trimmed, &resp) == nil {
				t.deliver(&resp)
			}
		}
		if err != nil {
			t.finish(err)
			return
		}
	}
}

// deliver routes a response to the pending request that is waiting for it.
func (t *stdioTransport) deliver(resp *response) {
	id, ok := parseMessageID(resp.ID)
	if !ok {
		return // server-initiated notification/request: not a response
	}
	t.mu.Lock()
	ch := t.pending[id]
	delete(t.pending, id)
	t.mu.Unlock()
	if ch != nil {
		ch <- resp
	}
}

func (t *stdioTransport) finish(err error) {
	t.mu.Lock()
	if t.readErr == nil {
		t.readErr = err
	}
	t.pending = make(map[int64]chan *response)
	t.mu.Unlock()
}

func (t *stdioTransport) readFailure() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	msg := "server connection closed"
	if t.readErr != nil && !errors.Is(t.readErr, io.EOF) {
		msg = "server connection closed: " + t.readErr.Error()
	}
	if detail := t.stderr.String(); detail != "" {
		msg += " (stderr: " + detail + ")"
	}
	return errors.New("mcp: " + msg)
}

func (t *stdioTransport) write(req *request) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := t.stdin.Write(data); err != nil {
		return fmt.Errorf("mcp: write: %w", err)
	}
	return nil
}

func (t *stdioTransport) roundTrip(ctx context.Context, req *request) (*response, error) {
	ch := make(chan *response, 1)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, errStdioClosed
	}
	t.pending[req.ID] = ch
	t.mu.Unlock()

	if err := t.write(req); err != nil {
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp == nil {
			return nil, t.readFailure()
		}
		return resp, nil
	case <-ctx.Done():
		t.mu.Lock()
		delete(t.pending, req.ID)
		t.mu.Unlock()
		return nil, ctx.Err()
	case <-t.done:
		return nil, t.readFailure()
	}
}

func (t *stdioTransport) notify(ctx context.Context, req *request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return t.write(req)
}

func (t *stdioTransport) close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	_ = t.stdin.Close()
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	_ = t.cmd.Wait()
	return nil
}
