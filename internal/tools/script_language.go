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

// Script language identifiers accepted by run_script's `language` parameter.
// They are the canonical, fully spelled names the schema enum lists; the common
// short spellings are accepted as aliases (see scriptLanguageOption.Aliases), so
// a model that names an engine the way it is usually written still reaches it.
const (
	// ScriptLanguagePowerShell runs the script through the host shell on
	// Windows: PowerShell, with the UTF-8 preamble when UTF-8 mode is on.
	ScriptLanguagePowerShell = "powershell"
	// ScriptLanguageShell runs the script through "sh -c" on hosts whose script
	// engine is the POSIX shell; it is the non-Windows counterpart of
	// ScriptLanguagePowerShell.
	ScriptLanguageShell = "sh"
	// ScriptLanguageBusyBox runs the script through the ash interpreter of
	// busybox64u.exe found on PATH. ash is a POSIX shell, so the source reads
	// like a sh script; the busybox64u build speaks UTF-8 on redirected stdio,
	// so its bytes never go through the host code page conversion.
	ScriptLanguageBusyBox = "busybox"
	// ScriptLanguagePython runs the script with a Python interpreter started
	// directly, without a shell in between.
	ScriptLanguagePython = "python"
)

// pythonProbeTimeout bounds the version probe so an interpreter that never
// answers cannot stall the tool description.
const pythonProbeTimeout = 5 * time.Second

// scriptLanguageOption is one selectable `language` value.
type scriptLanguageOption struct {
	// ID is the canonical identifier the model passes in `language` and the
	// schema enum lists.
	ID string
	// Aliases are the alternative spellings accepted for ID: the short form or
	// the name the engine is usually called by (ps, bash, python3, ...). They
	// resolve to ID and are never advertised as separate selectable values.
	Aliases []string
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

// busyBoxExecutable is the binary probed for the BusyBox script engine on
// Windows. Only the Unicode build (the trailing "u") is supported: it reads and
// writes UTF-8 on redirected stdio, so the host ANSI code page never garbles its
// output.
const busyBoxExecutable = "busybox64u.exe"

// busyBoxApplet is the applet the BusyBox script engine runs: ash, the POSIX
// shell of busybox. It receives the whole source through `-c`.
const busyBoxApplet = "ash"

// busyBoxProgram is a busybox64u binary found on PATH.
type busyBoxProgram struct {
	Path  string
	Found bool
}

var (
	systemBusyBoxOnce sync.Once
	systemBusyBoxVal  busyBoxProgram
)

// systemBusyBox returns the cached busybox64u detection result.
func systemBusyBox() busyBoxProgram {
	systemBusyBoxOnce.Do(func() { systemBusyBoxVal = detectBusyBox() })
	return systemBusyBoxVal
}

// detectBusyBox looks for busybox64u.exe on PATH. The engine is Windows-only:
// the binary is a Windows build and the ANSI host code page is what makes its
// always-UTF-8 stdio worth offering, so every other host reports none. Found
// stays false when the executable is missing, so the language is then never
// advertised.
func detectBusyBox() busyBoxProgram {
	if runtime.GOOS != "windows" {
		return busyBoxProgram{}
	}
	path, err := exec.LookPath(busyBoxExecutable)
	if err != nil || path == "" {
		return busyBoxProgram{}
	}
	return busyBoxProgram{Path: path, Found: true}
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
			ID:      ScriptLanguagePowerShell,
			Aliases: []string{"ps", "pwsh"},
			Hint:    "PowerShell script, host shell pwsh or powershell",
		}
	}
	return scriptLanguageOption{
		ID:      ScriptLanguageShell,
		Aliases: []string{"bash", "shell"},
		Hint:    "POSIX shell script, host shell sh -c",
	}
}

// scriptLanguageOptions lists the selectable languages: the host script engine
// first, then busybox ash when busybox64u.exe is on PATH, then Python once a
// usable interpreter was found on PATH. An engine that cannot run here is
// omitted entirely, so the model never picks one that is missing. The host
// engine stays first because it is what a call that omits `language` runs; the
// BusyBox engine is offered as a choice only, never as the default.
func scriptLanguageOptions() []scriptLanguageOption {
	options := []scriptLanguageOption{hostScriptLanguageOption()}
	if busybox := systemBusyBox(); busybox.Found {
		options = append(options, scriptLanguageOption{
			ID:      ScriptLanguageBusyBox,
			Aliases: []string{"ash"},
			Hint: "POSIX shell script, ash interpreter of " + busybox.Path +
				" (busybox64u, always UTF-8)",
		})
	}
	if python := systemPython(); python.Found {
		options = append(options, scriptLanguageOption{
			ID:      ScriptLanguagePython,
			Aliases: []string{"python3", "py"},
			Hint: "Python " + python.Version + " script, interpreter " + python.Path +
				" (started directly, no shell in between)",
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

// scriptLanguageAliasTarget resolves one spelling of a `language` value to the
// canonical identifier it names: the identifier itself or one of its aliases.
// ok is false for a spelling no selectable engine answers to. Aliases belong to
// the option, so this is also how an alias of an engine the host does not offer
// is rejected: `bash` on Windows and `ps` on Unix are not in the advertised set
// and fail here like any other unknown spelling.
func scriptLanguageAliasTarget(name string) (string, bool) {
	for _, option := range scriptLanguageOptions() {
		if name == option.ID {
			return option.ID, true
		}
		for _, alias := range option.Aliases {
			if name == alias {
				return option.ID, true
			}
		}
	}
	return "", false
}

// scriptLanguageAllowsPowerShell reports whether PowerShell is one of the
// selectable `language` values on this host, i.e. the host shell is PowerShell.
// The tool description only advertises PowerShell-specific options when it is.
func scriptLanguageAllowsPowerShell() bool {
	for _, id := range scriptLanguageIDs() {
		if id == ScriptLanguagePowerShell {
			return true
		}
	}
	return false
}

// powerShellFlagsHint states the interpreter flags lightagent passes to the
// PowerShell host shell, for the run_script description. The flags come from
// windowsShellFlags, so the wording never drifts from the invocation. It is
// empty unless PowerShell is a selectable language, so a host whose only engine
// is another shell never advertises PowerShell options.
func powerShellFlagsHint() string {
	if !scriptLanguageAllowsPowerShell() {
		return ""
	}
	return " On PowerShell the shell is started with `" + strings.Join(windowsShellFlags, " ") +
		"`, so no profile script is loaded, the shell never waits for input, and the execution " +
		"policy does not block the script."
}

// scriptLanguageSummary renders the selectable languages for the `language`
// parameter description: the canonical identifier, its accepted aliases and the
// hint that says what it runs. The tool description states the mechanism only,
// so the value list lives next to the enum in the parameter itself.
func scriptLanguageSummary() string {
	options := scriptLanguageOptions()
	parts := make([]string, 0, len(options))
	for _, option := range options {
		label := option.ID
		if len(option.Aliases) > 0 {
			label += " (aliases: " + strings.Join(option.Aliases, ", ") + ")"
		}
		parts = append(parts, label+" = "+option.Hint)
	}
	return strings.Join(parts, "; ")
}

// resolveScriptLanguage normalizes a `language` argument and checks it against
// the selectable set. The spelling is matched case-insensitively against the
// canonical identifiers and their aliases (ps/pwsh for powershell, bash/shell
// for sh, python3/py for python), so a model that spells an engine the way it is
// usually written still reaches it; an alias of an engine this host does not
// offer is rejected like any other unavailable value. An empty value (the
// parameter's default) selects the host script engine, i.e. the shell -
// PowerShell on Windows, sh elsewhere - which keeps calls that omit the
// parameter behaving as before.
func resolveScriptLanguage(raw string) (string, error) {
	language := strings.ToLower(trimSpace(raw))
	if language == "" {
		return hostScriptLanguageID(), nil
	}
	if id, ok := scriptLanguageAliasTarget(language); ok {
		return id, nil
	}
	return "", fmt.Errorf("unsupported language %q; available: %s", raw, strings.Join(scriptLanguageIDs(), ", "))
}

// scriptInvocation resolves the program and argument vector that runs script in
// the given language. language must come from resolveScriptLanguage; useUTF8 only
// affects the host shell, which gains the UTF-8 preamble in that mode (Python is
// told to speak UTF-8 through the child environment, see launch; busybox ash
// always speaks UTF-8, see scriptLanguageAlwaysUTF8).
func scriptInvocation(language, script string, useUTF8 bool) (string, []string, error) {
	switch language {
	case ScriptLanguageBusyBox:
		busybox := systemBusyBox()
		if !busybox.Found {
			return "", nil, fmt.Errorf("language %q is unavailable: %s was not found on PATH", ScriptLanguageBusyBox, busyBoxExecutable)
		}
		// The busybox binary is started directly and names the applet, so
		// the process tree is rooted at busybox itself; ash receives the
		// whole source through `-c`.
		return busybox.Path, []string{busyBoxApplet, "-c", script}, nil
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

// useUTF8ParamAvailable reports whether run_script offers the `use_utf8`
// parameter. Only Windows has an ANSI code page to fall back to, so the
// parameter is Windows-only and every other host always speaks UTF-8.
func useUTF8ParamAvailable() bool { return runtime.GOOS == "windows" }

// execUseUTF8 resolves the effective child stdio mode: Windows honours the
// configured value (and run_script's `use_utf8` parameter), every other host
// always speaks UTF-8, because the parameter is not offered there and there is no
// ANSI code page to convert.
func execUseUTF8(requested bool) bool {
	if !useUTF8ParamAvailable() {
		return true
	}
	return requested
}

// scriptLanguageAlwaysUTF8 reports whether the engine speaks UTF-8 on redirected
// stdio no matter what the host code page is, so its bytes must never be
// converted. busybox64u is a Unicode build: it reads and writes UTF-8 even on an
// ANSI host, which is why the language is useful there. A call that runs it in
// legacy mode therefore still gets the raw bytes (see ExecEngine.launch).
func scriptLanguageAlwaysUTF8(language string) bool {
	return language == ScriptLanguageBusyBox
}
