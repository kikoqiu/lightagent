package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// unlockRecordTag wraps every full schema delivered by unlock_tool. Its id
// attribute makes each record addressable inside the conversation, so both the
// runtime and the model can tell whether the schema for a given tool is still
// present in the visible context. When it is, re-unlocking only refreshes the
// grant and points back at the existing record instead of resending the schema.
const unlockRecordTag = "unlock_schema"

func unlockRecordOpen(name string) string {
	return fmt.Sprintf("<%s id=%q>", unlockRecordTag, name)
}

func unlockRecordClose() string {
	return fmt.Sprintf("</%s>", unlockRecordTag)
}

// UnlockRecordReference renders a human-readable pointer to the in-context
// schema record for name.
func UnlockRecordReference(name string) string {
	return unlockRecordOpen(name) + "..." + unlockRecordClose()
}

// xmlEscapeText escapes a string for safe embedding as XML element text or an
// attribute value.
func xmlEscapeText(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return replacer.Replace(s)
}

// formatCDATABlock wraps s in an XML CDATA section, splitting any accidental
// "]]>" terminator so the payload always stays well-formed.
func formatCDATABlock(s string) string {
	body := strings.ReplaceAll(s, "]]>", "]]]]><![CDATA[>")
	return "<![CDATA[" + body + "]]>"
}

// toolSchemaXML renders one full tool schema into a normalized XML
// <tools>/<tool>/<description>/<parameters> block. The JSON parameter schema is
// preserved verbatim inside a CDATA section.
func toolSchemaXML(name, description string, params map[string]any) (string, error) {
	if params == nil {
		params = map[string]any{"type": "object"}
	}
	// Encode without HTML escaping so the CDATA payload matches the real
	// parameter schema byte-for-byte.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(params); err != nil {
		return "", err
	}
	paramsJSON := strings.TrimSuffix(buf.String(), "\n")

	var b strings.Builder
	b.WriteString("<tools>\n")
	b.WriteString("  <tool name=\"")
	b.WriteString(xmlEscapeText(name))
	b.WriteString("\">\n")
	b.WriteString("    <description>")
	b.WriteString(xmlEscapeText(description))
	b.WriteString("</description>\n")
	b.WriteString("    <parameters>\n")
	b.WriteString(formatCDATABlock(paramsJSON))
	b.WriteString("\n    </parameters>\n")
	b.WriteString("  </tool>\n")
	b.WriteString("</tools>")
	return b.String(), nil
}

// UnlockTool activates unlock-mode (deferred) tools on demand.
//
// Model flow:
//  1. locked functions are never placed in the provider tools array; the model
//     finds their exact names through the BM25 discovery search;
//  2. the model calls unlock_tool(name) to activate the tool for ttl turns and
//     receive its full parameter schema as a normalized XML <tools> block;
//  3. the model invokes the tool through dynamic_call with name + a JSON
//     object built from the delivered schema;
//  4. if the grant has expired, execution returns a "tool is locked" error and
//     the model self-heals by calling unlock_tool again.
type UnlockTool struct {
	registry *Registry
	ttl      int
}

// NewUnlockTool creates the unlock_tool control-plane tool.
func NewUnlockTool(r *Registry, ttl int) *UnlockTool {
	return &UnlockTool{registry: r, ttl: ttl}
}

func (t *UnlockTool) Name() string {
	return UnlockToolName
}

func (t *UnlockTool) Description() string {
	return "Activate a locked function so it can be invoked through dynamic_call. " +
		"Locked functions are not in your native tool list, find their exact names with the discovery search tool. " +
		"Calling unlock_tool(name) grants temporary execution access for a limited number of turns and returns the function's complete parameter schema as a standardized <tools> XML definition. " +
		"After unlocking, call the function via dynamic_call. Calling a locked function without activation returns an error."
}

func (t *UnlockTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Exact name of the function to unlock, as returned by the discovery search.",
			},
		},
		"required": []string{"name"},
	}
}

func (t *UnlockTool) Execute(ctx context.Context, args map[string]any) *Result {
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return Fail("Missing or invalid 'name' argument. Must be the exact name of a locked function.")
	}

	reg := t.registry
	if reg == nil || !reg.HasRegistered(name) {
		return Fail(fmt.Sprintf(
			"tool %q does not exist in the registry. Run a discovery search (%s) to find the exact locked function name, then retry with that exact name.",
			name, BM25SearchToolName,
		))
	}
	if reg.IsCoreTool(name) {
		return Silent(fmt.Sprintf(
			"tool %q is a native always-available tool; it does not need to be unlocked. Its schema is already provided.",
			name,
		))
	}
	if !reg.IsDeferred(name) {
		return Fail(fmt.Sprintf("tool %q is not a locked function; call it directly.", name))
	}

	reg.GrantTools([]string{name}, t.ttl)

	tool, ok := reg.DeferredSchema(name)
	if !ok {
		return Fail(fmt.Sprintf("tool %q could not be unlocked: schema unavailable", name))
	}

	// The full schema record is still inside the model's visible context:
	// refresh the grant only and reference the existing record instead of
	// resending the schema.
	if lookup := UnlockLookupFrom(ctx); lookup != nil && lookup(name) {
		return Silent(fmt.Sprintf(
			"tool %q is now UNLOCKED for %d turns. Its full schema is already present earlier in this conversation as %s; build a JSON object from those parameters and pass it to %s.",
			name, t.ttl, UnlockRecordReference(name), DynamicCallToolName,
		))
	}

	toolsBlock, err := toolSchemaXML(name, tool.Description(), tool.Parameters())
	if err != nil {
		return Fail("Failed to format tool schema: " + err.Error())
	}

	record := unlockRecordOpen(name) + "\n" + toolsBlock + "\n" + unlockRecordClose()
	msg := fmt.Sprintf(
		"Tool %q is now UNLOCKED for %d turns. Its complete definition is delivered below as a standardized XML <tools> block:\n\n%s\n\n"+
			"To call it, use %s with name set to %q and arguments set to a JSON object built from the <parameters> schema above — do not attempt a direct tool_use for this tool.",
		name, t.ttl, record, DynamicCallToolName, name,
	)
	return Silent(msg)
}
