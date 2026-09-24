package tools

import (
	"strings"
	"testing"
)

// TestRenderStoredResult covers recovering the user-facing rendering of a
// persisted tool result: command results are stored as their JSON contract and
// are reformatted, while other tool results report ok=false.
func TestRenderStoredResult(t *testing.T) {
	completed := commandResult{
		Status:         statusCompleted,
		ExitCode:       intPtr(0),
		Output:         "hello",
		TotalLines:     1,
		TotalBytes:     5,
		ElapsedSeconds: 0.2,
	}.marshal()

	text, isErr, ok := RenderStoredResult(completed)
	if !ok || isErr {
		t.Fatalf("completed: ok=%v isErr=%v", ok, isErr)
	}
	if !strings.Contains(text, "Command completed.") || !strings.Contains(text, "hello") {
		t.Fatalf("completed text = %q", text)
	}

	failed := commandResult{Status: statusFailed, Output: "boom", ElapsedSeconds: 0.1}.marshal()
	text, isErr, ok = RenderStoredResult(failed)
	if !ok || !isErr {
		t.Fatalf("failed: ok=%v isErr=%v", ok, isErr)
	}
	if !strings.Contains(text, "Command failed.") || !strings.Contains(text, "boom") {
		t.Fatalf("failed text = %q", text)
	}

	interrupted := commandResult{Status: statusInterrupted, Output: "half a line", ElapsedSeconds: 0.1}.marshal()
	text, isErr, ok = RenderStoredResult(interrupted)
	if !ok || !isErr {
		t.Fatalf("interrupted: ok=%v isErr=%v", ok, isErr)
	}
	if !strings.Contains(text, "Command interrupted by user.") || !strings.Contains(text, "half a line") {
		t.Fatalf("interrupted text = %q", text)
	}

	// A poll that was interrupted keeps its "running" status — the process is
	// still there — so the warning is what tells the reader the wait ended early.
	polled := commandResult{
		Status:    statusRunning,
		SessionID: strPtr("abc"),
		Output:    "tick 1",
		Warning:   strPtr("poll interrupted by user"),
	}.marshal()
	text, isErr, ok = RenderStoredResult(polled)
	if !ok || isErr {
		t.Fatalf("poll: ok=%v isErr=%v", ok, isErr)
	}
	if !strings.Contains(text, "Command is running in session abc") || !strings.Contains(text, "poll interrupted by user") {
		t.Fatalf("poll text = %q", text)
	}

	// A plain-text result (e.g. an MCP tool) and unrelated JSON have no
	// user-facing rendering, so they are not restored.
	for _, content := range []string{"plain output", `{"status":"completed"}`, "not json", ""} {
		if text, isErr, ok := RenderStoredResult(content); ok || text != "" || isErr {
			t.Fatalf("RenderStoredResult(%q) = (%q, %v, %v), want (\"\", false, false)", content, text, isErr, ok)
		}
	}
}
