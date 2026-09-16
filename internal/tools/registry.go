// Package tools implements the built-in tools exposed to the model:
// exec_command, manage_session, read_file_lines, write_file and edit_file.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"lightagent/internal/llm"
)

// Tool is a callable capability exposed to the model.
type Tool interface {
	// Name is the unique tool identifier used in tool calls.
	Name() string
	// Description explains to the model when and how to use the tool.
	Description() string
	// Parameters is a JSON-schema object describing the arguments.
	Parameters() map[string]any
	// Execute runs the tool with already-decoded arguments.
	Execute(ctx context.Context, args map[string]any) *Result
}

// Result is the outcome of a tool execution.
type Result struct {
	// ForLLM is the content appended to the conversation as the tool result.
	ForLLM string
	// ForUser, when non-empty, is a user-facing rendering (CLI/web display).
	ForUser string
	// IsError marks the invocation as failed.
	IsError bool
	// Silent suppresses user-facing rendering even when ForUser is set.
	Silent bool
}

// OK creates a successful result.
func OK(forLLM string) *Result { return &Result{ForLLM: forLLM} }

// Silent creates a successful, user-silent result.
func Silent(forLLM string) *Result { return &Result{ForLLM: forLLM, Silent: true} }

// Fail creates an error result.
func Fail(message string) *Result { return &Result{ForLLM: message, IsError: true} }

// toolEntry is one registered tool together with its discovery metadata.
//
// Core tools are always advertised in the provider tools array and always
// callable. Deferred tools (unlock discovery mode) are never advertised; they
// form the locked-function library that the model finds through the BM25
// search tool and activates with unlock_tool. Execution of a deferred tool is
// gated by an active grant (see GrantTools).
type toolEntry struct {
	tool     Tool
	core     bool
	deferred bool
}

// Registry stores tools by name and drives dispatch.
type Registry struct {
	order   []string
	byName  map[string]*toolEntry
	grants  map[string]int // deferred name -> remaining granted turns
	version int            // bumped on registration; used by search caches
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		byName: make(map[string]*toolEntry),
		grants: make(map[string]int),
	}
}

// Register adds a core tool (always visible and callable). A later
// registration with the same name replaces the earlier one.
func (r *Registry) Register(t Tool) {
	r.register(t, true, false)
}

// RegisterDeferred adds a tool to the locked-function library (unlock
// discovery mode). A deferred tool is never placed in the provider tools
// array; it is discoverable through the BM25 search tool (name + one-line
// description fed back on a match) and its full schema is delivered on demand
// by unlock_tool. Registering a tool deferred does not grant it: the model
// must activate it first with unlock_tool.
func (r *Registry) RegisterDeferred(t Tool) {
	r.register(t, false, true)
}

func (r *Registry) register(t Tool, core, deferred bool) {
	name := t.Name()
	if _, exists := r.byName[name]; !exists {
		r.order = append(r.order, name)
	}
	r.byName[name] = &toolEntry{tool: t, core: core, deferred: deferred}
	r.version++
}

// Get returns a callable tool by name. Deferred tools are only callable while
// an active unlock grant exists; locked deferred tools report ok=false so the
// caller can surface the "tool is locked" error.
func (r *Registry) Get(name string) (Tool, bool) {
	entry, ok := r.byName[name]
	if !ok {
		return nil, false
	}
	if entry.deferred && r.grants[name] <= 0 {
		return nil, false
	}
	return entry.tool, true
}

// HasRegistered reports whether a tool name is present in the registry,
// including deferred tools that are currently locked.
func (r *Registry) HasRegistered(name string) bool {
	_, ok := r.byName[name]
	return ok
}

// IsCoreTool reports whether name is registered as a core (always callable)
// tool.
func (r *Registry) IsCoreTool(name string) bool {
	entry, ok := r.byName[name]
	return ok && entry.core
}

// IsDeferred reports whether name is registered as a deferred (locked) tool.
// The answer is a property of registration, not of the current grant.
func (r *Registry) IsDeferred(name string) bool {
	entry, ok := r.byName[name]
	return ok && entry.deferred
}

// IsDeferredLocked reports whether name is a deferred tool with no active
// grant.
func (r *Registry) IsDeferredLocked(name string) bool {
	entry, ok := r.byName[name]
	return ok && entry.deferred && r.grants[name] <= 0
}

// DeferredSchema returns the deferred tool registered under name so unlock_tool
// can read its description and parameter schema. It reports ok=false for core
// tools and unknown names.
func (r *Registry) DeferredSchema(name string) (Tool, bool) {
	entry, ok := r.byName[name]
	if !ok || !entry.deferred {
		return nil, false
	}
	return entry.tool, true
}

// GrantTools activates deferred tools for ttl turns. Granting a core tool or an
// unknown name is a no-op. Grants are the only state touched by unlock/lock:
// deferred tools never enter the provider tools array, so the model-visible
// prefix stays byte-stable across lock/unlock cycles.
func (r *Registry) GrantTools(names []string, ttl int) {
	for _, name := range names {
		if entry, ok := r.byName[name]; ok && entry.deferred {
			r.grants[name] = ttl
		}
	}
}

// RevokeAllGrants clears every unlock grant.
func (r *Registry) RevokeAllGrants() {
	clear(r.grants)
}

// TickTTL decrements the remaining turns of every active grant, dropping a
// grant once it reaches zero. It is called once per conversational turn.
func (r *Registry) TickTTL() {
	for name, ttl := range r.grants {
		if ttl <= 1 {
			delete(r.grants, name)
			continue
		}
		r.grants[name] = ttl - 1
	}
}

// Version returns the registry version. It changes whenever a tool is
// registered, so search caches can rebuild only when the library changed.
func (r *Registry) Version() int {
	return r.version
}

// DeferredToolCatalog returns every deferred tool as an ordered name +
// description list. The result is independent of grants so the search corpus
// stays stable across lock/unlock cycles.
func (r *Registry) DeferredToolCatalog() []ToolSearchResult {
	names := make([]string, 0, len(r.byName))
	for name, entry := range r.byName {
		if entry.deferred {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	docs := make([]ToolSearchResult, 0, len(names))
	for _, name := range names {
		entry := r.byName[name]
		docs = append(docs, ToolSearchResult{
			Name:        entry.tool.Name(),
			Description: trimSpace(entry.tool.Description()),
		})
	}
	return docs
}

// DeferredCount reports how many deferred (locked) tools are registered.
func (r *Registry) DeferredCount() int {
	count := 0
	for _, entry := range r.byName {
		if entry.deferred {
			count++
		}
	}
	return count
}

// Definitions returns the schemas of the currently declared tools in
// registration order. Deferred tools are catalog-only in unlock mode: their
// full schemas are delivered by unlock_tool, never injected into the provider
// tools array.
func (r *Registry) Definitions() []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(r.order))
	for _, name := range r.order {
		entry := r.byName[name]
		if entry.deferred {
			continue
		}
		defs = append(defs, llm.ToolDef{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        entry.tool.Name(),
				Description: entry.tool.Description(),
				Parameters:  entry.tool.Parameters(),
			},
		})
	}
	return defs
}

// Names returns the currently callable tool names (core tools plus deferred
// tools with an active grant), sorted. Locked deferred names are omitted.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.order))
	for _, name := range r.order {
		entry := r.byName[name]
		if entry.deferred && r.grants[name] <= 0 {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Execute dispatches a tool call whose arguments arrive as a raw JSON string.
func (r *Registry) Execute(ctx context.Context, name, argsJSON string) *Result {
	args := map[string]any{}
	if trimmed := trimSpace(argsJSON); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
			return Fail(fmt.Sprintf("invalid arguments JSON: %v", err))
		}
	}
	return r.ExecuteArgs(ctx, name, args)
}

// ExecuteArgs dispatches a tool call with already-decoded arguments. It is the
// path used both by Execute and by dynamic_call (which forwards the arguments
// object of a locked function).
func (r *Registry) ExecuteArgs(ctx context.Context, name string, args map[string]any) *Result {
	entry, ok := r.byName[name]
	if !ok {
		return Fail(fmt.Sprintf("unknown tool %q", name))
	}
	if entry.deferred && r.grants[name] <= 0 {
		return Fail(fmt.Sprintf(
			"tool %q is locked: no active unlock grant. Call %q to activate it and receive its full parameter schema, then retry.",
			name, UnlockToolName,
		))
	}
	if args == nil {
		args = map[string]any{}
	}
	return entry.tool.Execute(ctx, args)
}

// --- argument helpers -------------------------------------------------------

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// stringArg decodes args[key] as a string.
func stringArg(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// intArg decodes args[key] as an int with a fallback default.
func intArg(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case string:
		var parsed int
		if _, err := fmt.Sscanf(v, "%d", &parsed); err == nil {
			return parsed
		}
	}
	return def
}

// boolArg decodes args[key] as a bool.
func boolArg(args map[string]any, key string) bool {
	v, ok := args[key].(bool)
	return ok && v
}

// boolArgOr decodes args[key] as a bool, keeping def when the key is absent or
// not a bool. It is the default-aware counterpart of boolArg, which cannot tell
// an omitted key from an explicit false.
func boolArgOr(args map[string]any, key string, def bool) bool {
	v, ok := args[key].(bool)
	if !ok {
		return def
	}
	return v
}

// hasArg reports whether key is present (even with a null value).
func hasArg(args map[string]any, key string) bool {
	_, ok := args[key]
	return ok
}
