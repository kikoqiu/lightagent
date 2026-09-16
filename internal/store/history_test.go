package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lightagent/internal/llm"
)

// TestExists reports whether a session file is present.
func TestExists(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if st.Exists() {
		t.Fatal("Exists = true before any save")
	}
	if err := st.Save(State{Messages: []llm.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if !st.Exists() {
		t.Fatal("Exists = false after a save")
	}
}

// TestNewFileAndArchiveNaming covers explicit session paths.
func TestNewFileAndArchiveNaming(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "custom.json")
	st, err := NewFile(path)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if filepath.Base(st.Path()) != "custom.json" {
		t.Fatalf("Path = %q, want a custom.json name", st.Path())
	}
	if err := st.Save(State{Messages: []llm.Message{{Role: "user", Content: "x"}}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(st.Path()); err != nil {
		t.Fatalf("session not written: %v", err)
	}
	backup, err := st.Archive(time.Date(2026, 9, 12, 15, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if filepath.Base(backup) != "custom-20260912-150405.json" {
		t.Fatalf("archive = %q", backup)
	}
}

// TestNoDirectoryUntilSave verifies that building a store does not touch the
// disk: a run that never saves (--no-save, a one-shot prompt, or a declined
// exit prompt) must not leave a state directory behind.
func TestNoDirectoryUntilSave(t *testing.T) {
	workdir := t.TempDir()
	st, err := New(workdir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := os.Stat(st.Dir()); !os.IsNotExist(err) {
		t.Fatalf("state dir %s exists before a save, stat err = %v", st.Dir(), err)
	}
	if infos, err := st.List(); err != nil || len(infos) != 0 {
		t.Fatalf("List on a missing dir = (%d entries, %v), want (0, nil)", len(infos), err)
	}
	if path, err := st.Archive(time.Date(2026, 9, 12, 15, 4, 5, 0, time.UTC)); err != nil || path != "" {
		t.Fatalf("Archive on a missing dir = (%q, %v), want (\"\", nil)", path, err)
	}
	if err := st.Save(State{Messages: []llm.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(st.Dir()); err != nil {
		t.Fatalf("state dir missing after a save: %v", err)
	}
}

// TestNoDirectoryForExplicitSessionUntilSave covers the --session path: the
// parent directory of an explicit session file is created only when saved.
func TestNoDirectoryForExplicitSessionUntilSave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	st, err := NewFile(filepath.Join(dir, "custom.json"))
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("session dir %s exists before a save, stat err = %v", dir, err)
	}
	if err := st.Save(State{Messages: []llm.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("session dir missing after a save: %v", err)
	}
}

// TestListAndPrune covers listing sessions and pruning old archives.
func TestListAndPrune(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	save := func(content string) {
		t.Helper()
		if err := st.Save(State{Messages: []llm.Message{{Role: "user", Content: content}}}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	base := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		save(fmt.Sprintf("old-%d", i))
		if _, err := st.Archive(base.Add(time.Duration(i) * time.Minute)); err != nil {
			t.Fatalf("archive %d: %v", i, err)
		}
	}
	save("current")

	infos, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 4 {
		t.Fatalf("List = %d entries, want 4", len(infos))
	}
	var current int
	for _, info := range infos {
		if info.Current {
			current++
		}
	}
	if current != 1 {
		t.Fatalf("List flagged %d current sessions, want 1", current)
	}

	removed, err := st.PruneArchives(1)
	if err != nil {
		t.Fatalf("PruneArchives: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("pruned %d archives, want 2", len(removed))
	}
	if !st.Exists() {
		t.Fatal("the current session must survive pruning")
	}
	infos, _ = st.List()
	if len(infos) != 2 {
		t.Fatalf("after prune = %d entries, want 2", len(infos))
	}

	removed, err = st.PruneArchives(0)
	if err != nil {
		t.Fatalf("PruneArchives(0): %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("prune all removed %d, want 1", len(removed))
	}
	if !st.Exists() {
		t.Fatal("prune all must not delete the current session")
	}
}

// TestArchive covers renaming a previous session with a timestamp so a fresh
// start never loses the old conversation.
func TestArchive(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stamp := time.Date(2026, 9, 12, 15, 4, 5, 0, time.UTC)

	// Nothing to archive when no session exists yet.
	if path, err := st.Archive(stamp); err != nil || path != "" {
		t.Fatalf("Archive(empty) = (%q, %v), want (\"\", nil)", path, err)
	}

	if err := st.Save(State{Messages: []llm.Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	backup, err := st.Archive(stamp)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	want := filepath.Join(st.Dir(), "session-20260912-150405.json")
	if backup != want {
		t.Fatalf("Archive path = %q, want %q", backup, want)
	}
	if _, err := os.Stat(st.Path()); !os.IsNotExist(err) {
		t.Fatalf("session.json should be gone after archive, stat err = %v", err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("backup missing: %v", err)
	}

	// Archiving twice within the same second must not clobber the first backup.
	if err := st.Save(State{Messages: []llm.Message{{Role: "user", Content: "again"}}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	second, err := st.Archive(stamp)
	if err != nil {
		t.Fatalf("second Archive: %v", err)
	}
	wantSecond := filepath.Join(st.Dir(), "session-20260912-150405-1.json")
	if second != wantSecond {
		t.Fatalf("second Archive path = %q, want %q", second, wantSecond)
	}
}

// save, reload, and overwrite.
func TestStoreSaveLoadRoundTrip(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := st.Load()
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if got != nil {
		t.Fatalf("initial load = %+v, want nil", got)
	}

	want := State{
		Model:    "test-model",
		Summary:  "sum",
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}
	if err := st.Save(want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err = st.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got == nil {
		t.Fatal("load returned nil after save")
	}
	if got.Version == 0 {
		t.Fatal("version not defaulted")
	}
	if got.Model != "test-model" || got.Summary != "sum" {
		t.Fatalf("meta = %+v", got)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "hi" {
		t.Fatalf("messages = %+v", got.Messages)
	}

	// A second save must replace the previous snapshot.
	want.Summary = "sum2"
	want.Messages = append(want.Messages, llm.Message{Role: "assistant", Content: "yo"})
	if err := st.Save(want); err != nil {
		t.Fatalf("resave: %v", err)
	}
	got, err = st.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(got.Messages) != 2 || got.Summary != "sum2" {
		t.Fatalf("after overwrite = %+v", got)
	}
}
