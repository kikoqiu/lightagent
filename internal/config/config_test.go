package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lightagent/internal/passwd"
)

// readFile reads a config file for assertions.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestLoadFileTimeoutOverride verifies the inactivity timeout: a missing key
// keeps the default while an explicit 0 (disabled) survives the defaults pass.
func TestLoadFileTimeoutOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	// Missing key -> default.
	if err := os.WriteFile(path, []byte(`{"openai":{"model":"m"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, _, err := LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.OpenAI.TimeoutSec != Default().OpenAI.TimeoutSec {
		t.Fatalf("timeout = %d, want the default", cfg.OpenAI.TimeoutSec)
	}

	// Explicit 0 disables it.
	if err := os.WriteFile(path, []byte(`{"openai":{"model":"m","timeout_seconds":0}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, _, err = LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.OpenAI.TimeoutSec != 0 {
		t.Fatalf("timeout = %d, want 0 (disabled)", cfg.OpenAI.TimeoutSec)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg := Default()
	cfg.OpenAI.APIKey = "sk-test"
	cfg.Web.Port = 9000
	if err := Save(path, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	t.Setenv("LIGHTAGENT_CONFIG", path)
	loaded, gotPath, created, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if created {
		t.Fatal("created = true, want false for an existing file")
	}
	if gotPath != path {
		t.Fatalf("path = %s, want %s", gotPath, path)
	}
	if loaded.OpenAI.APIKey != "sk-test" {
		t.Fatalf("api_key = %q", loaded.OpenAI.APIKey)
	}
	if loaded.Web.Port != 9000 || !loaded.WebEnabled() {
		t.Fatalf("web port = %d, enabled = %v", loaded.Web.Port, loaded.WebEnabled())
	}
}

func TestLoadCreatesDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.json")
	t.Setenv("LIGHTAGENT_CONFIG", path)

	cfg, gotPath, created, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true for a missing file")
	}
	if gotPath != path {
		t.Fatalf("path = %s, want %s", gotPath, path)
	}
	if cfg.OpenAI.Model == "" || cfg.Context.ContextWindow == 0 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default config not written: %v", err)
	}
}

func TestApplyDefaultsOnPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// Only set an api key; everything else must fall back to defaults.
	if err := os.WriteFile(path, []byte(`{"openai":{"api_key":"sk-x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTAGENT_CONFIG", path)

	cfg, _, _, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.OpenAI.Model == "" {
		t.Fatal("model default not applied")
	}
	if cfg.Context.ContextWindow != 81960 {
		t.Fatalf("context_window = %d, want default", cfg.Context.ContextWindow)
	}
	if cfg.Agent.MaxToolIterations == 0 {
		t.Fatal("max_tool_iterations default not applied")
	}
}

func TestUIMarkdownDefaultAndOverride(t *testing.T) {
	if !Default().UI.Markdown {
		t.Fatal("ui.markdown should default to true")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"openai":{"api_key":"sk-x"},"ui":{"markdown":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTAGENT_CONFIG", path)

	cfg, _, _, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.UI.Markdown {
		t.Fatal("ui.markdown = true, want false from the config file")
	}

	// A file without a ui section keeps the default.
	if err := os.WriteFile(path, []byte(`{"openai":{"api_key":"sk-x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, _, err = Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.UI.Markdown {
		t.Fatal("ui.markdown = false, want the default true when ui is absent")
	}
}

// TestExecUseUTF8DefaultAndOverride verifies tools.exec.use_utf8 defaults to
// true and can be turned off explicitly.
func TestExecUseUTF8DefaultAndOverride(t *testing.T) {
	if !Default().Tools.Exec.UseUTF8 {
		t.Fatal("tools.exec.use_utf8 should default to true")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"openai":{"api_key":"sk-x"},"tools":{"exec":{"use_utf8":false}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTAGENT_CONFIG", path)

	cfg, _, _, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Tools.Exec.UseUTF8 {
		t.Fatal("tools.exec.use_utf8 = true, want false from the config file")
	}

	// A file without an exec section keeps the default.
	if err := os.WriteFile(path, []byte(`{"openai":{"api_key":"sk-x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, _, err = Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Tools.Exec.UseUTF8 {
		t.Fatal("tools.exec.use_utf8 = false, want the default true when exec is absent")
	}
}

// TestLoadFileExplicitPath covers -c/--config: an explicit path is used as-is
// and a missing file is created from defaults.
func TestLoadFileExplicitPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.json")
	if err := os.WriteFile(path, []byte(`{"openai":{"api_key":"sk-x","model":"m1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, got, created, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if created {
		t.Fatal("created = true for an existing file")
	}
	if filepath.Clean(got) != filepath.Clean(path) {
		t.Fatalf("resolved path = %q, want %q", got, path)
	}
	if cfg.OpenAI.Model != "m1" {
		t.Fatalf("model = %q, want m1", cfg.OpenAI.Model)
	}

	missing := filepath.Join(dir, "sub", "new.json")
	cfg2, got2, created2, err := LoadFile(missing)
	if err != nil {
		t.Fatalf("LoadFile(missing): %v", err)
	}
	if !created2 {
		t.Fatal("created = false for a missing file")
	}
	if filepath.Clean(got2) != filepath.Clean(missing) {
		t.Fatalf("resolved path = %q, want %q", got2, missing)
	}
	if cfg2.OpenAI.Model == "" {
		t.Fatal("defaults not applied")
	}
	if _, err := os.Stat(missing); err != nil {
		t.Fatalf("default config not written: %v", err)
	}
}

func TestWebHostDefaultAndOverride(t *testing.T) {
	if got := Default().Web.Host; got != "127.0.0.1" {
		t.Fatalf("default web.host = %q, want 127.0.0.1", got)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// Omitting web.host keeps the loopback default.
	if err := os.WriteFile(path, []byte(`{"openai":{"api_key":"sk-x"},"web":{"port":9000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTAGENT_CONFIG", path)
	cfg, _, _, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Web.Host != "127.0.0.1" {
		t.Fatalf("web.host = %q, want the default when omitted", cfg.Web.Host)
	}

	// An explicit bind address is preserved.
	if err := os.WriteFile(path, []byte(`{"openai":{"api_key":"sk-x"},"web":{"host":"0.0.0.0","port":9000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, _, err = Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Web.Host != "0.0.0.0" {
		t.Fatalf("web.host = %q, want 0.0.0.0", cfg.Web.Host)
	}
}

func TestValidate(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error when api_key is empty")
	}
	cfg.OpenAI.APIKey = "sk-x"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAgentPromptPath(t *testing.T) {
	got := AgentPromptPath(filepath.Join("a", "b", "config.json"))
	want := filepath.Join("a", "b", "agent.md")
	if got != want {
		t.Fatalf("AgentPromptPath = %q, want %q", got, want)
	}
}

// TestAgentMdOverridesPrompt verifies a program-directory agent.md wins over
// agent.system_prompt in config.json.
func TestAgentMdOverridesPrompt(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := Default()
	cfg.OpenAI.APIKey = "sk-x"
	cfg.Agent.SystemPrompt = "from-config"
	if err := Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	if err := os.WriteFile(AgentPromptPath(cfgPath), []byte("from-agent-md\n"), 0o644); err != nil {
		t.Fatalf("write agent.md: %v", err)
	}

	t.Setenv("LIGHTAGENT_CONFIG", cfgPath)
	loaded, _, _, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Agent.SystemPrompt != "from-agent-md" {
		t.Fatalf("system prompt = %q, want from-agent-md", loaded.Agent.SystemPrompt)
	}
}

// TestAgentMdMissingKeepsConfigPrompt verifies the config prompt is used when
// no agent.md exists.
func TestAgentMdMissingKeepsConfigPrompt(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := Default()
	cfg.OpenAI.APIKey = "sk-x"
	cfg.Agent.SystemPrompt = "from-config"
	if err := Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	t.Setenv("LIGHTAGENT_CONFIG", cfgPath)
	loaded, _, _, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Agent.SystemPrompt != "from-config" {
		t.Fatalf("system prompt = %q, want from-config", loaded.Agent.SystemPrompt)
	}
}

// TestDiscoveryConfigDefaults verifies the tool-discovery defaults.
func TestDiscoveryConfigDefaults(t *testing.T) {
	d := Default().Tools.Discovery
	if d.Enabled {
		t.Fatal("tools.discovery should default to disabled")
	}
	if d.EffectiveMode() != ToolDiscoveryModeUnlock {
		t.Fatalf("mode = %q, want %q", d.EffectiveMode(), ToolDiscoveryModeUnlock)
	}
	if d.TTL != 50 || d.MaxSearchResults != 10 || d.MinMatchRate != 0.5 || !d.UseBM25 {
		t.Fatalf("discovery defaults = %+v", d)
	}
}

// TestDiscoveryConfigValidate verifies mode and search-tool validation.
func TestDiscoveryConfigValidate(t *testing.T) {
	cfg := Default()
	cfg.OpenAI.APIKey = "sk-x"
	cfg.Tools.Discovery.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("enabled unlock discovery should validate: %v", err)
	}

	cfg.Tools.Discovery.Mode = "classic"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unsupported discovery mode must fail validation")
	}

	cfg.Tools.Discovery.Mode = ToolDiscoveryModeUnlock
	cfg.Tools.Discovery.UseBM25 = false
	if err := cfg.Validate(); err == nil {
		t.Fatal("disabling the only discovery search tool must fail validation")
	}
}

// TestDiscoveryConfigPartialFileDefaults verifies a partial discovery section
// keeps the built-in defaults when merged from a config file.
func TestDiscoveryConfigPartialFileDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	blob := `{"openai":{"api_key":"sk-x"},"tools":{"discovery":{"enabled":true}}}`
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTAGENT_CONFIG", path)

	cfg, _, _, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	d := cfg.Tools.Discovery
	if !d.Enabled || d.TTL != 50 || d.MaxSearchResults != 10 || d.MinMatchRate != 0.5 || !d.UseBM25 {
		t.Fatalf("merged discovery config = %+v, want defaults preserved", d)
	}
	if d.EffectiveMode() != ToolDiscoveryModeUnlock {
		t.Fatalf("mode = %q, want unlock", d.EffectiveMode())
	}
}

// TestMCPServerConfigEffectiveType verifies transport resolution.
func TestMCPServerConfigEffectiveType(t *testing.T) {
	cases := []struct {
		cfg  MCPServerConfig
		want string
	}{
		{MCPServerConfig{Command: "npx"}, MCPTransportStdio},
		{MCPServerConfig{URL: "http://x"}, MCPTransportHTTP},
		{MCPServerConfig{Type: "streamable-http", URL: "http://x"}, MCPTransportHTTP},
		{MCPServerConfig{Type: "http", URL: "http://x"}, MCPTransportHTTP},
		{MCPServerConfig{Type: "sse", URL: "http://x"}, MCPTransportSSE},
		{MCPServerConfig{Type: "stdio", Command: "node"}, MCPTransportStdio},
	}
	for _, tc := range cases {
		if got := tc.cfg.EffectiveType(); got != tc.want {
			t.Errorf("EffectiveType(%+v) = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}

// TestMCPConfigValidate verifies per-server transport validation.
func TestMCPConfigValidate(t *testing.T) {
	cfg := Default()
	cfg.OpenAI.APIKey = "sk-x"
	cfg.Tools.MCP.Enabled = true
	cfg.Tools.MCP.Servers = map[string]MCPServerConfig{
		"local":  {Enabled: true, Command: "node", Args: []string{"server.js"}},
		"remote": {Enabled: true, Type: "http", URL: "http://127.0.0.1:9999/mcp"},
		"off":    {Enabled: false},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid MCP config rejected: %v", err)
	}

	cfg.Tools.MCP.Servers["bad"] = MCPServerConfig{Enabled: true}
	if err := cfg.Validate(); err == nil {
		t.Fatal("stdio server without a command must fail validation")
	}
	delete(cfg.Tools.MCP.Servers, "bad")

	cfg.Tools.MCP.Servers["remote"] = MCPServerConfig{Enabled: true, Type: "http"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("http server without a url must fail validation")
	}

	cfg.Tools.MCP.Servers["remote"] = MCPServerConfig{Enabled: true, Type: "carrier-pigeon", URL: "http://x"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("unsupported transport must fail validation")
	}
}

// TestLoadPersistsMissingDefaults verifies a partial config file is completed
// with the built-in defaults on load, and that an already complete file is not
// rewritten (so neither its content nor its modification time changes).
func TestLoadPersistsMissingDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"openai":{"api_key":"sk-x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTAGENT_CONFIG", path)

	if _, _, _, err := Load(); err != nil {
		t.Fatalf("load: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse completed config: %v", err)
	}
	if _, ok := doc["ui"]; !ok {
		t.Fatalf("ui defaults were not written:\n%s", data)
	}
	agentDoc, _ := doc["agent"].(map[string]any)
	if agentDoc["include_working_dir"] != true {
		t.Fatalf("agent.include_working_dir = %v, want true written:\n%s", agentDoc["include_working_dir"], data)
	}
	if fileMissingDefaults(data) {
		t.Fatalf("a completed file still reports missing defaults:\n%s", data)
	}

	// A complete file is left byte-for-byte untouched: rewrite it compactly and
	// confirm that loading does not normalise it back.
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if err := os.WriteFile(path, compact.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, compact.Bytes()) {
		t.Fatalf("a complete config was rewritten:\nbefore: %s\nafter:  %s", compact.Bytes(), after)
	}
}

// TestLoadInsertsDisabledMCPServer verifies an empty tools.mcp.servers gains a
// disabled placeholder instance, while a file that already declares a server
// is left untouched.
func TestLoadInsertsDisabledMCPServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	blob := `{"openai":{"api_key":"sk-x"},"tools":{"mcp":{"enabled":false,"servers":{}}}}`
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTAGENT_CONFIG", path)

	cfg, _, _, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	srv, ok := cfg.Tools.MCP.Servers[DefaultMCPServerName]
	if !ok {
		t.Fatalf("placeholder %q not inserted: %+v", DefaultMCPServerName, cfg.Tools.MCP.Servers)
	}
	if srv.Enabled {
		t.Fatal("the placeholder server must stay disabled")
	}

	// The placeholder is persisted, so the next load sees an explicit server.
	reloaded, _, _, err := Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := reloaded.Tools.MCP.Servers[DefaultMCPServerName]; !ok {
		t.Fatal("placeholder server was not written to the file")
	}

	// A file that already declares a server is not augmented.
	withServer := `{"openai":{"api_key":"sk-x"},"tools":{"mcp":{"servers":{"local":{"enabled":true,"command":"node"}}}}}`
	if err := os.WriteFile(path, []byte(withServer), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, _, err = Load()
	if err != nil {
		t.Fatalf("load with a server: %v", err)
	}
	if _, ok := cfg.Tools.MCP.Servers[DefaultMCPServerName]; ok {
		t.Fatal("placeholder must not be added when a server is already declared")
	}
	if _, ok := cfg.Tools.MCP.Servers["local"]; !ok {
		t.Fatalf("declared server lost: %+v", cfg.Tools.MCP.Servers)
	}
}

// TestParseAppliesDefaults keeps the shared decode path honest: an editor reading
// a partial document must see the same values the next start would use.
func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`{"openai":{"api_key":"sk-x"}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.OpenAI.Model != Default().OpenAI.Model {
		t.Fatalf("model = %q, want the default", cfg.OpenAI.Model)
	}
	if cfg.Context.ContextWindow != Default().Context.ContextWindow {
		t.Fatalf("context_window = %d, want the default", cfg.Context.ContextWindow)
	}
	if _, ok := cfg.Tools.MCP.Servers[DefaultMCPServerName]; !ok {
		t.Fatalf("the disabled MCP placeholder should be seeded: %+v", cfg.Tools.MCP.Servers)
	}
}

// TestParseToleratesUnknownFields pins the loader's rule: a file written by
// another version still loads.
func TestParseToleratesUnknownFields(t *testing.T) {
	cfg, err := Parse([]byte(`{"openai":{"api_key":"sk-x","keep_recent_messages":4}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.OpenAI.APIKey != "sk-x" {
		t.Fatalf("api_key = %q", cfg.OpenAI.APIKey)
	}
}

// TestParseStrictRejectsUnknownFields pins the editor's rule: a typo in an edited
// document is reported instead of being dropped on save. An empty or malformed
// document is refused by both entry points.
func TestParseStrictRejectsUnknownFields(t *testing.T) {
	if _, err := ParseStrict([]byte(`{"openai":{"api_key":"sk-x","modle":"typo"}}`)); err == nil {
		t.Fatal(`ParseStrict should reject the unknown field "modle"`)
	}
	// A document pasted twice would silently keep only the first value.
	two := `{"openai":{"api_key":"sk-x"}}{"context":{"context_window":100}}`
	if _, err := ParseStrict([]byte(two)); err == nil {
		t.Fatal("ParseStrict should reject a second document")
	}
	for _, strict := range []bool{false, true} {
		parse := Parse
		if strict {
			parse = ParseStrict
		}
		if _, err := parse([]byte("  \n")); err == nil {
			t.Fatalf("strict=%v: an empty document should be refused", strict)
		}
		if _, err := parse([]byte("{")); err == nil {
			t.Fatalf("strict=%v: a malformed document should be refused", strict)
		}
	}
}

// TestMaskSecrets covers the display copy shared by --print-config and the web
// editor: the secrets are hidden, the receiver is untouched, the public salt
// stays visible and other fields survive.
func TestMaskSecrets(t *testing.T) {
	cfg := Default()
	cfg.OpenAI.APIKey = "sk-x"
	cfg.Web.Password = "hunter2"
	cfg.Web.PasswordSalt = "0123456789abcdef0123456789abcdef"
	masked := cfg.MaskSecrets()
	if masked.OpenAI.APIKey != MaskedSecret {
		t.Fatalf("api_key = %q, want %q", masked.OpenAI.APIKey, MaskedSecret)
	}
	if masked.Web.Password != MaskedSecret {
		t.Fatalf("password = %q, want %q", masked.Web.Password, MaskedSecret)
	}
	if masked.Web.PasswordSalt != cfg.Web.PasswordSalt {
		t.Fatalf("password_salt = %q, want the public salt unchanged", masked.Web.PasswordSalt)
	}
	if cfg.OpenAI.APIKey != "sk-x" || cfg.Web.Password != "hunter2" {
		t.Fatalf("the receiver must not be modified: %q / %q", cfg.OpenAI.APIKey, cfg.Web.Password)
	}
	if empty := Default().MaskSecrets(); empty.OpenAI.APIKey != "" || empty.Web.Password != "" {
		t.Fatalf("empty secrets stay empty, got %q / %q", empty.OpenAI.APIKey, empty.Web.Password)
	}
	if masked.Context != cfg.Context || !masked.UI.Markdown {
		t.Fatal("the copy must carry the other fields unchanged")
	}
}

// TestLoadAddsSaltToPlaintextPassword checks a hand-written password: it stays
// in the clear (by design) but gains a salt, which is what the browser's digest
// is bound to.
func TestLoadAddsSaltToPlaintextPassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	blob := `{"openai":{"api_key":"sk-x"},"web":{"host":"0.0.0.0","port":8791,"password":"hunter2"}}`
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, _, err := LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Web.Password != "hunter2" {
		t.Fatalf("password = %q, want the stored plaintext", cfg.Web.Password)
	}
	if len(cfg.Web.PasswordSalt) != 32 {
		t.Fatalf("password_salt = %q, want a generated 32-character salt", cfg.Web.PasswordSalt)
	}
	if !passwd.Matches(cfg.Web.PasswordSalt, "hunter2", passwd.Digest(cfg.Web.PasswordSalt, "hunter2")) {
		t.Fatal("the generated salt must work with the login digest")
	}

	for _, want := range []string{`"password": "hunter2"`, `"password_salt": "` + cfg.Web.PasswordSalt + `"`} {
		if !strings.Contains(readFile(t, path), want) {
			t.Fatalf("the file does not carry %s:\n%s", want, readFile(t, path))
		}
	}
}

// TestLoadKeepsACredentialedFile checks a file that already has a password and a
// salt: it is not rewritten and both values survive.
func TestLoadKeepsACredentialedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	doc := Default()
	doc.OpenAI.APIKey = "sk-x"
	doc.Web.Host = "127.0.0.1"
	doc.Web.Password = "hunter2"
	doc.Web.PasswordSalt = "0123456789abcdef0123456789abcdef"
	// A server entry keeps the file complete, so the loader has nothing else to
	// seed and must leave it alone.
	doc.Tools.MCP.Servers = defaultMCPServers()
	if err := Save(path, doc); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)

	loaded, _, _, err := LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Web.Password != "hunter2" || loaded.Web.PasswordSalt != doc.Web.PasswordSalt {
		t.Fatalf("credential = %q / %q, want the stored values", loaded.Web.Password, loaded.Web.PasswordSalt)
	}
	if after := readFile(t, path); after != before {
		t.Fatalf("a complete file was rewritten:\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestLoadDropsSaltWithoutPassword checks the reverse pairing: a salt left over
// after the password was removed is cleaned up instead of confusing the login.
func TestLoadDropsSaltWithoutPassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	doc := Default()
	doc.OpenAI.APIKey = "sk-x"
	doc.Web.Host = "127.0.0.1"
	doc.Web.PasswordSalt = "0123456789abcdef0123456789abcdef"
	doc.Tools.MCP.Servers = defaultMCPServers()
	if err := Save(path, doc); err != nil {
		t.Fatal(err)
	}

	loaded, _, _, err := LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Web.PasswordSalt != "" {
		t.Fatalf("password_salt = %q, want it cleared", loaded.Web.PasswordSalt)
	}
	if strings.Contains(readFile(t, path), "password_salt") {
		t.Fatal("the leftover salt is still in the file")
	}
}

// TestParseHandlesEchoedMask checks the editor round trip: a document that echoes
// the masked password is dropped rather than stored as the new password.
func TestParseHandlesEchoedMask(t *testing.T) {
	cfg, err := ParseStrict([]byte(`{"openai":{"api_key":"***"},"web":{"password":"***"}}`))
	if err != nil {
		t.Fatalf("ParseStrict: %v", err)
	}
	if cfg.OpenAI.APIKey != MaskedSecret {
		t.Fatalf("api_key = %q, want the echoed mask", cfg.OpenAI.APIKey)
	}
	if cfg.Web.Password != "" {
		t.Fatalf("the masked password must be dropped, got %q", cfg.Web.Password)
	}
}
