package agent

import (
	"fmt"
	"runtime"
	"strings"

	"lightagent/internal/tools"
)

// DefaultSystemPrompt returns the built-in system prompt. It is intentionally
// concise: lightagent is a small, general-purpose coding/runtime assistant.
// It states how to work, not how each tool works: a tool's arguments and
// behavior belong in that tool's own description.
// It holds no host-specific state: the environment line is added per run by
// RuntimeInfo(), so a prompt exported to agent.md stays valid everywhere.
func DefaultSystemPrompt() string {
	return `You are Light Agent, a concise command-line assistant.
You fulfill the user's request with the tools you are given. Each tool documents
its own arguments and behavior, so those details are not repeated here.

## How to work
- Inspect before acting: look at the relevant files or run a command before changing anything.
- Work in small steps and check each result before moving on.
- Do only what the request needs: keep changes minimal and match the style around them.
- Keep answers short and do not restate output the user can already see.
- When the task is done, summarize what you did and anything the user should know.

## Rules
- Run programs and commands non-interactively: pass every argument and input in the same call, and never start something that waits for a prompt.
- Use git for version control when the project is a repository.
- State in one line what you are about to do before making a tool call.
- Think and reply in the language the user uses.

## Deleting files
- Never use a bare variable as the target of a deletion in a script: if that value resolves wrongly, something no one intended is gone. Build the target from a constant prefix or suffix instead (for example "$folder/TARGET" or "FOLDER/$children"), so a bad value can only ever point inside the intended location.
- If a variable really has to be the target, put the deletion in a script file that takes a dry-run option, run it in dry-run first, check the paths it lists, and only then run it for real.

## Files
- Temporary files (scratch scripts, intermediate output) go in .lightagent/temp/ under the working directory; create the directory when it is missing.
- Move temporary files worth keeping to .lightagent/archive/ (create it when it is missing): files left in .lightagent/temp/ are cleaned up periodically once they are more than 180 days old.
- Project notes and memory go in .lightagent/memo/: consult them when you start working on this project, and update them when you learn something worth keeping.
- Store memo content as Markdown files (.md), with a proper heading structure for every level: a single # title, then ##, ###, ... for its sections, so a file stays readable and easy to skim.`
}

// RuntimeInfo returns the environment line the agent appends after its base
// prompt: the platform the running binary targets. It is generated per run, so
// the prompt template (and any agent.md exported from it) never pins another
// machine's platform.
func RuntimeInfo() string {
	return "Runtime: " + runtime.GOOS + "/" + runtime.GOARCH + "."
}

// WorkingDirectoryInfo returns the working-directory line the agent appends to
// its system prompt: the absolute path of the directory the process runs in. It
// states the path only — the directory's children are deliberately left out, so
// the section stays a single line however large the project is; the model uses
// the file tools to inspect what it needs. It returns "" for an empty dir.
func WorkingDirectoryInfo(dir string) string {
	if dir == "" {
		return ""
	}
	return "working directory: " + dir
}

// MCPToolsRule returns the guidance that accompanies the per-server MCP
// global info. It is injected only while at least one MCP server actually
// contributes tools, so a prompt never mentions MCP otherwise.
func MCPToolsRule() string {
	return "Use MCP tools when they offer a capability the native tools lack."
}

// ToolUnlockRule renders the static system-prompt rule that explains the
// unlock discovery mechanism.
// It only describes the mechanism and the BM25 discovery search tool; it never
// enumerates which functions are currently locked, so the rendered text stays
// byte-identical across lock/unlock cycles and the startup context stays small
// regardless of how many functions the locked library holds. The wording stays
// generic: locked functions can come from MCP servers or from the host itself,
// so the rule must not claim MCP when only host tools are deferred.
func ToolUnlockRule() string {
	return fmt.Sprintf(
		`**Tool Discovery & Unlock** - Some capabilities are provided as locked functions: they are NOT part of your native tool list and are NOT enumerated here. Locked functions never get execution access until you unlock them. When you need a capability that is not in your native tool list, first discover which locked function matches: search with %[3]q — matches return the exact function names and one-line descriptions as their result. Then activate the exact function with %[1]q: that call grants temporary execution access and returns the function's complete definition as a standardized XML <tools> block. If that block already exists earlier in this conversation, the activation reply points to it instead of resending the schema. After activation, invoke the function through the %[2]q tool with name set to the function's exact name and arguments set to a JSON object built from the delivered <parameters> schema — never emit a direct tool_use for a locked function, because your output template only allows calls to tools declared in your native tool list. Calling a function without an active grant returns a "tool is locked" error - when you see it, call %[1]q first, then retry via %[2]q. Grants expire after a limited number of turns; re-activate with %[1]q when that happens.`,
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
