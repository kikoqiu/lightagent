package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePromptFile writes a file (creating parent directories) for the include
// tests.
func writePromptFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// loadPrompt expands the agent.md that lives next to cfgPath and requires it to
// be present.
func loadPrompt(t *testing.T, cfgPath string) string {
	t.Helper()
	prompt, ok, err := LoadAgentPrompt(cfgPath)
	if err != nil {
		t.Fatalf("LoadAgentPrompt: %v", err)
	}
	if !ok {
		t.Fatal("LoadAgentPrompt: ok = false, want true")
	}
	return prompt
}

// TestAgentPromptIncludeRelativeFile verifies a file directive is resolved
// against the directory of the file that holds it and that nesting works.
func TestAgentPromptIncludeRelativeFile(t *testing.T) {
	dir := t.TempDir()
	writePromptFile(t, filepath.Join(dir, "parts", "deeper", "leaf.md"), "leaf body\n")
	// The nested path is relative to role.md, not to agent.md.
	writePromptFile(t, filepath.Join(dir, "parts", "role.md"),
		"role start\n@include(\"deeper/leaf.md\")\nrole end\n")
	writePromptFile(t, filepath.Join(dir, "agent.md"), "top\n@include(\"parts/role.md\")\nbottom\n")

	got := loadPrompt(t, filepath.Join(dir, "config.json"))
	want := "top\nrole start\nleaf body\nrole end\nbottom"
	if got != want {
		t.Fatalf("prompt =\n%q\nwant\n%q", got, want)
	}
}

// TestAgentPromptIncludeDirectory verifies a directory directive inserts every
// file below it, joined with a blank line and in lexical order.
func TestAgentPromptIncludeDirectory(t *testing.T) {
	dir := t.TempDir()
	writePromptFile(t, filepath.Join(dir, "snippets", "b.md"), "bravo\n")
	writePromptFile(t, filepath.Join(dir, "snippets", "a.md"), "alpha\n")
	writePromptFile(t, filepath.Join(dir, "snippets", "nested", "c.md"), "charlie\n")
	writePromptFile(t, filepath.Join(dir, "agent.md"), "head\n@include(\"snippets\")\ntail\n")

	got := loadPrompt(t, filepath.Join(dir, "config.json"))
	want := "head\nalpha\n\nbravo\n\ncharlie\ntail"
	if got != want {
		t.Fatalf("prompt =\n%q\nwant\n%q", got, want)
	}
}

// TestAgentPromptIncludeAbsolutePath verifies an absolute directive ignores the
// agent.md directory.
func TestAgentPromptIncludeAbsolutePath(t *testing.T) {
	other := t.TempDir()
	target := filepath.Join(other, "abs.md")
	writePromptFile(t, target, "absolute body\n")

	dir := t.TempDir()
	writePromptFile(t, filepath.Join(dir, "agent.md"), "  @include(\""+target+"\")\n")

	got := loadPrompt(t, filepath.Join(dir, "config.json"))
	if want := "absolute body"; got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
}

// TestAgentPromptIncludeDropsEmptyDirective verifies an include that contributes
// nothing (an empty directory) disappears instead of leaving a blank line.
func TestAgentPromptIncludeDropsEmptyDirective(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writePromptFile(t, filepath.Join(dir, "agent.md"), "top\n@include(\"empty\")\nbottom\n")

	got := loadPrompt(t, filepath.Join(dir, "config.json"))
	if want := "top\nbottom"; got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
}

// TestAgentPromptIncludeOnlyWholeLines verifies the directive is expanded only
// when it is the sole content of a line, so prose mentioning the syntax and an
// empty path stay literal.
func TestAgentPromptIncludeOnlyWholeLines(t *testing.T) {
	dir := t.TempDir()
	content := "see @include(\"missing.md\") inline\n@include(\"\")\nlast\n"
	writePromptFile(t, filepath.Join(dir, "agent.md"), content)

	got := loadPrompt(t, filepath.Join(dir, "config.json"))
	if want := strings.TrimSpace(content); got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
}

// TestAgentPromptIncludeMissingTarget verifies a missing target is an error.
func TestAgentPromptIncludeMissingTarget(t *testing.T) {
	dir := t.TempDir()
	writePromptFile(t, filepath.Join(dir, "agent.md"), "@include(\"nope.md\")\n")

	_, _, err := LoadAgentPrompt(filepath.Join(dir, "config.json"))
	if err == nil {
		t.Fatal("expected an error for a missing include target")
	}
	if !strings.Contains(err.Error(), "nope.md") {
		t.Fatalf("error %q does not name the missing file", err)
	}
}

// TestAgentPromptIncludeCycle verifies a self-referencing include chain is
// rejected instead of looping forever.
func TestAgentPromptIncludeCycle(t *testing.T) {
	dir := t.TempDir()
	writePromptFile(t, filepath.Join(dir, "agent.md"), "@include(\"a.md\")\n")
	writePromptFile(t, filepath.Join(dir, "a.md"), "@include(\"b.md\")\n")
	writePromptFile(t, filepath.Join(dir, "b.md"), "@include(\"a.md\")\n")

	_, _, err := LoadAgentPrompt(filepath.Join(dir, "config.json"))
	if err == nil {
		t.Fatal("expected an error for an include cycle")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error %q does not mention the cycle", err)
	}
}

// TestAgentPromptIncludeDepthLimit verifies an over-long include chain stops at
// MaxAgentIncludeDepth.
func TestAgentPromptIncludeDepthLimit(t *testing.T) {
	dir := t.TempDir()
	last := MaxAgentIncludeDepth + 3
	writePromptFile(t, filepath.Join(dir, "agent.md"), "@include(\"level0.md\")\n")
	for i := 0; i < last; i++ {
		writePromptFile(t, filepath.Join(dir, fmt.Sprintf("level%d.md", i)),
			fmt.Sprintf("@include(\"level%d.md\")\n", i+1))
	}
	writePromptFile(t, filepath.Join(dir, fmt.Sprintf("level%d.md", last)), "deep\n")

	_, _, err := LoadAgentPrompt(filepath.Join(dir, "config.json"))
	if err == nil {
		t.Fatal("expected an error for an include chain that is too deep")
	}
	if !strings.Contains(err.Error(), "levels deep") {
		t.Fatalf("error %q does not report the depth limit", err)
	}
}

// TestAgentPromptIncludeCRLF verifies a CRLF prompt file is expanded while
// keeping its own line endings.
func TestAgentPromptIncludeCRLF(t *testing.T) {
	dir := t.TempDir()
	writePromptFile(t, filepath.Join(dir, "part.md"), "crumb\r\n")
	writePromptFile(t, filepath.Join(dir, "agent.md"), "a\r\n@include(\"part.md\")\r\nb\r\n")

	got := loadPrompt(t, filepath.Join(dir, "config.json"))
	if want := "a\r\ncrumb\nb"; got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
}

// TestLoadFileReportsBrokenInclude verifies a broken include in agent.md
// surfaces as a config load error instead of silently using the built-in
// prompt.
func TestLoadFileReportsBrokenInclude(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := Default()
	cfg.OpenAI.APIKey = "sk-x"
	if err := Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	writePromptFile(t, filepath.Join(dir, "agent.md"), "@include(\"missing.md\")\n")

	if _, _, _, err := LoadFile(cfgPath); err == nil {
		t.Fatal("expected LoadFile to fail on a broken include")
	}
}
