package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// execCommandDefaultWaitSeconds is the default synchronous wait window.
const execCommandDefaultWaitSeconds = 10

// ExecEngine spawns shell commands and tracks their sessions. It is shared by
// exec_command and manage_session so both operate on one session pool.
type ExecEngine struct {
	sessions    *SessionManager
	runTimeout  time.Duration
	waitSeconds int
	useUTF8     bool
}

// NewExecEngine builds an engine. timeoutSeconds drives the default run_timeout
// and waitSeconds the default synchronous wait window. useUTF8 is the default
// child stdio mode on Windows (exec_command's `use_utf8` parameter overrides it
// per call): child processes read and write UTF-8 (shell preamble plus
// PYTHONIOENCODING) instead of the host ANSI code page; see childCodec. Hosts
// without an ANSI code page ignore it and always speak UTF-8, see execUseUTF8.
func NewExecEngine(timeoutSeconds, waitSeconds int, useUTF8 bool) *ExecEngine {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 3600
	}
	if waitSeconds <= 0 {
		waitSeconds = execCommandDefaultWaitSeconds
	}
	return &ExecEngine{
		sessions:    NewSessionManager(),
		runTimeout:  time.Duration(timeoutSeconds) * time.Second,
		waitSeconds: waitSeconds,
		useUTF8:     useUTF8,
	}
}

// Sessions exposes the shared session manager.
func (e *ExecEngine) Sessions() *SessionManager { return e.sessions }

// Close stops the session cleanup goroutine.
func (e *ExecEngine) Close() { e.sessions.Stop() }

// runTimeoutDefault returns the effective hard-lifetime default.
func (e *ExecEngine) runTimeoutDefault() time.Duration {
	if e.runTimeout > 0 {
		return e.runTimeout
	}
	return time.Hour
}

// childCodec returns the stdio conversion used for child processes in the given
// mode. With useUTF8 the child is told to speak UTF-8 (shell preamble plus
// PYTHONIOENCODING), so the bytes pass through unchanged; otherwise the host
// ANSI code page is converted in Go (see console_encoding.go): stdout/stderr
// bytes are decoded into UTF-8 and stdin text is encoded into that same code
// page.
func childCodec(useUTF8 bool) consoleCodec {
	if useUTF8 {
		return consoleCodec{}
	}
	return hostConsoleCodec()
}

// launch starts command and returns its session. language (see
// resolveScriptLanguage) selects the script engine: the host shell or a Python
// interpreter started directly. The returned session is already registered in
// the session manager. useUTF8 selects the child stdio mode: the bytes are
// either passed through untouched or converted in Go with the host ANSI code
// page; see childCodec.
func (e *ExecEngine) launch(language, command, cwd string, useUTF8 bool) (*ProcessSession, error) {
	name, args, err := scriptInvocation(language, command, useUTF8)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(name, args...)

	if cwd != "" {
		cmd.Dir = cwd
	}
	if useUTF8 {
		cmd.Env = utf8ChildEnvironment(os.Environ())
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	codec := childCodec(useUTF8)
	session := &ProcessSession{
		ID:           generateSessionID(),
		Command:      command,
		StartTime:    time.Now().Unix(),
		Status:       "running",
		stdin:        codec.wrapStdin(stdin),
		proc:         cmd,
		outputBuffer: &bytes.Buffer{},
	}
	stdout := newConsoleOutputWriter(session.appendOutput, codec.charset)
	stderr := newConsoleOutputWriter(session.appendOutput, codec.charset)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start command: %w", err)
	}
	session.PID = cmd.Process.Pid
	e.sessions.Add(session)

	go func() {
		waitErr := cmd.Wait()
		_ = stdin.Close()
		// Wait returned, so the output copiers are done: flush the decoders so
		// a character split by the final pipe read is still emitted.
		_ = stdout.Close()
		_ = stderr.Close()

		code := 0
		if waitErr != nil {
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				code = -1
			}
		}
		session.mu.Lock()
		if session.Status == "running" {
			session.Status = "done"
			session.ExitCode = code
		}
		session.mu.Unlock()
		session.signalDone()
	}()

	return session, nil
}

// hostShellEnvironment names the shell used for a human-readable tool
// description.
func hostShellEnvironment() string {
	if runtime.GOOS == "windows" {
		return "PowerShell (pwsh if available, otherwise powershell)"
	}
	return "sh"
}

// shellInvocation resolves the shell binary and argument vector for the host
// script language (ScriptLanguagePowerShell / ScriptLanguageShell). In UTF-8
// mode the script gains the windowsUTF8Preamble prefix so the shell and the
// programs it spawns read/write UTF-8; in legacy mode no preamble is injected (a
// UTF-8 preamble would garble programs that write the host code page, e.g.
// Python on a zh-CN host) and Go converts the host code page instead. Windows
// uses PowerShell, other hosts use "sh -c".
func shellInvocation(command string, useUTF8 bool) (string, []string) {
	if runtime.GOOS != "windows" {
		return "sh", []string{"-c", command}
	}
	return resolveWindowsShell(), []string{
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-Command", windowsCommandScript(command, useUTF8),
	}
}

// windowsCommandScript prepends the UTF-8 preamble to a PowerShell script when
// UTF-8 mode is on.
func windowsCommandScript(command string, useUTF8 bool) string {
	if !useUTF8 {
		return command
	}
	return windowsUTF8Preamble + command
}

// pythonIOEncodingEnv forces Python's stdin/stdout/stderr to UTF-8. The shell
// preamble cannot reach an interpreter started as a child process, so the
// variable is exported into the child environment instead.
const pythonIOEncodingEnv = "PYTHONIOENCODING"

// utf8ChildEnvironment returns base with PYTHONIOENCODING forced to utf-8,
// replacing an inherited value.
func utf8ChildEnvironment(base []string) []string {
	const entry = pythonIOEncodingEnv + "=utf-8"
	env := make([]string, 0, len(base)+1)
	replaced := false
	for _, kv := range base {
		name, _, found := strings.Cut(kv, "=")
		if found && strings.EqualFold(name, pythonIOEncodingEnv) {
			if !replaced {
				env = append(env, entry)
				replaced = true
			}
			continue
		}
		env = append(env, kv)
	}
	if !replaced {
		env = append(env, entry)
	}
	return env
}

// windowsUTF8Preamble switches Windows PowerShell itself, and the console code
// pages its children inherit, onto UTF-8 without a BOM. The [Console] setters
// call SetConsoleCP/SetConsoleOutputCP (so no separate chcp is needed) and
// $OutputEncoding covers PowerShell -> native-command pipelines; the shell then
// emits UTF-8 bytes that Go forwards unchanged. A host without an attached
// console makes the setters throw, hence the guards; the remaining settings
// still apply.
const windowsUTF8Preamble = "$utf8NoBom = New-Object System.Text.UTF8Encoding $false; " +
	"try { [Console]::InputEncoding = $utf8NoBom } catch {}; " +
	"try { [Console]::OutputEncoding = $utf8NoBom } catch {}; " +
	"$OutputEncoding = $utf8NoBom; "

var (
	windowsShellOnce sync.Once
	windowsShellExe  string
)

// resolveWindowsShell prefers PowerShell 7 (pwsh) and falls back to the
// built-in Windows PowerShell.
func resolveWindowsShell() string {
	windowsShellOnce.Do(func() {
		for _, candidate := range []string{"pwsh", "powershell"} {
			if p, err := exec.LookPath(candidate); err == nil && p != "" {
				windowsShellExe = p
				return
			}
		}
		windowsShellExe = "powershell"
	})
	return windowsShellExe
}

// ExecCommandTool runs shell commands with the wait-then-background model.
type ExecCommandTool struct {
	engine *ExecEngine
}

// NewExecCommandTool wraps a shared engine with the exec_command surface.
func NewExecCommandTool(engine *ExecEngine) *ExecCommandTool {
	return &ExecCommandTool{engine: engine}
}

// Name implements Tool.
func (t *ExecCommandTool) Name() string { return "exec_command" }

// Description implements Tool.
func (t *ExecCommandTool) Description() string {
	description := fmt.Sprintf("Execute a script with state-aware execution. `language` selects the "+
		"script language (available: %s; default: %s, the host shell). "+
		"Synchronously waits up to `wait_timeout` seconds (default: %d). If it finishes within "+
		"that window, returns exit_code and output directly. If it exceeds `wait_timeout` it "+
		"detaches to the background and returns a `session_id`. The whole process lifetime is "+
		"capped by `run_timeout` (default: %d seconds). Output is cleaned and truncated by "+
		"`max_lines`/`max_chars`.",
		scriptLanguageSummary(), hostScriptLanguageID(), t.engine.waitSeconds, int(t.engine.runTimeoutDefault()/time.Second))
	if !useUTF8ParamAvailable() {
		return description
	}
	return description + fmt.Sprintf(" Stdio encoding is selected by `use_utf8` (default: %t): "+
		"true forces PowerShell and Python onto UTF-8 (UTF-8 shell preamble plus "+
		"PYTHONIOENCODING=utf-8) and returns the raw bytes; false makes the agent decode the "+
		"host ANSI code page automatically (stdin is encoded into it as well) - everything "+
		"else behaves the same, but characters outside the host code page may not be displayed.",
		t.engine.useUTF8)
}

// Parameters implements Tool.
func (t *ExecCommandTool) Parameters() map[string]any {
	runDefault := int(t.engine.runTimeoutDefault() / time.Second)
	hostLanguage := hostScriptLanguageID()
	properties := map[string]any{
		"command": map[string]any{
			"type":        "string",
			"description": "The script content to execute.",
		},
		"language": map[string]any{
			"type":        "string",
			"enum":        scriptLanguageIDs(),
			"default":     hostLanguage,
			"description": "Script language used to run `command`.",
		},
		"wait_timeout": map[string]any{
			"type":        "integer",
			"default":     t.engine.waitSeconds,
			"description": "Max seconds to wait synchronously before auto-backgrounding. Default: " + fmt.Sprint(t.engine.waitSeconds) + ".",
		},
		"run_timeout": map[string]any{
			"type":        "integer",
			"default":     runDefault,
			"description": fmt.Sprintf("Absolute hard timeout in seconds for the whole process lifetime (background included). Default: %d. Set 0 to disable.", runDefault),
		},
		"cwd": map[string]any{
			"type":        "string",
			"description": "Working directory. ",
		},
	}
	// use_utf8 exists only where the host has an ANSI code page to fall back to.
	if useUTF8ParamAvailable() {
		properties["use_utf8"] = map[string]any{
			"type":        "boolean",
			"default":     t.engine.useUTF8,
			"description": fmt.Sprintf("Stdio encoding for this command (default: %t). true forces PowerShell and Python onto UTF-8: the script gains a UTF-8 preamble ([Console] input/output code pages and $OutputEncoding), PYTHONIOENCODING=utf-8 is exported and the output bytes are returned unchanged. false makes the agent decode the host ANSI code page (CP_ACP, e.g. GBK on a zh-CN host) automatically and encode stdin into it; everything else behaves the same, but characters outside the host code page may not be displayed. Use false for a program that only writes the host code page.", t.engine.useUTF8),
		}
	}
	properties["max_lines"] = map[string]any{
		"type":        "integer",
		"default":     200,
		"description": "Maximum output lines to return (head/tail folded). Default: 200.",
	}
	properties["max_chars"] = map[string]any{
		"type":        "integer",
		"default":     30000,
		"description": "Maximum output characters to return. Default: 30000.",
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   []string{"command"},
	}
}

// Execute implements Tool.
func (t *ExecCommandTool) Execute(ctx context.Context, args map[string]any) *Result {
	start := time.Now()
	fail := func(message string) *Result {
		return commandResult{Status: statusFailed, Output: message, ElapsedSeconds: elapsedSeconds(start)}.toResult()
	}

	if t.engine == nil {
		return fail("exec_command is not configured")
	}
	command, _ := stringArg(args, "command")
	if trimSpace(command) == "" {
		return fail("command is required")
	}
	rawLanguage, _ := stringArg(args, "language")
	language, err := resolveScriptLanguage(rawLanguage)
	if err != nil {
		return fail(err.Error())
	}

	waitTimeout := time.Duration(intArg(args, "wait_timeout", t.engine.waitSeconds)) * time.Second
	if waitTimeout <= 0 {
		waitTimeout = time.Duration(t.engine.waitSeconds) * time.Second
	}
	runTimeout := t.engine.runTimeoutDefault()
	if hasArg(args, "run_timeout") {
		if secs := intArg(args, "run_timeout", 0); secs > 0 {
			runTimeout = time.Duration(secs) * time.Second
		} else {
			runTimeout = 0
		}
	}
	maxLines := intArg(args, "max_lines", 200)
	maxChars := intArg(args, "max_chars", 30000)
	cwd, _ := stringArg(args, "cwd")
	// use_utf8 defaults to the configured stdio mode; the model may override it
	// per call (e.g. false for a program that writes the host code page). Hosts
	// without the parameter (see useUTF8ParamAvailable) always speak UTF-8.
	useUTF8 := execUseUTF8(boolArgOr(args, "use_utf8", t.engine.useUTF8))

	session, err := t.engine.launch(language, command, cwd, useUTF8)
	if err != nil {
		return fail(fmt.Sprintf("failed to start command: %v", err))
	}

	session.startWatchdog(runTimeout)

	// Adaptive wait: block up to wait_timeout. Whenever output arrives the loop
	// re-evaluates the remaining window; once the deadline passes the process is
	// left running in the background. A cancelled context (user interrupt) kills
	// the process instead of leaving it behind.
	deadline := start.Add(waitTimeout)
	for {
		done, timedOut, canceled := session.WaitForOutputContext(ctx, time.Until(deadline))
		if canceled {
			_ = session.Kill()
			return fail("interrupted by user")
		}
		if done || timedOut || !time.Now().Before(deadline) {
			break
		}
	}

	raw := session.ReadIncremental()
	shown, clean, truncated := sanitizeAndFold(raw, maxLines, maxChars)
	totalLines, totalBytes := countLinesAndBytes(clean)

	if session.IsDone() {
		code := session.GetExitCode()
		return commandResult{
			Status:         statusCompleted,
			ExitCode:       intPtr(code),
			Output:         shown,
			Truncated:      truncated,
			TotalLines:     totalLines,
			TotalBytes:     totalBytes,
			ElapsedSeconds: elapsedSeconds(start),
		}.toResult()
	}

	return commandResult{
		Status:         statusRunning,
		SessionID:      strPtr(session.ID),
		Output:         shown,
		Truncated:      truncated,
		TotalLines:     totalLines,
		TotalBytes:     totalBytes,
		ElapsedSeconds: elapsedSeconds(start),
	}.toResult()
}
