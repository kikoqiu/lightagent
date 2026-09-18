// Package config loads and persists the global lightagent configuration.
//
// The config file lives next to the executable (the "program directory") and
// is named config.json. The location can be overridden with the
// LIGHTAGENT_CONFIG environment variable, which is handy during development
// (go run points os.Executable at a temporary build directory).
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"lightagent/internal/passwd"
)

// Config is the root configuration document.
type Config struct {
	OpenAI  OpenAIConfig  `json:"openai"`
	Context ContextConfig `json:"context"`
	Web     WebConfig     `json:"web"`
	Tools   ToolsConfig   `json:"tools"`
	Agent   AgentConfig   `json:"agent"`
	UI      UIConfig      `json:"ui"`
}

// UIConfig controls how the CLI and the web mirror present output.
type UIConfig struct {
	// Markdown renders assistant replies as markdown (headings, emphasis, code,
	// lists, quotes, links). It defaults to true; loading starts from the
	// defaults, so an explicit "markdown": false is required to turn it off.
	Markdown bool `json:"markdown"`
}

// OpenAIConfig describes the OpenAI-compatible endpoint.
type OpenAIConfig struct {
	APIBase     string  `json:"api_base"`
	APIKey      string  `json:"api_key"`
	Model       string  `json:"model"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
	TimeoutSec  int     `json:"timeout_seconds"`
	Stream      bool    `json:"stream"`
	// ExtraBody holds provider-specific request parameters. Its keys are merged
	// into the top-level /chat/completions request body, so a value here
	// overrides the corresponding built-in field (e.g. "temperature").
	ExtraBody map[string]any `json:"extra_body,omitempty"`
}

// ContextConfig controls context-window management.
//
// How much of the newest conversation a compression pass keeps visible is
// derived: a token budget of
// (context_window - openai.max_tokens) divided by 10 (auto) or 20 (manual),
// capped at 3 (auto) or 2 (manual) complete user turns.
type ContextConfig struct {
	ContextWindow         int `json:"context_window"`
	SummarizeTokenPercent int `json:"summarize_token_percent"`
}

// WebConfig controls the optional web mirror service.
type WebConfig struct {
	// Host is the interface to bind. Empty (or omitted) means loopback only.
	// Use "0.0.0.0" (or a concrete local address) to expose the mirror on the
	// network.
	Host string `json:"host"`
	Port int    `json:"port"`
	// Password is the login password of the mirror, kept in the clear: the
	// server derives the salted digest a browser sends from it, and the file is
	// the user's own. To keep the login, use the page's password control (or
	// write it here); set web.password_salt next to it, or let the program
	// generate one on load.
	Password string `json:"password"`
	// PasswordSalt is the public salt for Password: it binds the digest the
	// browser computes to this configuration. The program always writes one when
	// a password is set, and rotating it (by changing the password) invalidates
	// every digest a browser saved.
	PasswordSalt string `json:"password_salt,omitempty"`
}

// ToolsConfig groups per-tool limits/switches.
type ToolsConfig struct {
	Exec          ExecToolConfig      `json:"exec"`
	ReadFileLines FsToolConfig        `json:"read_file_lines"`
	WriteFile     WriteToolConfig     `json:"write_file"`
	EditFile      ToggleToolConfig    `json:"edit_file"`
	Discovery     ToolDiscoveryConfig `json:"discovery"`
	MCP           MCPConfig           `json:"mcp"`
}

// Tool discovery (unlock) mode constants. deferred tools stay locked until
// the model discovers them with the BM25 search tool and activates them with
// unlock_tool.
const (
	// ToolDiscoveryModeUnlock decouples visibility from execution authority.
	ToolDiscoveryModeUnlock = "unlock"

	// defaultDiscoveryMinMatchRate is the share of the search query's keywords a
	// locked function must contain to be reported. It is the built-in
	// tools.discovery.min_match_rate default: half of the query keywords.
	defaultDiscoveryMinMatchRate = 0.5
)

// ToolDiscoveryConfig configures the MCP tool-discovery / unlock control plane.
// When enabled, the agent registers the discovery search tool, unlock_tool and
// dynamic_call, and the system prompt gains the "Tool Discovery & Unlock" rule.
// The locked-function library itself is populated by the host through
// Registry.RegisterDeferred.
type ToolDiscoveryConfig struct {
	Enabled          bool    `json:"enabled"`
	Mode             string  `json:"mode"`
	TTL              int     `json:"ttl"`
	MaxSearchResults int     `json:"max_search_results"`
	MinMatchRate     float64 `json:"min_match_rate"`
	UseBM25          bool    `json:"use_bm25"`
}

// EffectiveMode returns the discovery mode, defaulting to unlock.
func (c ToolDiscoveryConfig) EffectiveMode() string {
	if strings.TrimSpace(c.Mode) == "" {
		return ToolDiscoveryModeUnlock
	}
	return c.Mode
}

// MCP transport types.
const (
	// MCPTransportStdio launches a local process and speaks JSON-RPC over its
	// stdin/stdout (newline-delimited messages).
	MCPTransportStdio = "stdio"
	// MCPTransportHTTP speaks the Streamable HTTP transport (a POST per
	// JSON-RPC message; the response is JSON or an SSE stream).
	MCPTransportHTTP = "http"
	// MCPTransportStreamableHTTP is an alias of MCPTransportHTTP.
	MCPTransportStreamableHTTP = "streamable-http"
	// MCPTransportSSE speaks the legacy HTTP+SSE transport (a long-lived GET
	// event stream plus POSTs to the advertised endpoint).
	MCPTransportSSE = "sse"
)

// MCPConfig configures MCP client connections. Servers are declared in
// Servers; each enabled server is connected on startup and its tools are
// registered on the tool registry. With tools.discovery enabled the tools are
// registered as locked (deferred) functions; otherwise they are registered as
// ordinary core tools.
type MCPConfig struct {
	Enabled bool                       `json:"enabled"`
	Servers map[string]MCPServerConfig `json:"servers"`
}

// MCPServerConfig defines one MCP server connection.
type MCPServerConfig struct {
	// Enabled must be true for the server to be connected.
	Enabled bool `json:"enabled"`
	// Type selects the transport: "stdio", "http" (alias
	// "streamable-http") or "sse". It defaults to stdio when Command is set
	// and to http when only URL is set.
	Type string `json:"type,omitempty"`
	// Command is the executable to launch (stdio transport).
	Command string `json:"command,omitempty"`
	// Args are the command arguments (stdio transport).
	Args []string `json:"args,omitempty"`
	// Env holds extra environment variables for the server process.
	Env map[string]string `json:"env,omitempty"`
	// EnvFile is an optional .env-style file (KEY=value lines) whose entries
	// are merged into the server process environment. Relative paths resolve
	// against the process working directory.
	EnvFile string `json:"env_file,omitempty"`
	// URL is the endpoint for the http / sse transports.
	URL string `json:"url,omitempty"`
	// Headers are extra HTTP headers for the http / sse transports.
	Headers map[string]string `json:"headers,omitempty"`
}

// EffectiveType resolves the transport type for the server.
func (c MCPServerConfig) EffectiveType() string {
	switch strings.ToLower(strings.TrimSpace(c.Type)) {
	case "":
		if strings.TrimSpace(c.Command) != "" {
			return MCPTransportStdio
		}
		if strings.TrimSpace(c.URL) != "" {
			return MCPTransportHTTP
		}
		return MCPTransportStdio
	case MCPTransportStdio:
		return MCPTransportStdio
	case MCPTransportHTTP, MCPTransportStreamableHTTP:
		return MCPTransportHTTP
	case MCPTransportSSE:
		return MCPTransportSSE
	default:
		return strings.ToLower(strings.TrimSpace(c.Type))
	}
}

// DefaultMCPServerName is the placeholder server inserted when
// tools.mcp.servers is empty, so the file always documents where to declare a
// server. It stays disabled and is therefore never connected.
const DefaultMCPServerName = "example"

// defaultMCPServers returns the single disabled stdio server template used to
// seed an empty tools.mcp.servers map.
func defaultMCPServers() map[string]MCPServerConfig {
	return map[string]MCPServerConfig{
		DefaultMCPServerName: {
			Enabled: false,
			Type:    MCPTransportStdio,
			Command: "npx",
			Args:    []string{"-y", "@modelcontextprotocol/server-everything"},
		},
	}
}

// ensureMCPServer inserts the disabled placeholder when no server is declared.
func (c *Config) ensureMCPServer() {
	if len(c.Tools.MCP.Servers) == 0 {
		c.Tools.MCP.Servers = defaultMCPServers()
	}
}

// ExecToolConfig configures exec_command / manage_session.
type ExecToolConfig struct {
	Enabled        bool `json:"enabled"`
	TimeoutSeconds int  `json:"timeout_seconds"`
	WaitSeconds    int  `json:"wait_seconds"`
	// UseUTF8 makes child processes speak UTF-8 instead of the host ANSI code
	// page: on Windows the shell script gains a UTF-8 preamble (console input /
	// output code pages and $OutputEncoding) and PYTHONIOENCODING=utf-8 is
	// exported, so Go forwards the bytes without transcoding. It defaults to
	// true; loading starts from the defaults, so an explicit "use_utf8": false
	// is required to fall back to the legacy ANSI code page conversion.
	UseUTF8 bool `json:"use_utf8"`
}

// FsToolConfig configures the line-oriented reader.
type FsToolConfig struct {
	Enabled          bool `json:"enabled"`
	MaxReadFileSize  int  `json:"max_read_file_size"`
	MaxReadFileLines int  `json:"max_read_file_lines"`
}

// WriteToolConfig configures write_file.
type WriteToolConfig struct {
	Enabled  bool `json:"enabled"`
	MaxLines int  `json:"max_lines"`
	// AutoSplit spreads a write whose payload exceeds max_lines over several
	// calls instead of letting the tool truncate it: the model's own call keeps
	// the first part and the agent appends the remaining parts, each in its own
	// tool round. It defaults to true; loading starts from the defaults, so an
	// explicit "auto_split": false is required to turn it off.
	AutoSplit bool `json:"auto_split"`
}

// ToggleToolConfig is a simple enabled/disabled switch.
type ToggleToolConfig struct {
	Enabled bool `json:"enabled"`
}

// AgentConfig controls the agent loop.
type AgentConfig struct {
	MaxToolIterations int    `json:"max_tool_iterations"`
	SystemPrompt      string `json:"system_prompt"`
	// IncludeWorkingDir injects the current directory listing (the working
	// directory plus its direct children, subdirectories annotated with their
	// own direct-child count) into the system prompt. It defaults to true;
	// loading starts from the defaults, so an explicit false is required to
	// turn it off.
	IncludeWorkingDir bool `json:"include_working_dir"`
	// SummaryInSystemPrompt selects where a request carries the accumulated
	// context summary: false (the default) sends it as the first user message,
	// true appends it to the system prompt (the "CONVERSATION SUMMARY"
	// section). The switch shapes the message list that is sent and nothing
	// else.
	SummaryInSystemPrompt bool `json:"summary_in_system_prompt"`
}

// Default returns the built-in configuration used when no file exists yet.
func Default() *Config {
	return &Config{
		OpenAI: OpenAIConfig{
			APIBase:     "https://api.openai.com/v1",
			Model:       "gpt-4o-mini",
			Temperature: 0.0,
			MaxTokens:   40960,
			TimeoutSec:  4800,
			Stream:      true,
		},
		Context: ContextConfig{
			ContextWindow:         81960,
			SummarizeTokenPercent: 75,
		},
		Web: WebConfig{Host: "127.0.0.1", Port: 0, Password: ""},
		Tools: ToolsConfig{
			Exec:          ExecToolConfig{Enabled: true, TimeoutSeconds: 3600, WaitSeconds: 10, UseUTF8: true},
			ReadFileLines: FsToolConfig{Enabled: true, MaxReadFileSize: 32000, MaxReadFileLines: 200},
			WriteFile:     WriteToolConfig{Enabled: true, MaxLines: 200, AutoSplit: true},
			EditFile:      ToggleToolConfig{Enabled: true},
			Discovery: ToolDiscoveryConfig{
				Enabled:          false,
				Mode:             ToolDiscoveryModeUnlock,
				TTL:              50,
				MaxSearchResults: 10,
				MinMatchRate:     defaultDiscoveryMinMatchRate,
				UseBM25:          true,
			},
		},
		Agent: AgentConfig{MaxToolIterations: 200, IncludeWorkingDir: true, SummaryInSystemPrompt: false},
		UI:    UIConfig{Markdown: true},
	}
}

// Path resolves the config file path: LIGHTAGENT_CONFIG if set, otherwise
// config.json in the executable's directory.
func Path() (string, error) {
	if p := strings.TrimSpace(os.Getenv("LIGHTAGENT_CONFIG")); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	return filepath.Join(filepath.Dir(exe), "config.json"), nil
}

// AgentPromptFileName is the optional system-prompt override file that lives
// next to config.json.
const AgentPromptFileName = "agent.md"

// AgentPromptPath returns the agent.md path for a given config path.
func AgentPromptPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), AgentPromptFileName)
}

// LoadAgentPrompt reads the optional agent.md file. It returns ok=false when the
// file is missing or blank. @include directives are expanded, with relative
// paths resolved against the agent.md directory itself.
func LoadAgentPrompt(configPath string) (prompt string, ok bool, err error) {
	path := AgentPromptPath(configPath)
	data, readErr := os.ReadFile(path)
	if os.IsNotExist(readErr) {
		return "", false, nil
	}
	if readErr != nil {
		return "", false, readErr
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "", false, nil
	}

	expanded, expandErr := expandAgentIncludes(text, path, 0, []string{canonicalPath(path)})
	if expandErr != nil {
		return "", false, expandErr
	}
	text = strings.TrimSpace(expanded)
	if text == "" {
		return "", false, nil
	}
	return text, true, nil
}

// Parse decodes a config document the way LoadFile decodes the on-disk file:
// the built-in defaults are the starting point (so an omitted field keeps its
// default, including the "true unless explicitly false" switches) and the
// zero-value fallbacks are applied afterwards. Fields the program does not know
// are ignored, so a file written by another version still loads.
func Parse(data []byte) (*Config, error) {
	cfg, _, err := decode(data, false)
	return cfg, err
}

// ParseStrict is Parse with unknown fields rejected. A front-end that writes
// the document back (the web mirror's config editor) needs this: a typo such as
// "modle" must be reported instead of being silently dropped on save.
func ParseStrict(data []byte) (*Config, error) {
	cfg, _, err := decode(data, true)
	return cfg, err
}

// decode layers a raw config document over the built-in defaults and applies
// the zero-value fallbacks, so partial files remain usable after new fields are
// added. LoadFile and the config editor therefore share one rule set.
//
// It also reports whether the document changed on the way in (a plaintext web
// password turned into a hash), which is what tells LoadFile to write the file
// back.
func decode(data []byte, strict bool) (*Config, bool, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, false, errors.New("the config document is empty")
	}
	cfg := Default()
	if !strict {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, false, err
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(cfg); err != nil {
			return nil, false, err
		}
		// Anything after the document is a mistake (usually a paste accident),
		// so it is reported instead of ignored.
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return nil, false, errors.New("the config document must be a single JSON object")
		}
	}
	changed, err := cfg.ensureWebSalt()
	if err != nil {
		return nil, false, err
	}
	cfg.applyDefaults()
	return cfg, changed, nil
}

// ensureWebSalt makes sure a password is always paired with a salt and that an
// echoed MaskedSecret is dropped instead of being taken for a password. It
// reports whether the document changed.
//
// The salt is generated here rather than stored by the writer so that editing
// config.json by hand works too: write the password, and the program adds a
// public salt for the browser's digest.
func (c *Config) ensureWebSalt() (bool, error) {
	password := strings.TrimSpace(c.Web.Password)
	if password == MaskedSecret {
		// A masked value came back from a front-end: drop it and keep what the
		// file already holds (the caller restores it before saving).
		c.Web.Password = ""
		return true, nil
	}
	if password == "" {
		if strings.TrimSpace(c.Web.PasswordSalt) != "" {
			// No password: a leftover salt would only confuse the login.
			c.Web.PasswordSalt = ""
			return true, nil
		}
		return false, nil
	}
	if strings.TrimSpace(c.Web.PasswordSalt) != "" {
		return false, nil
	}
	salt, err := passwd.NewSalt()
	if err != nil {
		return false, err
	}
	c.Web.PasswordSalt = salt
	return true, nil
}

// Load reads the config file. When the file does not exist it writes the
// default config and reports created=true so the caller can inform the user.
func Load() (cfg *Config, path string, created bool, err error) {
	return LoadFile("")
}

// LoadFile reads the config at path. An empty path resolves through Path()
// (LIGHTAGENT_CONFIG, else next to the executable). When the file does not
// exist it writes the default config and reports created=true.
func LoadFile(path string) (cfg *Config, resolved string, created bool, err error) {
	if strings.TrimSpace(path) == "" {
		resolved, err = Path()
	} else {
		resolved, err = filepath.Abs(path)
	}
	if err != nil {
		return nil, "", false, err
	}

	data, readErr := os.ReadFile(resolved)
	if os.IsNotExist(readErr) {
		cfg = Default()
		cfg.ensureMCPServer()
		if writeErr := Save(resolved, cfg); writeErr != nil {
			return nil, resolved, false, fmt.Errorf("create default config: %w", writeErr)
		}
		return cfg, resolved, true, nil
	}
	if readErr != nil {
		return nil, resolved, false, fmt.Errorf("read config: %w", readErr)
	}

	// Capture whether the file declared any MCP server before Parse seeds the
	// disabled placeholder.
	var probe struct {
		Tools struct {
			MCP struct {
				Servers map[string]json.RawMessage `json:"servers"`
			} `json:"mcp"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(data, &probe) // a malformed document is reported by Parse
	serversEmpty := len(probe.Tools.MCP.Servers) == 0

	parsed, migrated, err := decode(data, false)
	if err != nil {
		return nil, resolved, false, fmt.Errorf("parse config %s: %w", resolved, err)
	}
	cfg = parsed

	// Keep the file aligned with the program's schema: when it omits fields
	// present in the built-in default (or declares no MCP server at all) write
	// the merged document back. A file that is already complete is left
	// untouched so neither its content nor its modification time changes.
	// A plaintext web password is also written back, hashed.
	if serversEmpty || migrated || fileMissingDefaults(data) {
		if writeErr := Save(resolved, cfg); writeErr != nil {
			return nil, resolved, false, fmt.Errorf("update config defaults: %w", writeErr)
		}
	}

	// A program-directory agent.md overrides any system prompt configured in
	// config.json (which in turn overrides the built-in default). A broken
	// @include is reported rather than silently falling back to the built-in
	// prompt.
	prompt, ok, perr := LoadAgentPrompt(resolved)
	if perr != nil {
		return nil, resolved, false, fmt.Errorf("load %s: %w", AgentPromptPath(resolved), perr)
	}
	if ok {
		cfg.Agent.SystemPrompt = prompt
	}
	return cfg, resolved, false, nil
}

// fileMissingDefaults reports whether the raw config document omits any field
// defined by the built-in default schema.
func fileMissingDefaults(data []byte) bool {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return false // a malformed document is reported by the caller
	}
	raw, err := json.Marshal(Default())
	if err != nil {
		return false
	}
	var def map[string]any
	if err := json.Unmarshal(raw, &def); err != nil {
		return false
	}
	return hasMissingKeys(doc, def)
}

// hasMissingKeys walks the default document and reports whether doc omits any
// of its object keys. A value of an unexpected shape counts as present so a
// user-provided value is never overwritten just for being unusual.
func hasMissingKeys(doc, def any) bool {
	defMap, ok := def.(map[string]any)
	if !ok {
		return false
	}
	docMap, ok := doc.(map[string]any)
	if !ok {
		return false
	}
	for key, defVal := range defMap {
		docVal, exists := docMap[key]
		if !exists {
			return true
		}
		if hasMissingKeys(docVal, defVal) {
			return true
		}
	}
	return false
}

// MaskedSecret is the placeholder that replaces a stored secret when a
// configuration is displayed (--print-config, GET /api/config). A front-end that
// writes the document back sends it unchanged to mean "keep the stored value";
// see the web mirror's config editor.
const MaskedSecret = "***"

// MaskSecrets returns a copy whose secrets are replaced by MaskedSecret, so the
// configuration can be shown without leaking them: the api key and the web
// password. The password salt is deliberately left visible — it is public (the
// page asks for it before signing in) and keeping it makes the document that
// comes back from the editor identical to the one on disk.
//
// The copy shares the nested maps (extra_body, MCP servers) with the receiver:
// that is fine for display, but callers must not mutate them.
func (c *Config) MaskSecrets() *Config {
	clone := *c
	if clone.OpenAI.APIKey != "" {
		clone.OpenAI.APIKey = MaskedSecret
	}
	if clone.Web.Password != "" {
		clone.Web.Password = MaskedSecret
	}
	return &clone
}

// Save writes the config as indented JSON.
func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

// applyDefaults fills zero values with defaults so partial config files remain
// usable after new fields are added.
func (c *Config) applyDefaults() {
	def := Default()
	if c.OpenAI.APIBase == "" {
		c.OpenAI.APIBase = def.OpenAI.APIBase
	}
	if c.OpenAI.Model == "" {
		c.OpenAI.Model = def.OpenAI.Model
	}
	if c.OpenAI.Temperature == 0 {
		c.OpenAI.Temperature = def.OpenAI.Temperature
	}
	if c.OpenAI.MaxTokens <= 0 {
		c.OpenAI.MaxTokens = def.OpenAI.MaxTokens
	}
	// timeout_seconds is an inactivity budget: a negative value is invalid and
	// falls back to the default, while an explicit 0 disables the timeout.
	if c.OpenAI.TimeoutSec < 0 {
		c.OpenAI.TimeoutSec = def.OpenAI.TimeoutSec
	}
	if strings.TrimSpace(c.Web.Host) == "" {
		c.Web.Host = def.Web.Host
	}
	if c.Context.ContextWindow <= 0 {
		c.Context.ContextWindow = def.Context.ContextWindow
	}
	if c.Context.SummarizeTokenPercent <= 0 || c.Context.SummarizeTokenPercent > 100 {
		c.Context.SummarizeTokenPercent = def.Context.SummarizeTokenPercent
	}
	if c.Tools.Exec.TimeoutSeconds <= 0 {
		c.Tools.Exec.TimeoutSeconds = def.Tools.Exec.TimeoutSeconds
	}
	if c.Tools.Exec.WaitSeconds <= 0 {
		c.Tools.Exec.WaitSeconds = def.Tools.Exec.WaitSeconds
	}
	if c.Tools.ReadFileLines.MaxReadFileSize <= 0 {
		c.Tools.ReadFileLines.MaxReadFileSize = def.Tools.ReadFileLines.MaxReadFileSize
	}
	if c.Tools.ReadFileLines.MaxReadFileLines <= 0 {
		c.Tools.ReadFileLines.MaxReadFileLines = def.Tools.ReadFileLines.MaxReadFileLines
	}
	if c.Tools.WriteFile.MaxLines <= 0 {
		c.Tools.WriteFile.MaxLines = def.Tools.WriteFile.MaxLines
	}
	if strings.TrimSpace(c.Tools.Discovery.Mode) == "" {
		c.Tools.Discovery.Mode = def.Tools.Discovery.Mode
	}
	if c.Tools.Discovery.TTL <= 0 {
		c.Tools.Discovery.TTL = def.Tools.Discovery.TTL
	}
	if c.Tools.Discovery.MaxSearchResults <= 0 {
		c.Tools.Discovery.MaxSearchResults = def.Tools.Discovery.MaxSearchResults
	}
	// min_match_rate is a share in (0, 1]: 0 (unset) and an impossible share
	// both fall back to the built-in default.
	if c.Tools.Discovery.MinMatchRate <= 0 || c.Tools.Discovery.MinMatchRate > 1 {
		c.Tools.Discovery.MinMatchRate = def.Tools.Discovery.MinMatchRate
	}
	if c.Agent.MaxToolIterations <= 0 {
		c.Agent.MaxToolIterations = def.Agent.MaxToolIterations
	}
	c.ensureMCPServer()
}

// WebEnabled reports whether the web mirror should start.
func (c *Config) WebEnabled() bool { return c.Web.Port > 0 }

// Validate returns a human-readable problem when required fields are missing.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.OpenAI.APIKey) == "" {
		return fmt.Errorf("openai.api_key is empty; edit the config file and try again")
	}
	// MCP tools always use the find/unlock mechanism, so enabling MCP also
	// requires a valid discovery configuration.
	if c.Tools.Discovery.Enabled || c.Tools.MCP.Enabled {
		switch c.Tools.Discovery.EffectiveMode() {
		case ToolDiscoveryModeUnlock:
		default:
			return fmt.Errorf("tools.discovery.mode %q is not supported (want %q)",
				c.Tools.Discovery.Mode, ToolDiscoveryModeUnlock)
		}
		if !c.Tools.Discovery.UseBM25 {
			return fmt.Errorf("MCP/find-unlock requires tools.discovery.use_bm25 = true (tool_search_tool_bm25 is the only discovery search in lightagent)")
		}
	}
	if c.Tools.MCP.Enabled {
		for name, server := range c.Tools.MCP.Servers {
			if !server.Enabled {
				continue
			}
			switch server.EffectiveType() {
			case MCPTransportStdio:
				if strings.TrimSpace(server.Command) == "" {
					return fmt.Errorf("tools.mcp.servers[%s]: command is required for the stdio transport", name)
				}
			case MCPTransportHTTP, MCPTransportSSE:
				if strings.TrimSpace(server.URL) == "" {
					return fmt.Errorf("tools.mcp.servers[%s]: url is required for the %s transport", name, server.EffectiveType())
				}
			default:
				return fmt.Errorf("tools.mcp.servers[%s]: unsupported type %q (want stdio, http, streamable-http or sse)", name, server.Type)
			}
		}
	}
	return nil
}
