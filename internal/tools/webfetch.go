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
	// when nothing else is configured. A page that does not fit is answered
	// with its main content (or its middle) and saved to disk instead (see
	// lightagentDir).
	WebFetchMaxLinesDefault = 100
	// bodySeparator divides the tool's own status line from the markdown it
	// carries. The answer is the tool's return value, recorded as an ordinary
	// tool message like every other tool's; nothing wraps it.
	bodySeparator = "---"
	// lightagentDir is the directory the agent keeps its state in, below the
	// working directory: the pages a fetch writes, and the browser profile of a
	// launch of its own.
	lightagentDir = ".lightagent"
	// webFetchDirName is the directory below lightagentDir that holds what a
	// fetch writes: the whole page of one that was too long to feed back, and
	// the page a call asked to save. A directory of its own keeps those pages
	// together and apart from the other state of the agent.
	webFetchDirName = "webfetch"
	// webFetchDirPath is that directory as the tool speaks of it in its own
	// messages: below the working directory, where the model is told the page
	// was put.
	webFetchDirPath = lightagentDir + "/" + webFetchDirName
	// webFetchMarkdownSuffix is the extension of a file that holds markdown
	// (the whole page of a long fetch, or the page a call asked to save).
	webFetchMarkdownSuffix = ".md"
	// webFetchHTMLSuffix is the extension of a file that holds HTML.
	webFetchHTMLSuffix = ".html"
	// webFetchFileTimeFormat is the timestamp layout a saved page is named
	// after, which is its whole name below webFetchDirName: the directory says
	// what the file holds.
	webFetchFileTimeFormat = "20060102-150405"
	// webFetchFileNameAttempts bounds the search for a free file name when
	// several fetches land in the same second.
	webFetchFileNameAttempts = 100
	// browserProfileName is the browser profile a launch of our own uses, below
	// lightagentDir.
	browserProfileName = "browser-profile"
	// nonPageHint tells the model what to do instead of fetching again when the
	// address does not serve a page.
	nonPageHint = "webfetch reads pages and text documents only; download or convert binary content " +
		"(PDF, images, archives, ...) with run_script instead."
	// webFetchInvokeHeader opens the block a script's value is reported under,
	// at the very top of the answer: the caller asked for that value, and it
	// belongs before the page it was read from.
	webFetchInvokeHeader = "invokejs return info:"
)

// webFetchMethod is what a call asks the answer to carry: the markdown itself
// (the default), the page written to a file instead of the text, or nothing but
// the status line — which is what a call that only wants the side effect of a
// fetch (a saved file it will read itself, a script's value) asks for.
type webFetchMethod string

const (
	// webFetchMethodMarkdown puts the markdown of the page in the answer
	// (cut to the feedback limit: see limitFeedback).
	webFetchMethodMarkdown webFetchMethod = "fetch_as_md"
	// webFetchMethodSaveHTML writes the fetched HTML to a file and reports its
	// path and size instead of the content.
	webFetchMethodSaveHTML webFetchMethod = "save_as_html"
	// webFetchMethodSaveMarkdown writes the markdown of the whole page to a
	// file and reports its path and size instead of the content.
	webFetchMethodSaveMarkdown webFetchMethod = "save_as_md"
	// webFetchMethodIgnore reports only that the page was fetched, keeping its
	// content out of the answer.
	webFetchMethodIgnore webFetchMethod = "ignore"
)

// webFetchMethods lists the methods a call may ask for, in the order the
// schema and the error messages speak of them.
var webFetchMethods = []webFetchMethod{
	webFetchMethodMarkdown,
	webFetchMethodSaveHTML,
	webFetchMethodSaveMarkdown,
	webFetchMethodIgnore,
}

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
		t.feedbackRule() + t.linkRule() + t.sideEffectRule() +
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
// page arrives cut away from its chrome (or cut in the middle when the converter
// located no main content), and that the whole page is on disk.
func (t *WebFetchTool) feedbackRule() string {
	if t.cfg.MaxLines < 0 {
		return "With the default method the answer carries the whole markdown of the page. "
	}
	return fmt.Sprintf("With the default method the answer carries a status line and at most %d lines of "+
		"markdown. A longer page is answered with its main content — the article or main section of the page, "+
		"with navigation, sidebars and footers left out — or, when no main content can be located, with the "+
		"middle of the page; either way the answer marks what it left out and where the content starts, and "+
		"the whole page is saved as markdown in the %s directory of the working directory, with the path "+
		"reported. ", t.cfg.MaxLines, webFetchDirPath)
}

// linkRule tells the model how the addresses inside the markdown are written, so
// a path it reads is understood as a link into the site of the fetched page
// rather than as an address of its own, and so a payload that was left out is
// understood for what it is.
func (t *WebFetchTool) linkRule() string {
	return "Links that stay on the site of the fetched page are written as paths from its root " +
		"(such as /docs/page), links to other sites keep their absolute address. An address that " +
		"carries its content inside it — a data URI, which is how a page inlines an image — is " +
		"replaced by a short marker with the type and the size of what was left out " +
		"(data:image/png;base64,omitted-1.2MiB), since the payload is far larger than the page " +
		"around it and nothing outside the page can use it. "
}

// sideEffectRule states what else a call can ask for besides the markdown: the
// page kept on disk instead of in the answer (method), and — for a tool that
// renders — a script run in the loaded page (invokejs), which is how a fetch is
// used for what only the page and a browser can give (a cookie, a token). The
// details are in the descriptions of the arguments, which is where the model
// reads them.
func (t *WebFetchTool) sideEffectRule() string {
	rule := "The method argument decides what the answer carries: the markdown itself (the default), the page " +
		"saved to a file whose path and size are reported instead of the content, or nothing but the status " +
		"line. "
	if t.rendersPages() {
		rule += "The invokejs argument runs a script of yours in the loaded page before its content is read, " +
			"which is how values only the page's JavaScript can reach — cookies, tokens — are handed back " +
			"for later commands. "
	}
	return rule
}

// Parameters implements Tool. The timeout the model reads states the value the
// system is configured with (tools.webfetch.timeout_seconds) as well as the
// floor every fetch has, so an argument can never promise less than the fetch
// will actually get. A tool that never renders leaves the argument for a script
// out of the schema: it could only ever be refused.
func (t *WebFetchTool) Parameters() map[string]any {
	configured := webFetchSeconds(t.cfg.Timeout)
	floor := webFetchSeconds(webFetchTimeoutDefault)
	properties := map[string]any{
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
		"method": map[string]any{
			"type":    "string",
			"enum":    webFetchMethodNames(),
			"default": string(webFetchMethodMarkdown),
			"description": "What the answer carries. fetch_as_md (default): the markdown of the page, at " +
				"most the configured number of lines, with the whole page saved next to it. save_as_md: " +
				"the markdown of the whole page written to a file in the working directory — the answer " +
				"reports the path, the line count and the size instead of the text, and you read the file " +
				"with read_file. save_as_html: the same, with the HTML the fetch got (the DOM the " +
				"browser built, or the source over HTTP). ignore: nothing but the status line — use it " +
				"when the call is made for its side effects (a script's value, a saved file, warming a " +
				"session) or when the page is already known.",
		},
		"invokejs": map[string]any{
			"type": "string",
			"description": "JavaScript to run in the page once it has loaded and before its content is " +
				"read. The script must call the global function _invokejs_done(value) to end the call: " +
				"the value is reported at the top of the answer as \"" + webFetchInvokeHeader + "\", a " +
				"value of undefined reports nothing, and the wait for it replaces the usual wait for the " +
				"page to stop loading in the background — so fetch what you need inside the script " +
				"(await it), or return at once. A script that throws is reported at once; one that never " +
				"calls the function holds the fetch until its timeout runs out, so always call it. Use " +
				"it to read what only the page's own JavaScript can reach — document.cookie, a token in " +
				"localStorage, a value the framework holds — and hand it back so a later command can use " +
				"the same credentials as the browser. It needs the page a browser builds: a fetch that " +
				"finds no browser fails instead of falling back to the source. Keep the value small (a " +
				"header, a token): it is cut at 8 KiB.",
		},
	}
	if !t.rendersPages() {
		delete(properties, "invokejs")
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   []string{"url"},
	}
}

// webFetchMethodNames lists the methods the schema offers, as strings.
func webFetchMethodNames() []string {
	names := make([]string, 0, len(webFetchMethods))
	for _, method := range webFetchMethods {
		names = append(names, string(method))
	}
	return names
}

// Execute implements Tool.
func (t *WebFetchTool) Execute(ctx context.Context, args map[string]any) *Result {
	rawURL, ok := stringArg(args, "url")
	if !ok || strings.TrimSpace(rawURL) == "" {
		return Fail("Missing or invalid 'url' argument. Must be a non-empty string.")
	}
	method, errText := webFetchMethodArg(args)
	if errText != "" {
		return Fail(errText)
	}
	script, errText := t.invokeJSArg(args)
	if errText != "" {
		return Fail(errText)
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

	page, err := utils.WebFetch(ctx, rawURL, t.fetchOptions(timeout, script)...)
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

	// The markdown is what two of the four methods carry, and where the numbers
	// of the display line come from; save_as_html and ignore ask for nothing
	// but the fetch itself, so neither of them converts anything.
	var converted utils.Html2MdConvertResult
	markdown := ""
	if method == webFetchMethodMarkdown || method == webFetchMethodSaveMarkdown {
		// The page's own links become paths of its own site, and the converter
		// is asked for the part of the page that holds its content: the answer
		// carries that part rather than the chrome around it (see
		// limitFeedback).
		converted, err = utils.Html2MdConvert(page.HTML,
			utils.WithBaseURL(page.FinalURL),
			utils.WithRelativeLinks(true),
			utils.WithMainContentSelection(true))
		if err != nil {
			return Fail(fmt.Sprintf("Fetch succeeded (%s) but the page could not be converted to markdown: %v",
				page.FinalURL, err))
		}
		markdown = strings.TrimSpace(converted.Markdown)
		if markdown == "" {
			return Fail(fmt.Sprintf("Fetch succeeded (%s) but the page holds no readable content to convert.",
				page.FinalURL))
		}
	}

	status, body, display, savedTo, failure := t.answer(method, page, converted, markdown)
	if failure != "" {
		// The page was fetched but not kept: the call did not do what it was
		// asked to do, so the answer is an error — carrying whatever the script
		// reported, which the caller can still use.
		return &Result{ForLLM: invokeBlock(page) + failure, IsError: true, ForUser: display}
	}
	res := &Result{
		// The tool returns its own text, which the agent records as the tool
		// message of the call like any other tool's answer. What the script of
		// the call reported comes first, since that is what it went out for.
		ForLLM:  invokeBlock(page) + status,
		ForUser: display,
	}
	if body != "" {
		res.ForLLM += "\n" + bodySeparator + "\n\n" + body
	}
	if savedTo != "" {
		res.ForUser += "; saved to " + savedTo
	}
	if page.InvokeNote != "" {
		res.ForUser += "\ninvokejs: " + page.InvokeNote
	} else if page.InvokeInfo != "" {
		res.ForUser += fmt.Sprintf("\ninvokejs: the script reported %d chars, carried at the top of the result",
			len(page.InvokeInfo))
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

// answer builds the five parts a method asks for: the status line the model
// reads, the body below it (empty for every method but the default one), the
// display line of the caller, the path of a file the page was written to, and
// the failure of a page that could not be written — which the caller reports as
// an error, since the call did not do what it was asked to do.
func (t *WebFetchTool) answer(method webFetchMethod, page utils.WebFetchResult,
	converted utils.Html2MdConvertResult, markdown string) (status, body, display, savedTo, failure string) {
	switch method {
	case webFetchMethodSaveHTML:
		// The file is written to be read: an inline data URI (a base64 image, a
		// font, a whole document) is the one thing that would not fit in a
		// context, so its payload is replaced by the short marker that stands
		// for it — the same marker the markdown carries (see utils.OmitDataURIs).
		html, omitted, omittedBytes := utils.OmitDataURIs(page.HTML)
		path, err := saveFetchedContent(html, webFetchHTMLSuffix)
		if err != nil {
			return "", "", fetchedAs(page, "HTML", 0, len(html)), "", fmt.Sprintf(
				"Fetch succeeded (%s) but the page could not be saved: %v", page.FinalURL, err)
		}
		message := fmt.Sprintf("Saved %s as HTML to %s (%d chars); its content is not repeated here (method %s) — read it with the file tools if it is needed.",
			page.FinalURL, path, len(html), method)
		if omitted > 0 {
			message += fmt.Sprintf(" %d inline data URIs were replaced by short markers (%s of payload left out).",
				omitted, humanBytes(int64(omittedBytes)))
		}
		return message, "", fetchedAs(page, "HTML", 0, len(html)), path, ""

	case webFetchMethodSaveMarkdown:
		path, err := saveFetchedContent(markdown, webFetchMarkdownSuffix)
		if err != nil {
			return "", "", fetchedAs(page, "markdown", countLines(markdown), len(markdown)), "", fmt.Sprintf(
				"Fetch succeeded (%s) but the page could not be saved: %v", page.FinalURL, err)
		}
		message := fmt.Sprintf("Saved %s as markdown to %s (%d lines / %d chars); its content is not repeated here (method %s) — read it with read_file if it is needed.",
			page.FinalURL, path, countLines(markdown), len(markdown), method)
		return message, "", fetchedAs(page, "markdown", countLines(markdown), len(markdown)), path, ""

	case webFetchMethodIgnore:
		return fmt.Sprintf("Fetched %s (%s); its content is not returned (method %s).",
				page.FinalURL, describeContentType(page.ContentType), method),
			"", fetchedAs(page, "HTML", 0, len(page.HTML)) + "; the content was not returned", "", ""
	}

	// The default method: the markdown itself, cut to the feedback limit.
	warnings := "none"
	if len(converted.Warnings) > 0 {
		warnings = strings.Join(converted.Warnings, "; ")
	}
	// The source address is part of the status because the markdown links
	// relative to it: the model needs the page it came from to follow them.
	body, overflow, path := t.limitFeedback(markdown, converted.Content)
	status = fmt.Sprintf("Conversion succeeded (source: %s). Converter warnings (if any): %s",
		page.FinalURL, warnings)
	if overflow != "" {
		status += "\n" + overflow
	}
	display = fetchedAs(page, "markdown", countLines(markdown), len(markdown))
	if overflow != "" {
		display += fmt.Sprintf("\nFeedback cut at %d lines", t.cfg.MaxLines)
	}
	return status, body, display, path, ""
}

// fetchedAs is the display line of a fetch: what was read, how and how long it
// took.
func fetchedAs(page utils.WebFetchResult, kind string, lines, chars int) string {
	if kind == "HTML" {
		return fmt.Sprintf("Fetched %s as %s (%s, %d chars, %.1fs)",
			page.FinalURL, kind, page.Method, chars, page.Elapsed.Seconds())
	}
	return fmt.Sprintf("Fetched %s as %s (%s, %d lines, %d chars, %.1fs)",
		page.FinalURL, kind, page.Method, lines, chars, page.Elapsed.Seconds())
}

// invokeBlock renders what the script of a call reported, as the first thing the
// answer carries: the value is what the call went out for, and it belongs before
// the page it was read from. A script that reported nothing is not spoken of at
// all; one that failed or ran out of time is, since the caller has to learn that
// the value it wanted is missing.
func invokeBlock(page utils.WebFetchResult) string {
	if page.InvokeInfo != "" {
		return webFetchInvokeHeader + "\n" + page.InvokeInfo + "\n\n"
	}
	if page.InvokeNote != "" {
		return webFetchInvokeHeader + " none — " + page.InvokeNote + "\n\n"
	}
	return ""
}

// webFetchMethodArg reads the method argument of a call.
func webFetchMethodArg(args map[string]any) (webFetchMethod, string) {
	raw, present := args["method"]
	if !present || raw == nil {
		return webFetchMethodMarkdown, ""
	}
	text, ok := raw.(string)
	if !ok {
		return "", fmt.Sprintf("Invalid 'method' argument. Must be one of %s.",
			strings.Join(webFetchMethodNames(), ", "))
	}
	if strings.TrimSpace(text) == "" {
		return webFetchMethodMarkdown, ""
	}
	method := webFetchMethod(strings.ToLower(strings.TrimSpace(text)))
	for _, known := range webFetchMethods {
		if method == known {
			return method, ""
		}
	}
	return "", fmt.Sprintf("Invalid 'method' argument %q. Must be one of %s.",
		text, strings.Join(webFetchMethodNames(), ", "))
}

// invokeJSArg reads the invokejs argument of a call: the script to run in the
// loaded page. Only a browser can run it, so a tool that never renders refuses
// it here, where the message can say why, instead of fetching and failing.
func (t *WebFetchTool) invokeJSArg(args map[string]any) (string, string) {
	raw, present := args["invokejs"]
	if !present || raw == nil {
		return "", ""
	}
	text, ok := raw.(string)
	if !ok {
		return "", "Invalid 'invokejs' argument. Must be a string of JavaScript."
	}
	script := strings.TrimSpace(text)
	if script == "" {
		return "", ""
	}
	if !t.rendersPages() {
		return "", fmt.Sprintf("Invalid 'invokejs' argument: a script needs a browser to run in, and this "+
			"tool reads pages over HTTP only (tools.webfetch.mode is %q).", t.cfg.Mode)
	}
	return script, ""
}

// limitFeedback cuts the markdown to the configured number of lines. A page that
// does not fit is answered with its main content when the converter located one,
// and with the middle of its markdown otherwise; either way the lines left out
// are replaced by a marker that says how many they were, and the note — part of
// the status line the answer carries — reports the totals, the choice and where
// the whole page went, so the model can read it in pieces with read_file
// instead of losing it; savedTo is that path, for the display line of the
// caller. A page that fits, or a tool that asks for no limit at all, is
// returned as it is, with an empty note.
func (t *WebFetchTool) limitFeedback(markdown string, content utils.ContentRegion) (body, note, savedTo string) {
	if t.cfg.MaxLines < 0 {
		return markdown, "", ""
	}
	total := countLines(markdown)
	if total <= t.cfg.MaxLines {
		return markdown, "", ""
	}

	limit := t.cfg.MaxLines
	var choice string
	if content.Located && content.Markdown != "" && content.StartLine >= 1 && content.LineCount >= 1 {
		body, choice = locatedFeedback(markdown, total, limit, content)
	} else {
		body, choice = middleFeedback(markdown, total, limit)
	}

	// The whole markdown is saved, not the HTML it came from: the file holds
	// the page the answer was cut from, so reading it back needs no second
	// conversion and its size is the one the note reports.
	head := fmt.Sprintf("The page is longer than the %d line feedback limit: it holds %d lines / %d bytes in total",
		limit, total, len(markdown))
	path, err := saveFetchedContent(markdown, webFetchMarkdownSuffix)
	if err != nil {
		return body, fmt.Sprintf("%s; %s; saving the whole page failed: %v", head, choice, err), ""
	}
	return body, fmt.Sprintf("%s; %s. The whole page was saved as markdown to %s — "+
		"read it with read_file if the rest is needed.", head, choice, path), path
}

// locatedFeedback builds the answer of a page whose main content was located:
// that region is what it carries, the lines above and below it are replaced by a
// marker each, and a cut inside the region is marked as well — which matters,
// because the lines the answer then shows no longer line up with the page's own
// numbering, and the marker says where they start.
func locatedFeedback(markdown string, total, limit int, content utils.ContentRegion) (body, choice string) {
	start := content.StartLine
	regionLines := content.LineCount
	if end := start + regionLines - 1; end > total {
		regionLines = total - start + 1
	}
	if regionLines < 1 {
		// A region that reaches past the markdown is no region at all.
		return middleFeedback(markdown, total, limit)
	}
	kept := regionLines
	if kept > limit {
		kept = limit
	}
	window := strings.Split(sliceLines(markdown, start, kept), "\n")
	// A cut that lands on a blank line would end the answer on nothing: the
	// line goes back to the rest the marker stands for.
	for len(window) > 1 && strings.TrimSpace(window[len(window)-1]) == "" {
		window = window[:len(window)-1]
	}
	kept = len(window)

	var b strings.Builder
	if before := start - 1; before > 0 {
		b.WriteString(omittedLines("above", before, total, "navigation, sidebars or banner"))
		// The marker is a line of its own: without the break it would run into
		// the first line of the content it introduces.
		b.WriteByte('\n')
	}
	b.WriteString(strings.Join(window, "\n"))
	if omitted := regionLines - kept; omitted > 0 {
		b.WriteString(fmt.Sprintf("\n...(the main content continues: %d of %d lines omitted, the feedback limit is %d lines)",
			omitted, regionLines, limit))
	}
	if after := total - (start + regionLines - 1); after > 0 {
		b.WriteString("\n" + omittedLines("below", after, total, "footer, related links or comments"))
	}

	if regionLines > kept {
		choice = fmt.Sprintf("main content (%s) was located at line %d and holds %d lines, response "+
			"carries its first %d lines and names the rest", content.Label, start, regionLines, kept)
	} else {
		choice = fmt.Sprintf("main content (%s) was located at line %d and holds %d lines, response "+
			"carries it whole, with the lines around it left out", content.Label, start, regionLines)
	}
	return b.String(), choice
}

// middleFeedback builds the answer of a page whose main content could not be
// located: the middle `limit` lines of its markdown, with the lines left out
// above and below marked.
func middleFeedback(markdown string, total, limit int) (body, choice string) {
	start := (total-limit)/2 + 1
	kept := total - start + 1
	if kept > limit {
		kept = limit
	}
	window := strings.Split(sliceLines(markdown, start, kept), "\n")
	// A window that opens or closes on a blank line starts on nothing: those
	// lines go back to the marker that stands for the lines around it.
	for len(window) > 1 && strings.TrimSpace(window[0]) == "" {
		window = window[1:]
		start++
	}
	for len(window) > 1 && strings.TrimSpace(window[len(window)-1]) == "" {
		window = window[:len(window)-1]
	}
	kept = len(window)

	var b strings.Builder
	if before := start - 1; before > 0 {
		b.WriteString(omittedLines("above", before, total, ""))
		b.WriteByte('\n')
	}
	b.WriteString(strings.Join(window, "\n"))
	if after := total - (start + kept - 1); after > 0 {
		b.WriteString("\n" + omittedLines("below", after, total, ""))
	}
	choice = fmt.Sprintf("no main content could be located, so the answer carries the middle %d lines (lines %d-%d)",
		kept, start, start+kept-1)
	return b.String(), choice
}

// omittedLines renders the line that stands in for the markdown an answer left
// out: how much of the page it was, and — when the cut is around a located main
// content — what such a stretch of a page usually holds. The line opens with an
// ellipsis, the English mark for text that was left out.
func omittedLines(where string, lines, total int, what string) string {
	if what == "" {
		return fmt.Sprintf("...(%s: %d of %d lines omitted)", where, lines, total)
	}
	return fmt.Sprintf("...(%s: %d of %d lines omitted — %s)", where, lines, total, what)
}

// sliceLines returns count lines of a text from the 1-based line start on (the
// lines that are there when the text ends first).
func sliceLines(text string, start, count int) string {
	if start < 1 || count < 1 {
		return ""
	}
	lines := strings.Split(text, "\n")
	if start > len(lines) {
		return ""
	}
	end := start - 1 + count
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start-1:end], "\n")
}

// saveFetchedContent writes a document below the working directory, in the
// directory a fetch keeps its pages in (.lightagent/webfetch), and returns the
// path of the file it created. The suffix is the extension the document wants
// (.md, .html) and the name is the moment of the fetch, so a sequence of them
// stays readable in the directory; the file is written exclusively, so two
// fetches within the same second produce two files instead of one overwriting
// the other.
func saveFetchedContent(content, suffix string) (string, error) {
	dir := filepath.Join(workingDir(), lightagentDir, webFetchDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	stamp := time.Now().Format(webFetchFileTimeFormat)
	for attempt := 0; attempt < webFetchFileNameAttempts; attempt++ {
		name := stamp + suffix
		if attempt > 0 {
			name = fmt.Sprintf("%s-%d%s", stamp, attempt, suffix)
		}
		path := filepath.Join(dir, name)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		_, writeErr := file.WriteString(content)
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
// fetcher apply. The script of the call, when there is one, travels with them.
func (t *WebFetchTool) fetchOptions(timeout time.Duration, script string) []utils.WebFetchOptionFunc {
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
	if script != "" {
		opts = append(opts, utils.WithFetchInvokeJS(script))
	}
	return opts
}

// rendersPages reports whether the configured mode reads a page a browser built,
// which is what a script of the caller needs to run in. The attached mode drives
// the browser of the user, so it renders as well.
func (t *WebFetchTool) rendersPages() bool {
	return t.cfg.Mode != utils.FetchModeHTTP
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
