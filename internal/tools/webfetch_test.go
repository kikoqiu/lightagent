package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lightagent/internal/utils"
)

// webfetchPageServer serves one page with a heading, a paragraph, a script and
// an ad-like block, so the conversion has something to simplify.
func webfetchPageServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html><html><head><title>Doc</title></head><body>
<h1>Getting started</h1><p>Install it and run it.</p>
<div class="ad">Buy now!</div>
<script>document.title = 'changed';</script>
</body></html>`)
	}))
	t.Cleanup(server.Close)
	return server
}

// webfetchLongPageServer serves a page whose markdown is longer than the small
// feedback limits the tests configure.
func webfetchLongPageServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!DOCTYPE html><html><body>")
		for i := 1; i <= 60; i++ {
			fmt.Fprintf(w, "<h2>Section %d</h2><p>Paragraph number %d of the page.</p>", i, i)
		}
		fmt.Fprint(w, "</body></html>")
	}))
	t.Cleanup(server.Close)
	return server
}

// webfetchBinaryServer serves content that is not a page.
func webfetchBinaryServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write([]byte("PK\x03\x04 archive bytes"))
	}))
	t.Cleanup(server.Close)
	return server
}

// newMarkdownWebFetchTool builds the tool in HTTP mode, so a test never depends
// on a browser being installed on the machine that runs it.
func newMarkdownWebFetchTool(cfg WebFetchConfig) *WebFetchTool {
	cfg.Mode = utils.FetchModeHTTP
	return NewWebFetchTool(cfg)
}

// TestWebFetchToolConvertsPageToMarkdown pins the answer the tool returns: the
// status line, the separator and the markdown the converter produced.
func TestWebFetchToolConvertsPageToMarkdown(t *testing.T) {
	server := webfetchPageServer(t)
	tool := newMarkdownWebFetchTool(WebFetchConfig{Timeout: 10 * time.Second})

	res := tool.Execute(context.Background(), map[string]any{"url": server.URL + "/docs"})
	if res.IsError {
		t.Fatalf("Execute reported an error: %s", res.ForLLM)
	}
	if !strings.HasPrefix(res.ForLLM, "Conversion succeeded. Converter warnings (if any): ") {
		t.Errorf("the answer is missing the status line:\n%s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "\n---\n\n") {
		t.Errorf("the answer is missing the body separator:\n%s", res.ForLLM)
	}
	// The answer carries no wrapper of its own: it is the tool's return value,
	// recorded as the tool message of the call.
	if got := strings.TrimSpace(res.ForLLM); strings.HasPrefix(got, "<") && strings.HasSuffix(got, ">") {
		t.Errorf("the tool wrapped its own answer:\n%s", res.ForLLM)
	}
	for _, want := range []string{"# Getting started", "Install it and run it."} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("the markdown is missing %q:\n%s", want, res.ForLLM)
		}
	}
	if strings.Contains(res.ForLLM, "document.title") {
		t.Errorf("the script should not reach the model:\n%s", res.ForLLM)
	}
	if !strings.Contains(res.ForUser, server.URL+"/docs") {
		t.Errorf("ForUser = %q, want the fetched address", res.ForUser)
	}
	// A page that fits carries no overflow note at all.
	if strings.Contains(res.ForLLM, "feedback limit") {
		t.Errorf("a page within the limit must not be reported as cut:\n%s", res.ForLLM)
	}
}

// TestWebFetchToolKeepsTheDefaultForANullTimeout pins that an explicit null is
// read as "use the configured timeout" instead of as an invalid value.
func TestWebFetchToolKeepsTheDefaultForANullTimeout(t *testing.T) {
	server := webfetchPageServer(t)
	tool := newMarkdownWebFetchTool(WebFetchConfig{Timeout: 10 * time.Second})
	res := tool.Execute(context.Background(), map[string]any{"url": server.URL, "timeout": nil})
	if res.IsError {
		t.Fatalf("Execute reported an error: %s", res.ForLLM)
	}
}

func TestWebFetchToolRejectsInvalidArguments(t *testing.T) {
	tool := newMarkdownWebFetchTool(WebFetchConfig{Timeout: 5 * time.Second})
	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing url", map[string]any{}},
		{"non-string url", map[string]any{"url": 42}},
		{"blank url", map[string]any{"url": "   "}},
		{"zero timeout", map[string]any{"url": "https://example.com", "timeout": 0}},
		{"negative timeout", map[string]any{"url": "https://example.com", "timeout": -1}},
	}
	for _, testCase := range cases {
		res := tool.Execute(context.Background(), testCase.args)
		if !res.IsError {
			t.Errorf("%s: Execute = %+v, want an error result", testCase.name, res)
		}
	}
}

// TestWebFetchToolDefaults pins the fallbacks of a tool built from a config that
// leaves everything out.
func TestWebFetchToolDefaults(t *testing.T) {
	tool := NewWebFetchTool(WebFetchConfig{})
	if tool.cfg.Mode != utils.FetchModeAuto {
		t.Errorf("mode = %q, want %q", tool.cfg.Mode, utils.FetchModeAuto)
	}
	if tool.cfg.Timeout != webFetchTimeoutDefault {
		t.Errorf("timeout = %s, want %s", tool.cfg.Timeout, webFetchTimeoutDefault)
	}
	if tool.cfg.MaxLines != WebFetchMaxLinesDefault || WebFetchMaxLinesDefault != 200 {
		t.Errorf("max lines = %d, want the built-in %d", tool.cfg.MaxLines, WebFetchMaxLinesDefault)
	}
}

// TestWebFetchToolDescriptionFollowsTheMode pins that the model is told how the
// pages are actually obtained — a tool pinned to the source must not advertise a
// browser — and what shape the answer has.
func TestWebFetchToolDescriptionFollowsTheMode(t *testing.T) {
	const automatic = "rendered in a visible browser window when a browser can be started"
	cases := []struct {
		mode utils.FetchMode
		want string
	}{
		{"", automatic},
		{utils.FetchModeAuto, automatic},
		{utils.FetchModeChromeHeadful, "rendered in a visible browser window."},
		{utils.FetchModeChromeHeadless, "rendered in a headless browser."},
		{utils.FetchModeChromeAttached, "rendered through a browser that is already running"},
		{utils.FetchModeHTTP, "downloaded over HTTP, without a browser."},
	}
	for _, tc := range cases {
		tool := NewWebFetchTool(WebFetchConfig{Mode: tc.mode})
		if !strings.Contains(tool.Description(), tc.want) {
			t.Errorf("Description() for mode %q = %q, want it to contain %q",
				tc.mode, tool.Description(), tc.want)
		}
	}
	if !strings.Contains(NewWebFetchTool(WebFetchConfig{}).Description(), "binary content") {
		t.Error("the description must state that binary content is refused")
	}
	if !strings.Contains(NewWebFetchTool(WebFetchConfig{MaxLines: 20}).Description(), "at most 20 lines") {
		t.Error("the description must state the feedback limit")
	}
	if !strings.Contains(NewWebFetchTool(WebFetchConfig{MaxLines: -1}).Description(), "whole markdown") {
		t.Error("a tool without a limit must say so instead of promising a cut")
	}
}

// TestWebFetchToolAppliesTheConfiguredWebSettings pins the settings a fetch
// carries: the configured user agent reaches the server and an oversized source
// is read only up to the configured byte cap.
func TestWebFetchToolAppliesTheConfiguredWebSettings(t *testing.T) {
	var agent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent = r.UserAgent()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html><html><body><h1>Headline</h1><p>Keep me.</p>`+
			strings.Repeat("<p>filler that the cap should cut away</p>", 200)+`</body></html>`)
	}))
	t.Cleanup(server.Close)

	tool := newMarkdownWebFetchTool(WebFetchConfig{
		Timeout:   10 * time.Second,
		UserAgent: "lightagent-test/1.0",
		MaxBytes:  512,
	})
	res := tool.Execute(context.Background(), map[string]any{"url": server.URL})
	if res.IsError {
		t.Fatalf("Execute reported an error: %s", res.ForLLM)
	}
	if agent != "lightagent-test/1.0" {
		t.Errorf("the server saw user agent %q, want the configured one", agent)
	}
	if !strings.Contains(res.ForUser, "truncated at 512 bytes") {
		t.Errorf("ForUser = %q, want the configured byte cap reported", res.ForUser)
	}
	if !strings.Contains(res.ForLLM, "Headline") {
		t.Errorf("the content before the cap is missing:\n%s", res.ForLLM)
	}
}

func TestWebFetchToolReportsFetchFailures(t *testing.T) {
	tool := newMarkdownWebFetchTool(WebFetchConfig{Timeout: 3 * time.Second})
	res := tool.Execute(context.Background(), map[string]any{"url": "127.0.0.1:1/unreachable"})
	if !res.IsError || !strings.Contains(res.ForLLM, "Fetch failed:") {
		t.Fatalf("Execute = %+v, want a fetch failure", res)
	}
}

// TestWebFetchToolRefusesContentThatIsNotAPage pins that an address serving
// something binary is an error that names the content and points the model at a
// command, instead of being converted into nonsense.
func TestWebFetchToolRefusesContentThatIsNotAPage(t *testing.T) {
	server := webfetchBinaryServer(t)
	tool := newMarkdownWebFetchTool(WebFetchConfig{Timeout: 10 * time.Second})
	res := tool.Execute(context.Background(), map[string]any{"url": server.URL + "/archive.zip"})
	if !res.IsError {
		t.Fatalf("Execute = %+v, want a refusal", res)
	}
	for _, want := range []string{"not a web page", "application/zip", "exec_command"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("the refusal is missing %q:\n%s", want, res.ForLLM)
		}
	}
}

// TestWebFetchToolCutsLongFeedbackAndSavesThePage pins the feedback limit: the
// markdown of a long page is cut, the whole page is written below .lightagent in
// the working directory, and the answer reports the totals and the path.
func TestWebFetchToolCutsLongFeedbackAndSavesThePage(t *testing.T) {
	t.Chdir(t.TempDir())
	server := webfetchLongPageServer(t)
	tool := newMarkdownWebFetchTool(WebFetchConfig{Timeout: 10 * time.Second, MaxLines: 5})

	res := tool.Execute(context.Background(), map[string]any{"url": server.URL})
	if res.IsError {
		t.Fatalf("Execute reported an error: %s", res.ForLLM)
	}
	// The status line before the separator explains the cut; the body itself is
	// the first five lines of the markdown.
	status, body, found := strings.Cut(res.ForLLM, "\n---\n\n")
	if !found {
		t.Fatalf("the answer is missing the body separator:\n%s", res.ForLLM)
	}
	for _, want := range []string{"longer than the 5 line feedback limit", "lines /", "bytes"} {
		if !strings.Contains(status, want) {
			t.Errorf("the status line is missing %q:\n%s", want, status)
		}
	}
	if got := countLines(body); got != 5 {
		t.Errorf("the body holds %d lines, want the configured 5:\n%s", got, body)
	}
	if strings.Contains(body, "Section 60") {
		t.Errorf("the body should hold the beginning of the page only:\n%s", body)
	}
	matches, err := filepath.Glob(filepath.Join(".lightagent", "webfetch-*.html"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("files under .lightagent = %v, want exactly one overflow file", matches)
	}
	if !strings.Contains(status, matches[0]) {
		t.Errorf("the status line does not name the file %s:\n%s", matches[0], status)
	}
	if !strings.Contains(res.ForUser, matches[0]) {
		t.Errorf("ForUser = %q, want the saved page named", res.ForUser)
	}
	saved, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read the overflow file: %v", err)
	}
	if !strings.Contains(string(saved), "<h2>Section 60</h2>") {
		t.Error("the overflow file must hold the whole page")
	}
}

// TestWebFetchToolKeepsTheWholePageWithoutALimit pins the escape hatch: a
// negative max_lines feeds the whole markdown back and writes nothing.
func TestWebFetchToolKeepsTheWholePageWithoutALimit(t *testing.T) {
	t.Chdir(t.TempDir())
	server := webfetchLongPageServer(t)
	tool := newMarkdownWebFetchTool(WebFetchConfig{Timeout: 10 * time.Second, MaxLines: -1})

	res := tool.Execute(context.Background(), map[string]any{"url": server.URL})
	if res.IsError {
		t.Fatalf("Execute reported an error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Section 60") {
		t.Errorf("the whole page must reach the model:\n%s", res.ForLLM)
	}
	if _, err := os.Stat(".lightagent"); !os.IsNotExist(err) {
		t.Errorf("no overflow file may be written without a limit (stat: %v)", err)
	}
}

// TestWebFetchToolBrowserProfileIsLocal pins where a launched browser keeps its
// profile: in the agent's directory below the working directory, next to the
// pages a fetch saved, not in the user's home.
func TestWebFetchToolBrowserProfileIsLocal(t *testing.T) {
	t.Chdir(t.TempDir())
	tool := NewWebFetchTool(WebFetchConfig{})

	var opts utils.WebFetchOptions
	for _, opt := range tool.fetchOptions(5 * time.Second) {
		opt(&opts)
	}
	want := filepath.Join(".lightagent", "browser-profile")
	if !strings.HasSuffix(opts.Browser.UserDataDir, want) {
		t.Errorf("profile = %q, want it to end in %q", opts.Browser.UserDataDir, want)
	}
	if !filepath.IsAbs(opts.Browser.UserDataDir) {
		t.Errorf("profile = %q, want an absolute path", opts.Browser.UserDataDir)
	}
	if cache, err := os.UserCacheDir(); err == nil && cache != "" &&
		strings.HasPrefix(opts.Browser.UserDataDir, cache) {
		t.Errorf("profile = %q, want it out of the user's cache directory", opts.Browser.UserDataDir)
	}
}

// TestWebFetchToolWiresTheBrowserMode pins what each mode asks the fetcher for:
// auto and chrome-headful drive a browser with a visible window on a profile of
// the agent's own (never the user's default one, which is what chrome-attached
// is for), chrome-headless one without, and chrome-attached the endpoint from
// the configuration.
func TestWebFetchToolWiresTheBrowserMode(t *testing.T) {
	cases := []struct {
		mode    utils.FetchMode
		headful bool
		address string
	}{
		{utils.FetchModeAuto, true, ""},
		{utils.FetchModeChromeHeadful, true, ""},
		{utils.FetchModeChromeHeadless, false, ""},
		{utils.FetchModeChromeAttached, false, "127.0.0.1:9333"},
		{utils.FetchModeHTTP, false, ""},
	}
	for _, tc := range cases {
		tool := NewWebFetchTool(WebFetchConfig{Mode: tc.mode, AttachEndpoint: "127.0.0.1:9333"})
		var opts utils.WebFetchOptions
		for _, opt := range tool.fetchOptions(5 * time.Second) {
			opt(&opts)
		}
		if opts.Mode != tc.mode {
			t.Errorf("mode %q: fetcher mode = %q", tc.mode, opts.Mode)
		}
		if opts.Browser.Headful != tc.headful {
			t.Errorf("mode %q: headful = %v, want %v", tc.mode, opts.Browser.Headful, tc.headful)
		}
		if opts.Browser.Address != tc.address {
			t.Errorf("mode %q: attach address = %q, want %q", tc.mode, opts.Browser.Address, tc.address)
		}
		if got := opts.Browser.UserDataDir != ""; got != tc.headful {
			t.Errorf("mode %q: own profile = %v, want %v", tc.mode, got, tc.headful)
		}
	}
}
