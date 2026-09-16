package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"lightagent/internal/llm"
)

// MCP tool-discovery (unlock mode) control plane:
//
//   - tool_search_tool_bm25 finds locked functions by natural-language query
//     and only reports their exact names and one-line descriptions;
//   - unlock_tool activates one function, writing a TTL-bounded execution
//     grant and delivering its full parameter schema as a standardized XML
//     <tools> block;
//   - dynamic_call (see dynamic_call.go) invokes an activated function
//     indirectly (some model templates drop tool_calls whose name is not in the
//     declared tools list).
const (
	// BM25SearchToolName is the natural-language discovery search tool.
	BM25SearchToolName = "tool_search_tool_bm25"
	// UnlockToolName is the control-plane tool that activates a locked function.
	UnlockToolName = "unlock_tool"
)

// ToolSearchResult is the discovery search result shown to the model. The
// parameter schema is intentionally omitted: the model receives it later from
// unlock_tool.
type ToolSearchResult struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// UnlockLookup reports whether a tool's full schema is already present in the
// current model-visible message context, so unlock_tool can skip resending it.
type UnlockLookup func(toolName string) bool

type unlockLookupKey struct{}

// WithUnlockLookup attaches a schema-presence lookup to ctx.
func WithUnlockLookup(ctx context.Context, lookup UnlockLookup) context.Context {
	if ctx == nil || lookup == nil {
		return ctx
	}
	return context.WithValue(ctx, unlockLookupKey{}, lookup)
}

// UnlockLookupFrom returns the lookup attached to ctx, or nil when the caller
// did not attach one (falls back to always resending the schema).
func UnlockLookupFrom(ctx context.Context) UnlockLookup {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(unlockLookupKey{}).(UnlockLookup)
	return v
}

// MessagesContainUnlockRecord reports whether any message already carries the
// full schema record for the named tool.
func MessagesContainUnlockRecord(msgs []llm.Message, name string) bool {
	needle := unlockRecordOpen(name)
	for i := range msgs {
		if strings.Contains(msgs[i].Content, needle) {
			return true
		}
	}
	return false
}

// formatUnlockDiscoveryResponse is the unlock-mode search feedback. It only
// reports the matched locked functions and never activates them: activation is
// unlock_tool's job and invocation is dynamic_call's job, so the provider
// tools array stays unchanged.
func formatUnlockDiscoveryResponse(results []ToolSearchResult) *Result {
	if len(results) == 0 {
		return Silent("No locked functions found matching the query.")
	}
	b, err := json.Marshal(results)
	if err != nil {
		return Fail("Failed to format search results: " + err.Error())
	}
	msg := fmt.Sprintf(
		"Found %d matching locked function(s) in the MCP tool library:\n%s\n\n"+
			"The functions above remain LOCKED and were NOT activated (they are not part of your native tool list). "+
			"To use one, call %q with its exact name to receive its full parameter schema and a temporary grant, "+
			"then invoke it through %q with name set to that exact name and arguments set to a JSON object built from the delivered <parameters> schema.",
		len(results), string(b), UnlockToolName, DynamicCallToolName,
	)
	return Silent(msg)
}
