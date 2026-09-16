package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lightagent/internal/agent"
	"lightagent/internal/cli"
	"lightagent/internal/config"
	"lightagent/internal/llm"
	"lightagent/internal/mcp"
	"lightagent/internal/store"
	"lightagent/internal/tools"
	"lightagent/internal/web"
)

// runSession wires up the agent and runs either the interactive REPL or a
// one-shot prompt (-p/--prompt).
func runSession(o *options, stdout io.Writer) error {
	if err := applyDir(o); err != nil {
		return err
	}

	out := stdout
	if o.logPath != "" {
		f, err := os.OpenFile(o.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("open log %s: %w", o.logPath, err)
		}
		defer f.Close()
		out = io.MultiWriter(stdout, f)
	}
	// A one-shot JSON run must stay machine-readable, so informational lines are
	// suppressed there.
	info := func(format string, args ...any) {
		if o.json {
			return
		}
		fmt.Fprintf(out, format, args...)
	}

	cfg, cfgPath, created, err := config.LoadFile(o.configPath)
	if err != nil {
		return err
	}
	applyOverrides(cfg, o)

	if o.printCfg {
		return printConfig(out, cfgPath, cfg)
	}
	if created {
		info("created default config at %s\n", cfgPath)
		info("edit it (at least openai.api_key) and run again.\n")
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("%v (config: %s)", err, cfgPath)
	}

	workdir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	st, err := resolveStore(o, workdir)
	if err != nil {
		return err
	}

	client := llm.NewClient(cfg.OpenAI)
	reg := tools.NewRegistry()
	if cfg.Tools.Exec.Enabled {
		engine := tools.NewExecEngine(cfg.Tools.Exec.TimeoutSeconds, cfg.Tools.Exec.WaitSeconds, cfg.Tools.Exec.UseUTF8)
		defer engine.Close()
		reg.Register(tools.NewExecCommandTool(engine))
		reg.Register(tools.NewManageSessionTool(engine))
	}
	fsCfg := tools.FsConfig{
		MaxReadFileSize:  cfg.Tools.ReadFileLines.MaxReadFileSize,
		MaxReadFileLines: cfg.Tools.ReadFileLines.MaxReadFileLines,
		MaxWriteLines:    cfg.Tools.WriteFile.MaxLines,
	}
	if cfg.Tools.ReadFileLines.Enabled {
		reg.Register(tools.NewReadFileLinesTool(fsCfg))
	}
	if cfg.Tools.WriteFile.Enabled {
		reg.Register(tools.NewWriteFileTool(fsCfg))
	}
	if cfg.Tools.EditFile.Enabled {
		reg.Register(tools.NewEditFileTool())
	}
	// MCP servers: connect to every enabled server and register the tools it
	// contributes as locked (deferred) functions. MCP tools always use the
	// find/unlock mechanism: they never enter the provider tools array,
	// discovery only reports their names, and unlock_tool delivers the schema
	// on demand.
	var mcpManager *mcp.Manager
	if cfg.Tools.MCP.Enabled {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		manager, mcpErr := mcp.Connect(ctx, cfg.Tools.MCP)
		cancel()
		mcpManager = manager
		if mcpErr != nil {
			info("mcp: %v\n", mcpErr)
		}
		defer mcpManager.Close()
		if servers := mcpManager.ServerNames(); len(servers) > 0 {
			info("mcp: connected %d server(s): %s\n", len(servers), strings.Join(servers, ", "))
		}
		mcp.RegisterTools(reg, mcpManager)
	}

	// Find/unlock control plane for locked (deferred) functions. It is exposed
	// only when there actually is something to unlock, so no extra
	// control-plane functions are declared to the model otherwise.
	if reg.DeferredCount() > 0 {
		reg.Register(tools.NewUnlockTool(reg, cfg.Tools.Discovery.TTL))
		reg.Register(tools.NewDynamicCallTool(reg))
		if cfg.Tools.Discovery.UseBM25 {
			reg.Register(tools.NewBM25SearchTool(reg, cfg.Tools.Discovery.MaxSearchResults))
		}
	}

	ag := agent.New(cfg, client, reg, agent.NewBus())
	if mcpManager != nil {
		ag.SetMCPServers(mcpServerInfo(mcpManager))
	}
	if o.result != nil {
		ag.SetToolResultsVisible(*o.result)
	}

	interactive := o.prompt == ""

	// Resume handling: -r always resumes. The interactive REPL otherwise asks
	// (default yes) and archives when declined. A one-shot run leaves an
	// existing session untouched unless -r is given.
	resume := o.resume
	if !resume && interactive && st.Exists() {
		if cli.Interactive(os.Stdin) {
			resume = cli.ConfirmLineTo(out, "a saved session exists for this directory; resume it? [Y/n] ", true)
		} else {
			resume = true
		}
	}

	var resumed *store.State
	if resume {
		cur, loadErr := st.Load()
		if loadErr != nil {
			return fmt.Errorf("load session: %w", loadErr)
		}
		if cur != nil && len(cur.Messages) > 0 {
			ag.Load(cur.Messages, cur.Summary)
			resumed = cur
			info("resumed %d messages from %s\n", len(cur.Messages), st.Path())
		} else {
			info("no saved session to resume; starting fresh\n")
		}
	} else if interactive {
		if backup, aerr := st.Archive(time.Now()); aerr != nil {
			return fmt.Errorf("archive previous session: %w", aerr)
		} else if backup != "" {
			info("archived previous session to %s\n", backup)
		}
	}

	c := cli.New(ag, st, client.Model(), cfg.UI.Markdown)
	c.SetOutput(out)
	switch {
	case o.save:
		c.SetSaveMode(cli.SaveAlways)
	case o.noSave:
		c.SetSaveMode(cli.SaveNever)
	case !interactive:
		// One-shot runs are script-friendly: never prompt to save.
		c.SetSaveMode(cli.SaveNever)
	}

	if !interactive {
		text := o.prompt
		if text == "-" {
			data, readErr := io.ReadAll(os.Stdin)
			if readErr != nil {
				return fmt.Errorf("read prompt from stdin: %w", readErr)
			}
			text = strings.TrimSpace(string(data))
		}
		if strings.TrimSpace(text) == "" {
			return errors.New("empty prompt")
		}
		if resumed != nil && !o.json {
			c.ShowHistory(resumed.Messages, resumed.Summary)
		}
		return c.RunOnce(context.Background(), text, o.json, o.quiet)
	}

	webHost := cfg.Web.Host
	webPort := 0
	if cfg.WebEnabled() {
		srv, werr := web.New(ag, webHost, cfg.Web.Port, cfg.UI.Markdown)
		if werr != nil {
			return fmt.Errorf("start web service: %w", werr)
		}
		// The page can edit config.json through /api/config. A save is written
		// to the file and only takes effect on the next start.
		srv.SetConfigPath(cfgPath)
		// The login is a salted digest the browser computes, so the password and
		// its public salt are all the mirror needs.
		srv.SetPassword(cfg.Web.Password, cfg.Web.PasswordSalt)
		srv.Start()
		defer srv.Close()
		webPort = srv.Port()
	}

	c.Banner(cfgPath, st.Dir(), webHost, webPort)
	if resumed != nil {
		c.ShowHistory(resumed.Messages, resumed.Summary)
	}
	return c.Run(context.Background())
}

// resolveStore builds the session store, honouring --session.
func resolveStore(o *options, workdir string) (*store.Store, error) {
	if strings.TrimSpace(o.session) != "" {
		return store.NewFile(o.session)
	}
	return store.New(workdir)
}

// applyOverrides layers command-line overrides on top of the loaded config.
func applyOverrides(cfg *config.Config, o *options) {
	if o.model != "" {
		cfg.OpenAI.Model = o.model
	}
	if o.apiBase != "" {
		cfg.OpenAI.APIBase = o.apiBase
	}
	if o.stream != nil {
		cfg.OpenAI.Stream = *o.stream
	}
	if o.timeout >= 0 {
		cfg.OpenAI.TimeoutSec = o.timeout
	}
	if o.markdown != nil {
		cfg.UI.Markdown = *o.markdown
	}
	if o.webHost != "" {
		cfg.Web.Host = o.webHost
	}
	switch {
	case o.noWeb:
		cfg.Web.Port = 0
	case o.webPort >= 0:
		cfg.Web.Port = o.webPort
	}
}

// printConfig writes the effective configuration (api key masked).
func printConfig(out io.Writer, path string, cfg *config.Config) error {
	data, err := json.MarshalIndent(cfg.MaskSecrets(), "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "# config file: %s\n", path)
	if _, err := out.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

// runSessions implements the `sessions` command.
func runSessions(o *options, args []string, out io.Writer) error {
	if err := applyDir(o); err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("usage: lightagent sessions <list|show|prune>")
	}
	workdir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	st, err := resolveStore(o, workdir)
	if err != nil {
		return err
	}

	switch args[0] {
	case "list", "ls":
		return sessionsList(st, out)
	case "show":
		return sessionsShow(st, args[1:], out)
	case "prune":
		return sessionsPrune(st, args[1:], out)
	default:
		return fmt.Errorf("sessions: unknown action %q (want list|show|prune)", args[0])
	}
}

// sessionsList prints every session file in the state directory.
func sessionsList(st *store.Store, out io.Writer) error {
	infos, err := st.List()
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		fmt.Fprintf(out, "no sessions in %s\n", st.Dir())
		return nil
	}
	fmt.Fprintf(out, "sessions in %s:\n", st.Dir())
	for _, info := range infos {
		marker := " "
		kind := "archived"
		if info.Current {
			marker = "*"
			kind = "current"
		}
		extra := ""
		if info.Model != "" {
			extra = "  model=" + info.Model
		}
		if info.HasSummary {
			extra += "  summary"
		}
		fmt.Fprintf(out, "%s %-34s %5d msgs  %s  %-8s%s\n",
			marker, info.Name, info.Messages, info.ModTime.Format("2006-01-02 15:04"), kind, extra)
	}
	return nil
}

// sessionsShow prints a saved conversation.
func sessionsShow(st *store.Store, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sessions show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("file", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := resolveSessionPath(st, *name)
	state, err := st.LoadPath(path)
	if err != nil {
		return err
	}
	if state == nil {
		return fmt.Errorf("no session file at %s", path)
	}
	fmt.Fprintf(out, "# %s (%d messages", path, len(state.Messages))
	if state.Model != "" {
		fmt.Fprintf(out, ", model=%s", state.Model)
	}
	if !state.UpdatedAt.IsZero() {
		fmt.Fprintf(out, ", updated %s", state.UpdatedAt.Format(time.RFC3339))
	}
	fmt.Fprintln(out, ")")
	if strings.TrimSpace(state.Summary) != "" {
		fmt.Fprintf(out, "[summary] %s\n", state.Summary)
	}
	for _, m := range state.Messages {
		switch m.Role {
		case "user":
			fmt.Fprintf(out, "> %s\n", m.Content)
		case "assistant":
			if strings.TrimSpace(m.Content) != "" {
				fmt.Fprintf(out, "agent: %s\n", m.Content)
			}
		}
	}
	return nil
}

// sessionsPrune deletes old archives (or one explicit file).
func sessionsPrune(st *store.Store, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sessions prune", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	keep := fs.Int("keep", 5, "")
	all := fs.Bool("all", false, "")
	file := fs.String("file", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if strings.TrimSpace(*file) != "" {
		path := resolveSessionPath(st, *file)
		if err := st.Remove(path); err != nil {
			return err
		}
		fmt.Fprintf(out, "removed %s\n", path)
		return nil
	}

	keepN := *keep
	if *all {
		keepN = 0
	}
	removed, err := st.PruneArchives(keepN)
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		fmt.Fprintln(out, "nothing to prune")
		return nil
	}
	for _, p := range removed {
		fmt.Fprintf(out, "removed %s\n", p)
	}
	fmt.Fprintf(out, "pruned %d archive(s), kept the newest %d\n", len(removed), keepN)
	return nil
}

// resolveSessionPath maps a --file value to a path. An empty name means the
// current session; a bare file name is looked up in the state directory.
func resolveSessionPath(st *store.Store, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return st.Path()
	}
	if filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) {
		return name
	}
	return filepath.Join(st.Dir(), name)
}

// runCompletion prints a shell completion script.
func runCompletion(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: lightagent completion <bash|zsh|powershell>")
	}
	switch args[0] {
	case "bash":
		fmt.Fprint(out, bashCompletion)
	case "zsh":
		fmt.Fprint(out, zshCompletion)
	case "powershell", "pwsh":
		fmt.Fprint(out, powershellCompletion)
	default:
		return fmt.Errorf("completion: unsupported shell %q (want bash|zsh|powershell)", args[0])
	}
	return nil
}

// mcpServerInfo builds the per-server system-prompt notes ("MCP global info")
// from the connected MCP servers: configured server name, contributed tool
// count, and the information each server reported in its initialize result
// (serverInfo + instructions). Function names are never included.
func mcpServerInfo(manager *mcp.Manager) []agent.MCPServerInfo {
	counts := make(map[string]int)
	for _, st := range manager.Tools() {
		counts[st.Server]++
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	infos := make([]agent.MCPServerInfo, 0, len(names))
	for _, name := range names {
		info := agent.MCPServerInfo{Server: name, ToolCount: counts[name]}
		if reported, ok := manager.ServerInfo(name); ok {
			info.ServerName = reported.Name
			info.ServerTitle = reported.Title
			info.ServerVersion = reported.Version
			info.Instructions = reported.Instructions
		}
		infos = append(infos, info)
	}
	return infos
}
