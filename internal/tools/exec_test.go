package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestExecCommandCompletes verifies the synchronous fast path of exec_command.
func TestExecCommandCompletes(t *testing.T) {
	command := "echo hello"
	if runtime.GOOS == "windows" {
		command = "Write-Output hello"
	}

	engine := NewExecEngine(60, 5, true)
	defer engine.Close()
	tool := NewExecCommandTool(engine)

	res := tool.Execute(context.Background(), map[string]any{"script": command})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, `"status":"completed"`) {
		t.Fatalf("status not completed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "hello") {
		t.Fatalf("output missing: %s", res.ForLLM)
	}
}

// TestExecCommandCollapsesCRProgress covers the noise a refresh-driven program
// makes: every write repaints the same terminal line with a leading CR, so only
// that line's last version may reach the model — never one line per refresh.
func TestExecCommandCollapsesCRProgress(t *testing.T) {
	// Each update is its own write, the way a progress bar repaints a line: a CR
	// back to column 0, an erase in line, then the new text.
	command := "printf '\\r\\033[KDownloading 1%%'; sleep 0.05; " +
		"printf '\\r\\033[KDownloading 2%%'; sleep 0.05; " +
		"printf '\\r\\033[KDownloading 99%%'; printf '\\nAll done\\n'"
	if runtime.GOOS == "windows" {
		// [Console]::OpenStandardOutput() writes the bytes straight to the pipe,
		// with no console encoding in between (see hostCodePageBytesCommand).
		command = "$out = [Console]::OpenStandardOutput(); $esc = [char]27; " +
			"foreach ($text in @(\"`r$esc[KDownloading 1%\", \"`r$esc[KDownloading 2%\", \"`r$esc[KDownloading 99%\", \"`nAll done`n\")) { " +
			"$bytes = [Text.Encoding]::ASCII.GetBytes($text); " +
			"$out.Write($bytes, 0, $bytes.Length); $out.Flush(); Start-Sleep -Milliseconds 50 }"
	}

	engine := NewExecEngine(60, 20, true)
	defer engine.Close()
	tool := NewExecCommandTool(engine)

	res := tool.Execute(context.Background(), map[string]any{"script": command})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	// The line's last version, immediately followed by the line printed after
	// it: the refreshes in between are gone.
	if !strings.Contains(res.ForLLM, `Downloading 99%\nAll done`) {
		t.Fatalf("the collapsed progress output is missing: %s", res.ForLLM)
	}
	for _, stale := range []string{"Downloading 1%", "Downloading 2%"} {
		if strings.Contains(res.ForLLM, stale) {
			t.Fatalf("a refreshed line reached the model: %s", res.ForLLM)
		}
	}
	// The erase sequence is consumed by the buffer, not forwarded to the model.
	for _, hidden := range []string{`\u001b`, "[K"} {
		if strings.Contains(res.ForLLM, hidden) {
			t.Fatalf("the erase sequence reached the model: %s", res.ForLLM)
		}
	}

	// When the process was still running as the wait window closed, the polls
	// that follow report the paint it had reached — the current state of the
	// line, one per poll, never a state older than one already reported.
	sessionID := sessionIDFromResult(t, res.ForLLM)
	if sessionID == "" {
		return
	}
	manageTool := NewManageSessionTool(engine)
	last := 0
	// The line printed after the repaints may arrive with the hand-over that
	// reports the exit, so the whole delta is checked at the end.
	var delta strings.Builder
	deadline := time.Now().Add(30 * time.Second)
	for {
		poll := manageTool.Execute(context.Background(), map[string]any{
			"action": "poll", "session_id": sessionID, "wait_timeout": 5,
		})
		if poll.IsError {
			t.Fatalf("poll error: %s", poll.ForLLM)
		}
		delta.WriteString(poll.ForLLM)
		if state := repaintNumber(t, poll.ForLLM, "Downloading"); state >= 0 {
			if state < last {
				t.Fatalf("an older state than the one already reported reached the model: %s", poll.ForLLM)
			}
			last = state
		}
		if strings.Contains(poll.ForLLM, `"status":"completed"`) {
			if !strings.Contains(delta.String(), "All done") {
				t.Fatalf("the line printed after the repaints is missing: %s", delta.String())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child never exited: %s", poll.ForLLM)
		}
	}
}

// TestExecCommandKeepsOverwrittenTail pins the terminal-accurate CR rule: a
// rewrite shorter than the text it covers leaves that text's tail behind, the way
// a terminal shows it (a CR moves the cursor, it does not erase).
func TestExecCommandKeepsOverwrittenTail(t *testing.T) {
	command := "printf 'abcdefgh\\rXY\\n'"
	if runtime.GOOS == "windows" {
		command = "[Console]::Out.Write(\"abcdefgh`rXY`n\"); [Console]::Out.Flush()"
	}

	engine := NewExecEngine(60, 20, true)
	defer engine.Close()
	tool := NewExecCommandTool(engine)

	res := tool.Execute(context.Background(), map[string]any{"script": command})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "XYcdefgh") {
		t.Fatalf("the overwritten line lost its tail: %s", res.ForLLM)
	}
}

// TestExecCommandBackgroundThenManageSession verifies that a command exceeding
// wait_timeout is backgrounded and can then be polled and killed.
func TestExecCommandBackgroundThenManageSession(t *testing.T) {
	command := "sleep 5; echo done"
	if runtime.GOOS == "windows" {
		command = "Start-Sleep -Seconds 5; Write-Output done"
	}

	engine := NewExecEngine(60, 1, true)
	defer engine.Close()
	execTool := NewExecCommandTool(engine)
	manageTool := NewManageSessionTool(engine)

	res := execTool.Execute(context.Background(), map[string]any{
		"script":       command,
		"wait_timeout": 1,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, `"status":"running"`) {
		t.Fatalf("expected a running session: %s", res.ForLLM)
	}

	sessionID := sessionIDFromResult(t, res.ForLLM)
	if sessionID == "" {
		t.Fatalf("no session_id in %s", res.ForLLM)
	}

	// Poll: the process is still sleeping, so it must remain running.
	poll := manageTool.Execute(context.Background(), map[string]any{
		"action": "poll", "session_id": sessionID, "wait_timeout": 1,
	})
	if poll.IsError {
		t.Fatalf("poll error: %s", poll.ForLLM)
	}
	if !strings.Contains(poll.ForLLM, `"status":"running"`) {
		t.Fatalf("expected running after poll: %s", poll.ForLLM)
	}

	// list must include the session.
	list := manageTool.Execute(context.Background(), map[string]any{"action": "list"})
	if list.IsError {
		t.Fatalf("list error: %s", list.ForLLM)
	}
	if !strings.Contains(list.ForLLM, sessionID) {
		t.Fatalf("session %s not listed: %s", sessionID, list.ForLLM)
	}

	// Kill must succeed.
	kill := manageTool.Execute(context.Background(), map[string]any{
		"action": "kill", "session_id": sessionID,
	})
	if kill.IsError {
		t.Fatalf("kill error: %s", kill.ForLLM)
	}
	if !strings.Contains(kill.ForLLM, "terminated") {
		t.Fatalf("unexpected kill output: %s", kill.ForLLM)
	}
}

// TestManageSessionPollReportsTheRepaintState covers the poll side of the
// terminal line model: a line that is still being repainted is published as its
// current state, so a reader watching a running child sees where the child got
// to instead of a silent buffer — and no call reports a state older than one
// already reported.
func TestManageSessionPollReportsTheRepaintState(t *testing.T) {
	// The child repaints one line for ~3s and prints a real line only afterwards,
	// so every call below lands inside the repainting phase.
	command := "i=0; while [ $i -lt 30 ]; do printf '\\rprogress %s' \"$i\"; i=$((i+1)); sleep 0.1; done; printf '\\ndone\\n'"
	if runtime.GOOS == "windows" {
		command = "$out = [Console]::OpenStandardOutput(); " +
			"1..30 | ForEach-Object { $bytes = [Text.Encoding]::ASCII.GetBytes(\"`rprogress $_\"); " +
			"$out.Write($bytes, 0, $bytes.Length); $out.Flush(); Start-Sleep -Milliseconds 100 }; " +
			"$bytes = [Text.Encoding]::ASCII.GetBytes(\"`ndone`n\"); $out.Write($bytes, 0, $bytes.Length)"
	}

	engine := NewExecEngine(60, 1, true)
	defer engine.Close()
	execTool := NewExecCommandTool(engine)
	manageTool := NewManageSessionTool(engine)

	res := execTool.Execute(context.Background(), map[string]any{"script": command})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, `"status":"running"`) {
		t.Fatalf("the command did not stay in the background: %s", res.ForLLM)
	}
	// The handoff reports the state the repaint had reached: the child paints for
	// ~3s while the window above was 1s, so the last state is not there yet.
	state := repaintNumber(t, res.ForLLM, "progress")
	if state < 0 || state > 29 {
		t.Fatalf("the handoff does not report the current state of the repaint: %s", res.ForLLM)
	}
	sessionID := sessionIDFromResult(t, res.ForLLM)
	if sessionID == "" {
		t.Fatalf("no session_id in %s", res.ForLLM)
	}

	// Every poll reports the state the repaint had reached as its own line, and
	// the states only move forward: a refresh no call observed never reaches the
	// model. The line printed after the repaints arrives with the hand-over that
	// reports the exit.
	deadline := time.Now().Add(30 * time.Second)
	var all strings.Builder
	for {
		poll := manageTool.Execute(context.Background(), map[string]any{
			"action": "poll", "session_id": sessionID, "wait_timeout": 1,
		})
		if poll.IsError {
			t.Fatalf("poll error: %s", poll.ForLLM)
		}
		all.WriteString(poll.ForLLM)
		if next := repaintNumber(t, poll.ForLLM, "progress"); next >= 0 {
			if next < state {
				t.Fatalf("an older state than the one already reported reached the model: %s", poll.ForLLM)
			}
			state = next
		}
		if strings.Contains(poll.ForLLM, `"status":"completed"`) {
			if !strings.Contains(all.String(), "done") {
				t.Fatalf("the line printed after the repaints is missing: %s", all.String())
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child never exited: %s", poll.ForLLM)
		}
	}
}

// TestManageSessionPollWaitsForExit pins the wait contract of poll: wait_timeout
// is the window granted to the process to finish, so output written on the way
// does not end the wait. An implementation that returned as soon as a line
// arrived would report status=running with only that line.
func TestManageSessionPollWaitsForExit(t *testing.T) {
	// A line, a pause, a second line, another pause, then exit: the poll below
	// starts during the first pause and the child is still alive after writing
	// the second line, so only a wait for the exit reports completed.
	command := "printf 'line one\\n'; sleep 1; printf 'line two\\n'; sleep 1"
	if runtime.GOOS == "windows" {
		command = "[Console]::Out.Write(\"line one`n\"); [Console]::Out.Flush(); Start-Sleep -Seconds 1; " +
			"[Console]::Out.Write(\"line two`n\"); [Console]::Out.Flush(); Start-Sleep -Seconds 1"
	}

	engine := NewExecEngine(60, 1, true)
	defer engine.Close()
	execTool := NewExecCommandTool(engine)
	manageTool := NewManageSessionTool(engine)

	res := execTool.Execute(context.Background(), map[string]any{"script": command, "wait_timeout": 1})
	sessionID := sessionIDFromResult(t, res.ForLLM)
	if sessionID == "" {
		t.Fatalf("the command did not stay in the background: %s", res.ForLLM)
	}

	poll := manageTool.Execute(context.Background(), map[string]any{
		"action": "poll", "session_id": sessionID, "wait_timeout": 10,
	})
	if poll.IsError {
		t.Fatalf("poll error: %s", poll.ForLLM)
	}
	if !strings.Contains(poll.ForLLM, `"status":"completed"`) {
		t.Fatalf("the poll returned before the process exited: %s", poll.ForLLM)
	}
	if !strings.Contains(poll.ForLLM, `line two`) {
		t.Fatalf("the delta does not hold what the child wrote: %s", poll.ForLLM)
	}
}

// TestManageSessionPollInterruptLeavesTheProcessRunning covers interrupting a
// long poll: the wait ends the way a wait_timeout ends — the output collected so
// far is reported with status=running and a warning saying the poll was
// interrupted — while the process keeps running, so a later poll still follows it
// to its exit. A poll only observes a session, so an interrupt must never kill
// what it watches.
func TestManageSessionPollInterruptLeavesTheProcessRunning(t *testing.T) {
	command := "for i in 1 2 3 4 5 6 7 8 9 10; do echo tick $i; sleep 0.3; done"
	if runtime.GOOS == "windows" {
		command = "1..10 | ForEach-Object { Write-Output \"tick $_\"; Start-Sleep -Milliseconds 300 }"
	}

	engine := NewExecEngine(60, 1, true)
	defer engine.Close()
	execTool := NewExecCommandTool(engine)
	manageTool := NewManageSessionTool(engine)

	// wait_timeout 1 backgrounds the command, which keeps printing for ~3s.
	res := execTool.Execute(context.Background(), map[string]any{"script": command, "wait_timeout": 1})
	sessionID := sessionIDFromResult(t, res.ForLLM)
	if sessionID == "" {
		t.Fatalf("the command did not stay in the background: %s", res.ForLLM)
	}
	session, err := engine.sessions.Get(sessionID)
	if err != nil {
		t.Fatalf("session %s is gone: %v", sessionID, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *Result, 1)
	go func() {
		done <- manageTool.Execute(ctx, map[string]any{
			"action": "poll", "session_id": sessionID, "wait_timeout": 30,
		})
	}()

	// The poll is waiting for the exit; interrupt it once it holds output, so
	// the interrupted answer has something to report.
	waitForBufferedOutput(t, engine, 5*time.Second)
	cancel()

	select {
	case poll := <-done:
		cr := commandResultFromResult(t, poll.ForLLM)
		if poll.IsError || cr.Status != statusRunning {
			t.Fatalf("poll = %+v (%s), want a still-running session", poll, poll.ForLLM)
		}
		if cr.SessionID == nil || *cr.SessionID != sessionID {
			t.Fatalf("session_id = %v, want %s: the session is still there to poll", cr.SessionID, sessionID)
		}
		if cr.Warning == nil || !strings.Contains(*cr.Warning, "poll interrupted by user") {
			t.Fatalf("warning = %v, want the interrupted-poll note (%s)", cr.Warning, poll.ForLLM)
		}
		if !strings.Contains(cr.Output, "tick") {
			t.Fatalf("output = %q, want the output collected before the interrupt", cr.Output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poll wait was not interrupted")
	}
	if session.IsDone() {
		t.Fatal("the interrupted poll terminated the process it was watching")
	}

	// The process is still running, and the next poll follows it to its exit.
	final := manageTool.Execute(context.Background(), map[string]any{
		"action": "poll", "session_id": sessionID, "wait_timeout": 30,
	})
	if final.IsError {
		t.Fatalf("final poll error: %s", final.ForLLM)
	}
	if cr := commandResultFromResult(t, final.ForLLM); cr.Status != statusCompleted {
		t.Fatalf("final poll = %s, want the process to have finished", final.ForLLM)
	}
}

// TestExecCommandInterruptKillsProcess covers interrupting a running command: the
// synchronous wait is cancelled, the process tree is terminated instead of being
// left behind in the background, and the output the process had printed until
// then is reported with the interrupted status instead of being discarded.
func TestExecCommandInterruptKillsProcess(t *testing.T) {
	engine := NewExecEngine(60, 30, true)
	defer engine.Close()
	tool := NewExecCommandTool(engine)

	command := "echo working; sleep 5; echo done"
	if runtime.GOOS == "windows" {
		command = "Write-Output working; Start-Sleep -Seconds 5; Write-Output done"
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *Result, 1)
	go func() {
		done <- tool.Execute(ctx, map[string]any{"script": command, "wait_timeout": 30})
	}()

	// Interrupt once the command runs and has printed its first line, so there
	// is output to report with the interrupted answer.
	waitForBufferedOutput(t, engine, 5*time.Second)
	cancel()

	select {
	case res := <-done:
		cr := commandResultFromResult(t, res.ForLLM)
		if !res.IsError || cr.Status != statusInterrupted {
			t.Fatalf("result = %+v (%s), want an interrupted failure", res, res.ForLLM)
		}
		if !strings.Contains(cr.Output, "working") {
			t.Fatalf("output = %q, want what the process had printed before the interrupt", cr.Output)
		}
		if cr.SessionID != nil {
			t.Fatalf("session_id = %q, want none: an interrupted command is not left behind", *cr.SessionID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command was not interrupted")
	}

	for _, info := range engine.sessions.List() {
		if info.Status == "running" {
			t.Fatalf("session %s is still running after the interrupt", info.ID)
		}
	}
}

// bufferedOutput reports how much output the session holds for the next
// hand-over, which is how a test waits for a child's first line.
func bufferedOutput(session *ProcessSession) int {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.output.published.Len()
}

// waitForBufferedOutput waits until the engine's session holds output.
func waitForBufferedOutput(t *testing.T, engine *ExecEngine, timeout time.Duration) *ProcessSession {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if list := engine.sessions.List(); len(list) > 0 {
			if session, err := engine.sessions.Get(list[0].ID); err == nil && bufferedOutput(session) > 0 {
				return session
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the command produced no output")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestEngineCloseKillsRunningSessions pins the engine exit path: closing the
// engine (what a session shutdown does when lightagent exits) terminates the
// sessions that are still running instead of leaving them behind. The heartbeat
// file tells a real kill from a session that was only marked done.
func TestEngineCloseKillsRunningSessions(t *testing.T) {
	heartbeat := filepath.Join(t.TempDir(), "heartbeat.txt")
	command := fmt.Sprintf("while true; do printf x >> '%s'; sleep 0.2; done", heartbeat)
	if runtime.GOOS == "windows" {
		command = fmt.Sprintf("while ($true) { Add-Content -LiteralPath '%s' -Value x; Start-Sleep -Milliseconds 200 }", heartbeat)
	}

	engine := NewExecEngine(300, 1, true)
	tool := NewExecCommandTool(engine)
	res := tool.Execute(context.Background(), map[string]any{"script": command, "wait_timeout": 1})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, `"status":"running"`) {
		t.Fatalf("expected a background session: %s", res.ForLLM)
	}
	waitForHeartbeat(t, heartbeat)

	engine.Close()

	// The whole tree is gone, so the loop cannot write again. A session that
	// was merely marked done would keep growing the file.
	size := heartbeatSize(t, heartbeat)
	time.Sleep(700 * time.Millisecond)
	if grown := heartbeatSize(t, heartbeat); grown != size {
		t.Fatalf("the session is still running: the heartbeat grew from %d to %d bytes", size, grown)
	}
}

// TestExecCommandCompletesWhenALeftoverHoldsThePipes covers the shape a script
// leaves behind when it starts a background program: the shell exits while that
// program keeps stdout/stderr open, so the session must still reach "completed"
// (execWaitDelay bounds the pipe wait) and the leftover program must be
// terminated instead of running on with the session.
func TestExecCommandCompletesWhenALeftoverHoldsThePipes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PowerShell does not hand the parent's pipes to a detached program")
	}
	heartbeat := filepath.Join(t.TempDir(), "heartbeat.txt")
	command := fmt.Sprintf("sh -c 'while true; do printf x >> %s; sleep 0.2; done' &", heartbeat)

	engine := NewExecEngine(300, 10, true)
	defer engine.Close()
	tool := NewExecCommandTool(engine)

	res := tool.Execute(context.Background(), map[string]any{"script": command})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, `"status":"completed"`) {
		t.Fatalf("the session did not finish: %s", res.ForLLM)
	}

	waitForHeartbeat(t, heartbeat)
	// Reap runs before the session is marked done, so the leftover program is
	// already gone; the heartbeat must not grow anymore.
	time.Sleep(700 * time.Millisecond)
	size := heartbeatSize(t, heartbeat)
	time.Sleep(700 * time.Millisecond)
	if grown := heartbeatSize(t, heartbeat); grown != size {
		t.Fatalf("the leftover program is still running: the heartbeat grew from %d to %d bytes", size, grown)
	}
}

// waitForHeartbeat waits until the command under test has written to path.
func waitForHeartbeat(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if heartbeatSize(t, path) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s was never written", path)
}

// heartbeatSize returns the current size of the heartbeat file (0 when it does
// not exist yet).
func heartbeatSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// sessionIDFromResult extracts session_id from a commandResult JSON string.
func sessionIDFromResult(t *testing.T, payload string) string {
	t.Helper()
	return derefString(commandResultFromResult(t, payload).SessionID)
}

// commandResultFromResult parses the structured contract out of a command tool
// result, so a test can assert on the fields the model reads.
func commandResultFromResult(t *testing.T, payload string) commandResult {
	t.Helper()
	var cr commandResult
	if err := json.Unmarshal([]byte(payload), &cr); err != nil {
		t.Fatalf("parse result: %v (%s)", err, payload)
	}
	return cr
}

// TestPollFlushesPerCallAndLimitsApplyPerCall pins what a poll is: it hands the
// whole output buffered up to that moment to the model and clears the buffer, and
// the max_lines/max_chars budget belongs to that call instead of the process
// lifetime — the second poll reports its own batch rather than inheriting the
// first one's fold.
func TestPollFlushesPerCallAndLimitsApplyPerCall(t *testing.T) {
	// The child prints one batch of 60 lines per input line it reads, so every
	// poll below has exactly one batch of its own to hand over.
	command := "while read line; do i=0; while [ $i -lt 60 ]; do i=$((i+1)); echo $i; done; done"
	if runtime.GOOS == "windows" {
		command = "while ($null -ne ($line = [Console]::In.ReadLine())) { " +
			"1..60 | ForEach-Object { \"line $_\" } }"
	}

	engine := NewExecEngine(60, 1, true)
	defer engine.Close()
	execTool := NewExecCommandTool(engine)
	manageTool := NewManageSessionTool(engine)

	// The child only waits for input, so its start hands over no output.
	res := execTool.Execute(context.Background(), map[string]any{"script": command, "max_lines": 10})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	sessionID := sessionIDFromResult(t, res.ForLLM)
	if sessionID == "" {
		t.Fatalf("the command did not stay in the background: %s", res.ForLLM)
	}

	for batch := 1; batch <= 2; batch++ {
		in := manageTool.Execute(context.Background(), map[string]any{
			"action": "input", "session_id": sessionID, "data": "go\n",
		})
		if in.IsError {
			t.Fatalf("batch %d: input error: %s", batch, in.ForLLM)
		}
		poll := manageTool.Execute(context.Background(), map[string]any{
			"action": "poll", "session_id": sessionID, "wait_timeout": 2, "max_lines": 10,
		})
		if poll.IsError {
			t.Fatalf("batch %d: poll error: %s", batch, poll.ForLLM)
		}
		cr := commandResultFromResult(t, poll.ForLLM)
		if !cr.Truncated {
			t.Fatalf("batch %d was not folded under max_lines=10: %s", batch, poll.ForLLM)
		}
		// The call counts what it received, not what it was allowed to return.
		if cr.TotalLines != 60 {
			t.Fatalf("batch %d reports %d lines, want the 60 of that call alone: %s", batch, cr.TotalLines, poll.ForLLM)
		}
		// The fold keeps the tail, so the batch's last line is what the model sees.
		if !strings.HasSuffix(strings.TrimSpace(cr.Output), "60") {
			t.Fatalf("batch %d lost the end of its output: %s", batch, poll.ForLLM)
		}
	}
}

// TestExecMaxLinesBudget pins the max_lines budget shared by exec_command and
// manage_session: the built-in pair is 50/100, both schemas advertise the
// configured default, a request above the configured maximum is truncated to it
// (which folds the output), and the description states the default, the maximum
// and the advice to redirect output that must survive in full to a file.
func TestExecMaxLinesBudget(t *testing.T) {
	engine := NewExecEngine(60, 1, true)
	defer engine.Close()

	if engine.maxLinesDefault != 50 || engine.maxLinesMax != 100 {
		t.Fatalf("built-in max_lines budget = %d/%d, want 50/100", engine.maxLinesDefault, engine.maxLinesMax)
	}
	engine.SetMaxLines(10, 20)
	if engine.maxLinesDefault != 10 || engine.maxLinesMax != 20 {
		t.Fatalf("configured max_lines budget = %d/%d, want 10/20", engine.maxLinesDefault, engine.maxLinesMax)
	}

	execTool := NewExecCommandTool(engine)
	manageTool := NewManageSessionTool(engine)

	// Both schemas carry the configured default.
	for name, tool := range map[string]Tool{"exec_command": execTool, "manage_session": manageTool} {
		props, _ := tool.Parameters()["properties"].(map[string]any)
		param, ok := props["max_lines"].(map[string]any)
		if !ok {
			t.Fatalf("%s: max_lines is missing from the parameters", name)
		}
		if param["default"] != 10 {
			t.Fatalf("%s: max_lines default = %v, want the configured 10", name, param["default"])
		}
	}
	// manage_session says its range equals exec_command's.
	props, _ := manageTool.Parameters()["properties"].(map[string]any)
	param, _ := props["max_lines"].(map[string]any)
	if desc, _ := param["description"].(string); !strings.Contains(desc, "exec_command") {
		t.Fatalf("manage_session max_lines does not point at exec_command's range: %s", desc)
	}

	// A request above the maximum is truncated to it, which folds the output.
	command := "i=0; while [ $i -lt 30 ]; do i=$((i+1)); echo line$i; done"
	if runtime.GOOS == "windows" {
		command = "1..30 | ForEach-Object { \"line$_\" }"
	}
	res := execTool.Execute(context.Background(), map[string]any{"script": command, "max_lines": 1000})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	cr := commandResultFromResult(t, res.ForLLM)
	if !cr.Truncated || cr.TotalLines != 30 {
		t.Fatalf("a request above the maximum was not folded to the 20 line cap: %s", res.ForLLM)
	}

	// The description states the budget and the advice to redirect output.
	description := execTool.Description()
	for _, want := range []string{"default: 10", "maximum: 20", "Redirect important output"} {
		if !strings.Contains(description, want) {
			t.Fatalf("the exec_command description is missing %q: %s", want, description)
		}
	}
}

// repaintNumber reads the number a repainting child printed in its "<label> N"
// line out of a tool result (-1 when the result does not carry that line).
func repaintNumber(t *testing.T, result, label string) int {
	t.Helper()
	at := strings.Index(result, label+" ")
	if at < 0 {
		return -1
	}
	rest := result[at+len(label)+1:]
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		t.Fatalf("no number after %q in %s", label, result)
	}
	number, err := strconv.Atoi(rest[:digits])
	if err != nil {
		t.Fatalf("bad number after %q in %s: %v", label, result, err)
	}
	return number
}

// TestExecCommandDecodesLocalizedOutput covers the legacy ANSI code page path
// (tools.exec.use_utf8 = false). PowerShell 7 writes UTF-8 regardless of the
// console code page, so the child emits raw host-code-page bytes itself and Go
// must decode them into readable text.
func TestExecCommandDecodesLocalizedOutput(t *testing.T) {
	const sample = "中文输出"
	command, ok := hostCodePageBytesCommand(t, sample)
	if !ok {
		t.Skip("only Windows child stdio needs a code page conversion")
	}

	engine := NewExecEngine(60, 10, false)
	defer engine.Close()
	tool := NewExecCommandTool(engine)

	res := tool.Execute(context.Background(), map[string]any{"script": command})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, sample) {
		t.Fatalf("localized output was not decoded: %s", res.ForLLM)
	}
}

// hostCodePageBytesCommand builds a script that writes sample to stdout as raw
// host-code-page bytes, reproducing what a console program on this host emits.
// ok=false means there is nothing to convert (non-Windows, or a UTF-8 host code
// page), so a caller that needs host-code-page output must skip.
func hostCodePageBytesCommand(t *testing.T, sample string) (string, bool) {
	t.Helper()
	if runtime.GOOS != "windows" {
		return "", false
	}
	enc := hostConsoleCodec().charset
	if enc == nil {
		return "", false
	}
	raw, err := enc.NewEncoder().Bytes([]byte(sample))
	if err != nil {
		t.Fatalf("encode %q with the host code page: %v", sample, err)
	}
	literals := make([]string, 0, len(raw))
	for _, b := range raw {
		literals = append(literals, fmt.Sprintf("0x%02X", b))
	}
	return "$bytes = [byte[]](" + strings.Join(literals, ",") + "); " +
		"[Console]::OpenStandardOutput().Write($bytes, 0, $bytes.Length)", true
}

// TestExecCommandDecodesPythonOutput covers the legacy ANSI code page path:
// Python writes stdout in the host code page when it is not attached to a
// console, so the tool must decode those bytes instead of feeding them to the
// model as UTF-8.
func TestExecCommandDecodesPythonOutput(t *testing.T) {
	interpreter, err := exec.LookPath("python")
	if err != nil {
		t.Skip("python is not installed")
	}
	const sample = "中文输出"
	skipIfHostCodePageIsLossy(t, sample)

	script := filepath.Join(t.TempDir(), "print_sample.py")
	body := "# -*- coding: utf-8 -*-\nprint(\"" + sample + "\")\n"
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// PYTHONIOENCODING must not leak in from the test runner: this case verifies
	// the Go-side conversion of host code page bytes.
	unsetEnvForTest(t, pythonIOEncodingEnv)

	engine := NewExecEngine(60, 20, false)
	defer engine.Close()
	tool := NewExecCommandTool(engine)

	// PowerShell needs the call operator to run a quoted path; sh takes the
	// quoted words directly.
	command := "'" + interpreter + "' '" + script + "'"
	if runtime.GOOS == "windows" {
		command = "& " + command
	}

	res := tool.Execute(context.Background(), map[string]any{"script": command})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, sample) {
		t.Fatalf("python output was not decoded (command %q): %s", command, res.ForLLM)
	}
}

// TestManageSessionInputRoundTrip sends localized text to a child's stdin and
// reads it back: stdin is encoded with the host charset and the reply is decoded
// again, so the text must come back unchanged.
func TestManageSessionInputRoundTrip(t *testing.T) {
	const sample = "中文输入"
	skipIfHostCodePageIsLossy(t, sample)

	command := "read line; echo \"$line\""
	if runtime.GOOS == "windows" {
		command = "Write-Output ([Console]::In.ReadLine())"
	}

	engine := NewExecEngine(60, 1, true)
	defer engine.Close()
	execTool := NewExecCommandTool(engine)
	manageTool := NewManageSessionTool(engine)

	res := execTool.Execute(context.Background(), map[string]any{"script": command, "wait_timeout": 1})
	sessionID := sessionIDFromResult(t, res.ForLLM)
	if sessionID == "" {
		t.Skipf("the command did not stay in the background: %s", res.ForLLM)
	}

	in := manageTool.Execute(context.Background(), map[string]any{
		"action": "input", "session_id": sessionID, "data": sample + "\n",
	})
	if in.IsError {
		t.Fatalf("input error: %s", in.ForLLM)
	}

	deadline := time.Now().Add(10 * time.Second)
	var output string
	for {
		poll := manageTool.Execute(context.Background(), map[string]any{
			"action": "poll", "session_id": sessionID, "wait_timeout": 1,
		})
		if poll.IsError {
			t.Fatalf("poll error: %s", poll.ForLLM)
		}
		output += poll.ForLLM
		if strings.Contains(poll.ForLLM, `"status":"completed"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child never exited: %s", output)
		}
	}
	if !strings.Contains(output, sample) {
		t.Fatalf("stdin/stdout round trip lost the text: %s", output)
	}
}

// TestExecCommandUseUTF8Param verifies the per-call use_utf8 parameter: it is
// advertised in the tool description, its schema carries the configured default
// and an explicit value overrides the engine (config) default both ways. Hosts
// without an ANSI code page do not offer the parameter at all.
func TestExecCommandUseUTF8Param(t *testing.T) {
	if !useUTF8ParamAvailable() {
		engine := NewExecEngine(60, 10, true)
		defer engine.Close()
		if desc := NewExecCommandTool(engine).Description(); strings.Contains(desc, "use_utf8") {
			t.Fatalf("the description mentions use_utf8 on a host without the parameter: %s", desc)
		}
		props, _ := NewExecCommandTool(engine).Parameters()["properties"].(map[string]any)
		if _, ok := props["use_utf8"]; ok {
			t.Fatal("use_utf8 is advertised on a host without the parameter")
		}
		return
	}

	// The model must be able to discover the parameter from the description.
	if desc := NewExecCommandTool(NewExecEngine(60, 10, true)).Description(); !strings.Contains(desc, "use_utf8") {
		t.Fatalf("the tool description does not mention use_utf8: %s", desc)
	}

	// The schema must carry the configured default.
	for _, def := range []bool{true, false} {
		engine := NewExecEngine(60, 10, def)
		props, _ := NewExecCommandTool(engine).Parameters()["properties"].(map[string]any)
		param, ok := props["use_utf8"].(map[string]any)
		if !ok {
			t.Fatal("use_utf8 is missing from the exec_command parameters")
		}
		if param["type"] != "boolean" || param["default"] != def {
			t.Fatalf("use_utf8 schema = %+v, want type boolean and default %v", param, def)
		}
		engine.Close()
	}

	const sample = "中文输出"

	// Config legacy, per-call UTF-8: the script gains the preamble, so the shell
	// emits UTF-8 and Go forwards it unchanged.
	utf8Engine := NewExecEngine(60, 10, false)
	defer utf8Engine.Close()
	utf8Tool := NewExecCommandTool(utf8Engine)
	utf8Command := "echo " + sample
	if runtime.GOOS == "windows" {
		utf8Command = "Write-Output '" + sample + "'"
	}
	res := utf8Tool.Execute(context.Background(), map[string]any{
		"script": utf8Command, "use_utf8": true,
	})
	if res.IsError || !strings.Contains(res.ForLLM, sample) {
		t.Fatalf("use_utf8=true was not honoured: %s", res.ForLLM)
	}

	// Config UTF-8, per-call legacy: host-code-page bytes must be decoded.
	rawCommand, ok := hostCodePageBytesCommand(t, sample)
	if !ok {
		t.Skip("the host code page needs no conversion")
	}
	legacyEngine := NewExecEngine(60, 10, true)
	defer legacyEngine.Close()
	legacyTool := NewExecCommandTool(legacyEngine)
	res = legacyTool.Execute(context.Background(), map[string]any{
		"script": rawCommand, "use_utf8": false,
	})
	if res.IsError || !strings.Contains(res.ForLLM, sample) {
		t.Fatalf("use_utf8=false was not honoured: %s", res.ForLLM)
	}
}

// TestBoolArgOrKeepsDefault checks the default-aware bool decoding used by the
// use_utf8 parameter: only an explicit bool wins, everything else keeps def.
func TestBoolArgOrKeepsDefault(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		def  bool
		want bool
	}{
		{"absent keeps true", map[string]any{}, true, true},
		{"absent keeps false", map[string]any{}, false, false},
		{"null keeps true", map[string]any{"use_utf8": nil}, true, true},
		{"string keeps false", map[string]any{"use_utf8": "yes"}, false, false},
		{"explicit true wins", map[string]any{"use_utf8": true}, false, true},
		{"explicit false wins", map[string]any{"use_utf8": false}, true, false},
	}
	for _, tc := range cases {
		if got := boolArgOr(tc.args, "use_utf8", tc.def); got != tc.want {
			t.Errorf("%s: boolArgOr = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestExecCommandUTF8ModeLocalizedOutput verifies UTF-8 mode (the default): the
// shell preamble makes the shell emit UTF-8, so Go forwards the bytes untouched.
func TestExecCommandUTF8ModeLocalizedOutput(t *testing.T) {
	const sample = "中文输出"

	command := "echo " + sample
	if runtime.GOOS == "windows" {
		command = "Write-Output '" + sample + "'"
	}

	engine := NewExecEngine(60, 10, true)
	defer engine.Close()
	tool := NewExecCommandTool(engine)

	res := tool.Execute(context.Background(), map[string]any{"script": command})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, sample) {
		t.Fatalf("UTF-8 output was mangled: %s", res.ForLLM)
	}
}

// TestExecCommandUTF8ModeForcesPythonUTF8 covers PYTHONIOENCODING: with the
// variable exported, Python prints UTF-8 even though its stdout is a pipe and
// would otherwise follow the host locale.
func TestExecCommandUTF8ModeForcesPythonUTF8(t *testing.T) {
	interpreter, err := exec.LookPath("python")
	if err != nil {
		t.Skip("python is not installed")
	}
	const sample = "中文输出"

	script := filepath.Join(t.TempDir(), "print_sample.py")
	body := "# -*- coding: utf-8 -*-\nprint(\"" + sample + "\")\n"
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// The parent must not provide the variable: UTF-8 mode has to add it.
	unsetEnvForTest(t, pythonIOEncodingEnv)

	engine := NewExecEngine(60, 20, true)
	defer engine.Close()
	tool := NewExecCommandTool(engine)

	// PowerShell needs the call operator to run a quoted path; sh takes the
	// quoted words directly.
	command := "'" + interpreter + "' '" + script + "'"
	if runtime.GOOS == "windows" {
		command = "& " + command
	}

	res := tool.Execute(context.Background(), map[string]any{"script": command})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, sample) {
		t.Fatalf("PYTHONIOENCODING was not applied (command %q): %s", command, res.ForLLM)
	}
}

// TestWindowsCommandScriptPreamble checks the UTF-8 preamble prefixing.
func TestWindowsCommandScriptPreamble(t *testing.T) {
	const command = "Write-Output hi"

	if got := windowsCommandScript(command, false); got != command {
		t.Fatalf("legacy script = %q, want the command untouched", got)
	}
	got := windowsCommandScript(command, true)
	if !strings.HasPrefix(got, windowsUTF8Preamble) || !strings.HasSuffix(got, command) {
		t.Fatalf("UTF-8 script = %q, want the preamble followed by the command", got)
	}
	for _, marker := range []string{
		"[Console]::InputEncoding",
		"[Console]::OutputEncoding",
		"$OutputEncoding",
	} {
		if !strings.Contains(windowsUTF8Preamble, marker) {
			t.Errorf("the preamble does not set %s: %q", marker, windowsUTF8Preamble)
		}
	}
}

// TestShellInvocationUTF8Preamble checks that only UTF-8 mode on Windows prepends
// the preamble.
func TestShellInvocationUTF8Preamble(t *testing.T) {
	name, args := shellInvocation("echo hi", true)
	script := args[len(args)-1]
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(script, windowsUTF8Preamble) {
			t.Fatalf("script = %q, want the UTF-8 preamble", script)
		}
		if !strings.Contains(name, "powershell") && !strings.Contains(name, "pwsh") {
			t.Fatalf("shell = %q, want PowerShell", name)
		}
	} else if script != "echo hi" {
		t.Fatalf("script = %q, want the command untouched", script)
	}

	_, legacyArgs := shellInvocation("echo hi", false)
	if legacy := legacyArgs[len(legacyArgs)-1]; legacy != "echo hi" {
		t.Fatalf("legacy script = %q, want the command untouched", legacy)
	}
}

// TestUTF8ChildEnvironment checks PYTHONIOENCODING injection: an inherited value
// is replaced in place and a missing one is appended exactly once.
func TestUTF8ChildEnvironment(t *testing.T) {
	env := utf8ChildEnvironment([]string{"PATH=/bin", "PYTHONIOENCODING=cp936", "HOME=/root"})
	if len(env) != 3 {
		t.Fatalf("env = %v, want the same number of entries", env)
	}
	if got := envValue(env, pythonIOEncodingEnv); got != "utf-8" {
		t.Fatalf("%s = %q, want utf-8", pythonIOEncodingEnv, got)
	}
	if got := envValue(env, "PATH"); got != "/bin" {
		t.Fatalf("PATH = %q, want /bin", got)
	}
	if got := envValue(env, "HOME"); got != "/root" {
		t.Fatalf("HOME = %q, want /root", got)
	}

	appended := utf8ChildEnvironment([]string{"PATH=/bin"})
	if len(appended) != 2 || envValue(appended, pythonIOEncodingEnv) != "utf-8" {
		t.Fatalf("appended env = %v, want %s appended once", appended, pythonIOEncodingEnv)
	}
}

// envValue returns the value of key in an environment slice ("" when absent).
func envValue(env []string, key string) string {
	for _, kv := range env {
		if name, value, ok := strings.Cut(kv, "="); ok && strings.EqualFold(name, key) {
			return value
		}
	}
	return ""
}

// unsetEnvForTest removes key from the process environment for the test's
// duration, restoring the previous value afterwards.
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	prev, ok := os.LookupEnv(key)
	if !ok {
		return
	}
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() { _ = os.Setenv(key, prev) })
}

// skipIfHostCodePageIsLossy skips a test when the host ANSI code page cannot
// represent sample (e.g. a Latin-1 host asked for Chinese characters).
func skipIfHostCodePageIsLossy(t *testing.T, sample string) {
	t.Helper()
	enc := hostConsoleCodec().charset
	if enc == nil {
		return
	}
	encoded, err := enc.NewEncoder().Bytes([]byte(sample))
	decoded, decodeErr := enc.NewDecoder().Bytes(encoded)
	if err != nil || decodeErr != nil || string(decoded) != sample {
		t.Skipf("the host code page cannot represent %q", sample)
	}
}

// TestScriptLanguageSelection covers the selectable `language` set: it starts
// with the host script engine and gains python exactly when an interpreter was
// found, advertised together with its version.
func TestScriptLanguageSelection(t *testing.T) {
	host := hostScriptLanguageID()
	if host != ScriptLanguagePowerShell && host != ScriptLanguageShell {
		t.Fatalf("host language = %q, want %q or %q", host, ScriptLanguagePowerShell, ScriptLanguageShell)
	}

	ids := scriptLanguageIDs()
	if len(ids) == 0 || ids[0] != host {
		t.Fatalf("language ids = %v, want the host engine %q first", ids, host)
	}

	python := systemPython()
	if !python.Found {
		if len(ids) != 1 {
			t.Fatalf("language ids = %v, want only the host engine when python is absent", ids)
		}
		if summary := scriptLanguageSummary(); strings.Contains(summary, ScriptLanguagePython) {
			t.Fatalf("python is advertised without an interpreter: %s", summary)
		}
		return
	}
	if len(ids) != 2 || ids[1] != ScriptLanguagePython {
		t.Fatalf("language ids = %v, want %q after the host engine", ids, ScriptLanguagePython)
	}
	if python.Version == "" || !strings.Contains(scriptLanguageSummary(), "Python "+python.Version) {
		t.Fatalf("the summary does not carry the python version: %s", scriptLanguageSummary())
	}
}

// TestExecCommandLanguageParameter verifies the `language` parameter: the
// selectable values are advertised by the enum (host engine first) and the host
// engine is the default. The parameter description is a fixed wording, while the
// host-dependent list (with the detected python version) is carried by the tool
// description.
func TestExecCommandLanguageParameter(t *testing.T) {
	engine := NewExecEngine(60, 10, true)
	defer engine.Close()
	tool := NewExecCommandTool(engine)

	host := hostScriptLanguageID()
	props, _ := tool.Parameters()["properties"].(map[string]any)
	param, ok := props["language"].(map[string]any)
	if !ok {
		t.Fatal("language is missing from the exec_command parameters")
	}
	if param["type"] != "string" || param["default"] != host {
		t.Fatalf("language schema = %+v, want type string and default %q", param, host)
	}
	enum, _ := param["enum"].([]string)
	if len(enum) != len(scriptLanguageIDs()) || enum[0] != host {
		t.Fatalf("language enum = %v, want %v", enum, scriptLanguageIDs())
	}

	// The parameter description is a plain statement: the model reads the
	// selectable values from the enum checked above.
	description, _ := param["description"].(string)
	if !strings.Contains(description, "Script language that interprets") {
		t.Errorf("the language parameter has no usable description: %q", description)
	}

	// The host-dependent list and the python version belong to the tool
	// description.
	toolDescription := tool.Description()
	if !strings.Contains(toolDescription, "language") || !strings.Contains(toolDescription, host) {
		t.Fatalf("the tool description does not advertise the language parameter: %s", toolDescription)
	}
	if python := systemPython(); python.Found {
		if !strings.Contains(toolDescription, python.Version) {
			t.Errorf("the tool description does not carry the python version %s: %s", python.Version, toolDescription)
		}
	} else if strings.Contains(toolDescription, ScriptLanguagePython) {
		t.Errorf("the tool description advertises python without an interpreter: %s", toolDescription)
	}
}

// TestPowerShellFlagsHint checks that the exec_command description advertises
// the PowerShell invocation flags exactly when PowerShell is a selectable
// language (the host shell on Windows), and never on a host that offers only
// another engine.
func TestPowerShellFlagsHint(t *testing.T) {
	engine := NewExecEngine(60, 10, true)
	defer engine.Close()
	tool := NewExecCommandTool(engine)
	description := tool.Description()

	selectable := false
	for _, id := range scriptLanguageIDs() {
		if id == ScriptLanguagePowerShell {
			selectable = true
		}
	}
	if selectable != scriptLanguageAllowsPowerShell() {
		t.Fatalf("scriptLanguageAllowsPowerShell() = %v, want %v", scriptLanguageAllowsPowerShell(), selectable)
	}
	const want = "-NoProfile -NonInteractive -ExecutionPolicy Bypass"
	if selectable {
		if hint := powerShellFlagsHint(); !strings.Contains(hint, want) {
			t.Fatalf("the PowerShell flags hint is missing the flags: %q", hint)
		}
		if !strings.Contains(description, want) {
			t.Fatalf("the description does not advertise the PowerShell flags: %s", description)
		}
		if runtime.GOOS == "windows" {
			_, args := shellInvocation("echo hi", true)
			if joined := strings.Join(args, " "); !strings.Contains(joined, want) {
				t.Fatalf("shellInvocation args = %q, want the advertised flags %q", joined, want)
			}
		}
		return
	}
	if hint := powerShellFlagsHint(); hint != "" {
		t.Fatalf("a host without PowerShell produced a flags hint: %q", hint)
	}
	if strings.Contains(description, "-NoProfile") {
		t.Fatalf("a host without PowerShell advertises its flags: %s", description)
	}
}

// TestResolveScriptLanguage covers language normalization: an omitted value keeps
// the previous behaviour (the host engine) and an unknown value is rejected with
// the selectable list.
func TestResolveScriptLanguage(t *testing.T) {
	host := hostScriptLanguageID()
	for _, raw := range []string{"", "   ", host, strings.ToUpper(host), " " + host + " "} {
		got, err := resolveScriptLanguage(raw)
		if err != nil || got != host {
			t.Errorf("resolveScriptLanguage(%q) = %q, %v; want %q", raw, got, err, host)
		}
	}
	if systemPython().Found {
		for _, raw := range []string{"python", "PYTHON", " python "} {
			got, err := resolveScriptLanguage(raw)
			if err != nil || got != ScriptLanguagePython {
				t.Errorf("resolveScriptLanguage(%q) = %q, %v; want %q", raw, got, err, ScriptLanguagePython)
			}
		}
	}
	if _, err := resolveScriptLanguage("ruby"); err == nil {
		t.Error("resolveScriptLanguage accepted an unknown language")
	}
}

// TestExecUseUTF8HostOverride checks the stdio mode resolution: Windows honours
// the requested value, every other host always speaks UTF-8.
func TestExecUseUTF8HostOverride(t *testing.T) {
	if useUTF8ParamAvailable() {
		if execUseUTF8(false) {
			t.Error("the host must honour use_utf8=false")
		}
		if !execUseUTF8(true) {
			t.Error("the host must honour use_utf8=true")
		}
		return
	}
	if !execUseUTF8(false) {
		t.Error("a host without the use_utf8 parameter must always speak UTF-8")
	}
}

// TestPythonVersionPattern covers the `python -V` parsing, including the Windows
// Store placeholder that must not pass for an interpreter.
func TestPythonVersionPattern(t *testing.T) {
	cases := []struct{ output, want string }{
		{"Python 3.14.3\n", "3.14.3"},
		{"Python 3.12.10\r\n", "3.12.10"},
		{"Python 3.13.0rc1\n", "3.13.0rc1"},
		{"Python 3\n", "3"},
		{"python 2.7.18\n", "2.7.18"},
		{"Python was not found; run without arguments to install from the Microsoft Store\n", ""},
		{"", ""},
	}
	for _, tc := range cases {
		got := ""
		if match := pythonVersionPattern.FindStringSubmatch(tc.output); match != nil {
			got = match[1]
		}
		if got != tc.want {
			t.Errorf("parse %q = %q, want %q", tc.output, got, tc.want)
		}
	}
}

// TestExecCommandRejectsUnsupportedLanguage verifies that an unknown language is
// reported together with the selectable ones.
func TestExecCommandRejectsUnsupportedLanguage(t *testing.T) {
	engine := NewExecEngine(60, 10, true)
	defer engine.Close()
	tool := NewExecCommandTool(engine)

	res := tool.Execute(context.Background(), map[string]any{"script": "echo hi", "language": "ruby"})
	if !res.IsError || !strings.Contains(res.ForLLM, "unsupported language") {
		t.Fatalf("result = %+v, want an unsupported language failure", res)
	}
	for _, id := range scriptLanguageIDs() {
		if !strings.Contains(res.ForLLM, id) {
			t.Errorf("the failure does not list %q: %s", id, res.ForLLM)
		}
	}
}

// TestExecCommandPythonLanguage runs the script through a directly started Python
// interpreter (no shell in between) and checks that the escape-decoded output
// survives the stdio round trip.
func TestExecCommandPythonLanguage(t *testing.T) {
	if !systemPython().Found {
		t.Skip("python is not installed")
	}
	engine := NewExecEngine(60, 20, true)
	defer engine.Close()
	tool := NewExecCommandTool(engine)

	// The source stays ASCII and spans several lines: Python decodes the \u
	// escapes into Chinese, and the whole text travels through `-c`.
	script := "print(\"line-1\")\n" + `print("\u4e2d\u6587\u8f93\u51fa")` + "\nprint(\"line-3\")"
	res := tool.Execute(context.Background(), map[string]any{
		"script": script, "language": ScriptLanguagePython,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, `"status":"completed"`) {
		t.Fatalf("status not completed: %s", res.ForLLM)
	}
	for _, want := range []string{"line-1", "中文输出", "line-3"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Fatalf("python output does not contain %q: %s", want, res.ForLLM)
		}
	}
}

// TestExecCommandPythonLanguageANSIMode covers the legacy code page path with
// python as the script engine: use_utf8=false exports no PYTHONIOENCODING, so
// Python writes host code page bytes that Go must decode.
func TestExecCommandPythonLanguageANSIMode(t *testing.T) {
	if !systemPython().Found {
		t.Skip("python is not installed")
	}
	if runtime.GOOS != "windows" {
		t.Skip("only Windows children write the ANSI code page")
	}
	const sample = "中文输出"
	skipIfHostCodePageIsLossy(t, sample)
	unsetEnvForTest(t, pythonIOEncodingEnv)

	engine := NewExecEngine(60, 20, false)
	defer engine.Close()
	tool := NewExecCommandTool(engine)

	script := `print("\u4e2d\u6587\u8f93\u51fa")`
	res := tool.Execute(context.Background(), map[string]any{
		"script": script, "language": ScriptLanguagePython, "use_utf8": false,
	})
	if res.IsError || !strings.Contains(res.ForLLM, sample) {
		t.Fatalf("python ANSI output was not decoded: %s", res.ForLLM)
	}
}
