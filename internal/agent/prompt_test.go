package agent

import (
	"runtime"
	"strings"
	"testing"
)

// TestDefaultSystemPromptHasNoHostState verifies the built-in template carries no
// host-specific data, so an agent.md exported from it stays valid on another
// machine. It still has to ask for non-interactive execution and point the model
// at the agent's scratch directories.
func TestDefaultSystemPromptHasNoHostState(t *testing.T) {
	prompt := DefaultSystemPrompt()
	if strings.Contains(prompt, "Runtime:") {
		t.Fatalf("built-in prompt must not embed the runtime line:\n%s", prompt)
	}
	if !strings.Contains(prompt, "non-interactive") {
		t.Fatalf("built-in prompt must ask for non-interactive commands:\n%s", prompt)
	}
	for _, dir := range []string{".lightagent/temp", ".lightagent/memo"} {
		if !strings.Contains(prompt, dir) {
			t.Fatalf("built-in prompt must point at %s:\n%s", dir, prompt)
		}
	}
}

// TestDefaultSystemPromptArchivesValuableTempFiles verifies the built-in
// template tells the model to move scratch files worth keeping out of the
// temporary folder, because what stays there for 180 days is cleaned up.
func TestDefaultSystemPromptArchivesValuableTempFiles(t *testing.T) {
	prompt := DefaultSystemPrompt()
	for _, want := range []string{".lightagent/archive", ".lightagent/temp", "180 days"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("built-in prompt must carry the archive rule (%q missing):\n%s", want, prompt)
		}
	}
}

// TestDefaultSystemPromptRequiresMarkdownMemos verifies the built-in template
// states how the memo folder is written: Markdown files (.md) with a heading
// structure of every level, so the notes stay readable and easy to skim.
func TestDefaultSystemPromptRequiresMarkdownMemos(t *testing.T) {
	prompt := DefaultSystemPrompt()
	if !strings.Contains(prompt, ".md") || !strings.Contains(prompt, "Markdown") {
		t.Fatalf("built-in prompt must ask for Markdown (.md) memo files:\n%s", prompt)
	}
	if !strings.Contains(prompt, "heading") {
		t.Fatalf("built-in prompt must ask for memo headings:\n%s", prompt)
	}
}

// TestDefaultSystemPromptGuardsVariableDeletion verifies the built-in template
// carries the deletion discipline: a script never uses a bare variable as the
// whole delete target, and a deletion that has to use one goes through a
// dry-run script file first.
func TestDefaultSystemPromptGuardsVariableDeletion(t *testing.T) {
	prompt := DefaultSystemPrompt()
	for _, want := range []string{"deletion in a script", "prefix or suffix", "dry-run", "script file"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("built-in prompt must carry the deletion discipline (%q missing):\n%s", want, prompt)
		}
	}
}

// TestDefaultSystemPromptLeavesToolUsageToToolDescriptions verifies the template
// stays a behavior guide: every tool documents its own arguments and edge cases,
// so the prompt must not enumerate tools or repeat their mechanics.
func TestDefaultSystemPromptLeavesToolUsageToToolDescriptions(t *testing.T) {
	prompt := DefaultSystemPrompt()
	for _, name := range []string{"read_file", "write_file", "edit_file", "run_script", "manage_session", "webfetch"} {
		if strings.Contains(prompt, name) {
			t.Fatalf("built-in prompt must not describe tool %q (its own description does):\n%s", name, prompt)
		}
	}
}

// TestDefaultSystemPromptHasNoMCPContent verifies the built-in template never
// mentions MCP: the MCP usage rule is a capability section injected only while
// an MCP server actually contributes tools (see MCPToolsRule).
func TestDefaultSystemPromptHasNoMCPContent(t *testing.T) {
	prompt := DefaultSystemPrompt()
	if strings.Contains(prompt, "MCP") {
		t.Fatalf("built-in prompt must not mention MCP without MCP tools:\n%s", prompt)
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
