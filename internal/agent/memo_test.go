package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lightagent/internal/config"
	"lightagent/internal/llm"
	"lightagent/internal/tools"
)

// writeMemoFile creates one file and gives it the given age (how long before now
// it last changed).
func writeMemoFile(t *testing.T, path, content string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// TestMemoInfoListsFilesNewestFirst verifies the block a user message carries:
// the working directory, then every memo file newest change first, with a
// friendly age and a readable size — subfolders included, named relative to the
// memo folder.
func TestMemoInfoListsFilesNewestFirst(t *testing.T) {
	dir := t.TempDir()
	writeMemoFile(t, filepath.Join(dir, ".lightagent", "memo", "old.md"), "# old\n", 3*24*time.Hour)
	writeMemoFile(t, filepath.Join(dir, ".lightagent", "memo", "new.md"), "# new\n", time.Minute)
	writeMemoFile(t, filepath.Join(dir, ".lightagent", "memo", "notes", "deep.md"), "# deep\n", 5*time.Hour)

	got := memoInfo(dir, true, time.Now())
	lines := strings.Split(got, "\n")
	if len(lines) != 7 {
		t.Fatalf("listing = %q, want the header lines, the directory and 3 files", got)
	}
	if lines[0] != "[engine meta info]" || lines[1] != "working directory: "+dir ||
		lines[2] != "The memo folder holds these files:" {
		t.Fatalf("listing header = %q", lines[:3])
	}
	for i, want := range []string{"new.md", "notes/deep.md", "old.md"} {
		if !strings.HasPrefix(lines[i+4], want+" ") {
			t.Fatalf("line %d = %q, want the file %q", i+4, lines[i+4], want)
		}
	}
	for _, want := range []string{"1 minute ago", "5 hours ago", "3 days ago"} {
		if !strings.Contains(got, want) {
			t.Fatalf("listing %q is missing %q", got, want)
		}
	}
}

// TestMemoInfoFormat verifies the exact shape of the block: the directory line,
// a header row and one aligned row per file.
func TestMemoInfoFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".lightagent", "memo", "a.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("0123456789ab"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	at := now.Add(-24 * time.Hour)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}

	want := "[engine meta info]\n" +
		"working directory: " + dir + "\n" +
		"The memo folder holds these files:\n" +
		"file  updated    size\n" +
		"a.md  1 day ago  12 B"
	if got := memoInfo(dir, true, now); got != want {
		t.Fatalf("memoInfo = %q, want %q", got, want)
	}
}

// TestMemoInfoWithoutFiles verifies the memo listing is optional while the
// directory is not: with no memo folder (or an empty one) the block states the
// directory alone, and it drops the directory line while
// agent.include_working_dir is off.
func TestMemoInfoWithoutFiles(t *testing.T) {
	dir := t.TempDir()
	wantDirOnly := "[engine meta info]\nworking directory: " + dir
	if got := memoInfo(dir, true, time.Now()); got != wantDirOnly {
		t.Fatalf("memoInfo without a memo folder = %q, want %q", got, wantDirOnly)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".lightagent", "memo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := memoInfo(dir, true, time.Now()); got != wantDirOnly {
		t.Fatalf("memoInfo of an empty memo folder = %q, want %q", got, wantDirOnly)
	}
	if got := memoInfo(dir, false, time.Now()); got != "" {
		t.Fatalf("memoInfo of an empty memo folder with no directory line = %q, want empty", got)
	}
	if got := memoInfo("", true, time.Now()); got != "" {
		t.Fatalf("memoInfo without a working directory = %q, want empty", got)
	}

	// A memo file still goes out while the directory line is off: the listing
	// is what the model needs, the path is what the switch withholds.
	writeMemoFile(t, filepath.Join(dir, ".lightagent", "memo", "a.md"), "# a\n", time.Minute)
	got := memoInfo(dir, false, time.Now())
	if strings.Contains(got, "working directory") {
		t.Fatalf("the directory line must stay off: %q", got)
	}
	if wantHead := "[engine meta info]\nThe memo folder holds these files:\n"; !strings.HasPrefix(got, wantHead) {
		t.Fatalf("memoInfo without the directory line = %q, want the listing below %q", got, wantHead)
	}
	if !strings.Contains(got, "a.md") {
		t.Fatalf("memoInfo without the directory line must still list the files: %q", got)
	}
}

// TestMemoAge covers the friendly ages, including a timestamp in the future.
func TestMemoAge(t *testing.T) {
	cases := []struct {
		age  time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{-2 * time.Hour, "just now"},
		{5 * time.Minute, "5 minutes ago"},
		{3 * time.Hour, "3 hours ago"},
		{26 * time.Hour, "1 day ago"},
		{40 * 24 * time.Hour, "1 month ago"},
		{400 * 24 * time.Hour, "1 year ago"},
	}
	for _, tc := range cases {
		if got := memoAge(tc.age); got != tc.want {
			t.Errorf("memoAge(%v) = %q, want %q", tc.age, got, tc.want)
		}
	}
}

// TestMemoSize covers the readable sizes.
func TestMemoSize(t *testing.T) {
	cases := []struct {
		size int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1536, "1.5 KB"},
		{5 * 1024 * 1024, "5.0 MB"},
	}
	for _, tc := range cases {
		if got := memoSize(tc.size); got != tc.want {
			t.Errorf("memoSize(%d) = %q, want %q", tc.size, got, tc.want)
		}
	}
}

// TestWithEngineMetaKeepsTheHistoryClean verifies the engine meta is rendered
// into the request only: the message it belongs to is copied, never changed, so
// the history (and what a front-end draws from it) keeps the user's text alone.
func TestWithEngineMetaKeepsTheHistoryClean(t *testing.T) {
	history := []llm.Message{
		{Role: "user", Content: "first", Meta: "[engine meta info]\nlisting"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "second"},
	}
	got := withEngineMeta(history)
	if want := "first\n\n[engine meta info]\nlisting"; got[0].Content != want {
		t.Fatalf("request content = %q, want %q", got[0].Content, want)
	}
	if got[1].Content != "answer" || got[2].Content != "second" {
		t.Fatalf("messages without meta must stay as they are: %+v", got)
	}
	if history[0].Content != "first" {
		t.Fatalf("the input history was modified: %+v", history)
	}
}


// recordingProvider starts a stub provider that records every request body and
// answers each call with a short final reply.
func recordingProvider(t *testing.T) (*httptest.Server, func() []llm.ChatRequest) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies []llm.ChatRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, req)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []llm.ChatRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]llm.ChatRequest(nil), bodies...)
	}
}

// newProviderAgent builds an agent that talks to srv and returns its event
// stream. An optional configure function adjusts the config first, so a test can
// flip one switch without rebuilding the wiring.
func newProviderAgent(t *testing.T, srv *httptest.Server, configure ...func(*config.Config)) (*Agent, <-chan Event) {
	t.Helper()
	cfg := config.Default()
	cfg.LLMs[0].APIBase = srv.URL
	cfg.LLMs[0].Stream = false
	for _, fn := range configure {
		fn(cfg)
	}
	bus := NewBus()
	events, cancel := bus.Subscribe()
	t.Cleanup(cancel)
	return New(cfg, llm.NewClient(cfg.LLMs[0].OpenAIConfig), tools.NewRegistry(), bus), events
}

// TestFirstUserMessageCarriesTheEngineMeta covers the send path: the first user
// message states the working directory and the memo folder's files, the block is
// stored as that message's engine meta (what a session file keeps) and appended
// to the request only, and later messages carry none of it.
func TestFirstUserMessageCarriesTheEngineMeta(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	writeMemoFile(t, filepath.Join(dir, ".lightagent", "memo", "plan.md"), "# plan\n", 48*time.Hour)

	srv, requests := recordingProvider(t)
	a, events := newProviderAgent(t, srv)

	a.Submit("first question")
	waitForTurnEnd(t, events)
	a.Submit("second question")
	waitForTurnEnd(t, events)

	history := a.History()
	if len(history) != 4 {
		t.Fatalf("history = %d messages, want user/assistant twice: %+v", len(history), history)
	}
	if history[0].Content != "first question" {
		t.Fatalf("the user's text must stay untouched: %q", history[0].Content)
	}
	for _, want := range []string{"[engine meta info]", "working directory: " + cwd, "plan.md"} {
		if !strings.Contains(history[0].Meta, want) {
			t.Fatalf("the first user message must carry %q in its meta: %q", want, history[0].Meta)
		}
	}
	if history[2].Meta != "" {
		t.Fatalf("a later message must not carry the block: %q", history[2].Meta)
	}

	bodies := requests()
	if len(bodies) != 2 {
		t.Fatalf("the model was asked %d time(s), want 2", len(bodies))
	}
	first := bodies[0].Messages[len(bodies[0].Messages)-1]
	wantHead := "first question\n\n[engine meta info]\nworking directory: " + cwd +
		"\nThe memo folder holds these files:\n"
	if !strings.HasPrefix(first.Content, wantHead) || !strings.Contains(first.Content, "plan.md") {
		t.Fatalf("the first request must carry the block after the text: %q", first.Content)
	}
	second := bodies[1].Messages[len(bodies[1].Messages)-1]
	if second.Content != "second question" {
		t.Fatalf("the second message must go out as it was typed: %q", second.Content)
	}
}

// TestFirstUserMessageWithoutMemoNotes verifies a conversation without memo
// files still states the working directory, and adds no memo section.
func TestFirstUserMessageWithoutMemoNotes(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	srv, requests := recordingProvider(t)
	a, events := newProviderAgent(t, srv)

	a.Submit("hello")
	waitForTurnEnd(t, events)

	want := "[engine meta info]\nworking directory: " + cwd
	if got := a.History()[0].Meta; got != want {
		t.Fatalf("the first user message must carry the directory alone: %q, want %q", got, want)
	}
	bodies := requests()
	if got, want := bodies[0].Messages[len(bodies[0].Messages)-1].Content, "hello\n\n"+want; got != want {
		t.Fatalf("request content = %q, want %q", got, want)
	}
}

// TestEngineMetaAfterCD covers what a /cd changes: the next user message states
// the new directory and lists the memo folder found below it, while the system
// prompt stays exactly as it was (so a provider's cached prefix survives the
// move).
func TestEngineMetaAfterCD(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	second := filepath.Join(base, "second")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(first)
	writeMemoFile(t, filepath.Join(first, ".lightagent", "memo", "one.md"), "# one\n", time.Minute)
	writeMemoFile(t, filepath.Join(second, ".lightagent", "memo", "two.md"), "# two\n", time.Minute)

	srv, requests := recordingProvider(t)
	a, events := newProviderAgent(t, srv)

	a.Submit("one")
	waitForTurnEnd(t, events)
	a.mu.Lock()
	before := a.buildMessagesLocked()[0].Content
	a.mu.Unlock()
	// /cd moves the directory: the next message reports the new one.
	a.SetWorkingDir(second)
	a.Submit("two")
	waitForTurnEnd(t, events)
	a.Submit("three")
	waitForTurnEnd(t, events)

	a.mu.Lock()
	after := a.buildMessagesLocked()[0].Content
	a.mu.Unlock()
	if after != before {
		t.Fatalf("a /cd must not change the system prompt:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	history := a.History()
	if got := history[0].Meta; !strings.Contains(got, "working directory: "+first) || !strings.Contains(got, "one.md") {
		t.Fatalf("the first message must state the original directory: %q", got)
	}
	if got := history[2].Meta; !strings.Contains(got, "working directory: "+second) ||
		!strings.Contains(got, "two.md") || strings.Contains(got, "one.md") {
		t.Fatalf("the first message after /cd must state the new directory and its memo files: %q", got)
	}
	if history[4].Meta != "" {
		t.Fatalf("a message after the block must not carry it again: %q", history[4].Meta)
	}

	bodies := requests()
	if got := len(bodies); got != 3 {
		t.Fatalf("the model was asked %d time(s), want 3", got)
	}
	wantHead := "two\n\n[engine meta info]\nworking directory: " + second + "\nThe memo folder holds these files:\n"
	if got := bodies[1].Messages[len(bodies[1].Messages)-1].Content; !strings.HasPrefix(got, wantHead) || !strings.Contains(got, "two.md") {
		t.Fatalf("the message after /cd must carry the new block: %q", got)
	}
	if got := bodies[2].Messages[len(bodies[2].Messages)-1].Content; got != "three" {
		t.Fatalf("the message after the block must go out as typed: %q", got)
	}
}

// TestEngineMetaOfASteeringMessageAfterCD verifies a /cd typed while a turn is
// running: the first message folded into that turn afterwards reports the new
// directory, just like the first message of a later turn.
func TestEngineMetaOfASteeringMessageAfterCD(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	a := New(config.Default(), nil, tools.NewRegistry(), NewBus())
	a.workingDir = first

	for _, text := range []string{"hello", "again"} {
		a.pushSteer("cli", text, nil)
		if a.drainSteering() {
			t.Fatal("a plain message must not pause the turn")
		}
	}
	a.SetWorkingDir(second)
	a.pushSteer("cli", "moved", nil)
	if a.drainSteering() {
		t.Fatal("a plain message must not pause the turn")
	}

	history := a.History()
	if len(history) != 3 {
		t.Fatalf("history = %+v, want three user messages", history)
	}
	if got := history[0].Meta; !strings.Contains(got, "working directory: "+first) {
		t.Fatalf("the first message must state the directory: %q", got)
	}
	if history[1].Meta != "" {
		t.Fatalf("a message in the same directory must not carry the block: %q", history[1].Meta)
	}
	if got := history[2].Meta; !strings.Contains(got, "working directory: "+second) {
		t.Fatalf("the first message after /cd must state the new directory: %q", got)
	}
}

// TestEngineMetaAfterLoadAndReset pins the conversation boundaries: a resumed
// conversation does not report its directory again (the message it resumes with
// already did), while a cleared one starts over and reports it.
func TestEngineMetaAfterLoadAndReset(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	srv, _ := recordingProvider(t)
	a, events := newProviderAgent(t, srv)

	a.Submit("first")
	waitForTurnEnd(t, events)
	if got := a.History()[0].Meta; !strings.Contains(got, "working directory: "+cwd) {
		t.Fatalf("the first message must state the directory: %q", got)
	}

	a.Load([]llm.Message{{Role: "user", Content: "resumed", Meta: "[engine meta info]\nworking directory: " + cwd}}, "")
	a.Submit("second")
	waitForTurnEnd(t, events)
	history := a.History()
	if len(history) != 3 {
		t.Fatalf("history = %+v, want resumed user/assistant plus the new message", history)
	}
	if history[2].Meta != "" {
		t.Fatalf("a resumed conversation must not report the directory again: %q", history[2].Meta)
	}

	// A cleared conversation is a fresh one: its first message reports again.
	a.Reset()
	a.Submit("third")
	waitForTurnEnd(t, events)
	if history := a.History(); len(history) != 2 || !strings.Contains(history[0].Meta, "working directory: "+cwd) {
		t.Fatalf("the first message of a cleared conversation must report the directory: %+v", history)
	}
}

// TestEngineMetaWithoutWorkingDirectoryLine verifies agent.include_working_dir
// withholds the path only: the memo listing still goes out.
func TestEngineMetaWithoutWorkingDirectoryLine(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeMemoFile(t, filepath.Join(dir, ".lightagent", "memo", "plan.md"), "# plan\n", time.Minute)

	srv, requests := recordingProvider(t)
	a, events := newProviderAgent(t, srv, func(cfg *config.Config) { cfg.Agent.IncludeWorkingDir = false })

	a.Submit("hello")
	waitForTurnEnd(t, events)

	meta := a.History()[0].Meta
	if strings.Contains(meta, "working directory") {
		t.Fatalf("the directory line must stay off: %q", meta)
	}
	if !strings.Contains(meta, "plan.md") {
		t.Fatalf("the memo listing must still go out: %q", meta)
	}
	bodies := requests()
	if got := bodies[0].Messages[len(bodies[0].Messages)-1].Content; strings.Contains(got, dir) {
		t.Fatalf("the request must not carry the path: %q", got)
	}
}
