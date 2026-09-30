package utils

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// testPage is a page whose content differs between the source and the rendered
// DOM: the script assembles a marker from two pieces and puts it in an
// attribute, so the source never contains the marker and the serialized DOM
// does.
const testPage = `<!DOCTYPE html>
<html><head><title>fetch me</title></head>
<body><div id="app">static-marker</div>
<script>document.getElementById('app').setAttribute('data-rendered', 'rendered' + '-marker');</script>
</body></html>`

// newPageServer serves the test page and the routes the fetch tests need.
func newPageServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, testPage)
		case "/redirect":
			http.Redirect(w, r, "/page", http.StatusFound)
		case "/gbk":
			w.Header().Set("Content-Type", "text/html; charset=gbk")
			encoded, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("<html><body>中文页面</body></html>"))
			if err != nil {
				t.Errorf("encode gbk: %v", err)
				return
			}
			_, _ = w.Write(encoded)
		case "/undeclared":
			// UTF-8 without any charset declaration: a strict sniffing would
			// read this as windows-1252.
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body>未声明编码</body></html>")
		case "/unknown-charset":
			w.Header().Set("Content-Type", "text/plain; charset=no-such-charset")
			fmt.Fprint(w, "plain body")
		case "/large":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, strings.Repeat("a", 4096))
		case "/binary":
			// Not a page: the fetcher refuses it by its media type.
			w.Header().Set("Content-Type", "application/pdf")
			fmt.Fprint(w, "%PDF-1.7 not a page")
		case "/echo":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			cookie, _ := r.Cookie("session")
			fmt.Fprintf(w, "<html><body>%s|%s|%v</body></html>",
				r.Header.Get("X-Test"), r.Header.Get("User-Agent"), cookie != nil)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// containsAll reports whether the notes together contain every fragment.
func containsAll(notes []string, fragments ...string) bool {
	joined := strings.Join(notes, "\n")
	for _, fragment := range fragments {
		if !strings.Contains(joined, fragment) {
			return false
		}
	}
	return true
}

func TestWebFetchHTTPModeReturnsSource(t *testing.T) {
	server := newPageServer(t)
	result, err := WebFetch(context.Background(), server.URL+"/page",
		WithFetchMode(FetchModeHTTP), WithFetchTimeout(10*time.Second))
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}
	if result.Method != FetchMethodHTTP {
		t.Errorf("method = %q, want %q", result.Method, FetchMethodHTTP)
	}
	if result.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", result.StatusCode)
	}
	if result.FinalURL != server.URL+"/page" {
		t.Errorf("final URL = %q, want %q", result.FinalURL, server.URL+"/page")
	}
	if !strings.Contains(result.HTML, "static-marker") {
		t.Errorf("the source is missing its static content: %q", result.HTML)
	}
	if strings.Contains(result.HTML, "rendered-marker") {
		t.Errorf("the source should not carry script output: %q", result.HTML)
	}
	if result.Browser != "" {
		t.Errorf("browser = %q, want empty for the HTTP method", result.Browser)
	}
}

func TestWebFetchFallsBackToSourceWithoutBrowser(t *testing.T) {
	server := newPageServer(t)
	missing := filepath.Join(t.TempDir(), "no-such-chrome")

	// Auto mode without a usable browser: the source is the answer.
	result, err := WebFetch(context.Background(), server.URL+"/page",
		WithFetchChromePath(missing), WithFetchTimeout(10*time.Second))
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}
	if result.Method != FetchMethodHTTP {
		t.Errorf("method = %q, want %q", result.Method, FetchMethodHTTP)
	}
	if !strings.Contains(result.HTML, "static-marker") {
		t.Errorf("the source is missing its static content: %q", result.HTML)
	}
	if !containsAll(result.Notes, "no chromium-based browser found") {
		t.Errorf("notes = %v, want the reason the browser was skipped", result.Notes)
	}

	// A mode that requires a browser without one: an error, and no source.
	if _, err := WebFetch(context.Background(), server.URL+"/page",
		WithFetchMode(FetchModeChromeHeadful),
		WithFetchBrowser(BrowserOptions{ExecPath: missing, Headful: true})); !errors.Is(err, ErrChromeNotFound) {
		t.Errorf("chrome-headful mode error = %v, want ErrChromeNotFound", err)
	}
}

// TestWebFetchRefusesContentThatIsNotAPage pins the boundary of the fetcher: an
// address that serves something binary is refused before its body is read, so a
// download cannot flood the caller.
func TestWebFetchRefusesContentThatIsNotAPage(t *testing.T) {
	server := newPageServer(t)
	_, err := WebFetch(context.Background(), server.URL+"/binary", WithFetchMode(FetchModeHTTP))
	if !errors.Is(err, ErrNotPage) {
		t.Fatalf("error = %v, want ErrNotPage", err)
	}
	if !strings.Contains(err.Error(), "application/pdf") {
		t.Errorf("error = %v, want it to name the media type", err)
	}
	if !IsPageContentType("text/html; charset=utf-8") || !IsPageContentType("") || !IsPageContentType("TEXT/PLAIN") {
		t.Error("an HTML or plain text response must count as a page")
	}
	if IsPageContentType("application/pdf") || IsPageContentType("image/png") ||
		IsPageContentType("application/octet-stream") {
		t.Error("binary content must not count as a page")
	}
}

// TestWebFetchChromeAttachedNeedsAnEndpoint pins the one mode that cannot guess
// what to attach to: it reports the missing endpoint instead of trying to
// connect somewhere.
func TestWebFetchChromeAttachedNeedsAnEndpoint(t *testing.T) {
	server := newPageServer(t)
	_, err := WebFetch(context.Background(), server.URL+"/page", WithFetchMode(FetchModeChromeAttached))
	if err == nil || !strings.Contains(err.Error(), "chrome-attached") {
		t.Fatalf("error = %v, want a refusal naming the mode", err)
	}
}

// TestWebFetchBrowserModeDecidesTheWindow pins that an explicit browser mode
// decides how the page is rendered, whatever the options say about the window:
// chrome-headless renders headless all the same.
func TestWebFetchBrowserModeDecidesTheWindow(t *testing.T) {
	requireBrowser(t)
	server := newPageServer(t)
	t.Cleanup(func() { _ = CloseSharedBrowsers() })

	result, err := WebFetch(testContext(t), server.URL+"/page",
		WithFetchMode(FetchModeChromeHeadless),
		// A caller asking for a window in the headless mode does not get one:
		// the mode is the more specific statement. The profile is still named:
		// a launch never invents one.
		WithFetchBrowser(BrowserOptions{UserDataDir: testProfileDir(t), Headful: true}),
		WithFetchTimeout(45*time.Second), WithFetchSettle(200*time.Millisecond))
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}
	if result.Method != FetchMethodHeadless {
		t.Errorf("method = %q, want %q", result.Method, FetchMethodHeadless)
	}
	if !strings.Contains(result.HTML, "rendered-marker") {
		t.Errorf("the DOM does not carry the script output: %.200q", result.HTML)
	}
}

func TestWebFetchFollowsRedirect(t *testing.T) {
	server := newPageServer(t)
	result, err := WebFetch(context.Background(), server.URL+"/redirect", WithFetchMode(FetchModeHTTP))
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}
	if result.FinalURL != server.URL+"/page" {
		t.Errorf("final URL = %q, want %q", result.FinalURL, server.URL+"/page")
	}
}

func TestWebFetchDecodesCharsets(t *testing.T) {
	server := newPageServer(t)

	declared, err := WebFetch(context.Background(), server.URL+"/gbk", WithFetchMode(FetchModeHTTP))
	if err != nil {
		t.Fatalf("WebFetch gbk: %v", err)
	}
	if !strings.Contains(declared.HTML, "中文页面") {
		t.Errorf("gbk page = %q, want the decoded Chinese text", declared.HTML)
	}

	undeclared, err := WebFetch(context.Background(), server.URL+"/undeclared", WithFetchMode(FetchModeHTTP))
	if err != nil {
		t.Fatalf("WebFetch undeclared: %v", err)
	}
	if !strings.Contains(undeclared.HTML, "未声明编码") {
		t.Errorf("undeclared page = %q, want the UTF-8 text untouched", undeclared.HTML)
	}
	// A charset nobody knows is reported, and the body is kept as it is rather
	// than lost.
	unknown, err := WebFetch(context.Background(), server.URL+"/unknown-charset", WithFetchMode(FetchModeHTTP))
	if err != nil {
		t.Fatalf("WebFetch unknown charset: %v", err)
	}
	if unknown.HTML != "plain body" {
		t.Errorf("body = %q, want it kept as it is", unknown.HTML)
	}
	if !containsAll(unknown.Notes, "unknown charset") {
		t.Errorf("notes = %v, want the unknown charset to be named", unknown.Notes)
	}
}

func TestWebFetchHTMLHelper(t *testing.T) {
	server := newPageServer(t)
	html, err := WebFetchHTML(context.Background(), server.URL+"/page", WithFetchMode(FetchModeHTTP))
	if err != nil {
		t.Fatalf("WebFetchHTML: %v", err)
	}
	if !strings.Contains(html, "static-marker") {
		t.Errorf("WebFetchHTML = %.200q, want the page", html)
	}
}

func TestBrowserPoolKey(t *testing.T) {
	// One configuration means one browser ...
	if browserPoolKey(BrowserOptions{}) != browserPoolKey(BrowserOptions{}) {
		t.Errorf("the same browser configuration produced two keys")
	}
	// ... and every difference that changes the browser keeps two
	// configurations apart, so they never share one.
	differences := []struct {
		name string
		a, b BrowserOptions
	}{
		{"attach address", BrowserOptions{Address: "127.0.0.1:9222"}, BrowserOptions{}},
		{"executable", BrowserOptions{ExecPath: "chrome"}, BrowserOptions{}},
		{"profile", BrowserOptions{UserDataDir: "profile"}, BrowserOptions{}},
		{"extra arguments", BrowserOptions{ExtraArgs: []string{"--no-sandbox"}}, BrowserOptions{}},
		{"visibility", BrowserOptions{Headful: true}, BrowserOptions{}},
		{"debug port", BrowserOptions{DebugPort: 9222}, BrowserOptions{}},
	}
	for _, difference := range differences {
		if browserPoolKey(difference.a) == browserPoolKey(difference.b) {
			t.Errorf("%s: two configurations share one key (%q)",
				difference.name, browserPoolKey(difference.a))
		}
	}
}

func TestWebFetchTruncatesLargeBody(t *testing.T) {
	server := newPageServer(t)
	result, err := WebFetch(context.Background(), server.URL+"/large",
		WithFetchMode(FetchModeHTTP), WithFetchMaxBytes(64))
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}
	if len(result.HTML) != 64 {
		t.Errorf("body length = %d, want 64", len(result.HTML))
	}
	if !containsAll(result.Notes, "truncated") {
		t.Errorf("notes = %v, want a truncation remark", result.Notes)
	}
}

func TestWebFetchSendsHeadersUserAgentAndCookies(t *testing.T) {
	server := newPageServer(t)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	jar.SetCookies(parsed, []*http.Cookie{{Name: "session", Value: "abc", Path: "/"}})

	headers := http.Header{}
	headers.Set("X-Test", "header-value")
	result, err := WebFetch(context.Background(), server.URL+"/echo",
		WithFetchMode(FetchModeHTTP),
		WithFetchHeaders(headers),
		WithFetchUserAgent("lightagent-test/1.0"),
		WithFetchJar(jar),
	)
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}
	if !strings.Contains(result.HTML, "header-value|lightagent-test/1.0|true") {
		t.Errorf("the server saw %q, want the header, the user agent and the cookie", result.HTML)
	}
}

// invokeJSServer serves a page whose title and cookies are what a script of the
// caller has to reach, plus an address that only a script asks for: the page
// itself requests nothing, so a value that arrives from it proves the script
// drove the fetch.
func invokeJSServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, "token-42")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc123"})
		fmt.Fprint(w, `<!DOCTYPE html><html><head><title>Invokejs page</title></head><body>
<h1>Invokejs page</h1>
</body></html>`)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestWebFetchInvokesAScriptInThePage pins the script a caller can have run in
// the loaded page: what it reports through _invokejs_done comes back as the info
// of the result, undefined comes back as nothing, a failure of the script and a
// script that never reports are told apart, and the page itself is still there.
func TestWebFetchInvokesAScriptInThePage(t *testing.T) {
	requireBrowser(t)
	server := invokeJSServer(t)
	t.Cleanup(func() { _ = CloseSharedBrowsers() })
	profile := testProfileDir(t)

	fetch := func(t *testing.T, script string, timeout time.Duration) WebFetchResult {
		t.Helper()
		result, err := WebFetch(testContext(t), server.URL+"/page",
			WithFetchMode(FetchModeChromeHeadless),
			WithFetchBrowser(BrowserOptions{UserDataDir: profile}),
			WithFetchInvokeJS(script),
			WithFetchTimeout(timeout), WithFetchSettle(200*time.Millisecond))
		if err != nil {
			t.Fatalf("WebFetch with invokejs %q: %v", script, err)
		}
		if !strings.Contains(result.HTML, "Invokejs page") {
			t.Errorf("the page is missing from the result of %q", script)
		}
		return result
	}

	// A string is reported as the script wrote it, and the cookie of the page
	// is exactly what such a script is for.
	reported := fetch(t, `_invokejs_done(document.cookie)`, 45*time.Second)
	if !strings.Contains(reported.InvokeInfo, "session=abc123") {
		t.Errorf("InvokeInfo = %q, want the cookie of the page", reported.InvokeInfo)
	}
	if reported.InvokeNote != "" {
		t.Errorf("InvokeNote = %q, want none for a script that reported", reported.InvokeNote)
	}

	// A script may await what it asks for: the wait for it replaces the wait
	// for the network, so what it fetched is what it reports.
	fetched := fetch(t, `const response = await fetch('/token'); _invokejs_done(await response.text());`,
		45*time.Second)
	if fetched.InvokeInfo != "token-42" {
		t.Errorf("InvokeInfo = %q, want what the script fetched", fetched.InvokeInfo)
	}

	// Another value travels as the JSON of it.
	object := fetch(t, `_invokejs_done({title: document.title, pressed: 1 === 1})`, 45*time.Second)
	for _, want := range []string{`"title":"Invokejs page"`, `"pressed":true`} {
		if !strings.Contains(object.InvokeInfo, want) {
			t.Errorf("InvokeInfo = %q, want it to hold %s", object.InvokeInfo, want)
		}
	}

	// The function is kept out of the way of the page: a page that walks its
	// own globals does not run into it (the property is not enumerable).
	hidden := fetch(t, `_invokejs_done([Object.keys(window).includes('_invokejs_done'), `+
		`Object.getOwnPropertyDescriptor(window, '_invokejs_done').enumerable].join(','))`, 45*time.Second)
	if hidden.InvokeInfo != "false,false" {
		t.Errorf("the injected function must not be enumerable, got %q", hidden.InvokeInfo)
	}

	// undefined reports nothing at all.
	empty := fetch(t, `_invokejs_done(undefined)`, 45*time.Second)
	if empty.InvokeInfo != "" || empty.InvokeNote != "" {
		t.Errorf("a script that reported undefined = %q / %q, want both empty",
			empty.InvokeInfo, empty.InvokeNote)
	}

	// A script that throws says so, and does not hold the fetch for the whole
	// window.
	failed := fetch(t, `throw new Error('boom')`, 45*time.Second)
	if !strings.Contains(failed.InvokeNote, "boom") {
		t.Errorf("InvokeNote = %q, want the message of the exception", failed.InvokeNote)
	}

	// A script that never calls the function ends when the wait runs out, and
	// the fetch still hands the page back.
	silent := fetch(t, `window.forgot = true;`, 5*time.Second)
	if !strings.Contains(silent.InvokeNote, "did not call _invokejs_done") {
		t.Errorf("InvokeNote = %q, want the wait to be reported as run out", silent.InvokeNote)
	}
	if silent.InvokeInfo != "" {
		t.Errorf("InvokeInfo = %q, want nothing for a script that never reported", silent.InvokeInfo)
	}
}

// TestWebFetchWithoutABrowserRefusesAScript pins the boundary of the script: it
// needs the page a browser builds, so a fetch that falls back to the source
// reports the failure of the browser instead of quietly dropping the script.
func TestWebFetchWithoutABrowserRefusesAScript(t *testing.T) {
	server := invokeJSServer(t)
	_, err := WebFetch(context.Background(), server.URL+"/page",
		WithFetchChromePath(filepath.Join(t.TempDir(), "no-such-chrome")),
		WithFetchInvokeJS(`_invokejs_done(1)`),
		WithFetchTimeout(10*time.Second))
	if err == nil {
		t.Fatal("a fetch with a script and no browser must fail")
	}
	if !strings.Contains(err.Error(), "script") {
		t.Errorf("error = %v, want it to name the script as the reason", err)
	}
}

// TestInvokeInfoTextRendersWhatAScriptReported pins how a reported value travels
// into the answer: a string stays what it is, other values are the JSON of them,
// nothing is reported for undefined, null and an empty string, and a value that
// ran away is cut instead of flooding the answer.
func TestInvokeInfoTextRendersWhatAScriptReported(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"string", `"session=abc123"`, "session=abc123"},
		{"string with escapes", `"a\nb"`, "a\nb"},
		{"empty string", `""`, ""},
		{"undefined", ``, ""},
		{"null", `null`, ""},
		{"object", `{"a":1}`, `{"a":1}`},
		{"number", `42`, "42"},
	}
	for _, tc := range cases {
		if got := invokeInfoText(json.RawMessage(tc.value)); got != tc.want {
			t.Errorf("%s: invokeInfoText(%s) = %q, want %q", tc.name, tc.value, got, tc.want)
		}
	}

	long := strings.Repeat("a", invokeInfoLimit+100)
	cut := invokeInfoText(json.RawMessage(`"` + long + `"`))
	if !strings.Contains(cut, "cut at") {
		t.Errorf("a value beyond the limit must say that it was cut: %.60q", cut)
	}
	if len(cut) >= len(long) {
		t.Errorf("the cut value must be shorter than the value itself: %d >= %d", len(cut), len(long))
	}
}

// TestWebFetchRejectsUnusableURLs pins that an address the fetcher cannot use is
// an error before anything is opened.
func TestWebFetchRejectsUnusableURLs(t *testing.T) {
	for _, raw := range []string{"", "   ", "ftp://example.com/x", "mailto:a@b.c", "http://"} {
		if _, err := WebFetch(context.Background(), raw, WithFetchMode(FetchModeHTTP)); err == nil {
			t.Errorf("WebFetch(%q) = nil error, want a failure", raw)
		}
	}
}

func TestNormalizeFetchURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"example.com/page", "https://example.com/page"},
		{"http://example.com", "http://example.com"},
		{"https://example.com/a?b=c#d", "https://example.com/a?b=c#d"},
	}
	for _, testCase := range cases {
		got, err := normalizeFetchURL(testCase.in)
		if err != nil {
			t.Errorf("normalizeFetchURL(%q): %v", testCase.in, err)
			continue
		}
		if got != testCase.want {
			t.Errorf("normalizeFetchURL(%q) = %q, want %q", testCase.in, got, testCase.want)
		}
	}
}

func TestWebFetchNetworkIdleOption(t *testing.T) {
	opts := defaultWebFetchOptions()
	if opts.NetworkIdle != WebFetchNetworkIdleDefault {
		t.Errorf("network idle = %s, want the built-in %s", opts.NetworkIdle, WebFetchNetworkIdleDefault)
	}
	WithFetchNetworkIdle(750 * time.Millisecond)(&opts)
	if opts.NetworkIdle != 750*time.Millisecond {
		t.Errorf("network idle = %s, want the configured 750ms", opts.NetworkIdle)
	}
	// Zero is the way to turn the wait off: nothing is watched, nothing waits.
	WithFetchNetworkIdle(0)(&opts)
	if opts.NetworkIdle != 0 {
		t.Errorf("network idle = %s, want the wait disabled", opts.NetworkIdle)
	}
}

func TestNetworkIdleLimit(t *testing.T) {
	// Without a deadline the fetch caps the wait on quiet windows: a few of
	// them, never below the built-in minimum.
	if got := networkIdleLimit(context.Background(), WebFetchNetworkIdleDefault); got != WebFetchNetworkIdleLimitDefault {
		t.Errorf("limit = %s, want %s", got, WebFetchNetworkIdleLimitDefault)
	}
	if got := networkIdleLimit(context.Background(), 2*time.Second); got != 8*time.Second {
		t.Errorf("limit = %s, want four quiet windows (8s)", got)
	}
	// The deadline of the fetch wins, and the margin the serialization of the
	// DOM needs is kept back.
	bounded, cancelBounded := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancelBounded()
	if got := networkIdleLimit(bounded, WebFetchNetworkIdleDefault); got > 4*time.Second || got < 3*time.Second {
		t.Errorf("limit = %s, want the remainder of the fetch (about 4s)", got)
	}
	// No room left: the wait is skipped instead of eating the fetch.
	tight, cancelTight := context.WithTimeout(context.Background(), time.Second)
	defer cancelTight()
	if got := networkIdleLimit(tight, WebFetchNetworkIdleDefault); got != 0 {
		t.Errorf("limit = %s, want 0 (no time left to watch the network)", got)
	}
}
