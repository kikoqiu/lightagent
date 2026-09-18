package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	res := tool.Execute(context.Background(), map[string]any{"command": command})
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
		"command":      command,
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

// TestExecCommandInterruptKillsProcess covers interrupting a running command: the
// synchronous wait is cancelled and the process is killed instead of being left
// behind in the background.
func TestExecCommandInterruptKillsProcess(t *testing.T) {
	engine := NewExecEngine(60, 30, true)
	defer engine.Close()
	tool := NewExecCommandTool(engine)

	command := "sleep 5; echo done"
	if runtime.GOOS == "windows" {
		command = "Start-Sleep -Seconds 5; Write-Output done"
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *Result, 1)
	go func() {
		done <- tool.Execute(ctx, map[string]any{"command": command, "wait_timeout": 30})
	}()

	// Wait until the session exists, then interrupt it.
	deadline := time.Now().Add(5 * time.Second)
	for len(engine.sessions.List()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the command never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	select {
	case res := <-done:
		if !res.IsError || !strings.Contains(res.ForLLM, "interrupted") {
			t.Fatalf("result = %+v, want an interrupted failure", res)
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
	res := tool.Execute(context.Background(), map[string]any{"command": command, "wait_timeout": 1})
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

	res := tool.Execute(context.Background(), map[string]any{"command": command})
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
	var parsed struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	return parsed.SessionID
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

	res := tool.Execute(context.Background(), map[string]any{"command": command})
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

	res := tool.Execute(context.Background(), map[string]any{"command": command})
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

	res := execTool.Execute(context.Background(), map[string]any{"command": command, "wait_timeout": 1})
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
		"command": utf8Command, "use_utf8": true,
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
		"command": rawCommand, "use_utf8": false,
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

	res := tool.Execute(context.Background(), map[string]any{"command": command})
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

	res := tool.Execute(context.Background(), map[string]any{"command": command})
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
	if !strings.Contains(description, "Script language used to run") {
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

	res := tool.Execute(context.Background(), map[string]any{"command": "echo hi", "language": "ruby"})
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
		"command": script, "language": ScriptLanguagePython,
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
		"command": script, "language": ScriptLanguagePython, "use_utf8": false,
	})
	if res.IsError || !strings.Contains(res.ForLLM, sample) {
		t.Fatalf("python ANSI output was not decoded: %s", res.ForLLM)
	}
}
