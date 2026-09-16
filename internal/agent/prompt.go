package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"lightagent/internal/tools"
)

// DefaultSystemPrompt returns the built-in system prompt. It is intentionally
// concise: lightagent is a small, general-purpose coding/runtime assistant.
// It holds no host-specific state: the environment line is added per run by
// RuntimeInfo(), so a prompt exported to agent.md stays valid everywhere.
func DefaultSystemPrompt() string {
	return `You are Light Agent, a concise and capable command-line assistant.
You help the user to fulfill their request using the available tools.
		
## Guidelines:
- Work iteratively step-by-step: inspect the environment or files before acting, then act.
- Prefer the dedicated file tools (read_file_lines, write_file, edit_file) for file work.
- Use exec_command to run scripts. Its language parameter selects the script language (the advertised values list what this machine supports; the default is the host shell).
- Use available MCP if needed.
- Use git commands to manage complex project if it's available.
- Keep responses focused and avoid unnecessary verbosity.
- When a task is complete, give a short summary of what you did.
- Think and reply in user's preferred language.

## Rules:
- Describe the action you are taking when you make a tool call.
- Careful, follow the line count limit of the write_file tool.`
}

// RuntimeInfo returns the environment line the agent appends after its base
// prompt: the platform the running binary targets. It is generated per run, so
// the prompt template (and any agent.md exported from it) never pins another
// machine's platform.
func RuntimeInfo() string {
	return "Runtime: " + runtime.GOOS + "/" + runtime.GOARCH + "."
}

// DirectoryListing renders the current-directory section injected into the
// system prompt. The first line is the absolute working directory and the second
// a legend for the section format; the rest are its direct children, one per
// line, with subdirectories listed first and annotated with their own
// direct-child count in brackets:
//
//	working directory: /path/to/dir
//	 dir[children count] / file
//	subdir-a[3]
//	subdir-b[0]
//	file1
//	file2
//
// It returns "" when dir cannot be read.
func DirectoryListing(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	dirs := make([]string, 0, len(entries))
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			files = append(files, name)
			continue
		}
		count := 0
		if children, cerr := os.ReadDir(filepath.Join(dir, name)); cerr == nil {
			count = len(children)
		}
		dirs = append(dirs, fmt.Sprintf("%s[%d]", name, count))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "working directory: %s", dir)
	b.WriteString("\n dir[children count] / file")
	for _, line := range dirs {
		b.WriteString("\n")
		b.WriteString(line)
	}
	for _, line := range files {
		b.WriteString("\n")
		b.WriteString(line)
	}
	return b.String()
}

// ToolUnlockRule renders the static system-prompt rule that explains the
// MCP unlock discovery mechanism.
// It only describes the mechanism and the BM25 discovery search tool; it never
// enumerates which functions are currently locked, so the rendered text stays
// byte-identical across lock/unlock cycles and the startup context stays small
// regardless of how many functions the locked library holds.
func ToolUnlockRule() string {
	return fmt.Sprintf(
		`**Tool Discovery & Unlock** - Some capabilities are contributed by MCP servers as locked functions: they are NOT part of your native tool list and are NOT enumerated here. Locked functions never get execution access until you unlock them. When you need a capability that is not in your native tool list, first discover which locked function matches: search with %[3]q — matches return the exact function names and one-line descriptions as their result. Then activate the exact function with %[1]q: that call grants temporary execution access and returns the function's complete definition as a standardized XML <tools> block. If that block already exists earlier in this conversation, the activation reply points to it instead of resending the schema. After activation, invoke the function through the %[2]q tool with name set to the function's exact name and arguments set to a JSON object built from the delivered <parameters> schema — never emit a direct tool_use for a locked function, because your output template only allows calls to tools declared in your native tool list. Calling a function without an active grant returns a "tool is locked" error - when you see it, call %[1]q first, then retry via %[2]q. Grants expire after a limited number of turns; re-activate with %[1]q when that happens.`,
		tools.UnlockToolName,
		tools.DynamicCallToolName,
		tools.BM25SearchToolName,
	)
}

// MCPServerInfo is one server's "MCP global info" injected into the system
// prompt: the configured server name, its contributed tool count, the
// availability clause, and the information the server returned in its MCP
// initialize result. It never lists the server's functions.
type MCPServerInfo struct {
	// Server is the configured server name (the key in tools.mcp.servers).
	Server    string
	ToolCount int

	// The following are reported by the server in its initialize result.
	ServerName    string // serverInfo.name
	ServerTitle   string // serverInfo.title
	ServerVersion string // serverInfo.version
	Instructions  string // initialize result "instructions", when present
}

// mcpServerInfoLine renders one server's global-info line: the original
// project's sentence (configured name + tool count + availability) followed by
// the information the server reported about itself. Function names are never
// listed.
func mcpServerInfoLine(info MCPServerInfo) string {
	availability := "registered as locked tools; discover their exact function names with the tool discovery search, " +
		"then unlock each with " + tools.UnlockToolName + " and call it via " + tools.DynamicCallToolName

	var b strings.Builder
	fmt.Fprintf(&b, "MCP server `%s` is connected. It contributes %d tool(s), currently %s.",
		info.Server, info.ToolCount, availability)
	if reported := reportedServerInfo(info); reported != "" {
		b.WriteString("\nReported by: ")
		b.WriteString(reported)
		b.WriteString(".")
	}
	if instructions := strings.TrimSpace(info.Instructions); instructions != "" {
		b.WriteString("\nServer instructions: ")
		b.WriteString(instructions)
	}
	return b.String()
}

// reportedServerInfo renders the identity fields the server returned, or "".
func reportedServerInfo(info MCPServerInfo) string {
	parts := make([]string, 0, 3)
	if info.ServerName != "" {
		parts = append(parts, fmt.Sprintf("name %q", info.ServerName))
	}
	if info.ServerTitle != "" {
		parts = append(parts, fmt.Sprintf("title %q", info.ServerTitle))
	}
	if info.ServerVersion != "" {
		parts = append(parts, "version "+info.ServerVersion)
	}
	return strings.Join(parts, ", ")
}
