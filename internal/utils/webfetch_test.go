package utils

import (
	"context"
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
		// the mode is the more specific statement.
		WithFetchBrowser(BrowserOptions{Headful: true}),
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
