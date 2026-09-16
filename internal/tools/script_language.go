package tools

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Script language identifiers accepted by exec_command's `language` parameter.
const (
	// ScriptLanguagePowerShell runs the script through the host shell on
	// Windows: PowerShell, with the UTF-8 preamble when UTF-8 mode is on.
	ScriptLanguagePowerShell = "ps"
	// ScriptLanguageShell runs the script through "sh -c" on hosts whose script
	// engine is the POSIX shell; it is the non-Windows counterpart of
	// ScriptLanguagePowerShell.
	ScriptLanguageShell = "sh"
	// ScriptLanguagePython runs the script with a Python interpreter started
	// directly, without a shell in between.
	ScriptLanguagePython = "python"
)

// pythonProbeTimeout bounds the version probe so an interpreter that never
// answers cannot stall the tool description.
const pythonProbeTimeout = 5 * time.Second

// scriptLanguageOption is one selectable `language` value.
type scriptLanguageOption struct {
	// ID is the identifier the model passes in `language`.
	ID string
	// Hint is the human-readable meaning advertised in the prompt.
	Hint string
}

// pythonInterpreter is a Python interpreter found on PATH.
type pythonInterpreter struct {
	Path    string
	Version string
	Found   bool
}

var (
	systemPythonOnce sync.Once
	systemPythonVal  pythonInterpreter
)

// pythonVersionPattern captures the version from `python -V` output, which reads
// "Python 3.14.3" (stdout since 3.4, stderr before that).
var pythonVersionPattern = regexp.MustCompile(`(?i)python\s+v?(\d+(?:\.\d+)*(?:[a-z]+\d*)?)`)

// systemPython returns the cached system Python detection result.
func systemPython() pythonInterpreter {
	systemPythonOnce.Do(func() { systemPythonVal = detectPython() })
	return systemPythonVal
}

// pythonInterpreterNames lists the executables probed for a Python interpreter,
// most specific first.
func pythonInterpreterNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"python", "python3"}
	}
	return []string{"python3", "python"}
}

// detectPython looks for a Python interpreter on PATH and reads its version.
// Found stays false when nothing usable is installed - including the Windows
// Store placeholder, which exits non-zero without printing a version - so that
// callers never advertise a language that cannot run.
func detectPython() pythonInterpreter {
	for _, name := range pythonInterpreterNames() {
		path, err := exec.LookPath(name)
		if err != nil || path == "" {
			continue
		}
		version, ok := pythonVersion(path)
		if !ok {
			continue
		}
		return pythonInterpreter{Path: path, Version: version, Found: true}
	}
	return pythonInterpreter{}
}

// pythonVersion runs `path -V` and parses the reported version.
func pythonVersion(path string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), pythonProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-V").CombinedOutput()
	if err != nil {
		return "", false
	}
	match := pythonVersionPattern.FindStringSubmatch(string(out))
	if match == nil {
		return "", false
	}
	return match[1], true
}

// hostScriptLanguageID is the `language` value that runs the script through the
// host shell: PowerShell on Windows, the POSIX shell elsewhere.
func hostScriptLanguageID() string {
	if runtime.GOOS == "windows" {
		return ScriptLanguagePowerShell
	}
	return ScriptLanguageShell
}

// hostScriptLanguageOption describes the host script engine.
func hostScriptLanguageOption() scriptLanguageOption {
	if runtime.GOOS == "windows" {
		return scriptLanguageOption{
			ID:   ScriptLanguagePowerShell,
			Hint: "PowerShell script, host shell pwsh or powershell",
		}
	}
	return scriptLanguageOption{ID: ScriptLanguageShell, Hint: "POSIX shell script, host shell sh -c"}
}

// scriptLanguageOptions lists the selectable languages: the host script engine
// first, then Python once a usable interpreter was found on PATH. Python is
// omitted entirely when it is not installed, so the model never picks an engine
// that cannot run.
func scriptLanguageOptions() []scriptLanguageOption {
	options := []scriptLanguageOption{hostScriptLanguageOption()}
	if python := systemPython(); python.Found {
		options = append(options, scriptLanguageOption{
			ID:   ScriptLanguagePython,
			Hint: "Python " + python.Version + " script, interpreter " + python.Path,
		})
	}
	return options
}

// scriptLanguageIDs returns the selectable language identifiers in advertisement
// order.
func scriptLanguageIDs() []string {
	options := scriptLanguageOptions()
	ids := make([]string, 0, len(options))
	for _, option := range options {
		ids = append(ids, option.ID)
	}
	return ids
}

// scriptLanguageSummary renders the selectable languages (identifier plus hint)
// for the tool description and the `language` parameter.
func scriptLanguageSummary() string {
	options := scriptLanguageOptions()
	parts := make([]string, 0, len(options))
	for _, option := range options {
		parts = append(parts, option.ID+" = "+option.Hint)
	}
	return strings.Join(parts, "; ")
}

// resolveScriptLanguage normalizes a `language` argument and checks it against
// the selectable set. An empty value (the parameter's default) selects the host
// script engine, i.e. the shell - PowerShell on Windows, sh elsewhere - which
// keeps calls that omit the parameter behaving as before.
func resolveScriptLanguage(raw string) (string, error) {
	language := strings.ToLower(trimSpace(raw))
	if language == "" {
		return hostScriptLanguageID(), nil
	}
	for _, id := range scriptLanguageIDs() {
		if language == id {
			return language, nil
		}
	}
	return "", fmt.Errorf("unsupported language %q; available: %s", raw, strings.Join(scriptLanguageIDs(), ", "))
}

// scriptInvocation resolves the program and argument vector that runs script in
// the given language. language must come from resolveScriptLanguage; useUTF8 only
// affects the host shell, which gains the UTF-8 preamble in that mode (Python is
// told to speak UTF-8 through the child environment, see launch).
func scriptInvocation(language, script string, useUTF8 bool) (string, []string, error) {
	switch language {
	case ScriptLanguagePython:
		python := systemPython()
		if !python.Found {
			return "", nil, fmt.Errorf("language %q is unavailable: no Python interpreter found on PATH", ScriptLanguagePython)
		}
		// The interpreter is started directly, so the script must be
		// self-contained: `-c` receives the whole source text.
		return python.Path, []string{"-c", script}, nil
	case ScriptLanguagePowerShell, ScriptLanguageShell:
		name, args := shellInvocation(script, useUTF8)
		return name, args, nil
	}
	return "", nil, fmt.Errorf("unsupported language %q; available: %s", language, strings.Join(scriptLanguageIDs(), ", "))
}

// useUTF8ParamAvailable reports whether exec_command offers the `use_utf8`
// parameter. Only Windows has an ANSI code page to fall back to, so the
// parameter is Windows-only and every other host always speaks UTF-8.
func useUTF8ParamAvailable() bool { return runtime.GOOS == "windows" }

// execUseUTF8 resolves the effective child stdio mode: Windows honours the
// configured value (and exec_command's `use_utf8` parameter), every other host
// always speaks UTF-8, because the parameter is not offered there and there is no
// ANSI code page to convert.
func execUseUTF8(requested bool) bool {
	if !useUTF8ParamAvailable() {
		return true
	}
	return requested
}
