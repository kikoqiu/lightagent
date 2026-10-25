package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"lightagent/internal/proc"
)

// runScriptDefaultWaitSeconds is the default synchronous wait window.
const runScriptDefaultWaitSeconds = 10

// Built-in max_lines budget of the exec tools: the default of one call and the
// most a call may ask for (tools.exec.max_lines / tools.exec.max_lines_max).
const (
	runScriptDefaultMaxLines = 50
	runScriptMaxLinesCap     = 100
)

// ExecEngine spawns shell commands and tracks their sessions. It is shared by
// run_script and manage_session so both operate on one session pool.
type ExecEngine struct {
	sessions        *SessionManager
	runTimeout      time.Duration
	waitSeconds     int
	maxLinesDefault int
	maxLinesMax     int
	useUTF8         bool
	// defaultLanguage is the engine an omitted `language` runs: the host shell
	// unless tools.exec.default_language picked another engine this host can
	// run (see SetDefaultLanguage).
	defaultLanguage string
}

// NewExecEngine builds an engine. timeoutSeconds drives the default run_timeout
// and waitSeconds the default synchronous wait window. useUTF8 is the default
// child stdio mode on Windows (run_script's `use_utf8` parameter overrides it
// per call): child processes read and write UTF-8 (shell preamble plus the
// Python UTF-8 variables) instead of the host ANSI code page; see childCodec. Hosts
// without an ANSI code page ignore it and always speak UTF-8, see execUseUTF8.
// The default script language starts at the host shell; SetDefaultLanguage
// applies tools.exec.default_language.
func NewExecEngine(timeoutSeconds, waitSeconds int, useUTF8 bool) *ExecEngine {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 3600
	}
	if waitSeconds <= 0 {
		waitSeconds = runScriptDefaultWaitSeconds
	}
	return &ExecEngine{
		sessions:        NewSessionManager(),
		runTimeout:      time.Duration(timeoutSeconds) * time.Second,
		waitSeconds:     waitSeconds,
		maxLinesDefault: runScriptDefaultMaxLines,
		maxLinesMax:     runScriptMaxLinesCap,
		useUTF8:         useUTF8,
		defaultLanguage: hostScriptLanguageID(),
	}
}

// SetMaxLines configures the max_lines budget of the exec tools
// (tools.exec.max_lines / tools.exec.max_lines_max). A call that omits the
// parameter gets defaultLines. A call that asks for more than maxLines is
// truncated to it. A non-positive value keeps the built-in one. A maximum
// below the default lowers the default to it.
func (e *ExecEngine) SetMaxLines(defaultLines, maxLines int) {
	if maxLines <= 0 {
		maxLines = runScriptMaxLinesCap
	}
	if defaultLines <= 0 {
		defaultLines = runScriptDefaultMaxLines
	}
	if defaultLines > maxLines {
		defaultLines = maxLines
	}
	e.maxLinesDefault = defaultLines
	e.maxLinesMax = maxLines
}

// effectiveMaxLines resolves the max_lines argument of a call. A missing, zero
// or negative value uses the configured default. A value above the configured
// maximum is truncated to it.
func (e *ExecEngine) effectiveMaxLines(args map[string]any) int {
	lines := intArg(args, "max_lines", e.maxLinesDefault)
	if lines <= 0 {
		lines = e.maxLinesDefault
	}
	return minInt(lines, e.maxLinesMax)
}

// Sessions exposes the shared session manager.
func (e *ExecEngine) Sessions() *SessionManager { return e.sessions }

// SetDefaultLanguage configures the engine a run_script call runs when it omits
// `language` (tools.exec.default_language): the named engine when this host
// offers it, the host shell otherwise. An empty value, an unknown name and an
// engine whose interpreter is missing all keep the host shell, so the default is
// always an engine that can run. It returns the engine now in effect and whether
// that is the one the configuration asked for (false = fell back to the host
// shell), which lets the caller report a setting that did not apply. The tool
// schema and description read the effective engine, so the prompt never names
// one that cannot run; the value is read at startup, so a change applies after a
// restart.
func (e *ExecEngine) SetDefaultLanguage(name string) (string, bool) {
	language, ok := resolveConfiguredScriptLanguage(name)
	e.defaultLanguage = language
	return language, ok
}

// DefaultLanguage returns the engine an omitted `language` runs.
func (e *ExecEngine) DefaultLanguage() string {
	if e.defaultLanguage == "" {
		return hostScriptLanguageID()
	}
	return e.defaultLanguage
}

// resolveLanguage resolves a run_script `language` argument the way resolveScriptLanguage
// does, except that an omitted value selects the configured default engine
// instead of always the host shell.
func (e *ExecEngine) resolveLanguage(raw string) (string, error) {
	return scriptLanguageForRequest(raw, e.DefaultLanguage())
}

// Close terminates every session that is still running and stops the session
// cleanup goroutine, so no child process tree outlives the engine.
func (e *ExecEngine) Close() {
	e.sessions.KillAll()
	e.sessions.Stop()
}

// runTimeoutDefault returns the effective hard-lifetime default.
func (e *ExecEngine) runTimeoutDefault() time.Duration {
	if e.runTimeout > 0 {
		return e.runTimeout
	}
	return time.Hour
}

// maxLinesRule states the max_lines budget of one call to the model: its
// default, the maximum a call may ask for, and the advice to redirect output
// that has to survive in full to a file.
func (e *ExecEngine) maxLinesRule() string {
	return fmt.Sprintf(" `max_lines` caps the lines one call returns (default: %d, maximum: %d, a "+
		"larger value is truncated). Redirect important output to a file and read it back.",
		e.maxLinesDefault, e.maxLinesMax)
}

// runScriptExamplesHint returns a short example pair for the run_script
// description. The first example is written in the default engine's own
// language (tools.exec.default_language, the host shell unless configured
// otherwise), so it is source that engine can actually run; when Python is
// selectable but is not the default, the recommended Python call follows, which
// keeps a model from wrapping Python in a shell with a `cd` at the front or an
// inline one-liner. The wording is advisory: the tool still runs whatever source
// the model sends.
func runScriptExamplesHint(defaultLanguage string) string {
	hint := " The default engine `" + defaultLanguage + "` runs a call that omits `language`." +
		" Example: " + runScriptExample(defaultLanguage) + "."
	if defaultLanguage == ScriptLanguagePython || !systemPython().Found {
		return hint
	}
	return hint + " For Python prefer {\"language\":\"python\",\"script\":\"print(1)\"}," +
		" or {\"language\":\"python\",\"cwd\":\"src\",\"script\":\"...\"} for another directory," +
		" over a shell wrapper such as {\"script\":\"cd src; python -c 'print(1)'\"}."
}

// runScriptExample is the example call for an engine: the source matches the
// language, so the default-engine example never hands Python source to a shell
// or a shell command to an interpreter.
func runScriptExample(language string) string {
	switch language {
	case ScriptLanguagePython:
		return `{"script":"print(1)"}`
	case ScriptLanguagePowerShell:
		return `{"script":"Get-ChildItem -Name"}`
	}
	return `{"script":"ls -la"}`
}

// defaultEngineDescription states which engine an omitted `language` runs, so
// the parameter description names the engine that is actually in effect. It
// deliberately names no configuration key: the model only needs to know which
// engine runs, not how the host was configured.
func defaultEngineDescription(defaultLanguage string) string {
	if defaultLanguage == hostScriptLanguageID() {
		return "the host shell (`" + defaultLanguage + "`)"
	}
	return "the default engine (`" + defaultLanguage + "`)"
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

// execWaitDelay bounds how long the session waits for the output pipes after the
// script's process itself has exited. A root that exits while a background
// program of the script still holds them open would otherwise strand the
// session in "running" until the hard timeout; with the delay the session
// finishes and the leftover program is terminated by the teardown (see
// proc.Reap).
const execWaitDelay = 2 * time.Second

// resolveWorkingDir returns the absolute directory the child process runs in:
// the call's `cwd` resolved against the agent's own directory, or that
// directory when the call names none (the child inherits it, see launch). It
// returns "" when the directory cannot be resolved, and the output cleanup then
// leaves the program's output alone.
func resolveWorkingDir(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		dir, err := os.Getwd()
		if err != nil {
			return ""
		}
		return dir
	}
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	return dir
}

// launch starts script and returns its session. language (see
// resolveScriptLanguage) selects the script engine: the host shell, the ash
// interpreter of busybox64u, or a Python interpreter started directly. The
// returned session is already registered in the session manager. useUTF8 selects
// the child stdio mode: the bytes are either passed through untouched or
// converted in Go with the host ANSI code page; see childCodec. An engine that
// speaks UTF-8 itself (busybox ash, see scriptLanguageAlwaysUTF8) always keeps
// its raw bytes, whatever the mode - converting them would only garble them.
func (e *ExecEngine) launch(language, script, cwd string, useUTF8 bool) (*ProcessSession, error) {
	name, args, err := scriptInvocation(language, script, useUTF8)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(name, args...)

	if cwd != "" {
		cmd.Dir = cwd
	}
	utf8Stdio := useUTF8 || scriptLanguageAlwaysUTF8(language)
	if utf8Stdio {
		cmd.Env = utf8ChildEnvironment(os.Environ())
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	codec := childCodec(utf8Stdio)
	session := &ProcessSession{
		ID:        generateSessionID(),
		Script:    script,
		Language:  language,
		StartTime: time.Now().Unix(),
		Status:    "running",
		stdin:     codec.wrapStdin(stdin),
		proc:      cmd,
	}
	stdout := newConsoleOutputWriter(session.appendOutput, codec.charset)
	stderr := newConsoleOutputWriter(session.appendOutput, codec.charset)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = execWaitDelay

	// The child leads its own process tree, so the session owns everything the
	// script spawns (see internal/proc).
	if err := proc.Start(cmd); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start command: %w", err)
	}
	session.PID = cmd.Process.Pid
	e.sessions.Add(session)

	go func() {
		waitErr := cmd.Wait()
		// The root process is gone; whatever it left behind in its tree (a
		// program the script started in the background, for example) is
		// terminated with it, so a finished session leaks nothing.
		proc.Reap(cmd)
		_ = stdin.Close()
		// Wait returned, so the output copiers are done: flush the decoders so
		// a character split by the final pipe read is still emitted.
		_ = stdout.Close()
		_ = stderr.Close()

		code := 0
		switch {
		case cmd.ProcessState != nil:
			// The real exit status, also when Wait reported something else on
			// top of it (ErrWaitDelay after a leftover program held the pipes).
			code = cmd.ProcessState.ExitCode()
		case waitErr != nil:
			code = -1
		}
		session.mu.Lock()
		session.markDoneLocked(code)
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

// windowsShellFlags are the options lightagent passes to PowerShell (pwsh or
// powershell) before `-Command`: -NoProfile loads no profile script,
// -NonInteractive never waits for input, and -ExecutionPolicy Bypass keeps the
// execution policy from blocking the script. The run_script description
// advertises them verbatim when PowerShell is a selectable language (see
// powerShellFlagsHint).
var windowsShellFlags = []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass"}

// shellInvocation resolves the shell binary and argument vector for the host
// script language (ScriptLanguagePowerShell / ScriptLanguageShell). In UTF-8
// mode the script gains the windowsUTF8Preamble prefix so the shell and the
// programs it spawns read/write UTF-8; in legacy mode no preamble is injected (a
// UTF-8 preamble would garble programs that write the host code page, e.g.
// Python on a zh-CN host) and Go converts the host code page instead. Windows
// uses PowerShell, other hosts use "sh -c".
func shellInvocation(script string, useUTF8 bool) (string, []string) {
	if runtime.GOOS != "windows" {
		return "sh", []string{"-c", script}
	}
	args := append([]string{}, windowsShellFlags...)
	args = append(args, "-Command", windowsCommandScript(script, useUTF8))
	return resolveWindowsShell(), args
}

// windowsCommandScript prepends the UTF-8 preamble to a PowerShell script when
// UTF-8 mode is on.
func windowsCommandScript(script string, useUTF8 bool) string {
	if !useUTF8 {
		return script
	}
	return windowsUTF8Preamble + script
}

// Python environment variables UTF-8 mode exports into a child. The shell
// preamble cannot reach an interpreter started as a child process, and a
// `python` the script itself calls is such a child too, so the variables are
// exported into the child environment instead; every program the script spawns
// inherits them.
const (
	// pythonIOEncodingEnv forces Python's stdin/stdout/stderr to UTF-8.
	pythonIOEncodingEnv = "PYTHONIOENCODING"
	// pythonUTF8Env turns on Python's UTF-8 mode. On Windows PYTHONIOENCODING
	// only covers the standard streams: Python keeps reading the filesystem
	// and locale through the ANSI code page, so an interpreter the script
	// starts (a PowerShell one-liner, for instance) still garbles non-ASCII
	// paths and arguments. PYTHONUTF8=1 makes Python speak UTF-8 for those
	// too, so a shell engine that calls python is safe as well.
	pythonUTF8Env = "PYTHONUTF8"
)

// utf8ChildEnvironment returns base with the Python UTF-8 variables forced:
// PYTHONIOENCODING=utf-8 always, plus PYTHONUTF8=1 on Windows (see the
// pythonUTF8Env comment). An inherited value is replaced in place and a missing
// one is appended exactly once.
func utf8ChildEnvironment(base []string) []string {
	entries := []string{pythonIOEncodingEnv + "=utf-8"}
	if runtime.GOOS == "windows" {
		entries = append(entries, pythonUTF8Env+"=1")
	}
	env := make([]string, 0, len(base)+len(entries))
	applied := make([]bool, len(entries))
	for _, kv := range base {
		name, _, found := strings.Cut(kv, "=")
		index := -1
		if found {
			index = envEntryIndex(name, entries)
		}
		if index < 0 {
			env = append(env, kv)
			continue
		}
		// Keep the variable where it was, and only its first spelling, so an
		// inherited duplicate does not survive next to the forced value.
		if !applied[index] {
			env = append(env, entries[index])
			applied[index] = true
		}
	}
	for i, entry := range entries {
		if !applied[i] {
			env = append(env, entry)
		}
	}
	return env
}

// envEntryIndex returns the index of the `NAME=value` entry whose name matches
// name case-insensitively, or -1.
func envEntryIndex(name string, entries []string) int {
	for i, entry := range entries {
		entryName, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, entryName) {
			return i
		}
	}
	return -1
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

// waitForUserMessage returns a context that is cancelled when signal fires,
// together with the function that releases the watch. A nil signal leaves ctx
// as it is. The cancellation is what ends a wait early (see
// ProcessSession.WaitForExitContext), and the caller tells the two
// cancellations apart: a user interrupt is still visible on the original ctx
// while a user message is not.
func waitForUserMessage(ctx context.Context, signal UserMessageSignal) (context.Context, func()) {
	if signal == nil {
		return ctx, func() {}
	}
	waitCtx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-signal:
			cancel()
		case <-waitCtx.Done():
		}
	}()
	return waitCtx, cancel
}

// userMessageWarning is the note run_script answers with when a new user message
// ended its wait: it tells the model why the wait stopped short, where the
// program went, and that the output below is what the console had printed at
// that moment.
func userMessageWarning(sessionID string) string {
	return fmt.Sprintf("a new user message arrived while the program was still running, so the wait ended early: "+
		"the program is now running in the background as session %s (poll it with manage_session). "+
		"The console output so far is reported below.", sessionID)
}

// RunScriptTool runs scripts with the wait-then-background model: the source is
// handed to the engine chosen by the `language` parameter (the host shell by
// default).
type RunScriptTool struct {
	engine *ExecEngine
}

// NewRunScriptTool wraps a shared engine with the run_script surface.
func NewRunScriptTool(engine *ExecEngine) *RunScriptTool {
	return &RunScriptTool{engine: engine}
}

// Name implements Tool.
func (t *RunScriptTool) Name() string { return "run_script" }

// Description implements Tool.
func (t *RunScriptTool) Description() string {
	description := fmt.Sprintf("Run a script with state-aware execution. This tool runs source text, "+
		"not a shell command line: `script` holds the source, `language` selects the engine that "+
		"interprets it, and the text is handed to that engine as-is. It is recommended to put "+
		"the program's own source directly in `script` and pick the `language` that matches it, rather "+
		"than shelling out to another interpreter's inline (`-c`) one-liner, and to set `cwd` for the "+
		"working directory instead of starting the script with a `cd`. "+
		"Synchronously waits up to `wait_timeout` seconds (default: %d). If it finishes within "+
		"that window, returns exit_code and output directly. If it exceeds `wait_timeout` it "+
		"detaches to the background and returns a `session_id`: the call is a start followed by "+
		"the same step as `manage_session` poll, so either one reports the whole output buffered "+
		"at that moment and clears it. A `wait_timeout` above the default also ends early when a "+
		"new user message arrives: the program keeps running in the background (same `session_id` "+
		"answer, plus a warning) and the message is handled first. The process lifetime is capped "+
		"by `run_timeout` (default: %d seconds). Output is cleaned and truncated by "+
		"`max_lines`/`max_chars`, which bound what a call returns rather than the process lifetime.",
		t.engine.waitSeconds, int(t.engine.runTimeoutDefault()/time.Second))
	description += runScriptExamplesHint(t.engine.DefaultLanguage())
	description += t.engine.maxLinesRule()
	description += powerShellFlagsHint()
	if !useUTF8ParamAvailable() {
		return description
	}
	return description + fmt.Sprintf(" Stdio encoding is selected by `use_utf8` (default: %t): "+
		"true forces PowerShell and Python onto UTF-8 (UTF-8 shell preamble plus "+
		"PYTHONIOENCODING=utf-8, and PYTHONUTF8=1 on Windows so a python the script calls "+
		"does not fall back to the host code page) and returns the raw bytes; false makes the agent decode the "+
		"host ANSI code page automatically (stdin is encoded into it as well) - everything "+
		"else behaves the same, but characters outside the host code page may not be displayed. "+
		"The busybox engine speaks UTF-8 itself, so its stdio is returned unchanged in either mode.",
		t.engine.useUTF8)
}

// Parameters implements Tool.
func (t *RunScriptTool) Parameters() map[string]any {
	runDefault := int(t.engine.runTimeoutDefault() / time.Second)
	defaultLanguage := t.engine.DefaultLanguage()
	properties := map[string]any{
		"script": map[string]any{
			"type": "string",
			"description": "The script source: the source code of the language named by `language`, i.e. the " +
				"text itself, not a path to a script file, handed to that engine as-is and run as one program.",
		},
		"language": map[string]any{
			"type":    "string",
			"enum":    scriptLanguageIDs(),
			"default": defaultLanguage,
			"description": "The engine that interprets `script`, i.e. the language its source is written " +
				"in: it selects what runs the text, not a label for the call. Available here: " +
				scriptLanguageSummary() + ". Omit to use " + defaultEngineDescription(defaultLanguage) + ".",
		},
		"wait_timeout": map[string]any{
			"type":    "integer",
			"default": t.engine.waitSeconds,
			"description": "Max seconds to wait synchronously before auto-backgrounding. Default: " +
				fmt.Sprint(t.engine.waitSeconds) + ". A value above the default yields to a new user message: " +
				"the wait ends early, the program keeps running in the background, and the answer reports the output so far.",
		},
		"run_timeout": map[string]any{
			"type":        "integer",
			"default":     runDefault,
			"description": fmt.Sprintf("Absolute hard timeout in seconds for the whole process lifetime (background included). Default: %d. Set 0 to disable.", runDefault),
		},
		"cwd": map[string]any{
			"type":        "string",
			"description": "Working directory for the script. Recommended over a `cd` at the start of the script.",
		},
	}
	// use_utf8 exists only where the host has an ANSI code page to fall back to.
	if useUTF8ParamAvailable() {
		properties["use_utf8"] = map[string]any{
			"type":        "boolean",
			"default":     t.engine.useUTF8,
			"description": fmt.Sprintf("Stdio encoding for this command (default: %t). true forces PowerShell and Python onto UTF-8: the script gains a UTF-8 preamble ([Console] input/output code pages and $OutputEncoding), PYTHONIOENCODING=utf-8 and (on Windows) PYTHONUTF8=1 are exported, and the output bytes are returned unchanged. false makes the agent decode the host ANSI code page (CP_ACP, e.g. GBK on a zh-CN host) automatically and encode stdin into it; everything else behaves the same, but characters outside the host code page may not be displayed. Use false for a program that only writes the host code page. The busybox engine always speaks UTF-8, so it is unaffected by either value.", t.engine.useUTF8),
		}
	}
	properties["max_lines"] = map[string]any{
		"type":    "integer",
		"default": t.engine.maxLinesDefault,
		"description": fmt.Sprintf("Maximum lines this call returns (head/tail folded). The limit is per "+
			"call, not over the process lifetime. Default: %d, maximum: %d, and a larger value is "+
			"truncated to it. Redirect important output to a file and read it back.",
			t.engine.maxLinesDefault, t.engine.maxLinesMax),
	}
	properties["max_chars"] = map[string]any{
		"type":        "integer",
		"default":     30000,
		"description": "Maximum characters this call returns. The limit is per call, not over the process lifetime. Default: 30000.",
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   []string{"script"},
	}
}

// Execute implements Tool.
func (t *RunScriptTool) Execute(ctx context.Context, args map[string]any) *Result {
	start := time.Now()
	fail := func(message string) *Result {
		return commandResult{Status: statusFailed, Output: message, ElapsedSeconds: elapsedSeconds(start)}.toResult()
	}

	if t.engine == nil {
		return fail("run_script is not configured")
	}
	script, _ := stringArg(args, "script")
	if trimSpace(script) == "" {
		return fail("script is required")
	}
	rawLanguage, _ := stringArg(args, "language")
	language, err := t.engine.resolveLanguage(rawLanguage)
	if err != nil {
		return fail(err.Error())
	}

	// The configured default is what a call that names no window gets; only a
	// window the model asked to be longer than that is the model deciding to
	// sit and watch a long-running program, and only that wait yields to a new
	// user message (see the wait below).
	defaultWait := time.Duration(t.engine.waitSeconds) * time.Second
	waitTimeout := time.Duration(intArg(args, "wait_timeout", t.engine.waitSeconds)) * time.Second
	if waitTimeout <= 0 {
		waitTimeout = defaultWait
	}
	runTimeout := t.engine.runTimeoutDefault()
	if hasArg(args, "run_timeout") {
		if secs := intArg(args, "run_timeout", 0); secs > 0 {
			runTimeout = time.Duration(secs) * time.Second
		} else {
			runTimeout = 0
		}
	}
	maxLines := t.engine.effectiveMaxLines(args)
	maxChars := intArg(args, "max_chars", 30000)
	cwd, _ := stringArg(args, "cwd")
	// use_utf8 defaults to the configured stdio mode; the model may override it
	// per call (e.g. false for a program that writes the host code page). Hosts
	// without the parameter (see useUTF8ParamAvailable) always speak UTF-8.
	useUTF8 := execUseUTF8(boolArgOr(args, "use_utf8", t.engine.useUTF8))
	// The directory the child runs in is what the output cleanup hides; it is
	// resolved before the launch, so a call that cannot be resolved simply
	// leaves the output alone.
	workDir := resolveWorkingDir(cwd)

	session, err := t.engine.launch(language, script, cwd, useUTF8)
	if err != nil {
		return fail(fmt.Sprintf("failed to start script: %v", err))
	}

	session.startWatchdog(runTimeout)

	// Wait up to wait_timeout for the process to exit. Output written on the way
	// does not end the wait: it accumulates and is reported with the result (see
	// sessionOutput). Once the window passes, the process is left running in the
	// background. A cancelled context (user interrupt) kills it instead of
	// leaving it behind — this is the one call that is not allowed to keep
	// running — and what the process had printed until then is reported with the
	// interrupted status instead of being discarded. A process that exited at
	// the very moment of the interrupt is still reported as completed.
	//
	// A new user message ends the wait too, but only a wait window the call
	// asked to be longer than the default: the model chose to watch a
	// long-running program and the user must not be held behind that choice.
	// The program is left running in the background and the model is told so
	// together with what the console printed until then, so it can answer the
	// user while the program keeps going; the window that was asked to be short
	// is not cut — it passes in a moment, and the steering message is folded
	// into the conversation by the round that follows anyway.
	var userMessage UserMessageSignal
	if waitTimeout > defaultWait {
		userMessage = UserMessageSignalFrom(ctx)
	}
	waitCtx, stopWaiting := waitForUserMessage(ctx, userMessage)
	defer stopWaiting()

	deadline := start.Add(waitTimeout)
	_, _, canceled := session.WaitForExitContext(waitCtx, time.Until(deadline))
	if canceled && !session.IsDone() {
		if ctx.Err() == nil {
			// A new user message arrived: the program is not killed, it is
			// left running in the background (it is polled like any other
			// session), and the answer carries what the console printed so
			// far so the model can act on the message right away.
			raw := session.TakeOutput()
			shown, truncated, totalLines, totalBytes := sanitizeScriptOutput(raw, maxLines, maxChars, workDir)
			return commandResult{
				Status:         statusRunning,
				SessionID:      strPtr(session.ID),
				Output:         shown,
				Truncated:      truncated,
				TotalLines:     totalLines,
				TotalBytes:     totalBytes,
				ElapsedSeconds: elapsedSeconds(start),
				Warning:        strPtr(userMessageWarning(session.ID)),
			}.toResult()
		}
		raw := session.TakeOutput()
		_ = session.Kill()
		// The tree is gone and its output was just reported, so nothing is left
		// to keep in the pool.
		t.engine.sessions.Collect(session)
		shown, truncated, totalLines, totalBytes := sanitizeScriptOutput(raw, maxLines, maxChars, workDir)
		return commandResult{
			Status:         statusInterrupted,
			Output:         shown,
			Truncated:      truncated,
			TotalLines:     totalLines,
			TotalBytes:     totalBytes,
			ElapsedSeconds: elapsedSeconds(start),
		}.toResult()
	}
	// The wait is over: take what the child produced, whether it exited within
	// the window or is still running. The whole buffer is handed over as it
	// stands — the line it is still repainting included — and emptied, so the
	// next call (manage_session poll, for instance) reports only what arrives
	// after this one. The exit is read first: a process that has already exited
	// has all of its output in the buffer (the child is reaped and its pipes are
	// closed before the session is marked done), so nothing the caller needs is
	// left behind.
	done := session.IsDone()
	raw := session.TakeOutput()
	shown, truncated, totalLines, totalBytes := sanitizeScriptOutput(raw, maxLines, maxChars, workDir)

	if done {
		code := session.GetExitCode()
		// The exit code and the last output are both in this answer, so the
		// session has nothing left to be kept for.
		t.engine.sessions.Collect(session)
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
