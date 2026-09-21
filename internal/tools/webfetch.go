package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"lightagent/internal/utils"
)

// WebFetchToolName is the tool identifier of the page fetcher.
const WebFetchToolName = "webfetch"

// webFetchTimeoutDefault bounds a fetch when no timeout is configured.
const webFetchTimeoutDefault = 30 * time.Second

// bodySeparator divides the tool's own status line from the markdown it carries.
// The answer is the tool's return value, recorded as an ordinary tool message
// like every other tool's; nothing wraps it.
const bodySeparator = "---"

// WebFetchConfig carries the webfetch settings.
type WebFetchConfig struct {
	// Timeout bounds one fetch (page load, rendering and conversion included).
	// It is the default of the tool's timeout argument. A non-positive value
	// falls back to webFetchTimeoutDefault.
	Timeout time.Duration
	// Compress asks the agent to run a self-compression pass over the fetched
	// markdown before it enters the context (tools.webfetch.compress).
	Compress bool
	// CompressRetries is how many times the agent may ask the model again after
	// a compression reply that does not follow the requested format
	// (tools.webfetch.compress_retries).
	CompressRetries int
	// Mode selects how the page is obtained. The empty value is the usual
	// default (utils.FetchModeAuto: render with a browser when one is
	// installed, else take the HTTP source).
	Mode utils.FetchMode
	// BrowserPath pins the browser executable to render with; empty means
	// auto-detection (utils.FindChrome). The HTTP path never uses it.
	BrowserPath string
	// UserAgent overrides the user agent of both paths. Empty keeps the default
	// of each one: the Go client's for the HTTP source, the browser's own for a
	// render.
	UserAgent string
	// MaxBytes caps the body read over HTTP (0 keeps the cap of internal/utils,
	// see utils.WebFetchMaxBytesDefault).
	MaxBytes int64
}

// WebFetchTool fetches a page, converts it to markdown and returns the markdown
// with a status line, as the answer of the call.
type WebFetchTool struct {
	cfg WebFetchConfig
}

// NewWebFetchTool creates the page fetcher.
func NewWebFetchTool(cfg WebFetchConfig) *WebFetchTool {
	if cfg.Timeout <= 0 {
		cfg.Timeout = webFetchTimeoutDefault
	}
	if cfg.Mode == "" {
		cfg.Mode = utils.FetchModeAuto
	}
	return &WebFetchTool{cfg: cfg}
}

// Name implements Tool.
func (t *WebFetchTool) Name() string { return WebFetchToolName }

// Description implements Tool.
func (t *WebFetchTool) Description() string {
	return "Fetch a web page and read it as markdown. " + t.strategy() +
		" The answer carries a status line (conversion result and any converter warning) and the markdown of the page. " +
		"Use it to read documentation, articles or any address of the web instead of downloading " +
		"the page with a command."
}

// strategy states how this tool is configured to obtain a page, so the model is
// never told about a browser it will not get.
func (t *WebFetchTool) strategy() string {
	switch t.cfg.Mode {
	case utils.FetchModeBrowser:
		return "The page is rendered with a browser, and the fetch fails when none is available."
	case utils.FetchModeHTTP:
		return "The page is downloaded over HTTP, without a browser."
	default:
		return "The page is rendered with a browser when one is available and downloaded over HTTP otherwise."
	}
}

// Parameters implements Tool.
func (t *WebFetchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "Address of the page (http or https; a missing scheme is read as https).",
			},
			"timeout": map[string]any{
				"type": "integer",
				"description": "Time limit for the whole fetch in seconds " +
					"(default: the configured tools.webfetch.timeout_seconds).",
			},
		},
		"required": []string{"url"},
	}
}

// Execute implements Tool.
func (t *WebFetchTool) Execute(ctx context.Context, args map[string]any) *Result {
	rawURL, ok := stringArg(args, "url")
	if !ok || strings.TrimSpace(rawURL) == "" {
		return Fail("Missing or invalid 'url' argument. Must be a non-empty string.")
	}
	timeout := t.cfg.Timeout
	// A missing or null timeout keeps the configured default; a value that is
	// present but unusable is an error, so the model learns what went wrong
	// instead of silently getting another timeout.
	if raw, present := args["timeout"]; present && raw != nil {
		seconds := intArg(args, "timeout", 0)
		if seconds <= 0 {
			return Fail("Invalid 'timeout' argument. Must be a positive number of seconds.")
		}
		timeout = time.Duration(seconds) * time.Second
	}

	page, err := utils.WebFetch(ctx, rawURL, t.fetchOptions(timeout)...)
	if err != nil {
		return Fail(fmt.Sprintf("Fetch failed: %v", err))
	}
	converted, err := utils.Html2MdConvert(page.HTML, utils.WithBaseURL(page.FinalURL))
	if err != nil {
		return Fail(fmt.Sprintf("Fetch succeeded (%s) but the page could not be converted to markdown: %v",
			page.FinalURL, err))
	}
	markdown := strings.TrimSpace(converted.Markdown)
	if markdown == "" {
		return Fail(fmt.Sprintf("Fetch succeeded (%s) but the page holds no readable content to convert.",
			page.FinalURL))
	}

	warnings := "none"
	if len(converted.Warnings) > 0 {
		warnings = strings.Join(converted.Warnings, "; ")
	}
	res := &Result{
		// The tool returns its own text, which the agent records as the tool
		// message of the call like any other tool's answer.
		ForLLM: fmt.Sprintf("Conversion succeeded. Converter warnings (if any): %s\n%s\n\n%s",
			warnings, bodySeparator, markdown),
		ForUser: fmt.Sprintf("Fetched %s as markdown (%s, %d chars, %.1fs)",
			page.FinalURL, page.Method, len(markdown), page.Elapsed.Seconds()),
	}
	if len(page.Notes) > 0 {
		res.ForUser += "\n" + strings.Join(page.Notes, "; ")
	}
	res.Compress = t.cfg.Compress
	res.CompressRetries = t.cfg.CompressRetries
	return res
}

// fetchOptions translates the configured web settings into the options of
// internal/utils, leaving out what was not configured so the defaults of the
// fetcher apply.
func (t *WebFetchTool) fetchOptions(timeout time.Duration) []utils.WebFetchOptionFunc {
	opts := []utils.WebFetchOptionFunc{
		utils.WithFetchMode(t.cfg.Mode),
		utils.WithFetchTimeout(timeout),
	}
	if t.cfg.BrowserPath != "" {
		opts = append(opts, utils.WithFetchChromePath(t.cfg.BrowserPath))
	}
	if t.cfg.UserAgent != "" {
		opts = append(opts, utils.WithFetchUserAgent(t.cfg.UserAgent))
	}
	if t.cfg.MaxBytes > 0 {
		opts = append(opts, utils.WithFetchMaxBytes(t.cfg.MaxBytes))
	}
	return opts
}

