package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"lightagent/internal/llm"
)

// TestNewLayoutPlacesSessionsUnderSessions pins the layout: the state directory
// keeps the lock and uploads, and the session files live under sessions/.
func TestNewLayoutPlacesSessionsUnderSessions(t *testing.T) {
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wantRoot := filepath.Join(dir, DirName)
	wantSessions := filepath.Join(wantRoot, SessionsDirName)
	if st.Root() != wantRoot {
		t.Fatalf("Root = %q, want %q", st.Root(), wantRoot)
	}
	if st.Dir() != wantSessions {
		t.Fatalf("Dir = %q, want %q", st.Dir(), wantSessions)
	}
	if want := filepath.Join(wantSessions, "session.json"); st.Path() != want {
		t.Fatalf("Path = %q, want %q", st.Path(), want)
	}
	if err := st.Save(State{Messages: []llm.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(st.Path()); err != nil {
		t.Fatalf("session not written at %q: %v", st.Path(), err)
	}
}

// TestNewFileKeepsTheGivenDirectory checks --session: the file's own directory
// is used and no sessions/ subdirectory is introduced.
func TestNewFileKeepsTheGivenDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.json")
	st, err := NewFile(path)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if st.Dir() != dir || st.Root() != dir || st.Path() != path {
		t.Fatalf("NewFile layout = root %q dir %q path %q", st.Root(), st.Dir(), st.Path())
	}
}

// TestSaveAsAndReset covers the mutable current file: SaveAs points at the new
// file (a later Save updates it), and Reset points back at the default.
func TestSaveAsAndReset(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	saved, err := st.SaveAs("notes", State{Messages: []llm.Message{{Role: "user", Content: "one"}}})
	if err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	want := filepath.Join(st.Dir(), "notes.json")
	if saved != want {
		t.Fatalf("SaveAs path = %q, want %q", saved, want)
	}
	if st.Path() != want {
		t.Fatalf("current file = %q, want %q", st.Path(), want)
	}
	// A later Save updates the file SaveAs made current, not session.json.
	if err := st.Save(State{Messages: []llm.Message{{Role: "user", Content: "two"}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	state, err := st.Load()
	if err != nil || state == nil {
		t.Fatalf("Load after SaveAs = (%v, %v)", state, err)
	}
	if len(state.Messages) != 1 || state.Messages[0].Content != "two" {
		t.Fatalf("Save did not update the current file: %+v", state.Messages)
	}
	// Reset points back at session.json.
	st.Reset()
	if got := st.Path(); got != filepath.Join(st.Dir(), "session.json") {
		t.Fatalf("after Reset, current file = %q", got)
	}
}

// TestResolvePath covers the name rules: a bare name gains a .json suffix and
// lives in the sessions directory; a name carrying a separator is used as given.
func TestResolvePath(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := st.ResolvePath("notes"); got != filepath.Join(st.Dir(), "notes.json") {
		t.Fatalf("ResolvePath(notes) = %q", got)
	}
	if got := st.ResolvePath("notes.JSON"); got != filepath.Join(st.Dir(), "notes.JSON") {
		t.Fatalf("ResolvePath(notes.JSON) = %q", got)
	}
	rel := filepath.Join("sub", "one.json")
	if got := st.ResolvePath(rel); got != rel {
		t.Fatalf("ResolvePath(%q) = %q, want it kept", rel, got)
	}
	abs := filepath.Join(t.TempDir(), "x.json")
	if got := st.ResolvePath(abs); got != abs {
		t.Fatalf("ResolvePath(%q) = %q, want it kept", abs, got)
	}
}

// TestLatestAndNamed pin the pieces startup and the exit save ride on: Latest is
// the newest file whatever its name, and Named tells the default session.json
// from a real name.
func TestLatestAndNamed(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if latest, err := st.Latest(); err != nil || latest != nil {
		t.Fatalf("Latest on an empty store = (%v, %v), want nil", latest, err)
	}
	if st.Named() {
		t.Fatal("a fresh store is nameless (the default session.json)")
	}

	base := time.Date(2020, 10, 5, 12, 0, 0, 0, time.UTC)
	oldPath, err := st.SaveAs("old", State{Messages: []llm.Message{{Role: "user", Content: "a"}}})
	if err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	if err := os.Chtimes(oldPath, base, base); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	// SaveAs points the store at the named file.
	if !st.Named() {
		t.Fatal("after SaveAs the session is named")
	}

	newPath, err := st.SaveAs("new", State{Messages: []llm.Message{{Role: "user", Content: "b"}}})
	if err != nil {
		t.Fatalf("SaveAs: %v", err)
	}
	stamp := base.Add(time.Hour)
	if err := os.Chtimes(newPath, stamp, stamp); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	latest, err := st.Latest()
	if err != nil || latest == nil {
		t.Fatalf("Latest = (%v, %v)", latest, err)
	}
	if latest.Name != "new.json" {
		t.Fatalf("Latest.Name = %q, want new.json", latest.Name)
	}

	// Reset points back at the default, which is nameless again.
	st.Reset()
	if st.Named() {
		t.Fatal("after Reset the session is the default (nameless) again")
	}
}

// newest n sessions, newest first, and all of them when n <= 0.
func TestRecentIsNewestFirstAndLimited(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for i, name := range []string{"one", "two", "three"} {
		path, err := st.SaveAs(name, State{Messages: []llm.Message{{Role: "user", Content: name}}})
		if err != nil {
			t.Fatalf("SaveAs(%s): %v", name, err)
		}
		stamp := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}
	infos, err := st.Recent(2)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(infos) != 2 || infos[0].Name != "three.json" || infos[1].Name != "two.json" {
		t.Fatalf("Recent(2) = %+v, want three.json then two.json", infos)
	}
	all, err := st.Recent(0)
	if err != nil || len(all) != 3 {
		t.Fatalf("Recent(0) = (%d, %v), want all three", len(all), err)
	}
}

// TestMigrateLegacyMovesTheOldLayout covers the upgrade path: session files an
// earlier layout left directly in .lightagent/ move into sessions/.
func TestMigrateLegacyMovesTheOldLayout(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, DirName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacy := filepath.Join(root, "session.json")
	if err := os.WriteFile(legacy, []byte(`{"version":1,"messages":[{"role":"user","content":"old"}]}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	archive := filepath.Join(root, "session-20260101-000000.json")
	if err := os.WriteFile(archive, []byte(`{"version":1,"messages":[]}`), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}

	st, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := st.MigrateLegacy(); err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if _, err := os.Stat(st.Path()); err != nil {
		t.Fatalf("legacy session not moved to %q: %v", st.Path(), err)
	}
	if _, err := os.Stat(filepath.Join(st.Dir(), filepath.Base(archive))); err != nil {
		t.Fatalf("legacy archive not moved: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy file still present, stat err = %v", err)
	}
	// A second run is a no-op (the sessions file already exists).
	if err := st.MigrateLegacy(); err != nil {
		t.Fatalf("second MigrateLegacy: %v", err)
	}
}

