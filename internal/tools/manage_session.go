package tools

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ManageSessionTool manages background processes created by exec_command.
type ManageSessionTool struct {
	engine *ExecEngine
}

// NewManageSessionTool wraps the shared engine with the manage_session surface.
func NewManageSessionTool(engine *ExecEngine) *ManageSessionTool {
	return &ManageSessionTool{engine: engine}
}

// Name implements Tool.
func (t *ManageSessionTool) Name() string { return "manage_session" }

// Description implements Tool.
func (t *ManageSessionTool) Description() string {
	return "Manage background processes created by exec_command. Supports non-blocking " +
		"and long-polling output retrieval (each call returns the whole output buffered " +
		"so far and clears it), input sending, listing and termination. A session whose " +
		"process exited is kept as a zombie until a poll takes its last output and exit " +
		"code (list shows it as status=zombie); it is dropped when it is collected, or " +
		"after 24 hours."
}

// Parameters implements Tool.
func (t *ManageSessionTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session_id": map[string]any{
				"type":        "string",
				"description": "Target background session ID. (Not required for action='list'.)",
			},
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"poll", "input", "kill", "list"},
				"description": "'poll': take the buffered output and the status; an exited session reports its exit code with its last output and ends there, so poll a finished session to avoid losing that output. 'input': write to stdin. 'kill': terminate the process tree and release the session, reporting the output it had produced (exit_code -1: the process did not exit on its own); a process that already exited is a failure, still reported with its last output and its real exit code. 'list': list the sessions, a finished session whose output was not collected yet showing status=zombie.",
			},
			"data": map[string]any{
				"type":        "string",
				"description": "Text or key sequence to write (required for action='input'). E.g. 'yes\\n', 'ctrl-c', 'enter'.",
			},
			"wait_timeout": map[string]any{
				"type":        "integer",
				"default":     10,
				"description": "For action='poll': max seconds to wait for the process to exit. Output arriving in the meantime does not end the wait; the buffered output is returned either way, including the line a running process is repainting. Default: 10.",
			},
			"max_lines": map[string]any{
				"type":        "integer",
				"default":     200,
				"description": "Maximum lines this call returns (head/tail folded). The limit is per call, not over the process lifetime. Default: 200.",
			},
			"max_chars": map[string]any{
				"type":        "integer",
				"default":     30000,
				"description": "Maximum characters this call returns. The limit is per call, not over the process lifetime. Default: 30000.",
			},
		},
		"required": []string{"action"},
	}
}

// Execute implements Tool.
func (t *ManageSessionTool) Execute(ctx context.Context, args map[string]any) *Result {
	start := time.Now()
	action := strings.TrimSpace(func() string {
		s, _ := stringArg(args, "action")
		return s
	}())
	fail := func(message string) *Result {
		return commandResult{Status: statusFailed, Output: message, ElapsedSeconds: elapsedSeconds(start)}.toResult()
	}

	if action == "" {
		return fail("action is required")
	}
	if t.engine == nil {
		return fail("manage_session is not configured")
	}

	waitTimeout := time.Duration(intArg(args, "wait_timeout", 10)) * time.Second
	if waitTimeout <= 0 {
		waitTimeout = 10 * time.Second
	}
	maxLines := intArg(args, "max_lines", 200)
	maxChars := intArg(args, "max_chars", 30000)

	switch action {
	case "list":
		return t.executeList(start)
	case "poll":
		return t.executePoll(ctx, start, args, waitTimeout, maxLines, maxChars)
	case "input":
		return t.executeInput(start, args)
	case "kill":
		return t.executeKill(start, args, maxLines, maxChars)
	default:
		return fail(fmt.Sprintf("unknown action %q; expected poll, input, kill or list", action))
	}
}

// lookup resolves the target session or returns a failure result.
func (t *ManageSessionTool) lookup(start time.Time, args map[string]any, action string) (*ProcessSession, *Result) {
	id, _ := stringArg(args, "session_id")
	id = strings.TrimSpace(id)
	if id == "" {
		cr := commandResult{
			Status:         statusFailed,
			Output:         fmt.Sprintf("session_id is required for action='%s'", action),
			ElapsedSeconds: elapsedSeconds(start),
		}
		return nil, cr.toResult()
	}
	session, err := t.engine.sessions.Get(id)
	if err != nil {
		cr := commandResult{
			Status:         statusFailed,
			Output:         fmt.Sprintf("session %q not found", id),
			ElapsedSeconds: elapsedSeconds(start),
		}
		return nil, cr.toResult()
	}
	return session, nil
}

func (t *ManageSessionTool) executeList(start time.Time) *Result {
	infos := t.engine.sessions.List()
	var sb strings.Builder
	zombies := 0
	if len(infos) == 0 {
		sb.WriteString("No background sessions.")
	} else {
		for _, info := range infos {
			sb.WriteString(fmt.Sprintf("session: %s\n", info.ID))
			sb.WriteString(fmt.Sprintf("  command: %s\n", info.Command))
			sb.WriteString(fmt.Sprintf("  status: %s\n", info.Status))
			sb.WriteString(fmt.Sprintf("  pid: %d\n", info.PID))
			if info.ExitCode != nil {
				sb.WriteString(fmt.Sprintf("  exit_code: %d\n", *info.ExitCode))
			}
			if info.Status == sessionStatusZombie {
				zombies++
			}
		}
	}
	if zombies > 0 {
		// A zombie still holds the output written right before the exit and the
		// exit code: polling is what delivers them and ends the session.
		sb.WriteString("zombie = the process has exited; poll the session to take its last output and exit code\n")
	}
	text := sb.String()
	return (commandResult{
		Status:         statusCompleted,
		ExitCode:       intPtr(0),
		Output:         text,
		TotalLines:     len(infos),
		TotalBytes:     len(text),
		ElapsedSeconds: elapsedSeconds(start),
	}).toResultInfo()
}

func (t *ManageSessionTool) executePoll(ctx context.Context, start time.Time, args map[string]any, waitTimeout time.Duration, maxLines, maxChars int) *Result {
	session, failResult := t.lookup(start, args, "poll")
	if failResult != nil {
		return failResult
	}

	// poll waits for the process to finish, up to wait_timeout: output written in
	// the meantime does not end the wait (see WaitForExit). Whether it exited or
	// is still alive, the whole buffer is then handed over and emptied (see
	// TakeOutput): a progress line is reported as the state it holds right now,
	// and the next poll only reports what arrives after it. The exit is read
	// first, so a process that exited during the window reports all of its output
	// (its pipes are closed before the session is marked done).
	//
	// A finished session is kept in the pool until this hand-over, so a process
	// that exited without anyone watching is still here as a zombie with its last
	// output and its exit code. Reporting them is all this poll was for, so the
	// session ends here (see SessionManager.Collect) instead of lingering; only a
	// zombie nobody polls lives on, and only up to zombieTTL.
	//
	// The wait is cancellable: a poll only observes the process, so the user
	// interrupting the turn ends the wait exactly like a wait_timeout does and
	// the process keeps running — the output collected so far is returned with a
	// warning saying so, and the next poll picks up from there.
	interrupted := false
	if !session.IsDone() {
		_, _, interrupted = session.WaitForExitContext(ctx, waitTimeout)
	}
	done := session.IsDone()
	shown, truncated, totalLines, totalBytes := takeOutput(session, maxLines, maxChars)

	cr := commandResult{
		Output:         shown,
		Truncated:      truncated,
		TotalLines:     totalLines,
		TotalBytes:     totalBytes,
		ElapsedSeconds: elapsedSeconds(start),
	}
	if done {
		code := session.GetExitCode()
		cr.Status = statusCompleted
		cr.ExitCode = intPtr(code)
		if shown == "" {
			cr.Output = fmt.Sprintf("process exited with code %d", code)
		}
		t.engine.sessions.Collect(session)
	} else {
		cr.Status = statusRunning
		cr.SessionID = strPtr(session.ID)
		if interrupted {
			cr.Warning = strPtr("poll interrupted by user: the process is still running; poll again to take its output")
		}
	}
	return cr.toResultInfo()
}

func (t *ManageSessionTool) executeInput(start time.Time, args map[string]any) *Result {
	session, failResult := t.lookup(start, args, "input")
	if failResult != nil {
		return failResult
	}
	data, ok := stringArg(args, "data")
	if !ok {
		cr := commandResult{Status: statusFailed, Output: "data is required for action='input'", ElapsedSeconds: elapsedSeconds(start)}
		return cr.toResult()
	}
	if session.IsDone() {
		// Nothing can be written anymore, but the exit is not lost: the session
		// is still there for the poll that takes its last output and exit code.
		cr := commandResult{
			Status:         statusFailed,
			Output:         fmt.Sprintf("process already exited with code %d; poll the session to take its last output", session.GetExitCode()),
			ElapsedSeconds: elapsedSeconds(start),
		}
		return cr.toResult()
	}
	if err := session.Write(encodeControlInput(data)); err != nil {
		cr := commandResult{
			Status:         statusFailed,
			Output:         fmt.Sprintf("failed to write input: %v", err),
			ElapsedSeconds: elapsedSeconds(start),
		}
		return cr.toResult()
	}
	return (commandResult{
		Status:         statusCompleted,
		ExitCode:       intPtr(0),
		Output:         fmt.Sprintf("Input written to session %s", session.ID),
		ElapsedSeconds: elapsedSeconds(start),
	}).toResultInfo()
}

// takeOutput hands the session's buffered output over and folds it the way a
// call reports it: shown is what the caller returns and clean is what the
// counters count (see sanitizeAndFold).
func takeOutput(session *ProcessSession, maxLines, maxChars int) (shown string, truncated bool, totalLines, totalBytes int) {
	raw := session.TakeOutput()
	shown, clean, truncated := sanitizeAndFold(raw, maxLines, maxChars)
	totalLines, totalBytes = countLinesAndBytes(clean)
	return shown, truncated, totalLines, totalBytes
}

// executeKill terminates the process tree of a session. Either way the session
// is released and what its tree had printed until then is handed over the way
// poll does. A process that is still running is terminated (a success); one that
// already exited has nothing left to terminate, which is a failure — but its
// exit state (the last output plus the exit code) is reported with it instead of
// being dropped.
func (t *ManageSessionTool) executeKill(start time.Time, args map[string]any, maxLines, maxChars int) *Result {
	session, failResult := t.lookup(start, args, "kill")
	if failResult != nil {
		return failResult
	}

	if session.IsDone() {
		code := session.GetExitCode()
		shown, truncated, totalLines, totalBytes := takeOutput(session, maxLines, maxChars)
		if shown == "" {
			shown = fmt.Sprintf("process already exited with code %d", code)
		} else {
			shown = fmt.Sprintf("process already exited with code %d\n%s", code, shown)
		}
		t.engine.sessions.Collect(session)
		return commandResult{
			Status:         statusFailed,
			ExitCode:       intPtr(code),
			Output:         shown,
			Truncated:      truncated,
			TotalLines:     totalLines,
			TotalBytes:     totalBytes,
			ElapsedSeconds: elapsedSeconds(start),
		}.toResult()
	}
	if err := session.Kill(); err != nil {
		cr := commandResult{
			Status:         statusFailed,
			Output:         fmt.Sprintf("failed to kill session: %v", err),
			ElapsedSeconds: elapsedSeconds(start),
		}
		return cr.toResult()
	}
	shown, truncated, totalLines, totalBytes := takeOutput(session, maxLines, maxChars)
	message := fmt.Sprintf("Process in session %s terminated", session.ID)
	if shown != "" {
		message += "\n" + shown
	}
	// The recorded code of a killed session is the kill's -1: the process did
	// not exit on its own, so there is no real exit status to report.
	code := session.GetExitCode()
	t.engine.sessions.Collect(session)
	return commandResult{
		Status:         statusCompleted,
		ExitCode:       intPtr(code),
		Output:         message,
		Truncated:      truncated,
		TotalLines:     totalLines,
		TotalBytes:     totalBytes,
		ElapsedSeconds: elapsedSeconds(start),
	}.toResultInfo()
}

// controlKeys maps friendly names to their byte sequences.
var controlKeys = map[string]string{
	"ctrl-c":    "\x03",
	"ctrl-d":    "\x04",
	"ctrl-z":    "\x1a",
	"ctrl-\\":   "\x1c",
	"enter":     "\r",
	"return":    "\r",
	"tab":       "\t",
	"esc":       "\x1b",
	"escape":    "\x1b",
	"up":        "\x1b[A",
	"down":      "\x1b[B",
	"right":     "\x1b[C",
	"left":      "\x1b[D",
	"backspace": "\b",
}

// encodeControlInput maps a control-key token to its byte sequence; any other
// content (plain text, 'yes\n') is written verbatim.
func encodeControlInput(data string) string {
	token := strings.ToLower(strings.TrimSpace(data))
	if token != "" && !strings.ContainsAny(token, " \t\r\n") {
		if encoded, ok := controlKeys[token]; ok {
			return encoded
		}
	}
	return data
}
