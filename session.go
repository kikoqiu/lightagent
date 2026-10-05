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
	"lightagent/internal/lock"
	"lightagent/internal/mcp"
	"lightagent/internal/store"
	"lightagent/internal/tools"
	"lightagent/internal/utils"
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
	// One instance per state directory: the lock is taken before anything else
	// touches the directory and released on the way out. A signal exit and the
	// restart hand-over release it through the shutdown hooks as well, so the
	// replacement process (which resumes this session) can take it over.
	lk, err := lock.Acquire(st.Root())
	if err != nil {
		return err
	}
	defer func() { _ = lk.Release() }()
	onShutdown(func() { _ = lk.Release() })
	// An earlier layout kept the session files directly in .lightagent/; move
	// them into sessions/ so a resumed conversation is still found. A failure
	// is reported but not fatal: the run simply starts without them.
	if err := st.MigrateLegacy(); err != nil {
		info("session migration: %v\n", err)
	}

	client := llm.NewClient(cfg.OpenAI)
	reg := tools.NewRegistry()
	if cfg.Tools.Exec.Enabled {
		engine := tools.NewExecEngine(cfg.Tools.Exec.TimeoutSeconds, cfg.Tools.Exec.WaitSeconds, cfg.Tools.Exec.UseUTF8)
		engine.SetMaxLines(cfg.Tools.Exec.MaxLines, cfg.Tools.Exec.MaxLinesMax)
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
	// The multimedia capability is configured on the model side
	// (openai.media_types) and switched on per tool
	// (tools.upload_media.enabled): only the two together register the uploader,
	// which is also the capability the web mirror's attach control offers.
	mediaCfg, uploadTool := mediaCapability(cfg)
	if uploadTool {
		reg.Register(tools.NewUploadMediaTool(mediaCfg))
	}
	if cfg.Tools.WebFetch.Enabled {
		// The configured mode names are the ones internal/utils reads, so the
		// string travels as it is (tools.webfetch.mode).
		reg.Register(tools.NewWebFetchTool(tools.WebFetchConfig{
			Timeout:        time.Duration(cfg.Tools.WebFetch.TimeoutSeconds) * time.Second,
			Mode:           utils.FetchMode(cfg.Tools.WebFetch.EffectiveMode()),
			MaxLines:       cfg.Tools.WebFetch.MaxLines,
			BrowserPath:    cfg.Tools.WebFetch.BrowserPath,
			UserAgent:      cfg.Tools.WebFetch.UserAgent,
			MaxBytes:       cfg.Tools.WebFetch.MaxBytes,
			AttachEndpoint: cfg.Tools.WebFetch.AttachEndpoint(),
		}))
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
			reg.Register(tools.NewBM25SearchTool(reg, cfg.Tools.Discovery.MaxSearchResults, cfg.Tools.Discovery.MinMatchRate))
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
	// (default yes) and starts fresh when declined. The session resumed is the
	// directory's newest file, not necessarily session.json, so the conversation
	// keeps the name it was saved under. A one-shot run leaves everything
	// untouched unless -r is given.
	latest, lerr := st.Latest()
	if lerr != nil {
		return fmt.Errorf("list sessions: %w", lerr)
	}
	resume := o.resume
	if !resume && interactive && latest != nil {
		if cli.Interactive(os.Stdin) {
			resume = cli.ConfirmLineTo(out, fmt.Sprintf("resume the latest session %q? [Y/n] ", latest.Name), true)
		} else {
			resume = true
		}
	}

	var resumed *store.State
	if resume {
		path := st.Path()
		if latest != nil {
			path = latest.Path
		}
		cur, loadErr := st.LoadPath(path)
		if loadErr != nil {
			return fmt.Errorf("load session: %w", loadErr)
		}
		if cur != nil && len(cur.Messages) > 0 {
			// A session file keeps where each attachment came from, not its
			// bytes (see llm.ReferenceMedia), so the files are read back here:
			// the conversation enters the loop exactly as it left it.
			cur.Messages = llm.ResolveMedia(cur.Messages)
			ag.Load(cur.Messages, cur.Summary)
			// The conversation now carries this file's name: a later save (and
			// the exit prompt) writes it back there, not to the default.
			st.UseFile(path)
			resumed = cur
			info("resumed %d messages from %s\n", len(cur.Messages), path)
		} else {
			info("no saved session to resume; starting fresh\n")
		}
	}

	c := cli.New(ag, st, client.Model(), cfg.UI.Markdown)
	c.SetOutput(out)
	// A signal-triggered exit happens while the editor is still running, so
	// runRaw's deferred terminal restore would never run; the shutdown hook
	// covers that path.
	onShutdown(c.RestoreTerminal)
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
		// The page's /save writes through the CLI, so the browser and the
		// terminal persist exactly the same session. The session commands
		// (/saveas, /load, /list, /rm) reach the same CLI methods too.
		srv.SetSessionSaver(c.SaveSession)
		srv.SetSessionSaverAs(c.SaveSessionAs)
		srv.SetSessionLoader(c.LoadSession)
		srv.SetSessionLister(c.ListSessions)
		srv.SetSessionRemover(c.RemoveSession)
		// The page's Restart button saves the session (the endpoint does that,
		// so the conversation is on disk before anything is given up) and then
		// hands the run over to a fresh process, which resumes it: the same
		// conversation continues there, with the config.json that was just
		// written put in effect. The mirror gives up its listening address on the
		// way out, so the replacement binds it again and open pages reconnect to
		// it on their own.
		srv.SetRestarter(func() error { return restartProgram(srv.ReleaseListener) })
		// Attachments: the accepted media types plus the directory the browser
		// uploads land in (.lightagent/uploads beside the session files). They
		// travel with the next user message.
		srv.SetMedia(mediaCfg, filepath.Join(st.Root(), web.UploadsDirName))
		srv.Start()
		defer srv.Close()
		webPort = srv.Port()
	}

	c.Banner(cfgPath, st.Root(), webHost, webPort)
	if resumed != nil {
		c.ShowHistory(resumed.Messages, resumed.Summary)
	}
	return c.Run(context.Background())
}

// mediaCapability decides the multimedia side of a run from the two settings
// that together make it up: the types the model accepts (openai.media_types)
// turn the capability on, and the tool's own switch (tools.upload_media.enabled)
// decides whether the model gets upload_media as well.
//
// The returned config always carries the accepted types (the web mirror's attach
// control is offered whenever any type is configured); the second value reports
// whether the tool itself must be registered, which needs both sides.
func mediaCapability(cfg *config.Config) (tools.MediaConfig, bool) {
	media := tools.NewMediaConfig(cfg.OpenAI.MediaTypes, cfg.Tools.UploadMedia.MaxBytes)
	return media, media.Enabled() && cfg.Tools.UploadMedia.Enabled
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
	// Bring an older layout's session files under sessions/ as well, so
	// `sessions list` shows them (best effort; the run holds no lock here).
	_ = st.MigrateLegacy()

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
