package tools

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// sleepThenPrint returns a script that exits after seconds, printing text just
// before it does.
func sleepThenPrint(seconds int, text string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("Start-Sleep -Seconds %d; Write-Output %s", seconds, text)
	}
	return fmt.Sprintf("sleep %d; echo %s", seconds, text)
}

// sleepThenPrintExit is sleepThenPrint with an explicit exit code, so a test can
// tell a real exit status from the -1 of a killed session.
func sleepThenPrintExit(seconds int, text string, code int) string {
	return fmt.Sprintf("%s; exit %d", sleepThenPrint(seconds, text), code)
}

// waitForSessionExit waits until the session's process has exited, which is what
// turns it into a zombie (or, when a call is waiting, into a collected session).
func waitForSessionExit(t *testing.T, session *ProcessSession) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !session.IsDone() {
		if time.Now().After(deadline) {
			t.Fatal("the process never exited")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// backgroundCommand starts a command that is still running when the wait window
// closes, and returns its session id.
func backgroundCommand(t *testing.T, engine *ExecEngine) string {
	t.Helper()
	return startBackground(t, engine, sleepThenPrint(2, "late"))
}

// startBackground starts command with a one-second wait window, so it is left in
// the background, and returns its session id.
func startBackground(t *testing.T, engine *ExecEngine, command string) string {
	t.Helper()
	tool := NewExecCommandTool(engine)
	res := tool.Execute(context.Background(), map[string]any{
		"command": command, "wait_timeout": 1,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	sessionID := sessionIDFromResult(t, res.ForLLM)
	if sessionID == "" {
		t.Fatalf("the command did not stay in the background: %s", res.ForLLM)
	}
	return sessionID
}

// TestManageSessionPollCollectsTheExitState pins the zombie contract on the poll
// side: a process that exited after the wait window keeps its last output and
// its exit code in the pool, so a later poll still reports them — and that poll
// is what releases the session, since nothing is left to keep it for.
func TestManageSessionPollCollectsTheExitState(t *testing.T) {
	engine := NewExecEngine(60, 1, true)
	defer engine.Close()
	manageTool := NewManageSessionTool(engine)

	sessionID := backgroundCommand(t, engine)
	session, err := engine.sessions.Get(sessionID)
	if err != nil {
		t.Fatalf("session %s is gone: %v", sessionID, err)
	}
	waitForSessionExit(t, session)

	poll := manageTool.Execute(context.Background(), map[string]any{
		"action": "poll", "session_id": sessionID,
	})
	if poll.IsError {
		t.Fatalf("poll error: %s", poll.ForLLM)
	}
	cr := commandResultFromResult(t, poll.ForLLM)
	if cr.Status != statusCompleted || cr.ExitCode == nil || *cr.ExitCode != 0 {
		t.Fatalf("poll = %s, want the exit state of the finished process", poll.ForLLM)
	}
	if !strings.Contains(cr.Output, "late") {
		t.Fatalf("poll = %s, want the output written before the exit", poll.ForLLM)
	}
	if _, err := engine.sessions.Get(sessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("the collected session is still in the pool (err = %v)", err)
	}
}

// TestManageSessionListReportsZombies covers the list side: a finished session
// whose output nobody took yet is a zombie — still listed, with its exit code —
// and it leaves the pool once it is polled.
func TestManageSessionListReportsZombies(t *testing.T) {
	engine := NewExecEngine(60, 1, true)
	defer engine.Close()
	manageTool := NewManageSessionTool(engine)

	sessionID := backgroundCommand(t, engine)
	session, err := engine.sessions.Get(sessionID)
	if err != nil {
		t.Fatalf("session %s is gone: %v", sessionID, err)
	}
	waitForSessionExit(t, session)
	if !session.Zombie() {
		t.Fatal("the finished session is not reported as a zombie")
	}

	list := manageTool.Execute(context.Background(), map[string]any{"action": "list"})
	if list.IsError {
		t.Fatalf("list error: %s", list.ForLLM)
	}
	for _, want := range []string{sessionID, "status: zombie", "exit_code: 0"} {
		if !strings.Contains(list.ForLLM, want) {
			t.Fatalf("list = %s, want it to carry %q", list.ForLLM, want)
		}
	}

	// Polling takes the exit state, and the session leaves the pool with it.
	poll := manageTool.Execute(context.Background(), map[string]any{
		"action": "poll", "session_id": sessionID,
	})
	if cr := commandResultFromResult(t, poll.ForLLM); cr.Status != statusCompleted || !strings.Contains(cr.Output, "late") {
		t.Fatalf("poll = %s, want the zombie's last output", poll.ForLLM)
	}
	list = manageTool.Execute(context.Background(), map[string]any{"action": "list"})
	if !strings.Contains(list.ForLLM, "No background sessions.") {
		t.Fatalf("list = %s, want an empty pool after the poll", list.ForLLM)
	}
}

// TestSessionZombieSurvivesCleanupUntilPolled is the regression the pool rule
// fixes: a session that started long ago and has just exited — exactly the shape
// of a long-running background command — keeps its last output and exit code
// until it is polled. Cleaning up by start time dropped it 30 minutes after the
// start, so a poll that arrived later answered "session not found" and the exit
// state was lost.
func TestSessionZombieSurvivesCleanupUntilPolled(t *testing.T) {
	engine := NewExecEngine(60, 1, true)
	defer engine.Close()
	manageTool := NewManageSessionTool(engine)

	sessionID := backgroundCommand(t, engine)
	session, err := engine.sessions.Get(sessionID)
	if err != nil {
		t.Fatalf("session %s is gone: %v", sessionID, err)
	}
	waitForSessionExit(t, session)

	// The command had been running for a long time before it exited.
	session.mu.Lock()
	session.StartTime = time.Now().Add(-31 * time.Minute).Unix()
	session.mu.Unlock()
	engine.sessions.cleanupOldSessions()

	if _, err := engine.sessions.Get(sessionID); err != nil {
		t.Fatalf("the zombie was cleaned up before it was polled: %v", err)
	}
	poll := manageTool.Execute(context.Background(), map[string]any{
		"action": "poll", "session_id": sessionID,
	})
	if poll.IsError {
		t.Fatalf("poll error: %s", poll.ForLLM)
	}
	cr := commandResultFromResult(t, poll.ForLLM)
	if cr.Status != statusCompleted || !strings.Contains(cr.Output, "late") {
		t.Fatalf("poll = %s, want the zombie's last output and exit code", poll.ForLLM)
	}
}

// TestSessionManagerExpiresUnpolledZombies pins the ceiling: a zombie nobody
// polls is dropped once zombieTTL has passed since the exit, while a fresh
// zombie and a running session stay in the pool.
func TestSessionManagerExpiresUnpolledZombies(t *testing.T) {
	sm := NewSessionManager()
	defer sm.Stop()

	sm.Add(&ProcessSession{ID: "fresh", Status: sessionStatusDone, exitAt: time.Now().Unix()})
	sm.Add(&ProcessSession{
		ID:     "aged",
		Status: sessionStatusDone,
		exitAt: time.Now().Add(-zombieTTL - time.Minute).Unix(),
	})
	sm.Add(&ProcessSession{ID: "live", Status: sessionStatusRunning})

	sm.cleanupOldSessions()

	for id, want := range map[string]bool{"fresh": true, "aged": false, "live": true} {
		_, err := sm.Get(id)
		if kept := err == nil; kept != want {
			t.Fatalf("session %s kept = %t, want %t (err = %v)", id, kept, want, err)
		}
	}
}

// TestExecCommandCompletionLeavesNoSession covers the other half of the rule:
// when the process finishes inside the wait window, its output and exit code are
// already in the answer, so the session is released immediately instead of
// waiting in the pool as a zombie.
func TestExecCommandCompletionLeavesNoSession(t *testing.T) {
	command := "echo quick"
	if runtime.GOOS == "windows" {
		command = "Write-Output quick"
	}

	engine := NewExecEngine(60, 10, true)
	defer engine.Close()
	tool := NewExecCommandTool(engine)

	res := tool.Execute(context.Background(), map[string]any{"command": command})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if cr := commandResultFromResult(t, res.ForLLM); cr.Status != statusCompleted {
		t.Fatalf("result = %s, want the process to have finished", res.ForLLM)
	}
	if list := engine.sessions.List(); len(list) != 0 {
		t.Fatalf("the collected session is still in the pool: %+v", list)
	}
}

// TestManageSessionKillReportsTheOutputOfARunningProcess covers the running
// process branch of kill: the tree is terminated, the session is released, and
// what the process had printed until then is handed over with the answer instead
// of being dropped with the session.
func TestManageSessionKillReportsTheOutputOfARunningProcess(t *testing.T) {
	command := "for i in 1 2 3 4 5 6 7 8 9 10; do echo tick $i; sleep 0.3; done"
	if runtime.GOOS == "windows" {
		command = "1..10 | ForEach-Object { Write-Output \"tick $_\"; Start-Sleep -Milliseconds 300 }"
	}

	engine := NewExecEngine(60, 1, true)
	defer engine.Close()
	manageTool := NewManageSessionTool(engine)

	sessionID := startBackground(t, engine, command)
	// Wait until the process holds output the kill answer has to carry.
	waitForBufferedOutput(t, engine, 5*time.Second)

	kill := manageTool.Execute(context.Background(), map[string]any{
		"action": "kill", "session_id": sessionID,
	})
	if kill.IsError {
		t.Fatalf("kill error: %s", kill.ForLLM)
	}
	cr := commandResultFromResult(t, kill.ForLLM)
	if cr.Status != statusCompleted {
		t.Fatalf("kill = %s, want the running process to be terminated", kill.ForLLM)
	}
	if cr.ExitCode == nil || *cr.ExitCode != -1 {
		t.Fatalf("kill = %s, want exit_code -1: the process was terminated, it did not exit on its own", kill.ForLLM)
	}
	if !strings.Contains(cr.Output, "terminated") || !strings.Contains(cr.Output, "tick") {
		t.Fatalf("kill = %s, want the termination note and the output printed until then", kill.ForLLM)
	}
	if _, err := engine.sessions.Get(sessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("the killed session is still in the pool (err = %v)", err)
	}
}

// TestManageSessionKillReportsAProcessThatAlreadyExited covers the other branch
// of kill: there is no tree to terminate, so the call is a failure — but it still
// hands the exit state over (the output written before the exit plus the exit
// code) and releases the session, instead of answering with a bare error that
// would lose them.
func TestManageSessionKillReportsAProcessThatAlreadyExited(t *testing.T) {
	engine := NewExecEngine(60, 1, true)
	defer engine.Close()
	manageTool := NewManageSessionTool(engine)

	sessionID := startBackground(t, engine, sleepThenPrintExit(2, "late", 3))
	session, err := engine.sessions.Get(sessionID)
	if err != nil {
		t.Fatalf("session %s is gone: %v", sessionID, err)
	}
	waitForSessionExit(t, session)

	kill := manageTool.Execute(context.Background(), map[string]any{
		"action": "kill", "session_id": sessionID,
	})
	if !kill.IsError {
		t.Fatalf("kill = %s, want a failure: the process had already exited", kill.ForLLM)
	}
	cr := commandResultFromResult(t, kill.ForLLM)
	if cr.Status != statusFailed || cr.ExitCode == nil || *cr.ExitCode != 3 {
		t.Fatalf("kill = %s, want a failure carrying the real exit code", kill.ForLLM)
	}
	if !strings.Contains(cr.Output, "process already exited with code 3") || !strings.Contains(cr.Output, "late") {
		t.Fatalf("kill = %s, want the reason and the output written before the exit", kill.ForLLM)
	}
	if _, err := engine.sessions.Get(sessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("the released session is still in the pool (err = %v)", err)
	}
}
