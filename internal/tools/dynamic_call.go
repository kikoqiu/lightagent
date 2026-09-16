package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// DynamicCallToolName is the fixed control-plane tool used to invoke deferred
// (locked) functions without declaring them in the provider tools array. Some
// model templates drop tool_calls whose tool name is not in the declared tools
// list; dynamic_call is itself fully declared, so the model can legally emit it
// while the real target name travels as an ordinary string parameter and the
// target arguments travel as an ordinary JSON object parameter.
const DynamicCallToolName = "dynamic_call"

// DynamicCallTool dispatches a locked-function invocation on behalf of the
// model. Authorization is enforced by the registry's grant gate for the target
// tool, so this tool carries no unlock/TTL state of its own.
type DynamicCallTool struct {
	registry *Registry
}

// NewDynamicCallTool creates the dynamic_call dispatch tool.
func NewDynamicCallTool(r *Registry) *DynamicCallTool {
	return &DynamicCallTool{registry: r}
}

func (t *DynamicCallTool) Name() string {
	return DynamicCallToolName
}

func (t *DynamicCallTool) Description() string {
	return "Invoke a locked function that was activated with unlock_tool. " +
		"Locked functions (from MCP servers) are not in your declared tool list, and your output template may refuse direct tool calls for tools outside that list, so locked functions are called indirectly: " +
		"'name' is the exact locked-function name returned by a discovery search (tool_search_tool_bm25), and 'arguments' is a JSON object that follows that function's unlocked parameter schema. " +
		"Calling a function without an active grant returns a 'tool is locked' error — unlock it first with unlock_tool."
}

func (t *DynamicCallTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Exact name of the locked function to invoke, as returned by a discovery search.",
			},
			"arguments": map[string]any{
				"type":        "object",
				"description": "JSON object with the parameters required by the unlocked tool. Build it from the <parameters> block returned by unlock_tool.",
			},
		},
		"required": []string{"name"},
	}
}

func (t *DynamicCallTool) Execute(ctx context.Context, args map[string]any) *Result {
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return Fail("Missing or invalid 'name' argument. Use the exact name of a locked function returned by a discovery search.")
	}

	reg := t.registry
	if reg == nil || !reg.HasRegistered(name) {
		return Fail(fmt.Sprintf(
			"tool %q does not exist in the registry. Run a discovery search (%s) to find the exact locked function name, then retry.",
			name, BM25SearchToolName,
		))
	}
	if name == DynamicCallToolName || name == UnlockToolName {
		return Fail(fmt.Sprintf("tool %q is a control-plane tool and cannot be invoked through %s.", name, DynamicCallToolName))
	}
	if reg.IsCoreTool(name) {
		return Fail(fmt.Sprintf("tool %q is a native always-available tool; call it directly instead of through %s.", name, DynamicCallToolName))
	}
	if !reg.IsDeferred(name) {
		return Fail(fmt.Sprintf("tool %q is not a locked function; call it directly.", name))
	}

	inner, err := parseDynamicCallArguments(args["arguments"])
	if err != nil {
		return Fail(fmt.Sprintf(
			"invalid 'arguments' for tool %q: %v. 'arguments' must be a JSON object built from the unlocked parameter schema.",
			name, err,
		))
	}

	// Delegate to the registry so the target tool reuses the exact execution
	// path of a direct call, including the unlock grant gate. The result is
	// returned unchanged so the outer tool loop treats it identically to a
	// direct invocation.
	return reg.ExecuteArgs(ctx, name, inner)
}

// parseDynamicCallArguments resolves the model-provided 'arguments' parameter
// into the inner argument map. Its declared schema type is a JSON object, so a
// map[string]any is the normal form; a JSON object string is still accepted for
// backward compatibility with older prompts and direct callers.
func parseDynamicCallArguments(raw any) (map[string]any, error) {
	switch v := raw.(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return v, nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return map[string]any{}, nil
		}
		if stripped := stripJSONCodeFence(s); stripped != "" {
			s = stripped
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(s), &parsed); err != nil {
			return nil, err
		}
		if parsed == nil {
			return map[string]any{}, nil
		}
		return parsed, nil
	default:
		return nil, fmt.Errorf("unexpected type %T", raw)
	}
}

// stripJSONCodeFence removes an optional ```json ... ``` code fence that wraps
// a legacy string-typed arguments payload.
func stripJSONCodeFence(s string) string {
	for _, prefix := range []string{"```json", "```JSON"} {
		if strings.HasPrefix(s, prefix) && strings.HasSuffix(s, "```") {
			inner := strings.TrimSuffix(s[len(prefix):], "```")
			return strings.TrimSpace(inner)
		}
	}
	return ""
}
