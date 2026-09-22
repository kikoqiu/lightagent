package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os/exec"
	"sync"
	"time"

	"lightagent/internal/proc"
)

// Session lifecycle errors.
var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionDone     = errors.New("session already completed")
	ErrNoStdin         = errors.New("no stdin available")
)

// ProcessSession tracks one running (or finished) shell process.
type ProcessSession struct {
	mu        sync.Mutex
	ID        string
	PID       int
	Command   string
	StartTime int64
	ExitCode  int
	Status    string // "running" or "done"

	proc  *exec.Cmd
	stdin io.WriteCloser

	output sessionOutput // terminal-accurate, bounded child-output buffer

	doneCh     chan struct{}
	doneOnce   sync.Once
	chInitOnce sync.Once
}

// SessionInfo is a snapshot used by manage_session list.
type SessionInfo struct {
	ID        string `json:"id"`
	Command   string `json:"command"`
	Status    string `json:"status"`
	PID       int    `json:"pid"`
	StartedAt int64  `json:"started_at"`
}

// initChannels creates the done channel lazily, so a session assembled by hand
// (as a test may do) still works.
func (s *ProcessSession) initChannels() {
	s.chInitOnce.Do(func() {
		if s.doneCh == nil {
			s.doneCh = make(chan struct{})
		}
	})
}

func (s *ProcessSession) signalDone() {
	s.initChannels()
	s.doneOnce.Do(func() { close(s.doneCh) })
}

// appendOutput adds raw bytes to the line-aware, bounded buffer.
func (s *ProcessSession) appendOutput(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.output.append(p)
}

// flushOutput releases the line the buffer still held back because the child was
// rewriting it in place. The output writers call it once the child is gone: no
// byte can rewrite the line anymore, so what it holds is what the child left on
// screen (see sessionOutput).
func (s *ProcessSession) flushOutput() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.output.flush()
}

// startWatchdog force-terminates the process once timeout elapses.
func (s *ProcessSession) startWatchdog(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	s.initChannels()
	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			s.expireAfterTimeout()
		case <-s.doneCh:
		}
	}()
}

func (s *ProcessSession) expireAfterTimeout() {
	s.mu.Lock()
	if s.Status != "running" || s.proc == nil {
		s.mu.Unlock()
		return
	}
	cmd := s.proc
	s.mu.Unlock()

	// Kill the whole tree, not just the shell: a background child of the
	// script must not outlive its session.
	_ = proc.Kill(cmd)

	s.mu.Lock()
	if s.Status == "running" {
		s.Status = "done"
		s.ExitCode = -1
	}
	s.mu.Unlock()
	s.signalDone()
}

// WaitForExit blocks until the process exits or the timeout elapses. Output does
// not end the wait: wait_timeout is the window a caller grants the program to
// finish, so everything it writes in the meantime accumulates and is reported in
// one delta. A non-positive timeout only reports the current state.
func (s *ProcessSession) WaitForExit(timeout time.Duration) (done bool, timedOut bool) {
	done, timedOut, _ = s.WaitForExitContext(context.Background(), timeout)
	return done, timedOut
}

// WaitForExitContext is WaitForExit with cancellation: it reports canceled when
// ctx is done before the process exits or the timeout elapses.
func (s *ProcessSession) WaitForExitContext(ctx context.Context, timeout time.Duration) (done bool, timedOut bool, canceled bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.initChannels()
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		finished := s.Status == "done"
		s.mu.Unlock()
		if finished {
			return true, false, false
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, true, false
		}
		select {
		case <-ctx.Done():
			return false, false, true
		case <-s.doneCh:
		case <-time.After(remaining):
		}
	}
}

// IsDone reports whether the process has exited or was killed.
func (s *ProcessSession) IsDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Status == "done"
}

// GetStatus returns the current status string.
func (s *ProcessSession) GetStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Status
}

// GetExitCode returns the recorded exit code.
func (s *ProcessSession) GetExitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ExitCode
}

// ReadIncremental returns the unconsumed output delta and advances the cursor.
func (s *ProcessSession) ReadIncremental() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output.read()
}

// ReadAllPending returns the pending delta plus whether the process exited.
func (s *ProcessSession) ReadAllPending() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	done := s.Status == "done"
	return s.output.read(), done
}

// Write sends data to the process stdin.
func (s *ProcessSession) Write(data string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Status != "running" {
		return ErrSessionDone
	}
	if s.stdin == nil {
		return ErrNoStdin
	}
	_, err := io.WriteString(s.stdin, data)
	return err
}

// Kill terminates the process tree and marks the session done.
func (s *ProcessSession) Kill() error {
	s.mu.Lock()
	if s.Status != "running" {
		s.mu.Unlock()
		return ErrSessionDone
	}
	cmd := s.proc
	s.Status = "done"
	s.ExitCode = -1
	s.mu.Unlock()

	// The whole tree goes down: the shell plus everything it spawned.
	_ = proc.Kill(cmd)
	s.signalDone()
	return nil
}

// ToSessionInfo snapshots the session for listing.
func (s *ProcessSession) ToSessionInfo() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionInfo{
		ID:        s.ID,
		Command:   s.Command,
		Status:    s.Status,
		PID:       s.PID,
		StartedAt: s.StartTime,
	}
}

// SessionManager is a thread-safe registry of process sessions.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*ProcessSession
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewSessionManager creates a manager and starts its cleanup goroutine.
func NewSessionManager() *SessionManager {
	sm := &SessionManager{
		sessions: make(map[string]*ProcessSession),
		stopCh:   make(chan struct{}),
	}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-sm.stopCh:
				return
			case <-ticker.C:
				sm.cleanupOldSessions()
			}
		}
	}()
	return sm
}

// Stop shuts down the background cleanup goroutine.
func (sm *SessionManager) Stop() {
	sm.stopOnce.Do(func() { close(sm.stopCh) })
}

// KillAll terminates every session that is still running, so no process tree
// outlives the manager. The engine calls it when lightagent exits.
func (sm *SessionManager) KillAll() {
	sm.mu.RLock()
	running := make([]*ProcessSession, 0, len(sm.sessions))
	for _, session := range sm.sessions {
		if !session.IsDone() {
			running = append(running, session)
		}
	}
	sm.mu.RUnlock()

	// A kill may briefly wait for its tree to exit on its own, so the
	// independent trees are terminated in parallel and the exit stays short.
	var wg sync.WaitGroup
	for _, session := range running {
		wg.Add(1)
		go func(session *ProcessSession) {
			defer wg.Done()
			_ = session.Kill()
		}(session)
	}
	wg.Wait()
}

// cleanupOldSessions removes done sessions older than 30 minutes.
func (sm *SessionManager) cleanupOldSessions() {
	cutoff := time.Now().Add(-30 * time.Minute).Unix()
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for id, session := range sm.sessions {
		if session.IsDone() && session.StartTime < cutoff {
			delete(sm.sessions, id)
		}
	}
}

// Add registers a session.
func (sm *SessionManager) Add(session *ProcessSession) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.sessions[session.ID] = session
}

// Get returns a session by id.
func (sm *SessionManager) Get(sessionID string) (*ProcessSession, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	session, ok := sm.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return session, nil
}

// Remove deletes a session from the registry.
func (sm *SessionManager) Remove(sessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.sessions, sessionID)
}

// List snapshots all sessions.
func (sm *SessionManager) List() []SessionInfo {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	result := make([]SessionInfo, 0, len(sm.sessions))
	for _, session := range sm.sessions {
		result = append(result, session.ToSessionInfo())
	}
	return result
}

// generateSessionID returns a short random hex identifier.
func generateSessionID() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return time.Now().Format("150405.000")
	}
	return hex.EncodeToString(buf)
}
