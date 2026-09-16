package termcolor

import (
	"os"
	"testing"
)

func TestColorHonorsEnabledFlag(t *testing.T) {
	original := Enabled()
	defer SetEnabled(original)

	SetEnabled(true)
	if got, want := Red("x"), "\x1b[31mx\x1b[0m"; got != want {
		t.Fatalf("Red(x) = %q, want %q", got, want)
	}
	SetEnabled(false)
	if got := Red("x"); got != "x" {
		t.Fatalf("Red(x) = %q, want plain text", got)
	}
}

// TestColorAllowed covers the environment switches that disable color.
func TestColorAllowed(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	if colorAllowed() {
		t.Fatal("colorAllowed = true with NO_COLOR set")
	}

	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if colorAllowed() {
		t.Fatal("colorAllowed = true with TERM=dumb")
	}

	t.Setenv("TERM", "xterm-256color")
	if !colorAllowed() {
		t.Fatal("colorAllowed = false for a capable environment")
	}
}

// TestShouldEnableRejectsPipe keeps piped/redirected output free of escapes.
func TestShouldEnableRejectsPipe(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	if shouldEnable(w) {
		t.Fatal("shouldEnable(pipe) = true, want false")
	}
}
