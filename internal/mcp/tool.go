package mcp

import (
	"context"
	"fmt"
	"strings"

	"lightagent/internal/tools"
)

// MCPTool adapts one MCP server tool to the lightagent tools.Tool interface.
// The model-facing name is mcp_<server>_<tool> (sanitized); the description and
// parameter schema come straight from the server.
type MCPTool struct {
	manager *Manager
	server  string
	tool    Tool
	name    string
}

// NewTool wraps a discovered server tool.
func NewTool(manager *Manager, st ServerTool) *MCPTool {
	return &MCPTool{
		manager: manager,
		server:  st.Server,
		tool:    st.Tool,
		name:    toolID(st.Server, st.Tool.Name),
	}
}

func (t *MCPTool) Name() string { return t.name }

func (t *MCPTool) Description() string {
	if desc := strings.TrimSpace(t.tool.Description); desc != "" {
		return desc
	}
	return fmt.Sprintf("MCP tool %q provided by server %q.", t.tool.Name, t.server)
}

func (t *MCPTool) Parameters() map[string]any {
	if t.tool.InputSchema == nil {
		return map[string]any{"type": "object"}
	}
	return t.tool.InputSchema
}

func (t *MCPTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	text, isError, err := t.manager.CallTool(ctx, t.server, t.tool.Name, args)
	if err != nil {
		return tools.Fail(fmt.Sprintf("MCP tool %q failed: %v", t.tool.Name, err))
	}
	if isError {
		if strings.TrimSpace(text) == "" {
			text = fmt.Sprintf("MCP tool %q reported an error.", t.tool.Name)
		}
		return tools.Fail(text)
	}
	return tools.OK(text)
}

// RegisterTools registers every discovered MCP tool as a locked (deferred)
// function. MCP tools always use the find/unlock mechanism, so they are never
// declared in the provider tools array: the model discovers them with the
// search tool and activates them on demand with unlock_tool.
func RegisterTools(reg *tools.Registry, manager *Manager) {
	if reg == nil || manager == nil {
		return
	}
	for _, st := range manager.Tools() {
		reg.RegisterDeferred(NewTool(manager, st))
	}
}

// toolID builds the model-facing tool name: mcp_<server>_<tool>.
func toolID(server, tool string) string {
	return "mcp_" + sanitizeComponent(server) + "_" + sanitizeComponent(tool)
}

// sanitizeComponent normalizes a string so it is safe inside a function
// identifier: lowercase, [a-z0-9_-] only, collapsed underscores, trimmed and
// length-capped.
func sanitizeComponent(s string) string {
	const maxLen = 64

	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	prevUnderscore := false
	for _, r := range s {
		allowed := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if !allowed {
			if !prevUnderscore {
				b.WriteRune('_')
				prevUnderscore = true
			}
			continue
		}
		if r == '_' {
			if prevUnderscore {
				continue
			}
			prevUnderscore = true
		} else {
			prevUnderscore = false
		}
		b.WriteRune(r)
	}
	result := strings.Trim(b.String(), "_")
	if result == "" {
		return "unnamed"
	}
	if len(result) > maxLen {
		result = strings.Trim(result[:maxLen], "_")
	}
	if result == "" {
		return "unnamed"
	}
	return result
}
