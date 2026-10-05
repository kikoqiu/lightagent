package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultHasOneEnabledOpenAIInterface pins the out-of-the-box document: a
// single OpenAI interface named "default", enabled, carrying the request shape
// and the model's context window.
func TestDefaultHasOneEnabledOpenAIInterface(t *testing.T) {
	cfg := Default()
	if len(cfg.LLMs) != 1 {
		t.Fatalf("providers = %d, want 1", len(cfg.LLMs))
	}
	api := cfg.LLMs[0]
	if api.Name != DefaultLLMName || api.EffectiveType() != LLMTypeOpenAI || !api.Enabled {
		t.Fatalf("default interface = %+v, want the enabled openai default", api)
	}
	if api.ContextWindow <= 0 {
		t.Fatalf("context_window = %d, want the built-in window", api.ContextWindow)
	}
	if idx, active, found := cfg.ActiveLLM(); !found || idx != 0 || active.Name != DefaultLLMName {
		t.Fatalf("ActiveLLM = (%d, %q, %v), want the default", idx, active.Name, found)
	}
}

// TestLegacyOpenAIDocumentIsMigrated pins the old single-endpoint layout: the
// "openai" block (and the context window that used to sit under "context") is
// folded into one providers entry named "default", and the file is rewritten in the
// new shape.
func TestLegacyOpenAIDocumentIsMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := `{"openai":{"api_base":"https://example.test/v1","api_key":"sk-x","model":"m1"},` +
		`"context":{"context_window":1234,"summarize_token_percent":60}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, _, err := LoadFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.LLMs) != 1 {
		t.Fatalf("providers = %d, want the single migrated interface", len(cfg.LLMs))
	}
	api := cfg.LLMs[0]
	if api.Name != DefaultLLMName || api.EffectiveType() != LLMTypeOpenAI || !api.Enabled {
		t.Fatalf("migrated interface = %+v", api)
	}
	if api.APIBase != "https://example.test/v1" || api.APIKey != "sk-x" || api.Model != "m1" {
		t.Fatalf("migrated endpoint = %+v", api.OpenAIConfig)
	}
	if api.ContextWindow != 1234 {
		t.Fatalf("context_window = %d, want the legacy window 1234", api.ContextWindow)
	}

	// The file is rewritten in the new shape: the "openai" key is gone and a
	// "providers" array took its place.
	rewritten := readFile(t, path)
	if strings.Contains(rewritten, `"openai":`) {
		t.Fatalf("the legacy block survived the migration:\n%s", rewritten)
	}
	if !strings.Contains(rewritten, `"providers"`) || !strings.Contains(rewritten, `"name": "default"`) {
		t.Fatalf("the file was not rewritten in the new shape:\n%s", rewritten)
	}
}

// TestMultipleInterfacesAndLookup pins that a document may declare several
// interfaces, that the first enabled one is the active choice, and that a
// /switchapi argument resolves by 1-based number or by name.
func TestMultipleInterfacesAndLookup(t *testing.T) {
	doc := `{"providers":[` +
		`{"name":"a","type":"openai","enabled":false,"api_key":"k1"},` +
		`{"name":"b","type":"openai","enabled":true,"api_key":"k2","context_window":4096}]}`
	cfg, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	idx, active, found := cfg.ActiveLLM()
	if !found || idx != 1 || active.Name != "b" {
		t.Fatalf("ActiveLLM = (%d, %q, %v), want the first enabled (b)", idx, active.Name, found)
	}
	if active.ContextWindow != 4096 {
		t.Fatalf("context_window = %d, want 4096", active.ContextWindow)
	}
	if got, ok := cfg.LLMIndex("b"); !ok || got != 1 {
		t.Fatalf("LLMIndex(b) = %d/%v, want 1/true", got, ok)
	}
	if got, ok := cfg.LLMIndex("2"); !ok || got != 1 {
		t.Fatalf("LLMIndex(2) = %d/%v, want 1/true", got, ok)
	}
	if _, ok := cfg.LLMIndex("nope"); ok {
		t.Fatal("LLMIndex(nope) matched an unknown name")
	}
	if _, ok := cfg.LLMIndex("3"); ok {
		t.Fatal("LLMIndex(3) matched out of range")
	}
}

// TestValidateLLMInterfaces covers the interface rules: at least one enabled
// entry, a supported type and a non-empty key.
func TestValidateLLMInterfaces(t *testing.T) {
	ok := Default()
	ok.LLMs[0].APIKey = "sk-x"
	if err := ok.Validate(); err != nil {
		t.Fatalf("a configured interface must validate: %v", err)
	}

	noKey := Default()
	noKey.LLMs[0].APIKey = ""
	if err := noKey.Validate(); err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("an enabled interface without api_key must be rejected, got %v", err)
	}

	disabled := Default()
	disabled.LLMs[0].Enabled = false
	if err := disabled.Validate(); err == nil || !strings.Contains(err.Error(), "enabled") {
		t.Fatalf("a document with nothing enabled must be rejected, got %v", err)
	}

	badType := Default()
	badType.LLMs[0].APIKey = "sk-x"
	badType.LLMs[0].Type = "anthropic"
	if err := badType.Validate(); err == nil || !strings.Contains(err.Error(), "type") {
		t.Fatalf("an unsupported type must be rejected, got %v", err)
	}
}

// TestMaskSecretsMasksEveryInterface pins that the display copy hides each
// interface's key and never writes through to the receiver.
func TestMaskSecretsMasksEveryInterface(t *testing.T) {
	cfg := Default()
	cfg.LLMs[0].APIKey = "sk-a"
	cfg.LLMs = append(cfg.LLMs, LLMConfig{
		Name: "b", Type: LLMTypeOpenAI, Enabled: true,
		OpenAIConfig: OpenAIConfig{APIKey: "sk-b"},
	})
	masked := cfg.MaskSecrets()
	if masked.LLMs[0].APIKey != MaskedSecret || masked.LLMs[1].APIKey != MaskedSecret {
		t.Fatalf("keys not masked: %+v", masked.LLMs)
	}
	if cfg.LLMs[0].APIKey != "sk-a" || cfg.LLMs[1].APIKey != "sk-b" {
		t.Fatalf("the receiver was modified: %+v", cfg.LLMs)
	}
}

// TestAPIInfos pins the picker listing: the 1-based number, the name/model and
// which entry is active.
func TestAPIInfos(t *testing.T) {
	cfg := Default()
	cfg.LLMs = append(cfg.LLMs, LLMConfig{
		Name: "b", Type: LLMTypeOpenAI, Enabled: false,
		OpenAIConfig: OpenAIConfig{Model: "m2"},
	})
	infos := cfg.APIInfos(0)
	if len(infos) != 2 {
		t.Fatalf("infos = %d, want 2", len(infos))
	}
	if infos[0].Index != 1 || !infos[0].Active || infos[0].Name != DefaultLLMName || !infos[0].Enabled {
		t.Fatalf("infos[0] = %+v", infos[0])
	}
	if infos[1].Index != 2 || infos[1].Active || infos[1].Enabled || infos[1].Model != "m2" {
		t.Fatalf("infos[1] = %+v", infos[1])
	}
}

// TestParseStrictAcceptsLegacyOpenAI pins that a hand-pasted old document still
// passes the strict decode the config editor uses (the "openai" key stays known),
// and that the deprecated key is cleared after migration.
func TestParseStrictAcceptsLegacyOpenAI(t *testing.T) {
	cfg, err := ParseStrict([]byte(`{"openai":{"api_key":"sk-x","model":"m"}}`))
	if err != nil {
		t.Fatalf("strict parse of a legacy document: %v", err)
	}
	if cfg.LLMs[0].APIKey != "sk-x" || cfg.LLMs[0].Model != "m" {
		t.Fatalf("migrated interface = %+v", cfg.LLMs[0])
	}
	if cfg.LegacyOpenAI != nil {
		t.Fatal("the deprecated openai block must be cleared after migration")
	}
}
