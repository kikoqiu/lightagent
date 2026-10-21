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

// TestMemoInfoListsFilesNewestFirst verifies the block the first user message
// carries: the header, then every memo file newest change first, with a friendly
// age and a readable size — subfolders included, named relative to the memo
// folder.
func TestMemoInfoListsFilesNewestFirst(t *testing.T) {
	dir := t.TempDir()
	writeMemoFile(t, filepath.Join(dir, ".lightagent", "memo", "old.md"), "# old\n", 3*24*time.Hour)
	writeMemoFile(t, filepath.Join(dir, ".lightagent", "memo", "new.md"), "# new\n", time.Minute)
	writeMemoFile(t, filepath.Join(dir, ".lightagent", "memo", "notes", "deep.md"), "# deep\n", 5*time.Hour)

	got := memoInfo(dir, time.Now())
	lines := strings.Split(got, "\n")
	if len(lines) != 6 {
		t.Fatalf("listing = %q, want the header lines plus 3 files", got)
	}
	if lines[0] != "[engine meta info]" || lines[1] != "The memo folder holds these files:" {
		t.Fatalf("listing header = %q", lines[:2])
	}
	for i, want := range []string{"new.md", "notes/deep.md", "old.md"} {
		if !strings.HasPrefix(lines[i+3], want+" ") {
			t.Fatalf("line %d = %q, want the file %q", i+3, lines[i+3], want)
		}
	}
	for _, want := range []string{"1 minute ago", "5 hours ago", "3 days ago"} {
		if !strings.Contains(got, want) {
			t.Fatalf("listing %q is missing %q", got, want)
		}
	}
}

// TestMemoInfoFormat verifies the exact shape of the block: a header row and one
// aligned row per file.
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
		"The memo folder holds these files:\n" +
		"file  updated    size\n" +
		"a.md  1 day ago  12 B"
	if got := memoInfo(dir, now); got != want {
		t.Fatalf("memoInfo = %q, want %q", got, want)
	}
}

// TestMemoInfoWithoutFiles verifies a conversation adds nothing when the memo
// folder is missing or empty.
func TestMemoInfoWithoutFiles(t *testing.T) {
	dir := t.TempDir()
	if got := memoInfo(dir, time.Now()); got != "" {
		t.Fatalf("memoInfo without a memo folder = %q, want empty", got)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".lightagent", "memo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := memoInfo(dir, time.Now()); got != "" {
		t.Fatalf("memoInfo of an empty memo folder = %q, want empty", got)
	}
	if got := memoInfo("", time.Now()); got != "" {
		t.Fatalf("memoInfo without a working directory = %q, want empty", got)
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
// stream.
func newProviderAgent(t *testing.T, srv *httptest.Server) (*Agent, <-chan Event) {
	t.Helper()
	cfg := config.Default()
	cfg.LLMs[0].APIBase = srv.URL
	cfg.LLMs[0].Stream = false
	bus := NewBus()
	events, cancel := bus.Subscribe()
	t.Cleanup(cancel)
	return New(cfg, llm.NewClient(cfg.LLMs[0].OpenAIConfig), tools.NewRegistry(), bus), events
}

// TestFirstUserMessageCarriesTheMemoListing covers the send path: the memo
// folder is scanned when the first user message goes out, the listing is stored
// as that message's engine meta (what a session file keeps) and appended to the
// request only, and later messages carry none of it.
func TestFirstUserMessageCarriesTheMemoListing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
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
	if !strings.Contains(history[0].Meta, "[engine meta info]") || !strings.Contains(history[0].Meta, "plan.md") {
		t.Fatalf("the first user message must carry the memo listing as its meta: %q", history[0].Meta)
	}
	if history[2].Meta != "" {
		t.Fatalf("a later message must not carry the listing: %q", history[2].Meta)
	}

	bodies := requests()
	if len(bodies) != 2 {
		t.Fatalf("the model was asked %d time(s), want 2", len(bodies))
	}
	first := bodies[0].Messages[len(bodies[0].Messages)-1]
	wantHead := "first question\n\n[engine meta info]\nThe memo folder holds these files:\n"
	if !strings.HasPrefix(first.Content, wantHead) || !strings.Contains(first.Content, "plan.md") {
		t.Fatalf("the first request must carry the listing after the text: %q", first.Content)
	}
	second := bodies[1].Messages[len(bodies[1].Messages)-1]
	if second.Content != "second question" {
		t.Fatalf("the second message must go out as it was typed: %q", second.Content)
	}
}

// TestFirstUserMessageWithoutMemoNotes verifies a conversation without memo
// files sends its first user message unchanged.
func TestFirstUserMessageWithoutMemoNotes(t *testing.T) {
	t.Chdir(t.TempDir())

	srv, requests := recordingProvider(t)
	a, events := newProviderAgent(t, srv)

	a.Submit("hello")
	waitForTurnEnd(t, events)

	if msgs := a.History(); msgs[0].Meta != "" {
		t.Fatalf("no memo folder must add no meta: %q", msgs[0].Meta)
	}
	bodies := requests()
	if got := bodies[0].Messages[len(bodies[0].Messages)-1].Content; got != "hello" {
		t.Fatalf("request content = %q, want the message as typed", got)
	}
}
