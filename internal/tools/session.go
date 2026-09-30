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

// Session status values. A session whose process exited is "done"; while its
// exit state (the output buffered at the exit plus the exit code) has not been
// handed over yet, list reports it as a "zombie" (see ProcessSession.Zombie).
const (
	sessionStatusRunning = "running"
	sessionStatusDone    = "done"
	sessionStatusZombie  = "zombie"
)

// zombieTTL is how long an exited session nobody collected is kept: its last
// output and its exit code stay retrievable for this long, then the session is
// dropped (see SessionManager.cleanupOldSessions).
const zombieTTL = 24 * time.Hour

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

	// exitAt is the wall clock at which the process was marked done — the
	// anchor of zombieTTL — and collected marks the exit state as handed over
	// to a caller (see markCollected).
	exitAt    int64
	collected bool

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
	// ExitCode is set once the process exited (nil while it is still running).
	ExitCode *int `json:"exit_code"`
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

// TakeOutput hands the whole buffered output over and empties the buffer: every
// call reports what the child has written since the previous one, whether or not
// it has exited, including the line it is still repainting (see sessionOutput).
// Nothing is withheld, and nothing already handed over is reported again.
func (s *ProcessSession) TakeOutput() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output.take()
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
	if s.Status != sessionStatusRunning || s.proc == nil {
		s.mu.Unlock()
		return
	}
	cmd := s.proc
	s.mu.Unlock()

	// Kill the whole tree, not just the shell: a background child of the
	// script must not outlive its session.
	_ = proc.Kill(cmd)

	s.mu.Lock()
	s.markDoneLocked(-1)
	s.mu.Unlock()
	s.signalDone()
}

// markDoneLocked records the exit under the held lock. The first caller wins, so
// the session keeps the exit code of whoever ended it first: for a killed
// session that is the kill's -1 rather than whatever the reaper reports for the
// terminated tree.
func (s *ProcessSession) markDoneLocked(code int) {
	if s.Status != sessionStatusRunning {
		return
	}
	s.Status = sessionStatusDone
	s.ExitCode = code
	s.exitAt = time.Now().Unix()
}

// markCollected records that the session's exit state (the output taken just now
// plus the exit code) has been handed over to a caller, and reports whether that
// was the last thing the session had to offer: a done session lives in the pool
// only until this happens (see SessionManager.Collect), while a running one
// still has to be polled or killed.
func (s *ProcessSession) markCollected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Status != sessionStatusDone {
		return false
	}
	s.collected = true
	return true
}

// Zombie reports whether the process has exited while its exit state has not
// been handed over yet: the session stays in the pool so a later poll can still
// deliver the last output and the exit code (see zombieTTL for how long).
func (s *ProcessSession) Zombie() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.zombieLocked()
}

// zombieLocked is Zombie under the held lock.
func (s *ProcessSession) zombieLocked() bool {
	return s.Status == sessionStatusDone && !s.collected
}

// expiredAt reports whether the session is a zombie whose exit state has been
// waiting for collection since before cutoff.
func (s *ProcessSession) expiredAt(cutoff int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.zombieLocked() && s.exitAt < cutoff
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
	return s.Status == sessionStatusDone
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

// Write sends data to the process stdin.
func (s *ProcessSession) Write(data string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Status != sessionStatusRunning {
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
	if s.Status != sessionStatusRunning {
		s.mu.Unlock()
		return ErrSessionDone
	}
	cmd := s.proc
	s.markDoneLocked(-1)
	s.mu.Unlock()

	// The whole tree goes down: the shell plus everything it spawned.
	_ = proc.Kill(cmd)
	s.signalDone()
	return nil
}

// ToSessionInfo snapshots the session for listing. A done session whose exit
// state has not been collected yet is reported as a zombie: it is still in the
// pool only to be polled, and its exit code is already known.
func (s *ProcessSession) ToSessionInfo() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := SessionInfo{
		ID:        s.ID,
		Command:   s.Command,
		Status:    s.Status,
		PID:       s.PID,
		StartedAt: s.StartTime,
	}
	if s.Status == sessionStatusDone {
		if s.zombieLocked() {
			info.Status = sessionStatusZombie
		}
		code := s.ExitCode
		info.ExitCode = &code
	}
	return info
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

// cleanupOldSessions drops the zombies nobody collected within zombieTTL: a
// finished session is kept so its last output and its exit code can still be
// polled, but not forever.
func (sm *SessionManager) cleanupOldSessions() {
	cutoff := time.Now().Add(-zombieTTL).Unix()
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for id, session := range sm.sessions {
		if session.expiredAt(cutoff) {
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

// Collect releases a session whose exit state (the output just handed over plus
// the exit code) has been reported to a caller. A done session exists in the
// pool only to be collected, so it leaves here; a running session stays — it
// still has to be polled or killed.
func (sm *SessionManager) Collect(session *ProcessSession) {
	if !session.markCollected() {
		return
	}
	sm.Remove(session.ID)
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
