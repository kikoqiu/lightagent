package tools

import (
	"context"
	"strings"
)

// searchDoc is the BM25 corpus document for the locked-function library.
type searchDoc struct {
	Name        string
	Description string
}

// BM25SearchTool is the unlock-mode discovery search tool. A search is
// discovery-only: it feeds back the matched functions' exact names and
// one-line descriptions and never promotes or grants anything, so matched
// functions never enter the provider tools array. Activation stays with
// unlock_tool and invocation with dynamic_call.
type BM25SearchTool struct {
	registry         *Registry
	maxSearchResults int

	// Cache: the lowercased text snapshot, rebuilt only when the registry
	// version changes (new deferred tools registered).
	cachedEngine *bm25Engine[searchDoc]
	cacheVersion int
}

// NewBM25SearchTool creates the unlock-mode BM25 discovery search tool over the
// registry's deferred (locked) function library.
func NewBM25SearchTool(r *Registry, maxSearchResults int) *BM25SearchTool {
	return &BM25SearchTool{registry: r, maxSearchResults: maxSearchResults}
}

func (t *BM25SearchTool) Name() string {
	return BM25SearchToolName
}

func (t *BM25SearchTool) Description() string {
	return "Search the locked MCP function library using a natural-language query describing the action you need. " +
		"Returns the exact names and descriptions " +
		"To use it, activate it with unlock_tool, then invoke it through dynamic_call."
}

func (t *BM25SearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Natural-language description of the capability you need.",
			},
		},
		"required": []string{"query"},
	}
}

func (t *BM25SearchTool) Execute(_ context.Context, args map[string]any) *Result {
	query, ok := args["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		// An empty query would match the whole library and bloat the context.
		return Fail("Missing or invalid 'query' argument. Must be a non-empty string.")
	}

	engine := t.getOrBuildEngine()
	if engine == nil {
		return Silent("No locked functions found matching the query.")
	}

	ranked := engine.search(query, t.maxSearchResults)
	if len(ranked) == 0 {
		return Silent("No locked functions found matching the query.")
	}

	results := make([]ToolSearchResult, len(ranked))
	for i, r := range ranked {
		results[i] = ToolSearchResult{
			Name:        r.Document.Name,
			Description: r.Document.Description,
		}
	}
	return formatUnlockDiscoveryResponse(results)
}

// getOrBuildEngine returns a BM25 engine over a text snapshot of the deferred
// tool library (name + description), rebuilding it only when the registry
// version changed. It returns nil when no deferred tools are registered.
func (t *BM25SearchTool) getOrBuildEngine() *bm25Engine[searchDoc] {
	if t.registry == nil {
		return nil
	}
	version := t.registry.Version()
	if t.cachedEngine != nil && t.cacheVersion == version {
		return t.cachedEngine
	}

	catalog := t.registry.DeferredToolCatalog()
	if len(catalog) == 0 {
		t.cachedEngine = nil
		t.cacheVersion = version
		return nil
	}

	docs := make([]searchDoc, len(catalog))
	for i, d := range catalog {
		docs[i] = searchDoc{Name: d.Name, Description: d.Description}
	}
	t.cachedEngine = newBM25Engine(docs, func(d searchDoc) string {
		return d.Name + " " + d.Description
	})
	t.cacheVersion = version
	return t.cachedEngine
}
