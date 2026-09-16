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
		"and long-polling output retrieval, input sending, listing and termination."
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
				"description": "'poll': fetch incremental logs and status; 'input': write to stdin; 'kill': terminate; 'list': list active sessions.",
			},
			"data": map[string]any{
				"type":        "string",
				"description": "Text or key sequence to write (required for action='input'). E.g. 'yes\\n', 'ctrl-c', 'enter'.",
			},
			"wait_timeout": map[string]any{
				"type":        "integer",
				"default":     10,
				"description": "For action='poll': max seconds to wait for new output or exit. Default: 10.",
			},
			"max_lines": map[string]any{
				"type":        "integer",
				"default":     200,
				"description": "Maximum incremental lines to return. Default: 200.",
			},
			"max_chars": map[string]any{
				"type":        "integer",
				"default":     30000,
				"description": "Maximum incremental characters to return. Default: 30000.",
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
		return t.executePoll(start, args, waitTimeout, maxLines, maxChars)
	case "input":
		return t.executeInput(start, args)
	case "kill":
		return t.executeKill(start, args)
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
	if len(infos) == 0 {
		sb.WriteString("No active background sessions.")
	} else {
		for _, info := range infos {
			sb.WriteString(fmt.Sprintf("session: %s\n", info.ID))
			sb.WriteString(fmt.Sprintf("  command: %s\n", info.Command))
			sb.WriteString(fmt.Sprintf("  status: %s\n", info.Status))
			sb.WriteString(fmt.Sprintf("  pid: %d\n", info.PID))
		}
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

func (t *ManageSessionTool) executePoll(start time.Time, args map[string]any, waitTimeout time.Duration, maxLines, maxChars int) *Result {
	session, failResult := t.lookup(start, args, "poll")
	if failResult != nil {
		return failResult
	}

	if !session.IsDone() {
		session.WaitForOutput(waitTimeout)
	}
	raw, done := session.ReadAllPending()
	shown, clean, truncated := sanitizeAndFold(raw, maxLines, maxChars)
	totalLines, totalBytes := countLinesAndBytes(clean)

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
	} else {
		cr.Status = statusRunning
		cr.SessionID = strPtr(session.ID)
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
		cr := commandResult{
			Status:         statusFailed,
			Output:         fmt.Sprintf("process already exited with code %d", session.GetExitCode()),
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

func (t *ManageSessionTool) executeKill(start time.Time, args map[string]any) *Result {
	session, failResult := t.lookup(start, args, "kill")
	if failResult != nil {
		return failResult
	}
	if session.IsDone() {
		cr := commandResult{
			Status:         statusFailed,
			Output:         fmt.Sprintf("process already exited with code %d", session.GetExitCode()),
			ElapsedSeconds: elapsedSeconds(start),
		}
		return cr.toResult()
	}
	if err := session.Kill(); err != nil {
		cr := commandResult{
			Status:         statusFailed,
			Output:         fmt.Sprintf("failed to kill session: %v", err),
			ElapsedSeconds: elapsedSeconds(start),
		}
		return cr.toResult()
	}
	t.engine.sessions.Remove(session.ID)
	return (commandResult{
		Status:         statusCompleted,
		ExitCode:       intPtr(0),
		Output:         fmt.Sprintf("Process in session %s terminated", session.ID),
		ElapsedSeconds: elapsedSeconds(start),
	}).toResultInfo()
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
