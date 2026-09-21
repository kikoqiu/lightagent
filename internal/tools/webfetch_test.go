package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// TestWebFetchToolRaisesTimeoutsBelowTheFloor pins that the effective timeout
// never goes below the built-in floor: the configured timeout, and a timeout
// argument of a call, are raised when they are shorter, because a page cannot be
// loaded, rendered and converted in less.
func TestWebFetchToolRaisesTimeoutsBelowTheFloor(t *testing.T) {
	tool := newMarkdownWebFetchTool(WebFetchConfig{Timeout: 5 * time.Second})
	if tool.cfg.Timeout != webFetchTimeoutDefault {
		t.Errorf("configured timeout = %s, want it raised to %s", tool.cfg.Timeout, webFetchTimeoutDefault)
	}
	cases := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{"below the floor", time.Second, webFetchTimeoutDefault},
		{"zero", 0, webFetchTimeoutDefault},
		{"negative", -time.Minute, webFetchTimeoutDefault},
		{"the floor itself", webFetchTimeoutDefault, webFetchTimeoutDefault},
		{"above the floor", 2 * time.Minute, 2 * time.Minute},
	}
	for _, tc := range cases {
		if got := raiseWebFetchTimeout(tc.timeout); got != tc.want {
			t.Errorf("%s: raiseWebFetchTimeout(%s) = %s, want %s", tc.name, tc.timeout, got, tc.want)
		}
	}
}

// TestWebFetchToolRaisesATimeoutArgumentBelowTheFloor pins the same rule on the
// call itself: a fetch asked for one second against a page that takes longer
// still succeeds, because the argument was raised to the floor.
func TestWebFetchToolRaisesATimeoutArgumentBelowTheFloor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html><html><body><h1>Slow page</h1></body></html>`)
	}))
	t.Cleanup(server.Close)

	tool := newMarkdownWebFetchTool(WebFetchConfig{Timeout: time.Minute})
	res := tool.Execute(context.Background(), map[string]any{"url": server.URL, "timeout": 1})
	if res.IsError {
		t.Fatalf("Execute reported an error for a timeout below the floor: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Slow page") {
		t.Errorf("the page is missing from the answer:\n%s", res.ForLLM)
	}
}

// TestWebFetchToolAdvertisesTheConfiguredTimeout pins what the model reads about
// the timeout argument: the concrete seconds the system is configured with (and
// that the argument defaults to), plus the smallest value a fetch can actually
// run with, instead of a bare pointer at the setting.
func TestWebFetchToolAdvertisesTheConfiguredTimeout(t *testing.T) {
	timeoutParam := func(cfg WebFetchConfig) map[string]any {
		t.Helper()
		props, ok := newMarkdownWebFetchTool(cfg).Parameters()["properties"].(map[string]any)
		if !ok {
			t.Fatalf("the schema of %+v carries no properties", cfg)
		}
		param, ok := props["timeout"].(map[string]any)
		if !ok {
			t.Fatalf("the schema of %+v does not describe the timeout argument", cfg)
		}
		return param
	}

	param := timeoutParam(WebFetchConfig{Timeout: 45 * time.Second})
	if got := param["default"]; got != 45 {
		t.Errorf("default = %v, want the configured 45 seconds", got)
	}
	description, _ := param["description"].(string)
	for _, want := range []string{"45", "30", "tools.webfetch.timeout_seconds"} {
		if !strings.Contains(description, want) {
			t.Errorf("the timeout description is missing %q: %s", want, description)
		}
	}
	// A configured value below the floor is advertised as the value that is
	// used, not as what the file says.
	if got := timeoutParam(WebFetchConfig{Timeout: 5 * time.Second})["default"]; got != 30 {
		t.Errorf("default = %v, want the floor of 30 seconds for a shorter configured value", got)
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
// browser, and one that renders must name the Chrome that does it and say what a
// rendered answer carries — and what shape the answer has.
func TestWebFetchToolDescriptionFollowsTheMode(t *testing.T) {
	const automatic = "rendered by Chrome in a visible window when a browser can be started"
	cases := []struct {
		mode utils.FetchMode
		want string
	}{
		{"", automatic},
		{utils.FetchModeAuto, automatic},
		{utils.FetchModeChromeHeadful, "rendered by Chrome, in a visible window."},
		{utils.FetchModeChromeHeadless, "rendered by Chrome, in a headless instance with no window."},
		{utils.FetchModeChromeAttached, "rendered by Chrome through the browser the user is using"},
		{utils.FetchModeHTTP, "downloaded over HTTP and never rendered"},
	}
	for _, tc := range cases {
		tool := NewWebFetchTool(WebFetchConfig{Mode: tc.mode})
		if !strings.Contains(tool.Description(), tc.want) {
			t.Errorf("Description() for mode %q = %q, want it to contain %q",
				tc.mode, tool.Description(), tc.want)
		}
		if strings.Contains(tool.Description(), "  ") {
			t.Errorf("Description() for mode %q has a double space:\n%s", tc.mode, tool.Description())
		}
	}
	// A tool that renders says so in the terms of the browser it drives: Chrome,
	// its JavaScript run, its DOM handed back.
	for _, mode := range []utils.FetchMode{
		utils.FetchModeAuto, utils.FetchModeChromeHeadful, utils.FetchModeChromeHeadless,
	} {
		description := NewWebFetchTool(WebFetchConfig{Mode: mode}).Description()
		for _, want := range []string{"Chrome", "runs the JavaScript of the page", "hands back the DOM it built"} {
			if !strings.Contains(description, want) {
				t.Errorf("Description() for mode %q is missing %q:\n%s", mode, want, description)
			}
		}
	}
	// The attached mode says whose Chrome it is: the answer carries the logins,
	// cookies and sessions of the browser the user is using, which is the whole
	// point of that mode.
	attached := NewWebFetchTool(WebFetchConfig{Mode: utils.FetchModeChromeAttached}).Description()
	for _, want := range []string{"the browser the user is using", "logins, cookies and sessions"} {
		if !strings.Contains(attached, want) {
			t.Errorf("an attached tool is missing %q:\n%s", want, attached)
		}
	}
	// It speaks of the browser the user has open, not of how it is driven.
	if strings.Contains(attached, "DevTools") {
		t.Errorf("an attached tool must state the idea, not the protocol:\n%s", attached)
	}
	// And a tool pinned to the source never mentions a browser of any kind.
	fromSource := NewWebFetchTool(WebFetchConfig{Mode: utils.FetchModeHTTP}).Description()
	for _, unwanted := range []string{"Chrome", "JavaScript", "DOM", "browser"} {
		if strings.Contains(fromSource, unwanted) {
			t.Errorf("a tool pinned to the source must not mention %q:\n%s", unwanted, fromSource)
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
// markdown of a long page is cut, the whole markdown is written below .lightagent
// in the working directory, and the answer reports the totals and the path.
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
	matches, err := filepath.Glob(filepath.Join(".lightagent", "webfetch-*.md"))
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
	// The file holds the markdown of the whole page, not the HTML it came from:
	// the part the feedback cut away is there as markdown too.
	if !strings.Contains(string(saved), "## Section 60") {
		t.Error("the overflow file must hold the whole page as markdown")
	}
	if strings.Contains(string(saved), "<h2>") {
		t.Error("the overflow file must not hold the HTML source")
	}
	// It is the same text as the feedback, so it continues exactly where the
	// feedback was cut.
	if !strings.HasPrefix(string(saved), body) {
		t.Errorf("the overflow file must begin with the markdown that was fed back:\n%s", saved)
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

// TestWebFetchToolBrowserProfileIsLocal pins the profile of the tool: the modes
// that start a browser of their own — the headless one included — render on one
// directory below the agent's directory in the working directory, never on a
// temporary directory of the system or one of the user's home, so the cookies and
// logins of a fetch are there for the next one. A mode that starts no browser
// names no profile at all: http needs none, and chrome-attached drives the
// browser the user has open, whose profile is theirs.
func TestWebFetchToolBrowserProfileIsLocal(t *testing.T) {
	t.Chdir(t.TempDir())
	want := filepath.Join(".lightagent", "browser-profile")
	rendering := []utils.FetchMode{
		"", utils.FetchModeAuto, utils.FetchModeChromeHeadful, utils.FetchModeChromeHeadless,
	}
	for _, mode := range rendering {
		tool := NewWebFetchTool(WebFetchConfig{Mode: mode})
		var opts utils.WebFetchOptions
		for _, opt := range tool.fetchOptions(5 * time.Second) {
			opt(&opts)
		}
		if !strings.HasSuffix(opts.Browser.UserDataDir, want) {
			t.Errorf("mode %q: profile = %q, want it to end in %q", mode, opts.Browser.UserDataDir, want)
		}
		if !filepath.IsAbs(opts.Browser.UserDataDir) {
			t.Errorf("mode %q: profile = %q, want an absolute path", mode, opts.Browser.UserDataDir)
		}
		if cache, err := os.UserCacheDir(); err == nil && cache != "" &&
			strings.HasPrefix(opts.Browser.UserDataDir, cache) {
			t.Errorf("mode %q: profile = %q, want it out of the user's cache directory", mode, opts.Browser.UserDataDir)
		}
	}
	for _, mode := range []utils.FetchMode{utils.FetchModeChromeAttached, utils.FetchModeHTTP} {
		tool := NewWebFetchTool(WebFetchConfig{Mode: mode})
		var opts utils.WebFetchOptions
		for _, opt := range tool.fetchOptions(5 * time.Second) {
			opt(&opts)
		}
		if opts.Browser.UserDataDir != "" {
			t.Errorf("mode %q: profile = %q, want no profile for a mode that starts no browser",
				mode, opts.Browser.UserDataDir)
		}
	}
}

// TestWebFetchToolRendersHeadlessOnThePersistentProfile pins the end of the
// profile story for the headless mode: the fetch renders on the agent's profile
// below the working directory, and closing the browsers — which is what the exit
// path does — leaves that profile, with everything the fetch wrote into it, on
// disk for the next fetch.
func TestWebFetchToolRendersHeadlessOnThePersistentProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: no browser is started")
	}
	if _, err := utils.FindChrome(""); err != nil {
		t.Skipf("no Chromium-based browser available: %v", err)
	}
	workDir := t.TempDir()
	t.Chdir(workDir)
	server := webfetchPageServer(t)

	tool := NewWebFetchTool(WebFetchConfig{Mode: utils.FetchModeChromeHeadless})
	t.Cleanup(func() { _ = utils.CloseSharedBrowsers() })
	res := tool.Execute(context.Background(), map[string]any{"url": server.URL})
	if res.IsError {
		t.Fatalf("Execute reported an error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForUser, string(utils.FetchMethodHeadless)) {
		t.Errorf("ForUser = %q, want the headless method", res.ForUser)
	}
	profile := filepath.Join(workDir, ".lightagent", "browser-profile")
	if _, err := os.Stat(profile); err != nil {
		t.Fatalf("the headless fetch did not use %s: %v", profile, err)
	}

	// Closing the browsers is the exit path: the profile stays behind, so the
	// cookies and logins of this fetch are the ones the next fetch starts from.
	if err := utils.CloseSharedBrowsers(); err != nil {
		t.Fatalf("CloseSharedBrowsers: %v", err)
	}
	entries, err := os.ReadDir(profile)
	if err != nil {
		t.Fatalf("the profile is gone after closing the browsers: %v", err)
	}
	if len(entries) == 0 {
		t.Error("the profile holds nothing the next fetch could reuse")
	}
}

// TestWebFetchToolKeepsCookiesAcrossFetches pins the point of the headless mode
// rendering on the agent's profile: the first fetch is handed a cookie, the
// browsers are closed (which is what the exit path does) and the next fetch —
// a browser of its own, started fresh on the same profile — still sends that
// cookie.
func TestWebFetchToolKeepsCookiesAcrossFetches(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: no browser is started")
	}
	if _, err := utils.FindChrome(""); err != nil {
		t.Skipf("no Chromium-based browser available: %v", err)
	}
	t.Chdir(t.TempDir())

	// Only the page itself is counted: a browser asks for a favicon of its own.
	var (
		mu     sync.Mutex
		visits []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		cookie, err := r.Cookie("session")
		mu.Lock()
		if err == nil {
			visits = append(visits, "with-cookie:"+cookie.Value)
		} else {
			visits = append(visits, "without-cookie")
		}
		mu.Unlock()
		if err == nil {
			fmt.Fprint(w, `<!DOCTYPE html><html><body><h1>Welcome back</h1></body></html>`)
			return
		}
		// A cookie that outlives the browser: a session cookie is kept in memory
		// only, so nothing of it would reach the profile on disk.
		http.SetCookie(w, &http.Cookie{
			Name: "session", Value: "stored", Path: "/", Expires: time.Now().Add(time.Hour),
		})
		fmt.Fprint(w, `<!DOCTYPE html><html><body><h1>First visit</h1></body></html>`)
	}))
	t.Cleanup(server.Close)

	tool := NewWebFetchTool(WebFetchConfig{Mode: utils.FetchModeChromeHeadless})
	t.Cleanup(func() { _ = utils.CloseSharedBrowsers() })
	if res := tool.Execute(context.Background(), map[string]any{"url": server.URL}); res.IsError {
		t.Fatalf("first fetch: %s", res.ForLLM)
	}
	// The exit path closes the browsers and leaves the profile behind, so the
	// cookie of the first fetch has to be in it.
	if err := utils.CloseSharedBrowsers(); err != nil {
		t.Fatalf("CloseSharedBrowsers: %v", err)
	}
	if res := tool.Execute(context.Background(), map[string]any{"url": server.URL}); res.IsError {
		t.Fatalf("second fetch: %s", res.ForLLM)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(visits) < 2 {
		t.Fatalf("the page was requested %d times, want two fetches", len(visits))
	}
	if !strings.HasPrefix(visits[1], "with-cookie:stored") {
		t.Errorf("the second fetch arrived as %q, want the cookie the first one left in the profile", visits[1])
	}
}

// TestWebFetchToolWiresTheBrowserMode pins what each mode asks the fetcher for:
// auto and chrome-headful drive a browser with a visible window on a profile of
// the agent's own, chrome-headless one without but on that same profile,
// chrome-attached the endpoint from the configuration and no profile of ours, and
// http neither a browser nor a profile.
func TestWebFetchToolWiresTheBrowserMode(t *testing.T) {
	cases := []struct {
		mode    utils.FetchMode
		headful bool
		address string
		profile bool
	}{
		{utils.FetchModeAuto, true, "", true},
		{utils.FetchModeChromeHeadful, true, "", true},
		{utils.FetchModeChromeHeadless, false, "", true},
		{utils.FetchModeChromeAttached, false, "127.0.0.1:9333", false},
		{utils.FetchModeHTTP, false, "", false},
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
		if got := opts.Browser.UserDataDir != ""; got != tc.profile {
			t.Errorf("mode %q: own profile = %v, want %v", tc.mode, got, tc.profile)
		}
	}
}
