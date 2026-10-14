package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestReplaceWorkingDir covers the working-directory rewrite of a program's
// output: a whole path element is replaced by the relative marker, a longer
// name that merely starts with the directory is not, and the token boundaries
// and the case folding follow the host.
func TestReplaceWorkingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	sep := string(os.PathSeparator)
	marker := cwdRelativeMarker()

	// The same element glued to a preceding name byte — the shape "mnttmp/x"
	// has for dir "/tmp/x" — is not a path of its own, so it stays.
	glued := "mnt" + dir + sep + "f.txt"
	// A longer name that merely starts with the directory is a different path,
	// so it stays too.
	longerName := dir + "2" + sep + "f.txt"
	// The case a program happens to report the path in is the host's business:
	// Windows paths are case-insensitive, so the same path in another case is
	// still the directory. Every other host keeps them apart.
	caseFolded := strings.ToUpper(dir) + sep + "f.txt"
	caseFoldedWant := caseFolded
	if runtime.GOOS == "windows" {
		caseFoldedWant = marker + "f.txt"
	}

	cases := []struct {
		name        string
		in          string
		want        string
		wantChanged bool
	}{
		{"a leading path element", dir + sep + "main.go", marker + "main.go", true},
		{"a trailing separator", dir + sep, marker, true},
		{"a bare mention", "cwd: " + dir, "cwd: " + marker, true},
		{"a quoted mention", `"` + dir + `"`, `"` + marker + `"`, true},
		{"brackets around it", "(" + dir + ")", "(" + marker + ")", true},
		{"several mentions", dir + sep + "a.txt" + "\n" + dir, marker + "a.txt\n" + marker, true},
		{"a mention inside a longer path", dir + sep + "a.txt" + sep + dir, marker + "a.txt" + sep + dir, true},
		{"a longer name", longerName, longerName, false},
		{"a glued name", glued, glued, false},
		{"another case", caseFolded, caseFoldedWant, runtime.GOOS == "windows"},
		{"no mention", "hello world", "hello world", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := replaceWorkingDir(tc.in, dir)
			if got != tc.want || changed != tc.wantChanged {
				t.Fatalf("replaceWorkingDir(%q, %q) = (%q, %v), want (%q, %v)",
					tc.in, dir, got, changed, tc.want, tc.wantChanged)
			}
		})
	}
}

// TestReplaceWorkingDirLeavesPathRoots covers the one directory the cleanup
// refuses: a path root names every absolute path of its volume, so rewriting it
// would touch paths that have nothing to do with the call's directory.
func TestReplaceWorkingDirLeavesPathRoots(t *testing.T) {
	root := filepath.VolumeName(t.TempDir()) + string(os.PathSeparator)
	text := root + "some" + string(os.PathSeparator) + "path"
	if got, changed := replaceWorkingDir(text, root); changed || got != text {
		t.Fatalf("replaceWorkingDir(%q, %q) = (%q, %v), want the text untouched", text, root, got, changed)
	}
	if got, changed := replaceWorkingDir(text, ""); changed || got != text {
		t.Fatalf("an unresolved directory must not change the output: (%q, %v)", got, changed)
	}
}

// TestSanitizeScriptOutputAddsHeaderOnlyWhenReplaced pins the shape of the
// cleanup at the pipeline level: the header opens the output only when the
// working directory really was substituted, and the counters describe the
// cleaned output rather than counting the note.
func TestSanitizeScriptOutputAddsHeaderOnlyWhenReplaced(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	marker := cwdRelativeMarker()
	raw := dir + string(os.PathSeparator) + "a.txt\n"

	shown, truncated, lines, bytes := sanitizeScriptOutput(raw, 10, 30000, dir)
	if truncated {
		t.Fatalf("a one-line output was folded")
	}
	// The terminal normalization turns the row into one line, so the delivered
	// body is the cleaned row alone.
	if want := marker + "a.txt"; shown != workingDirCleanupHeader(dir)+"\n"+want {
		t.Fatalf("shown = %q, want the header above %q", shown, want)
	}
	if lines != 1 || bytes != len(marker+"a.txt") {
		t.Fatalf("lines = %d, bytes = %d, want the cleaned row counted", lines, bytes)
	}

	shown, _, lines, bytes = sanitizeScriptOutput("plain", 10, 30000, dir)
	if shown != "plain" || lines != 1 || bytes != len("plain") {
		t.Fatalf("an output without the directory = (%q, %d, %d), want it untouched", shown, lines, bytes)
	}
}

// TestRunScriptRewritesWorkingDirectory covers the cleanup through the tool: a
// script that prints an absolute path of its working directory delivers the path
// relative to that directory, and a header naming the substitution opens the
// output.
func TestRunScriptRewritesWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "file.txt")

	engine := NewExecEngine(60, 10, true)
	defer engine.Close()
	tool := NewRunScriptTool(engine)

	res := tool.Execute(context.Background(), map[string]any{
		"script": "echo '" + target + "'",
		"cwd":    dir,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	cr := commandResultFromResult(t, res.ForLLM)

	header := workingDirCleanupHeader(dir)
	if !strings.HasPrefix(cr.Output, header+"\n") {
		t.Fatalf("output = %q, want the cleanup header %q above it", cr.Output, header)
	}
	body := strings.TrimPrefix(cr.Output, header+"\n")
	if want := cwdRelativeMarker() + "file.txt"; !strings.Contains(body, want) {
		t.Fatalf("output body = %q, want the path rewritten to %q", body, want)
	}
	if strings.Contains(body, dir) {
		t.Fatalf("the output body still leaks the working directory: %q", body)
	}
}

// TestRunScriptWithoutWorkingDirectoryMentionHasNoHeader covers the other half:
// an output that does not name the working directory is delivered as it stands,
// with no cleanup header.
func TestRunScriptWithoutWorkingDirectoryMentionHasNoHeader(t *testing.T) {
	engine := NewExecEngine(60, 10, true)
	defer engine.Close()
	tool := NewRunScriptTool(engine)

	res := tool.Execute(context.Background(), map[string]any{"script": "echo hello"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	cr := commandResultFromResult(t, res.ForLLM)
	if strings.Contains(cr.Output, "[output:") {
		t.Fatalf("output = %q, want no cleanup header", cr.Output)
	}
	if strings.TrimRight(cr.Output, "\n") != "hello" {
		t.Fatalf("output = %q, want the program's output untouched", cr.Output)
	}
}
