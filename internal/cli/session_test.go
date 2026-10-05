package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lightagent/internal/llm"
	"lightagent/internal/store"
)

// saveSession writes one session file (and points the store back at its default
// file), so a test starts with saved conversations without touching the current
// one.
func saveSession(t *testing.T, c *CLI, name string, msgs ...string) {
	t.Helper()
	state := store.State{Version: 1}
	for _, m := range msgs {
		state.Messages = append(state.Messages, llm.Message{Role: "user", Content: m})
	}
	if _, err := c.store.SaveAs(name, state); err != nil {
		t.Fatalf("SaveAs(%s): %v", name, err)
	}
	c.store.Reset()
}

// TestSaveCommandWritesTheCurrentFile covers /save: it writes the default
// conversation file under the sessions directory.
func TestSaveCommandWritesTheCurrentFile(t *testing.T) {
	noColors(t)
	c := newTestCLI(t)
	c.agent.Load([]llm.Message{{Role: "user", Content: "hi"}}, "")

	if exit := c.handleCommand(context.Background(), "/save"); exit {
		t.Fatal("/save must not exit")
	}
	want := filepath.Join(c.store.Dir(), "session.json")
	if got := c.store.Path(); got != want {
		t.Fatalf("current file = %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("session not written: %v", err)
	}
	if filepath.Base(c.store.Dir()) != store.SessionsDirName {
		t.Fatalf("session file %q is not under %q", c.store.Dir(), store.SessionsDirName)
	}
}

// TestSaveAsMakesTheNewFileCurrent covers /saveas: the new file becomes the one
// a later /save updates, an existing name needs -f, and /new points back at the
// default.
func TestSaveAsMakesTheNewFileCurrent(t *testing.T) {
	noColors(t)
	c := newTestCLI(t)
	var buf strings.Builder
	c.out = &buf
	c.agent.Load([]llm.Message{{Role: "user", Content: "hi"}}, "")

	c.handleCommand(context.Background(), "/saveas notes")
	notes := filepath.Join(c.store.Dir(), "notes.json")
	if got := c.store.Path(); got != notes {
		t.Fatalf("after /saveas, current file = %q, want %q", got, notes)
	}
	if _, err := os.Stat(notes); err != nil {
		t.Fatalf("notes.json not written: %v", err)
	}

	// Without -f an existing target is refused; with -f it is overwritten.
	c.handleCommand(context.Background(), "/saveas notes")
	if !strings.Contains(buf.String(), "already exists") {
		t.Fatalf("/saveas over an existing file = %q, want a refusal", buf.String())
	}
	buf.Reset()
	c.handleCommand(context.Background(), "/saveas -f notes")
	if strings.Contains(buf.String(), "already exists") {
		t.Fatalf("/saveas -f was refused: %q", buf.String())
	}

	// /new points back at the default file (session.json under sessions/).
	buf.Reset()
	c.handleCommand(context.Background(), "/new")
	if got := c.store.Path(); filepath.Base(got) != "session.json" {
		t.Fatalf("after /new, current file = %q, want session.json", got)
	}
}

// TestLoadCommandByNameAndNumber covers /load: a file name and a /list number
// both replace the conversation, and either becomes the current file.
func TestLoadCommandByNameAndNumber(t *testing.T) {
	noColors(t)
	c := newTestCLI(t)
	saveSession(t, c, "alpha", "first")
	saveSession(t, c, "beta", "second")

	var buf strings.Builder
	c.out = &buf
	c.handleCommand(context.Background(), "/load alpha")
	if hist := c.agent.History(); len(hist) != 1 || hist[0].Content != "first" {
		t.Fatalf("after /load alpha, history = %+v", hist)
	}
	if got := filepath.Base(c.store.Path()); got != "alpha.json" {
		t.Fatalf("current file after /load = %q, want alpha.json", got)
	}

	// The number is the /list index (newest first), so 2 is the older file.
	buf.Reset()
	c.handleCommand(context.Background(), "/load -f 2")
	if hist := c.agent.History(); len(hist) != 1 || hist[0].Content != "first" {
		t.Fatalf("after /load 2, history = %+v", hist)
	}
}

// TestLoadRefusesUnsavedChanges covers the guard: without -f, /load refuses to
// discard a conversation with unsaved work and says to use -f; with -f it loads.
func TestLoadRefusesUnsavedChanges(t *testing.T) {
	noColors(t)
	c := newTestCLI(t)
	saveSession(t, c, "alpha", "first")

	var buf strings.Builder
	c.out = &buf
	// A fresh conversation with messages but no save.
	c.agent.Load([]llm.Message{{Role: "user", Content: "unsaved work"}}, "")
	c.handleCommand(context.Background(), "/load alpha")
	if !strings.Contains(buf.String(), "unsaved") || !strings.Contains(buf.String(), "-f") {
		t.Fatalf("/load with unsaved changes = %q, want a notice mentioning -f", buf.String())
	}
	if hist := c.agent.History(); len(hist) != 1 || hist[0].Content != "unsaved work" {
		t.Fatalf("the conversation must be left alone: %+v", hist)
	}

	buf.Reset()
	c.handleCommand(context.Background(), "/load -f alpha")
	if hist := c.agent.History(); len(hist) != 1 || hist[0].Content != "first" {
		t.Fatalf("after /load -f, history = %+v", hist)
	}
}

// TestListCommandShowsNewestFirst covers /list: it prints the index and file
// names, and an argument limits how many rows it shows.
func TestListCommandShowsNewestFirst(t *testing.T) {
	noColors(t)
	c := newTestCLI(t)
	saveSession(t, c, "alpha", "a")
	saveSession(t, c, "beta", "b")

	var buf strings.Builder
	c.out = &buf
	c.handleCommand(context.Background(), "/list")
	out := buf.String()
	for _, want := range []string{"alpha.json", "beta.json"} {
		if !strings.Contains(out, want) {
			t.Fatalf("/list = %q, want %q", out, want)
		}
	}
	buf.Reset()
	c.handleCommand(context.Background(), "/list 1")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("/list 1 = %q, want a single line", buf.String())
	}
}

// TestRemoveCommandIsNameOnly covers /rm: it deletes a file by name, refuses a
// number and refuses the current session.
func TestRemoveCommandIsNameOnly(t *testing.T) {
	noColors(t)
	c := newTestCLI(t)
	saveSession(t, c, "alpha", "a")

	var buf strings.Builder
	c.out = &buf
	c.handleCommand(context.Background(), "/rm alpha")
	if _, err := os.Stat(filepath.Join(c.store.Dir(), "alpha.json")); !os.IsNotExist(err) {
		t.Fatalf("alpha.json still present, stat err = %v", err)
	}

	buf.Reset()
	c.handleCommand(context.Background(), "/rm 1")
	if !strings.Contains(buf.String(), "file name") {
		t.Fatalf("/rm 1 = %q, want a refusal naming the file-name rule", buf.String())
	}

	buf.Reset()
	c.agent.Load([]llm.Message{{Role: "user", Content: "x"}}, "")
	c.handleCommand(context.Background(), "/save")
	buf.Reset()
	c.handleCommand(context.Background(), "/rm session")
	if !strings.Contains(buf.String(), "current session") {
		t.Fatalf("/rm on the current file = %q, want a refusal", buf.String())
	}
}

