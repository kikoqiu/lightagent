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

	// A plain-text result (e.g. an MCP tool) and unrelated JSON have no
	// user-facing rendering, so they are not restored.
	for _, content := range []string{"plain output", `{"status":"completed"}`, "not json", ""} {
		if text, isErr, ok := RenderStoredResult(content); ok || text != "" || isErr {
			t.Fatalf("RenderStoredResult(%q) = (%q, %v, %v), want (\"\", false, false)", content, text, isErr, ok)
		}
	}
}
