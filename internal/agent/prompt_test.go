package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDirectoryListing verifies the working-directory section format: the path
// line, the format legend, then subdirectories (annotated with their direct-child
// count) and finally files, each group sorted.
func TestDirectoryListing(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, sub := range []string{"alpha", "beta"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// alpha holds two direct children; beta stays empty.
	for _, name := range []string{"one.go", "two.go"} {
		if err := os.WriteFile(filepath.Join(dir, "alpha", name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := DirectoryListing(dir)
	want := "working directory: " + dir + "\n" +
		" dir[children count] / file\n" +
		"alpha[2]\nbeta[0]\na.txt\nb.txt"
	if got != want {
		t.Fatalf("DirectoryListing =\n%s\nwant\n%s", got, want)
	}
}

// TestDirectoryListingUnreadable verifies an unreadable directory yields an
// empty section instead of an error.
func TestDirectoryListingUnreadable(t *testing.T) {
	if got := DirectoryListing(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Fatalf("DirectoryListing of a missing dir = %q, want empty", got)
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
