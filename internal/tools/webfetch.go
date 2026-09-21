package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lightagent/internal/utils"
)

// WebFetchToolName is the tool identifier of the page fetcher.
const WebFetchToolName = "webfetch"

const (
	// webFetchTimeoutDefault bounds a fetch when no timeout is configured, and
	// is the floor of every fetch: the configured timeout and the timeout
	// argument of a call are raised to it, because loading, rendering and
	// converting a page cannot finish in less.
	webFetchTimeoutDefault = 30 * time.Second
	// WebFetchMaxLinesDefault is how many lines of markdown the tool feeds back
	// when nothing else is configured. A page that does not fit is cut there and
	// saved to disk instead (see lightagentDir).
	WebFetchMaxLinesDefault = 200
	// bodySeparator divides the tool's own status line from the markdown it
	// carries. The answer is the tool's return value, recorded as an ordinary
	// tool message like every other tool's; nothing wraps it.
	bodySeparator = "---"
	// lightagentDir is the directory the agent keeps its state in, below the
	// working directory: the pages of a fetch that was too long to feed back,
	// and the browser profile of a launch of its own.
	lightagentDir = ".lightagent"
	// webFetchOverflowPrefix names the files a long page is saved in, followed by
	// the timestamp of the fetch so a sequence of them stays readable in the
	// directory. The file holds the whole markdown of the page, under the .md
	// suffix, so it can be read back with the file tools as it was fed back.
	webFetchOverflowPrefix = "webfetch-"
	// webFetchOverflowSuffix is the extension of an overflow file.
	webFetchOverflowSuffix = ".md"
	// webFetchOverflowTimeFormat is the timestamp layout of an overflow file.
	webFetchOverflowTimeFormat = "20060102-150405"
	// webFetchOverflowNameAttempts bounds the search for a free file name when
	// several fetches land in the same second.
	webFetchOverflowNameAttempts = 100
	// browserProfileName is the browser profile a launch of our own uses, below
	// lightagentDir.
	browserProfileName = "browser-profile"
	// nonPageHint tells the model what to do instead of fetching again when the
	// address does not serve a page.
	nonPageHint = "webfetch reads pages and text documents only; download or convert binary content " +
		"(PDF, images, archives, ...) with exec_command instead."
)

// WebFetchConfig carries the webfetch settings.
type WebFetchConfig struct {
	// Timeout bounds one fetch (page load, rendering and conversion included).
	// It is the default of the tool's timeout argument. A non-positive value
	// falls back to webFetchTimeoutDefault, and a smaller one is raised to it:
	// no fetch runs with less than that floor.
	Timeout time.Duration
	// Mode selects how the page is obtained. The empty value is the usual
	// default (utils.FetchModeAuto: a visible browser, else the HTTP source).
	Mode utils.FetchMode
	// MaxLines caps how many lines of markdown the tool feeds back; a longer
	// page is cut there and saved in full below the working directory. 0 keeps
	// WebFetchMaxLinesDefault, a negative value asks for no limit at all.
	MaxLines int
	// BrowserPath pins the browser executable to render with; empty means
	// auto-detection (utils.FindChrome). A mode that attaches never uses it.
	BrowserPath string
	// UserAgent overrides the user agent of both paths. Empty keeps the default
	// of each one: the Go client's for the HTTP source, the browser's own for a
	// render.
	UserAgent string
	// MaxBytes caps the body read over HTTP (0 keeps the cap of internal/utils,
	// see utils.WebFetchMaxBytesDefault).
	MaxBytes int64
	// AttachEndpoint is the DevTools endpoint of the browser to drive in
	// chrome-attached mode: "127.0.0.1:9222", "9222", an http:// or a ws://
	// URL. It is only read in that mode.
	AttachEndpoint string
}

// WebFetchTool fetches a page, converts it to markdown and returns the markdown
// with a status line, as the answer of the call.
type WebFetchTool struct {
	cfg WebFetchConfig
}

// NewWebFetchTool creates the page fetcher.
func NewWebFetchTool(cfg WebFetchConfig) *WebFetchTool {
	// A missing or unusable value keeps the built-in default, and a value below
	// the floor of every fetch is raised to it, so the configured timeout is
	// always one the fetcher can honor.
	cfg.Timeout = raiseWebFetchTimeout(cfg.Timeout)
	if cfg.Mode == "" {
		cfg.Mode = utils.FetchModeAuto
	}
	if cfg.MaxLines == 0 {
		cfg.MaxLines = WebFetchMaxLinesDefault
	}
	return &WebFetchTool{cfg: cfg}
}

// Name implements Tool.
func (t *WebFetchTool) Name() string { return WebFetchToolName }

// Description implements Tool.
func (t *WebFetchTool) Description() string {
	return "Fetch a web page and read it as markdown. " + t.strategy() + t.renderRule() +
		"Only pages and other text documents are supported: an address that serves binary content " +
		"(a PDF, an image, an archive, ...) is refused with an error, so use a command to get such content. " +
		t.feedbackRule() +
		"Use it to read documentation, articles or any address of the web instead of downloading " +
		"the page with a command."
}

// strategy states how this tool is configured to obtain a page, so the model is
// never told about a browser it will not get: the mode is fixed by the
// configuration, and it is what decides whether Chrome is used at all. Every
// sentence ends with a space, so it can be followed by renderRule.
func (t *WebFetchTool) strategy() string {
	switch t.cfg.Mode {
	case utils.FetchModeHTTP:
		return "The page is downloaded over HTTP and never rendered: the source the server sends is what is read. "
	case utils.FetchModeChromeHeadless:
		return "The page is rendered by Chrome, in a headless instance with no window. "
	case utils.FetchModeChromeAttached:
		return "The page is rendered by Chrome through the browser the user is using. "
	case utils.FetchModeChromeHeadful:
		return "The page is rendered by Chrome, in a visible window. "
	default:
		return "The page is rendered by Chrome in a visible window when a browser can be started, " +
			"and downloaded over HTTP otherwise. "
	}
}

// renderRule states what rendering with Chrome is, since the model only reads
// the markdown: a rendered fetch carries the DOM a real browser built — with the
// content the page's JavaScript added — rather than the plain source. A tool
// pinned to the source says nothing about Chrome, hence the empty answer.
func (t *WebFetchTool) renderRule() string {
	switch t.cfg.Mode {
	case utils.FetchModeHTTP:
		return ""
	case utils.FetchModeChromeAttached:
		return "Rendering means Chrome opens a tab in that browser, runs the JavaScript of the page and hands back " +
			"the DOM it built, so the fetch carries the logins, cookies and sessions of that browser. "
	default:
		return "Rendering means a real Chrome loads the address, runs the JavaScript of the page and hands back " +
			"the DOM it built, so content the page adds at run time is read as well; the browser uses a profile " +
			"of the agent's own, not one of the user. "
	}
}

// feedbackRule states what the answer carries: the model has to know that a long
// page arrives cut, and that the whole page is on disk.
func (t *WebFetchTool) feedbackRule() string {
	if t.cfg.MaxLines < 0 {
		return "The answer carries the whole markdown of the page. "
	}
	return fmt.Sprintf("The answer carries a status line and at most %d lines of markdown; a longer page is cut "+
		"there and saved in full as markdown in the %s directory of the working directory, with the path "+
		"reported in the answer. ", t.cfg.MaxLines, lightagentDir)
}

// Parameters implements Tool. The timeout the model reads states the value the
// system is configured with (tools.webfetch.timeout_seconds) as well as the
// floor every fetch has, so an argument can never promise less than the fetch
// will actually get.
func (t *WebFetchTool) Parameters() map[string]any {
	configured := webFetchSeconds(t.cfg.Timeout)
	floor := webFetchSeconds(webFetchTimeoutDefault)
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type": "string",
				"description": "Address of a web page (http or https; a missing scheme is read as https). " +
					"Addresses that serve other content are refused.",
			},
			"timeout": map[string]any{
				"type":    "integer",
				"default": configured,
				"description": fmt.Sprintf("Time limit for the whole fetch in seconds (page load, rendering and "+
					"conversion included). Default: %d, the configured tools.webfetch.timeout_seconds. "+
					"The smallest value that is used is %d: a smaller number is raised to it.",
					configured, floor),
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
			return Fail(fmt.Sprintf("Invalid 'timeout' argument. Must be a positive number of seconds "+
				"(the smallest value used is %d).", webFetchSeconds(webFetchTimeoutDefault)))
		}
		// A shorter limit is not honored: the fetch takes the floor instead,
		// which is what the schema promises the model.
		timeout = raiseWebFetchTimeout(time.Duration(seconds) * time.Second)
	}

	page, err := utils.WebFetch(ctx, rawURL, t.fetchOptions(timeout)...)
	if err != nil {
		return Fail(t.fetchFailure(err))
	}
	// A browser hands back whatever it rendered, including a PDF viewer or an
	// image, so the type of the document is checked here as well: webfetch reads
	// pages, and a caller that asked for something else has to learn that.
	if !utils.IsPageContentType(page.ContentType) {
		return Fail(fmt.Sprintf("Not a web page: %s serves %s. %s",
			page.FinalURL, describeContentType(page.ContentType), nonPageHint))
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
	body, overflow, savedTo := t.limitFeedback(markdown)
	status := fmt.Sprintf("Conversion succeeded. Converter warnings (if any): %s", warnings)
	if overflow != "" {
		status += "\n" + overflow
	}

	res := &Result{
		// The tool returns its own text, which the agent records as the tool
		// message of the call like any other tool's answer.
		ForLLM: status + "\n" + bodySeparator + "\n\n" + body,
		ForUser: fmt.Sprintf("Fetched %s as markdown (%s, %d lines, %d chars, %.1fs)",
			page.FinalURL, page.Method, countLines(markdown), len(markdown), page.Elapsed.Seconds()),
	}
	if overflow != "" {
		res.ForUser += fmt.Sprintf("\nFeedback cut at %d lines", t.cfg.MaxLines)
		if savedTo != "" {
			res.ForUser += "; the whole page is in " + savedTo
		}
	}
	if len(page.Notes) > 0 {
		res.ForUser += "\n" + strings.Join(page.Notes, "; ")
	}
	return res
}

// fetchFailure renders a failed fetch for the model. A refusal of the content
// itself is spelled out: the model has to learn that another tool is the way
// forward instead of asking for the same address again.
func (t *WebFetchTool) fetchFailure(err error) string {
	if errors.Is(err, utils.ErrNotPage) {
		return fmt.Sprintf("Fetch failed: %v. %s", err, nonPageHint)
	}
	return fmt.Sprintf("Fetch failed: %v", err)
}

// limitFeedback cuts the markdown to the configured number of lines. A page that
// does not fit is written to an overflow file in full, and the returned note —
// part of the status line the answer carries — reports how long the page is and
// where it went, so the model can read it in pieces with read_file_lines instead
// of losing it; savedTo is that path, for the display line of the caller. A page
// that fits, or a tool that asks for no limit at all, is returned as it is, with
// an empty note.
func (t *WebFetchTool) limitFeedback(markdown string) (body, note, savedTo string) {
	if t.cfg.MaxLines < 0 {
		return markdown, "", ""
	}
	total := countLines(markdown)
	if total <= t.cfg.MaxLines {
		return markdown, "", ""
	}
	body = firstLines(markdown, t.cfg.MaxLines)
	// The whole markdown is saved, not the HTML it came from: the file continues
	// exactly where the feedback was cut, so reading it back needs no second
	// conversion and its size is the one the note reports.
	path, err := saveOverflowMarkdown(markdown)
	if err != nil {
		return body, fmt.Sprintf("The page is longer than the %d line feedback limit: it holds %d lines / %d bytes "+
			"in total, so only the first %d lines follow; saving the whole page failed: %v",
			t.cfg.MaxLines, total, len(markdown), t.cfg.MaxLines, err), ""
	}
	return body, fmt.Sprintf("The page is longer than the %d line feedback limit: it holds %d lines / %d bytes "+
		"in total, so only the first %d lines follow. The whole page was saved as markdown to %s — "+
		"read it with read_file_lines if the rest is needed.",
		t.cfg.MaxLines, total, len(markdown), t.cfg.MaxLines, path), path
}

// saveOverflowMarkdown writes the whole markdown of a fetched page below the
// working directory and returns the path of the file it created.
func saveOverflowMarkdown(markdown string) (string, error) {
	dir := filepath.Join(workingDir(), lightagentDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	stamp := time.Now().Format(webFetchOverflowTimeFormat)
	for attempt := 0; attempt < webFetchOverflowNameAttempts; attempt++ {
		name := webFetchOverflowPrefix + stamp + webFetchOverflowSuffix
		if attempt > 0 {
			name = fmt.Sprintf("%s%s-%d%s", webFetchOverflowPrefix, stamp, attempt, webFetchOverflowSuffix)
		}
		path := filepath.Join(dir, name)
		// Exclusive creation: two fetches within the same second produce two
		// files instead of one overwriting the other.
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		_, writeErr := file.WriteString(markdown)
		closeErr := file.Close()
		if writeErr != nil {
			return "", writeErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		return path, nil
	}
	return "", fmt.Errorf("no free file name left in %s", dir)
}

// workingDir is the directory the process runs in. A failure falls back to the
// relative path, which still lands in that directory.
func workingDir() string {
	if dir, err := os.Getwd(); err == nil {
		return dir
	}
	return "."
}

// countLines counts the lines of a text: its newlines plus the last, possibly
// unterminated one.
func countLines(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

// firstLines returns the first n lines of a text (the whole text when it is
// shorter than that).
func firstLines(text string, n int) string {
	if n <= 0 {
		return ""
	}
	offset := 0
	for i := 0; i < n; i++ {
		next := strings.IndexByte(text[offset:], '\n')
		if next < 0 {
			return text
		}
		offset += next + 1
	}
	// The cut keeps whole lines: everything up to the terminator of the nth one.
	return strings.TrimSuffix(text[:offset], "\n")
}

// describeContentType names a media type in a message (a response that carries
// none is described instead of left blank).
func describeContentType(contentType string) string {
	if trimmed := strings.TrimSpace(contentType); trimmed != "" {
		return trimmed
	}
	return "an unidentified content type"
}

// agentBrowserProfileDir returns the profile directory a browser the tool starts
// uses: a profile of our own, below the agent's directory in the working
// directory. The browser path creates it when it is missing and nothing ever
// deletes it, so the cookies and logins a fetch collects are there for the next
// one, the state belongs to the project rather than to the user's home, and the
// browser the user is using is never touched (chrome-attached reaches that one by
// attaching to it, and names no profile of ours).
func agentBrowserProfileDir() string {
	return filepath.Join(workingDir(), lightagentDir, browserProfileName)
}

// webFetchSeconds reports a timeout as the whole number of seconds the tool
// speaks in its schema and its messages.
func webFetchSeconds(timeout time.Duration) int {
	return int(timeout / time.Second)
}

// raiseWebFetchTimeout lifts a timeout to the floor of every fetch
// (webFetchTimeoutDefault): loading, rendering and converting a page cannot
// finish in less, so a shorter limit would only make a fetch fail early. The
// configured timeout and the timeout argument of a call both go through it, and
// the floor is the smallest value the schema offers the model.
func raiseWebFetchTimeout(timeout time.Duration) time.Duration {
	if timeout < webFetchTimeoutDefault {
		return webFetchTimeoutDefault
	}
	return timeout
}

// fetchOptions translates the configured web settings into the options of
// internal/utils, leaving out what was not configured so the defaults of the
// fetcher apply.
func (t *WebFetchTool) fetchOptions(timeout time.Duration) []utils.WebFetchOptionFunc {
	// Every mode that starts a browser of its own renders on a profile of the
	// agent's own — the headless one included: one directory below the agent's
	// directory in the working directory, created when it is missing and never
	// deleted, so the cookies and logins one fetch collects are there for the
	// next one. A mode that starts no browser names no profile: http needs none,
	// and chrome-attached drives the browser the user has open, whose profile is
	// theirs and is not ours to point at.
	browser := utils.BrowserOptions{Headful: t.headfulWindow()}
	if t.launchesBrowser() {
		browser.UserDataDir = agentBrowserProfileDir()
	}
	opts := []utils.WebFetchOptionFunc{
		utils.WithFetchMode(t.cfg.Mode),
		utils.WithFetchTimeout(timeout),
		utils.WithFetchBrowser(browser),
	}
	if t.launchesBrowser() && t.cfg.BrowserPath != "" {
		opts = append(opts, utils.WithFetchChromePath(t.cfg.BrowserPath))
	}
	if t.cfg.Mode == utils.FetchModeChromeAttached && t.cfg.AttachEndpoint != "" {
		opts = append(opts, utils.WithFetchBrowserAddress(t.cfg.AttachEndpoint))
	}
	if t.cfg.UserAgent != "" {
		opts = append(opts, utils.WithFetchUserAgent(t.cfg.UserAgent))
	}
	if t.cfg.MaxBytes > 0 {
		opts = append(opts, utils.WithFetchMaxBytes(t.cfg.MaxBytes))
	}
	return opts
}

// launchesBrowser reports whether the configured mode starts a browser of its
// own (chrome-attached and http do not).
func (t *WebFetchTool) launchesBrowser() bool {
	switch t.cfg.Mode {
	case utils.FetchModeHTTP, utils.FetchModeChromeAttached:
		return false
	default:
		return true
	}
}

// headfulWindow reports whether the configured mode drives a browser with a
// visible window. Auto and chrome-headful do: a browser with a window is the
// one that gets blocked the least, which is why it is the default.
func (t *WebFetchTool) headfulWindow() bool {
	switch t.cfg.Mode {
	case utils.FetchModeHTTP, utils.FetchModeChromeHeadless, utils.FetchModeChromeAttached:
		return false
	default:
		return true
	}
}

