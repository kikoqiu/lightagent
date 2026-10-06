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
	"strconv"
	"strings"

	"lightagent/internal/passwd"
)

// Config is the root configuration document.
type Config struct {
	// LLMs lists the LLM providers (interfaces) the program can talk to. A run
	// starts on the first enabled one; /switchapi picks another at runtime (in
	// memory only — the file is not rewritten). The field is spelled "providers"
	// in the file.
	LLMs    []LLMConfig   `json:"providers"`
	Context ContextConfig `json:"context"`
	Web     WebConfig     `json:"web"`
	Tools   ToolsConfig   `json:"tools"`
	Agent   AgentConfig   `json:"agent"`
	UI      UIConfig      `json:"ui"`
	// LegacyOpenAI carries the pre-multi-interface single-endpoint block. It is
	// read on load and folded into LLMs, then dropped; it is never written.
	// Keeping the field (rather than ignoring the key) also lets a hand-written
	// old document pass the strict decode the config editor uses.
	LegacyOpenAI *OpenAIConfig `json:"openai,omitempty"`
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
	// MediaTypes lists the multimedia types this model accepts as attachments,
	// e.g. ["image/png", "image/jpeg", "image/*", "audio/wav",
	// "application/pdf"]. Entries are lower cased; a bare family name means the
	// whole family ("image" == "image/*"). The list is what turns the
	// capability on: while it is empty neither upload_media (which also needs
	// tools.upload_media.enabled) nor the web mirror's attach control exists.
	// It is left out of the file when empty.
	MediaTypes []string `json:"media_types,omitempty"`
	// ExtraBody holds provider-specific request parameters. Its keys are merged
	// into the top-level /chat/completions request body, so a value here
	// overrides the corresponding built-in field (e.g. "temperature").
	ExtraBody map[string]any `json:"extra_body,omitempty"`
}

// LLM interface types. Only the OpenAI-compatible endpoint exists today; the
// field is here so a future provider can be added without reshaping the file.
const LLMTypeOpenAI = "openai"

// DefaultLLMName is the name of the interface the built-in default produces and
// the one the migration from the legacy single-endpoint block uses.
const DefaultLLMName = "default"

// LLMConfig is one LLM interface: its identity (name, type, enabled), the
// request shape it carries and the context window of the model behind it. The
// OpenAI-compatible fields are embedded, so an entry reads
// {"name":"default","type":"openai","enabled":true,"api_base":...}.
type LLMConfig struct {
	// Name is the human identifier /switchapi and the web picker use.
	Name string `json:"name"`
	// Type selects the interface kind; only LLMTypeOpenAI is understood.
	Type string `json:"type"`
	// Enabled marks the interfaces a run may use. The first enabled one is the
	// one a run starts with.
	Enabled bool `json:"enabled"`
	OpenAIConfig
	// ContextWindow is the context window of this interface's model, in tokens.
	// It drives compression and the usage display, so an interface with a
	// smaller window compresses sooner. 0 (unset) takes the built-in window.
	ContextWindow int `json:"context_window"`
}

// EffectiveType returns the interface type, defaulting to openai so a document
// that omits the field keeps working.
func (c LLMConfig) EffectiveType() string {
	if t := strings.ToLower(strings.TrimSpace(c.Type)); t != "" {
		return t
	}
	return LLMTypeOpenAI
}

// ContextConfig controls context-window management. The window size itself
// lives on each interface (LLMConfig.ContextWindow), because it is a property
// of the model behind it.
type ContextConfig struct {
	SummarizeTokenPercent int `json:"summarize_token_percent"`
	// SummarizeKeep is the retention policy of the two configurable compaction
	// passes (see SummarizeKeepPolicy).
	SummarizeKeep SummarizeKeepConfig `json:"summarize_keep"`
}

// SummarizeKeepConfig configures how much of the newest conversation a compaction
// pass leaves raw instead of summarizing it. The overflow recovery — the pass
// that follows a request the provider rejected as too large — is deliberately not
// configurable: the estimate was just proven too small there, so only the summary
// may survive the retry.
type SummarizeKeepConfig struct {
	// Auto is the policy of the proactive post-turn pass.
	Auto SummarizeKeepPolicy `json:"auto"`
	// Manual is the policy of the pass /compact triggers.
	Manual SummarizeKeepPolicy `json:"manual"`
}

// SummarizeKeepPolicy is the retention policy of one compaction pass: how much of
// the newest history the pass keeps visible next to the summary.
//
// Retaining raw messages makes the request that follows a compaction start at an
// earlier prefix than the one the provider cached, and some inference engines do
// not preserve their prompt cache across such a rollback. Both values therefore
// default to 0: the whole compressed history is replaced by the accumulated
// summary, and the next request is the live prefix (system prompt plus summary)
// followed by whatever comes after it, which keeps the cache valid. Raise them
// only for a provider that does keep its cache across a rollback — the newest
// turns are covered by the summary either way.
type SummarizeKeepPolicy struct {
	// BudgetPercent is the share, in percent, of the available input budget
	// (context_window minus openai.max_tokens) the pass may keep raw. 0 (the
	// default) keeps nothing.
	BudgetPercent int `json:"budget_percent"`
	// Turns caps how many complete turns — a user message plus the assistant and
	// tool messages up to the next user message — may stay raw. 0 (the default)
	// keeps none. Whichever limit is reached first stops the walk.
	Turns int `json:"turns"`
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
	WebFetch      WebFetchToolConfig  `json:"webfetch"`
	UploadMedia   MediaToolConfig     `json:"upload_media"`
	Discovery     ToolDiscoveryConfig `json:"discovery"`
	MCP           MCPConfig           `json:"mcp"`
}

// MediaToolConfig configures the upload_media tool.
//
// The tool exists only when the model declares the media types it accepts
// (openai.media_types) and this switch is on: the type list alone does not
// register it, and the switch alone has nothing to accept. It defaults to false,
// so both sides have to be set on purpose.
type MediaToolConfig struct {
	Enabled bool `json:"enabled"`
	// MaxBytes caps one uploaded file, in bytes. 0 (or a negative value) keeps
	// the built-in cap (tools.MediaMaxBytesDefault, 20 MiB); the key is left
	// out of the file when 0.
	MaxBytes int64 `json:"max_bytes,omitempty"`
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

// Built-in exec output limits: the default of the shells' max_lines parameter
// and the most one call may ask for (tools.exec.max_lines / max_lines_max).
const (
	// ExecMaxLinesDefault is the default line budget of one run_script or
	// manage_session answer.
	ExecMaxLinesDefault = 50
	// ExecMaxLinesMaxDefault is the upper bound of that budget: a call that
	// asks for more lines is truncated to it.
	ExecMaxLinesMaxDefault = 100
)

// ExecToolConfig configures run_script / manage_session.
type ExecToolConfig struct {
	Enabled        bool `json:"enabled"`
	TimeoutSeconds int  `json:"timeout_seconds"`
	WaitSeconds    int  `json:"wait_seconds"`
	// MaxLines is the default of the tools' max_lines parameter. It says how
	// many lines one answer may carry. A missing or unusable value keeps
	// ExecMaxLinesDefault (50).
	MaxLines int `json:"max_lines"`
	// MaxLinesMax is the upper bound of that parameter. A call that asks for
	// more lines is truncated to it. It defaults to ExecMaxLinesMaxDefault
	// (100). A value below MaxLines lowers MaxLines to it.
	MaxLinesMax int `json:"max_lines_max"`
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

// WebFetch fetch strategies (tools.webfetch.mode). The values are the mode
// names internal/utils reads, so the session wiring passes them through as they
// are.
const (
	// WebFetchModeAuto renders the page in a visible browser (the way
	// WebFetchModeHeadful works) and falls back to the HTTP source when no
	// browser can be started. A browser with a window is far less likely to be
	// blocked than a headless one, which is why it is the default.
	WebFetchModeAuto = "auto"
	// WebFetchModeHeadful renders the page in a visible browser window started
	// for the request.
	WebFetchModeHeadful = "chrome-headful"
	// WebFetchModeHeadless renders the page in a headless browser started for
	// the request.
	WebFetchModeHeadless = "chrome-headless"
	// WebFetchModeAttached renders the page through a browser that is already
	// running, reached over the DevTools endpoint of attach_address (the user's
	// own browser, with their logins and cookies).
	WebFetchModeAttached = "chrome-attached"
	// WebFetchModeHTTP downloads the source and never starts a browser.
	WebFetchModeHTTP = "http"
)

// Built-in webfetch limits and endpoints.
const (
	// WebFetchMaxLinesDefault is how many lines of markdown webfetch feeds back
	// before a page is answered with its main content (or its middle) and saved
	// to disk instead (tools.webfetch.max_lines).
	WebFetchMaxLinesDefault = 100
	// WebFetchAttachAddressDefault is the DevTools endpoint a chrome-attached
	// fetch reaches for when attach_address is empty.
	WebFetchAttachAddressDefault = "127.0.0.1:9222"
)

// WebFetchToolConfig configures webfetch.
type WebFetchToolConfig struct {
	// Enabled registers the tool. It defaults to true; loading starts from the
	// defaults, so an explicit "enabled": false is required to turn it off.
	Enabled bool `json:"enabled"`
	// TimeoutSeconds bounds one fetch (page load, rendering and conversion
	// included) and is the default of the tool's own timeout argument.
	TimeoutSeconds int `json:"timeout_seconds"`
	// Mode selects how a page is obtained: WebFetchModeAuto (a visible browser,
	// else the HTTP source), WebFetchModeHeadful, WebFetchModeHeadless,
	// WebFetchModeAttached or WebFetchModeHTTP. Any other value is rejected by
	// Validate.
	Mode string `json:"mode"`
	// MaxLines caps the lines of markdown the tool feeds back. A page that does
	// not fit is cut there and saved in full as markdown below the .lightagent
	// directory of the working directory, with the path reported to the model.
	// 0 keeps WebFetchMaxLinesDefault, a negative value asks for no limit.
	MaxLines int `json:"max_lines"`
	// BrowserPath pins the browser executable used to render a page (an empty
	// value discovers an installed one). A mode that attaches never uses it. It
	// is left out of the file when empty.
	BrowserPath string `json:"browser_path,omitempty"`
	// UserAgent overrides the user agent of both paths: the browser when one
	// renders the page, the HTTP request otherwise. Empty keeps the default of
	// each path. It is left out of the file when empty.
	UserAgent string `json:"user_agent,omitempty"`
	// MaxBytes caps the body read over HTTP, in bytes. 0 keeps the built-in cap
	// (8 MiB); the key is left out of the file when 0.
	MaxBytes int64 `json:"max_bytes,omitempty"`
	// AttachAddress is the DevTools endpoint of the browser to drive in
	// WebFetchModeAttached. One string says all of it: "9222" (a port on
	// 127.0.0.1), "192.168.0.5:9223", "127.0.0.1:9222", or an http:// / ws://
	// URL. Empty means WebFetchAttachAddressDefault. It is left out of the file
	// when empty.
	AttachAddress string `json:"attach_address,omitempty"`
}

// EffectiveMode returns the fetch strategy, defaulting to auto. The value is
// normalized (trimmed, lower-case) so a hand-written "HTTP" works too.
func (c WebFetchToolConfig) EffectiveMode() string {
	mode := strings.ToLower(strings.TrimSpace(c.Mode))
	if mode == "" {
		return WebFetchModeAuto
	}
	return mode
}

// AttachEndpoint returns the DevTools endpoint a chrome-attached fetch reaches
// for: the configured address, or the built-in one when nothing is configured.
// It is what the tool hands to internal/utils as the browser to drive.
func (c WebFetchToolConfig) AttachEndpoint() string {
	if address := strings.TrimSpace(c.AttachAddress); address != "" {
		return address
	}
	return WebFetchAttachAddressDefault
}

// ToggleToolConfig is a simple enabled/disabled switch.
type ToggleToolConfig struct {
	Enabled bool `json:"enabled"`
}

// AgentConfig controls the agent loop.
type AgentConfig struct {
	MaxToolIterations int    `json:"max_tool_iterations"`
	SystemPrompt      string `json:"system_prompt"`
	// IncludeWorkingDir appends the working-directory line (the absolute
	// directory the process runs in, the path only) to the system prompt. It
	// defaults to true; loading starts from the defaults, so an explicit false
	// is required to turn it off.
	IncludeWorkingDir bool `json:"include_working_dir"`
	// SummaryInSystemPrompt selects where a request carries the accumulated
	// context summary: false (the default) sends it as the first user message,
	// true appends it to the system prompt (the "CONVERSATION SUMMARY"
	// section). The switch shapes the message list that is sent and nothing
	// else.
	SummaryInSystemPrompt bool `json:"summary_in_system_prompt"`
	// IncludeOnlyThink keeps an assistant message that carries only the model's
	// thinking — no visible text and no tool calls — in the history. It
	// defaults to true; loading starts from the defaults, so an explicit false
	// is required to turn it off. With it off such a reply is dropped, and
	// ContinueOnlyThink below has no effect.
	IncludeOnlyThink bool `json:"include_only_think"`
	// ContinueOnlyThink asks the model again when its reply carried only
	// thinking, instead of ending the turn, so a reply that carries nothing but
	// thinking still gets an answer — whatever the provider's finish_reason
	// (stop, a cut-off at max_tokens, or none). It defaults to true and only
	// applies while IncludeOnlyThink is on: reasoning that was not recorded
	// cannot be carried into a retry.
	ContinueOnlyThink bool `json:"continue_only_think"`
}

// Default returns the built-in configuration used when no file exists yet.
func Default() *Config {
	return &Config{
		// One OpenAI-compatible interface named "default" out of the box.
		LLMs: []LLMConfig{{
			Name:    DefaultLLMName,
			Type:    LLMTypeOpenAI,
			Enabled: true,
			OpenAIConfig: OpenAIConfig{
				APIBase:     "https://api.openai.com/v1",
				Model:       "gpt-4o-mini",
				Temperature: -1.0,
				MaxTokens:   40960,
				TimeoutSec:  4800,
				Stream:      true,
			},
			ContextWindow: 81960,
		}},
		Context: ContextConfig{
			SummarizeTokenPercent: 75,
			// Both passes keep no raw message: some inference engines do not
			// preserve the prompt cache across the rollback a retention
			// performs, so the compressed history is replaced by the accumulated
			// summary in full (see SummarizeKeepPolicy).
			SummarizeKeep: SummarizeKeepConfig{
				Auto:   SummarizeKeepPolicy{BudgetPercent: 0, Turns: 0},
				Manual: SummarizeKeepPolicy{BudgetPercent: 0, Turns: 0},
			},
		},
		Web: WebConfig{Host: "127.0.0.1", Port: 0, Password: ""},
		Tools: ToolsConfig{
			Exec:          ExecToolConfig{Enabled: true, TimeoutSeconds: 3600, WaitSeconds: 10, MaxLines: ExecMaxLinesDefault, MaxLinesMax: ExecMaxLinesMaxDefault, UseUTF8: true},
			ReadFileLines: FsToolConfig{Enabled: true, MaxReadFileSize: 32000, MaxReadFileLines: 200},
			WriteFile:     WriteToolConfig{Enabled: true, MaxLines: 200, AutoSplit: true},
			EditFile:      ToggleToolConfig{Enabled: true},
			WebFetch: WebFetchToolConfig{
				Enabled:        true,
				Mode:           WebFetchModeAuto,
				TimeoutSeconds: 30,
				MaxLines:       WebFetchMaxLinesDefault,
			},
			// The upload tool is off until it is asked for: with the default
			// it only becomes available once openai.media_types is configured
			// AND this switch is turned on.
			UploadMedia: MediaToolConfig{Enabled: false},
			Discovery: ToolDiscoveryConfig{
				Enabled:          false,
				Mode:             ToolDiscoveryModeUnlock,
				TTL:              50,
				MaxSearchResults: 10,
				MinMatchRate:     defaultDiscoveryMinMatchRate,
				UseBM25:          true,
			},
		},
		Agent: AgentConfig{MaxToolIterations: 200, IncludeWorkingDir: true, SummaryInSystemPrompt: false, IncludeOnlyThink: true, ContinueOnlyThink: true},
		UI:    UIConfig{Markdown: true},
	}
}

// ActiveLLM returns the interface a run starts with: the first enabled one. It
// falls back to the first interface when none is enabled, so a document that
// switched them all off still has something to show; found reports whether an
// enabled interface was picked.
func (c *Config) ActiveLLM() (index int, api LLMConfig, found bool) {
	for i, candidate := range c.LLMs {
		if candidate.Enabled {
			return i, candidate, true
		}
	}
	if len(c.LLMs) > 0 {
		return 0, c.LLMs[0], false
	}
	return 0, LLMConfig{}, false
}

// LLMIndex resolves a /switchapi argument — a 1-based number or an interface
// name — to its position in LLMs. It reports ok=false when nothing matches.
func (c *Config) LLMIndex(spec string) (int, bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(spec); err == nil {
		if n < 1 || n > len(c.LLMs) {
			return 0, false
		}
		return n - 1, true
	}
	for i, api := range c.LLMs {
		if api.Name == spec {
			return i, true
		}
	}
	return 0, false
}

// APIInfo describes one interface for a front-end picker (the CLI's /switchapi
// listing and the web mirror's model dropdown).
type APIInfo struct {
	// Index is the 1-based position, the number /switchapi accepts.
	Index   int    `json:"index"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Model   string `json:"model"`
	Enabled bool   `json:"enabled"`
	// Active marks the interface currently in use.
	Active bool `json:"active"`
}

// APIInfos describes every interface, marking the active one by its 0-based
// position (the runtime selection).
func (c *Config) APIInfos(active int) []APIInfo {
	infos := make([]APIInfo, 0, len(c.LLMs))
	for i, api := range c.LLMs {
		infos = append(infos, APIInfo{
			Index:   i + 1,
			Name:    api.Name,
			Type:    api.EffectiveType(),
			Model:   api.Model,
			Enabled: api.Enabled,
			Active:  i == active,
		})
	}
	return infos
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
	// A document written before the multi-interface layout carries a single
	// "openai" block and no "providers". Read it first, so the interface it describes
	// replaces the seeded default instead of sitting next to it.
	legacy := probeLegacyLLM(data)
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
	migrated := cfg.migrateLegacyLLM(legacy)
	changed, err := cfg.ensureWebSalt()
	if err != nil {
		return nil, false, err
	}
	cfg.applyDefaults()
	return cfg, migrated || changed, nil
}

// legacyLLMInput captures the pre-multi-interface layout of a document: whether
// a "providers" array is present at all, the raw single "openai" block, and the
// context window that used to live under "context".
type legacyLLMInput struct {
	hasLLMs       bool
	openai        json.RawMessage
	contextWindow int
}

// probeLegacyLLM reads the legacy shape out of a raw document without touching
// the parsed config: the caller uses it to decide whether the seeded default
// interface has to be replaced by a migrated one.
func probeLegacyLLM(data []byte) legacyLLMInput {
	var probe struct {
		LLMs    json.RawMessage `json:"providers"`
		OpenAI  json.RawMessage `json:"openai"`
		Context struct {
			ContextWindow int `json:"context_window"`
		} `json:"context"`
	}
	_ = json.Unmarshal(data, &probe) // a malformed document is reported by the caller
	return legacyLLMInput{
		hasLLMs:       len(bytes.TrimSpace(probe.LLMs)) > 0,
		openai:        probe.OpenAI,
		contextWindow: probe.Context.ContextWindow,
	}
}

// migrateLegacyLLM folds a legacy single-endpoint document into cfg.LLMs. It is
// a no-op for a document that already carries a "providers" array; a document with
// neither keeps the default interface. It reports whether cfg changed and must
// be written back.
//
// The migrated interface starts from the built-in default entry (so an omitted
// legacy field keeps its default — the temperature -1 sentinel included) and is
// then overlaid with the legacy "openai" block and its context window.
func (c *Config) migrateLegacyLLM(in legacyLLMInput) bool {
	if in.hasLLMs {
		// A new-format document: drop any legacy block the decoder picked up.
		if c.LegacyOpenAI != nil {
			c.LegacyOpenAI = nil
			return true
		}
		return false
	}
	api := Default().LLMs[0]
	if len(bytes.TrimSpace(in.openai)) > 0 {
		_ = json.Unmarshal(in.openai, &api.OpenAIConfig)
	}
	if in.contextWindow > 0 {
		api.ContextWindow = in.contextWindow
	}
	c.LLMs = []LLMConfig{api}
	c.LegacyOpenAI = nil
	return true
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
// configuration can be shown without leaking them: every interface's api key
// and the web password. The password salt is deliberately left visible — it is
// public (the page asks for it before signing in) and keeping it makes the
// document that comes back from the editor identical to the one on disk.
//
// The copy owns its LLMs slice but shares the nested maps (extra_body,
// media_types, MCP servers) with the receiver: that is fine for display, but
// callers must not mutate them.
func (c *Config) MaskSecrets() *Config {
	clone := *c
	// The interfaces get their own slice, so masking one never writes through to
	// the receiver. The nested maps/slices (extra_body, media_types) are still
	// shared, which is fine for display.
	clone.LLMs = make([]LLMConfig, len(c.LLMs))
	copy(clone.LLMs, c.LLMs)
	for i := range clone.LLMs {
		if clone.LLMs[i].APIKey != "" {
			clone.LLMs[i].APIKey = MaskedSecret
		}
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

// sanitizeKeepPolicy replaces an unusable retention policy with the default one: a
// share outside [0, 100] and a negative turn cap are both invalid.
func sanitizeKeepPolicy(p, def SummarizeKeepPolicy) SummarizeKeepPolicy {
	if p.BudgetPercent < 0 || p.BudgetPercent > 100 {
		p.BudgetPercent = def.BudgetPercent
	}
	if p.Turns < 0 {
		p.Turns = def.Turns
	}
	return p
}

// applyDefaults fills zero values with defaults so partial config files remain
// usable after new fields are added.
func (c *Config) applyDefaults() {
	def := Default()
	// Each interface is completed on its own, so a partial entry (or one written
	// by hand) still yields a usable endpoint.
	defAPI := def.LLMs[0]
	if len(c.LLMs) == 0 {
		c.LLMs = def.LLMs
	}
	for i := range c.LLMs {
		api := &c.LLMs[i]
		if strings.TrimSpace(api.Type) == "" {
			api.Type = LLMTypeOpenAI
		}
		if strings.TrimSpace(api.Name) == "" {
			api.Name = fmt.Sprintf("api-%d", i+1)
		}
		if api.APIBase == "" {
			api.APIBase = defAPI.APIBase
		}
		if api.Model == "" {
			api.Model = defAPI.Model
		}
		// temperature is deliberately not defaulted here: the built-in default
		// of -1 is the "unset" sentinel (a negative value is left out of the
		// request so the provider decides), and any value a document sets — 0
		// included — must survive. An omitted key already keeps -1 because
		// decoding starts from Default().
		if api.MaxTokens <= 0 {
			api.MaxTokens = defAPI.MaxTokens
		}
		// timeout_seconds is an inactivity budget: a negative value is invalid
		// and falls back to the default, while an explicit 0 disables it.
		if api.TimeoutSec < 0 {
			api.TimeoutSec = defAPI.TimeoutSec
		}
		// context_window is a property of the model behind the interface: 0
		// (unset) takes the built-in window.
		if api.ContextWindow <= 0 {
			api.ContextWindow = defAPI.ContextWindow
		}
	}
	if strings.TrimSpace(c.Web.Host) == "" {
		c.Web.Host = def.Web.Host
	}
	if c.Context.SummarizeTokenPercent <= 0 || c.Context.SummarizeTokenPercent > 100 {
		c.Context.SummarizeTokenPercent = def.Context.SummarizeTokenPercent
	}
	// summarize_keep is a policy whose default is "keep nothing", so an
	// out-of-range value falls back to that default rather than to some non-zero
	// window.
	c.Context.SummarizeKeep.Auto = sanitizeKeepPolicy(c.Context.SummarizeKeep.Auto, def.Context.SummarizeKeep.Auto)
	c.Context.SummarizeKeep.Manual = sanitizeKeepPolicy(c.Context.SummarizeKeep.Manual, def.Context.SummarizeKeep.Manual)
	if c.Tools.Exec.TimeoutSeconds <= 0 {
		c.Tools.Exec.TimeoutSeconds = def.Tools.Exec.TimeoutSeconds
	}
	if c.Tools.Exec.WaitSeconds <= 0 {
		c.Tools.Exec.WaitSeconds = def.Tools.Exec.WaitSeconds
	}
	// max_lines is the default of the shells' per-call line budget and
	// max_lines_max its upper bound. A missing or unusable value falls back to
	// the built-in one. A bound below the default lowers the default to it.
	if c.Tools.Exec.MaxLinesMax <= 0 {
		c.Tools.Exec.MaxLinesMax = def.Tools.Exec.MaxLinesMax
	}
	if c.Tools.Exec.MaxLines <= 0 {
		c.Tools.Exec.MaxLines = def.Tools.Exec.MaxLines
	}
	if c.Tools.Exec.MaxLines > c.Tools.Exec.MaxLinesMax {
		c.Tools.Exec.MaxLines = c.Tools.Exec.MaxLinesMax
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
	if c.Tools.WebFetch.TimeoutSeconds <= 0 {
		c.Tools.WebFetch.TimeoutSeconds = def.Tools.WebFetch.TimeoutSeconds
	}
	// upload_media.max_bytes is a cap: 0 (unset) keeps the built-in one and a
	// negative value falls back to it as well.
	if c.Tools.UploadMedia.MaxBytes < 0 {
		c.Tools.UploadMedia.MaxBytes = def.Tools.UploadMedia.MaxBytes
	}
	// An omitted mode keeps the built-in strategy; an unusable value is
	// reported by Validate rather than silently replaced.
	if strings.TrimSpace(c.Tools.WebFetch.Mode) == "" {
		c.Tools.WebFetch.Mode = def.Tools.WebFetch.Mode
	}
	// max_lines: 0 (unset) means the built-in limit; a negative value is kept as
	// the caller asking for no limit at all.
	if c.Tools.WebFetch.MaxLines == 0 {
		c.Tools.WebFetch.MaxLines = def.Tools.WebFetch.MaxLines
	}
	// max_bytes is a cap: 0 (unset) means the built-in one, and a negative
	// value falls back to it as well.
	if c.Tools.WebFetch.MaxBytes < 0 {
		c.Tools.WebFetch.MaxBytes = def.Tools.WebFetch.MaxBytes
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
	if len(c.LLMs) == 0 {
		return fmt.Errorf("providers is empty; add at least one interface")
	}
	enabled := 0
	for i, api := range c.LLMs {
		if !api.Enabled {
			continue
		}
		enabled++
		switch api.EffectiveType() {
		case LLMTypeOpenAI:
		default:
			return fmt.Errorf("providers[%d] (%s): type %q is not supported (want %q)",
				i, api.Name, api.Type, LLMTypeOpenAI)
		}
		if strings.TrimSpace(api.APIKey) == "" {
			return fmt.Errorf("providers[%d] (%s): api_key is empty; edit the config file and try again", i, api.Name)
		}
	}
	if enabled == 0 {
		return fmt.Errorf("no llm interface is enabled; set \"enabled\": true on one")
	}
	// webfetch obtains a page in one of a few ways, and only those: a typo must
	// be reported instead of silently falling back to the default.
	switch c.Tools.WebFetch.EffectiveMode() {
	case WebFetchModeAuto, WebFetchModeHeadful, WebFetchModeHeadless, WebFetchModeAttached, WebFetchModeHTTP:
	default:
		return fmt.Errorf("tools.webfetch.mode %q is not supported (want %q, %q, %q, %q or %q)",
			c.Tools.WebFetch.Mode, WebFetchModeAuto, WebFetchModeHeadful, WebFetchModeHeadless,
			WebFetchModeAttached, WebFetchModeHTTP)
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
