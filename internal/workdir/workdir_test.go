package workdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExpandResolvesHome pins the ~ spelling: a bare ~ and a ~/prefix become
// the home directory, while anything else is left untouched.
func TestExpandResolvesHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory on this host")
	}
	if got := Expand("~"); got != home {
		t.Fatalf("Expand(~) = %q, want %q", got, home)
	}
	want := filepath.Join(home, "projects")
	if got := Expand("~/projects"); got != want {
		t.Fatalf("Expand(~/projects) = %q, want %q", got, want)
	}
	for _, in := range []string{"", "plain", "/abs", "not~/home", "  spaced  "} {
		if got := Expand(in); got != strings.TrimSpace(in) {
			t.Fatalf("Expand(%q) = %q, want it unchanged", in, got)
		}
	}
}

// TestSetMovesTheWorkingDirectory pins that Set enters the directory and reports
// the absolute path it landed on, and that a failure leaves the directory alone.
func TestSetMovesTheWorkingDirectory(t *testing.T) {
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(old) }()

	dir := t.TempDir()
	got, err := Set(dir)
	if err != nil {
		t.Fatalf("Set(%q): %v", dir, err)
	}
	if want, _ := filepath.EvalSymlinks(dir); got != want && got != dir {
		t.Fatalf("Set(%q) = %q, want %q", dir, got, dir)
	}
	now, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if now != got {
		t.Fatalf("working directory = %q, want %q", now, got)
	}
	if _, err := Set(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Fatal("Set on a missing directory should fail")
	}
	if after, _ := os.Getwd(); after != got {
		t.Fatalf("a failed Set moved the directory to %q, want %q", after, got)
	}
	if _, err := Set("  "); err == nil {
		t.Fatal("Set with a blank path should fail")
	}
}

// fakeGuard stands in for the run's directory lock in a /cd test: it records the
// target and performs the directory change through enter, the way
// lock.Holder.Move does between taking the new lock and giving up the old one.
type fakeGuard struct {
	target string
	err    error
}

func (g *fakeGuard) Move(target string, enter func(string) (string, error)) (string, error) {
	g.target = target
	if g.err != nil {
		return "", g.err
	}
	return enter(target)
}

// TestChangeGoesThroughTheGuard pins the /cd contract: with a guard registered
// the directory change is the guard's job (it moves the lock), the target is
// expanded before it sees it, and a refusal leaves the process where it was. A
// nil guard keeps the plain directory change (the --session case).
func TestChangeGoesThroughTheGuard(t *testing.T) {
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(old) }()

	dir := t.TempDir()
	guard := &fakeGuard{}
	got, err := Change(dir, guard)
	if err != nil {
		t.Fatalf("Change(%q): %v", dir, err)
	}
	if guard.target != dir {
		t.Fatalf("the guard was asked for %q, want %q", guard.target, dir)
	}
	now, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if now != got {
		t.Fatalf("working directory = %q, want %q", now, got)
	}

	// A guard that refuses the move (a directory another instance holds) keeps
	// the process where it is.
	refused := errors.New("another lightagent instance is already running there")
	if _, err := Change(t.TempDir(), &fakeGuard{err: refused}); !errors.Is(err, refused) {
		t.Fatalf("Change = %v, want the guard's refusal", err)
	}
	if after, _ := os.Getwd(); after != now {
		t.Fatalf("a refused /cd moved the directory to %q, want %q", after, now)
	}

	// Without a guard it is the plain directory change.
	plain := t.TempDir()
	if _, err := Change(plain, nil); err != nil {
		t.Fatalf("Change(%q) without a guard: %v", plain, err)
	}
	if after, _ := os.Getwd(); after != plain {
		t.Fatalf("working directory = %q, want %q", after, plain)
	}
	if _, err := Change("  ", nil); err == nil {
		t.Fatal("Change with a blank path should fail")
	}
}

// TestListingMarksDirectoriesAndSkipsHidden pins the /ls text: the absolute
// directory first, entries sorted with a trailing slash on directories, hidden
// names skipped, and an empty directory saying so.
func TestListingMarksDirectoriesAndSkipsHidden(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha.txt", "beta.txt", ".hidden"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := Listing(dir)
	if err != nil {
		t.Fatalf("Listing(%q): %v", dir, err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 4 {
		t.Fatalf("Listing = %q, want a header and three entries", out)
	}
	if !strings.HasPrefix(lines[0], dir) || !strings.HasSuffix(lines[0], ":") {
		t.Fatalf("the first line is %q, want the directory header", lines[0])
	}
	want := []string{"alpha.txt", "beta.txt", "sub/"}
	for i, entry := range want {
		if !strings.HasSuffix(lines[i+1], entry) {
			t.Fatalf("entry %d = %q, want %q", i, lines[i+1], entry)
		}
	}
	if strings.Contains(out, "hidden") {
		t.Fatalf("a hidden entry showed up: %q", out)
	}

	empty := t.TempDir()
	if got, err := Listing(empty); err != nil || !strings.Contains(got, "(empty)") {
		t.Fatalf("Listing(empty) = (%q, %v), want the empty note", got, err)
	}

	// A missing directory is an error rather than a bogus listing.
	if _, err := Listing(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("Listing on a missing directory should fail")
	}
}

// TestListingDefaultsToTheWorkingDirectory pins that an empty argument lists the
// process working directory.
func TestListingDefaultsToTheWorkingDirectory(t *testing.T) {
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Set(dir); err != nil {
		t.Fatal(err)
	}
	out, err := Listing("")
	if err != nil {
		t.Fatalf("Listing(\"\"): %v", err)
	}
	if !strings.Contains(out, "marker.txt") {
		t.Fatalf("Listing(\"\") = %q, want the working directory's contents", out)
	}
}
