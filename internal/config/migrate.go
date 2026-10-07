package config

import (
	"bytes"
	"encoding/json"
)

// legacyLLMInput captures the pre-multi-interface layout of a document: whether
// a "providers" array is present at all, the raw single "openai" block, and the
// context window that used to live under "context".
type legacyLLMInput struct {
	hasLLMs       bool
	openai        json.RawMessage
	contextWindow int
}

// probeLegacyLLM reads the legacy shape out of a raw document without touching
// the parsed config: the caller uses it to decide whether the seeded default
// interface has to be replaced by a migrated one.
func probeLegacyLLM(data []byte) legacyLLMInput {
	var probe struct {
		LLMs    json.RawMessage `json:"providers"`
		OpenAI  json.RawMessage `json:"openai"`
		Context struct {
			ContextWindow int `json:"context_window"`
		} `json:"context"`
	}
	_ = json.Unmarshal(data, &probe) // a malformed document is reported by the caller
	return legacyLLMInput{
		hasLLMs:       len(bytes.TrimSpace(probe.LLMs)) > 0,
		openai:        probe.OpenAI,
		contextWindow: probe.Context.ContextWindow,
	}
}

// migrateLegacyLLM folds a legacy single-endpoint document into cfg.LLMs. It is
// a no-op for a document that already carries a "providers" array; a document with
// neither keeps the default interface. It reports whether cfg changed and must
// be written back.
//
// The migrated interface starts from the built-in default entry (so an omitted
// legacy field keeps its default — the temperature -1 sentinel included) and is
// then overlaid with the legacy "openai" block and its context window.
func (c *Config) migrateLegacyLLM(in legacyLLMInput) bool {
	if in.hasLLMs {
		// A new-format document: drop any legacy block the decoder picked up.
		if c.LegacyOpenAI != nil {
			c.LegacyOpenAI = nil
			return true
		}
		return false
	}
	api := Default().LLMs[0]
	if len(bytes.TrimSpace(in.openai)) > 0 {
		_ = json.Unmarshal(in.openai, &api.OpenAIConfig)
	}
	if in.contextWindow > 0 {
		api.ContextWindow = in.contextWindow
	}
	c.LLMs = []LLMConfig{api}
	c.LegacyOpenAI = nil
	return true
}
