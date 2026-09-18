package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestWorkingDirectoryInfo verifies the working-directory line states the path
// only: the directory's children are never listed, so the section stays one line
// however large the project is.
func TestWorkingDirectoryInfo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := WorkingDirectoryInfo(dir)
	want := "working directory: " + dir
	if got != want {
		t.Fatalf("WorkingDirectoryInfo = %q, want %q", got, want)
	}
}

// TestWorkingDirectoryInfoEmpty verifies an unknown directory yields an empty
// section instead of a path line.
func TestWorkingDirectoryInfoEmpty(t *testing.T) {
	if got := WorkingDirectoryInfo(""); got != "" {
		t.Fatalf("WorkingDirectoryInfo of an empty dir = %q, want empty", got)
	}
}

// TestDefaultSystemPromptHasNoHostState verifies the built-in template carries no
// host-specific data, so an agent.md exported from it stays valid on another
// machine. It still has to point the model at the exec_command language
// parameter.
func TestDefaultSystemPromptHasNoHostState(t *testing.T) {
	prompt := DefaultSystemPrompt()
	if strings.Contains(prompt, "Runtime:") {
		t.Fatalf("built-in prompt must not embed the runtime line:\n%s", prompt)
	}
	if !strings.Contains(prompt, "language") {
		t.Fatalf("built-in prompt must mention the exec_command language parameter:\n%s", prompt)
	}
}

// TestRuntimeInfo verifies the runtime line reports the platform the binary
// actually runs on.
func TestRuntimeInfo(t *testing.T) {
	want := "Runtime: " + runtime.GOOS + "/" + runtime.GOARCH + "."
	if got := RuntimeInfo(); got != want {
		t.Fatalf("RuntimeInfo = %q, want %q", got, want)
	}
}
