package utils

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// requireBrowser skips a test when no Chromium-based browser is installed, so
// the suite stays runnable on a machine (or a CI runner) without one.
func requireBrowser(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode: browser tests are skipped")
	}
	if _, err := FindChrome(""); err != nil {
		t.Skipf("no Chromium-based browser available: %v", err)
	}
}

// testProfileDir returns the profile directory a test launch uses. A launch
// names one, and a directory of the test's own keeps the browser of one test out
// of the profile of another (and out of the one the machine's user browses in).
func testProfileDir(t *testing.T) string {
	t.Helper()
	return cleanProfileAt(t, filepath.Join(t.TempDir(), "browser-profile"))
}

// cleanProfileAt registers the teardown of a profile directory: the browsers of
// the test are closed first, and the directory is then removed while the process
// that ran on it lets go of its files — a browser that was just closed can still
// hold them for a moment, and the test framework removes what is left of the
// test's directory afterwards, which is empty by then.
func cleanProfileAt(t *testing.T, dir string) string {
	t.Helper()
	t.Cleanup(func() {
		_ = CloseSharedBrowsers()
		for attempt := 0; attempt < 20; attempt++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	return dir
}

// testContext bounds a browser test; launching a browser and loading a page
// takes a moment, but never a minute.
func testContext(t *testing.T) context.Context {
	t.Helper()
	if deadline, ok := t.Deadline(); ok {
		ctx, cancel := context.WithDeadline(context.Background(), deadline.Add(-time.Second))
		t.Cleanup(cancel)
		return ctx
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestFindChrome(t *testing.T) {
	missing := t.TempDir() + "/no-such-chrome"
	if _, err := FindChrome(missing); err == nil {
		t.Errorf("FindChrome(%q) = nil error, want a failure", missing)
	}
	if ChromeAvailable(missing) {
		t.Errorf("ChromeAvailable(%q) = true, want false", missing)
	}

	// An override through the environment is used verbatim.
	if _, err := FindChrome(""); err != nil {
		t.Skipf("no browser to point the environment override at: %v", err)
	}
	found, err := FindChrome("")
	if err != nil {
		t.Fatalf("FindChrome: %v", err)
	}
	t.Setenv(ChromePathEnv, found)
	fromEnv, err := FindChrome("")
	if err != nil {
		t.Fatalf("FindChrome with env: %v", err)
	}
	if fromEnv != found {
		t.Errorf("FindChrome from env = %q, want %q", fromEnv, found)
	}
}

func TestWebFetchRendersDOMWithBrowser(t *testing.T) {
	requireBrowser(t)
	server := newPageServer(t)
	profile := testProfileDir(t)
	t.Cleanup(func() { _ = CloseSharedBrowsers() })

	result, err := WebFetch(testContext(t), server.URL+"/page",
		WithFetchBrowser(BrowserOptions{UserDataDir: profile}),
		WithFetchTimeout(45*time.Second), WithFetchSettle(200*time.Millisecond))
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}
	if result.Method != FetchMethodHeadless {
		t.Errorf("method = %q, want %q", result.Method, FetchMethodHeadless)
	}
	if !result.Loaded {
		t.Errorf("loaded = false, want a finished load (notes: %v)", result.Notes)
	}
	if !strings.Contains(result.HTML, "rendered-marker") {
		t.Errorf("the DOM does not carry the script output: %.200q", result.HTML)
	}
	if !strings.Contains(result.HTML, "static-marker") {
		t.Errorf("the DOM lost its static content: %.200q", result.HTML)
	}
	if !hasDoctypePrefix(result.HTML) {
		t.Errorf("the serialization lost the doctype: %.100q", result.HTML)
	}
	if result.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", result.StatusCode)
	}
	if !strings.Contains(result.ContentType, "html") {
		t.Errorf("content type = %q, want an HTML document", result.ContentType)
	}
	if result.Browser == "" {
		t.Errorf("browser = %q, want the browser identification", result.Browser)
	}
	if !strings.Contains(result.FinalURL, "/page") {
		t.Errorf("final URL = %q, want the requested page", result.FinalURL)
	}
}

func TestWebFetchKeepsTheDOMWhenTheLoadTimesOut(t *testing.T) {
	requireBrowser(t)
	server := newHangingServer(t)
	profile := testProfileDir(t)
	t.Cleanup(func() { _ = CloseSharedBrowsers() })

	result, err := WebFetch(testContext(t), server.URL+"/hanging",
		WithFetchBrowser(BrowserOptions{UserDataDir: profile}),
		WithFetchTimeout(45*time.Second), WithFetchLoadTimeout(700*time.Millisecond))
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}
	if result.Loaded {
		t.Errorf("loaded = true, want a timeout: the subresource never arrives")
	}
	if !containsAll(result.Notes, "had not finished loading") {
		t.Errorf("notes = %v, want the load timeout", result.Notes)
	}
	if !strings.Contains(result.HTML, "hanging-marker") {
		t.Errorf("the DOM was not captured: %.200q", result.HTML)
	}
}

// newHangingServer serves a page whose load never finishes: an image request
// stays open until the test ends.
func newHangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hanging":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!DOCTYPE html><html><head><title>slow</title></head>`+
				`<body><div>hanging-marker</div><img src="/never-arrives"></body></html>`)
		default:
			// Hold the response until the test releases it.
			select {
			case <-release:
			case <-r.Context().Done():
			}
			http.Error(w, "slow", http.StatusGatewayTimeout)
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return server
}

// newDelayedAjaxServer serves a page whose content arrives through a request it
// starts a moment after its document was parsed, and whose answer takes another
// moment: the marker is therefore never in the source, and only a fetch that
// waits for the network to settle sees it.
func newDelayedAjaxServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/delayed":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!DOCTYPE html><html><head><title>delayed</title></head><body>`+
				`<div id="app">delayed-waiting</div>`+
				`<script>setTimeout(function(){fetch('/delayed-data')`+
				`.then(function(response){return response.text()})`+
				`.then(function(text){document.getElementById('app').textContent = text})}, 600)</script>`+
				`</body></html>`)
		case "/delayed-data":
			time.Sleep(300 * time.Millisecond)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, "delayed-marker")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// newBusyServer serves a page that keeps requesting a beacon while it is open,
// so its network never goes quiet.
func newBusyServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/busy":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!DOCTYPE html><html><head><title>busy</title></head><body>`+
				`<div>busy-marker</div>`+
				`<script>setInterval(function(){fetch('/beacon').catch(function(){})}, 100)</script>`+
				`</body></html>`)
		default:
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, "beat")
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestWebFetchWaitsForDelayedAjax covers the content a page loads after its own
// document: without the wait for the network to settle, the DOM would be
// serialized before the answer of the request lands.
func TestWebFetchWaitsForDelayedAjax(t *testing.T) {
	requireBrowser(t)
	server := newDelayedAjaxServer(t)
	profile := testProfileDir(t)
	t.Cleanup(func() { _ = CloseSharedBrowsers() })

	result, err := WebFetch(testContext(t), server.URL+"/delayed",
		WithFetchBrowser(BrowserOptions{UserDataDir: profile}),
		WithFetchTimeout(45*time.Second))
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}
	if !strings.Contains(result.HTML, "delayed-marker") {
		t.Errorf("the DOM does not carry the delayed content: %.300q", result.HTML)
	}
	if result.Elapsed < 800*time.Millisecond {
		t.Errorf("the fetch returned after %s, want it to have waited for the delayed request", result.Elapsed)
	}
	if containsAll(result.Notes, "still busy") {
		t.Errorf("notes = %v, want an idle network (the page stops talking)", result.Notes)
	}
}

// TestWebFetchCapturesAPageThatNeverGoesQuiet covers the cap of the wait: a page
// whose network never goes quiet is captured after it, with a note, instead of
// eating the whole fetch and failing.
func TestWebFetchCapturesAPageThatNeverGoesQuiet(t *testing.T) {
	requireBrowser(t)
	server := newBusyServer(t)
	profile := testProfileDir(t)
	t.Cleanup(func() { _ = CloseSharedBrowsers() })

	result, err := WebFetch(testContext(t), server.URL+"/busy",
		WithFetchBrowser(BrowserOptions{UserDataDir: profile}),
		WithFetchTimeout(30*time.Second), WithFetchNetworkIdle(200*time.Millisecond))
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}
	if !strings.Contains(result.HTML, "busy-marker") {
		t.Errorf("the DOM was not captured: %.300q", result.HTML)
	}
	if !containsAll(result.Notes, "still busy") {
		t.Errorf("notes = %v, want the give-up of the wait for a quiet network", result.Notes)
	}
	if result.Elapsed > 15*time.Second {
		t.Errorf("the fetch took %s, want the wait to be capped well below the timeout", result.Elapsed)
	}
}

func TestSharedBrowserReusesOneInstance(t *testing.T) {
	requireBrowser(t)
	ctx := testContext(t)
	// A negative keep-alive keeps the browser until it is closed explicitly.
	opts := BrowserOptions{UserDataDir: testProfileDir(t), KeepAlive: -1}
	t.Cleanup(func() { _ = CloseSharedBrowsers() })

	first, err := SharedBrowser(ctx, opts)
	if err != nil {
		t.Fatalf("SharedBrowser: %v", err)
	}
	second, err := SharedBrowser(ctx, opts)
	if err != nil {
		t.Fatalf("SharedBrowser again: %v", err)
	}
	if first != second {
		t.Errorf("the pool started a second browser instead of reusing the first")
	}
	if !first.Alive() {
		t.Errorf("the shared browser is not alive")
	}
	if got := SharedBrowserInstance(); got != first {
		t.Errorf("SharedBrowserInstance = %p, want %p", got, first)
	}
	if err := CloseSharedBrowsers(); err != nil {
		t.Errorf("CloseSharedBrowsers: %v", err)
	}
	if first.Alive() {
		t.Errorf("the browser survived CloseSharedBrowsers")
	}
	if SharedBrowserInstance() != nil {
		t.Errorf("the pool still holds a browser after CloseSharedBrowsers")
	}
}

func TestSharedBrowserClosesAfterIdle(t *testing.T) {
	requireBrowser(t)
	ctx := testContext(t)
	browser, err := SharedBrowser(ctx, BrowserOptions{UserDataDir: testProfileDir(t), KeepAlive: 400 * time.Millisecond})
	if err != nil {
		t.Fatalf("SharedBrowser: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for SharedBrowserInstance() != nil || browser.Alive() {
		if time.Now().After(deadline) {
			t.Fatalf("the idle browser was not closed (alive: %v)", browser.Alive())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestBrowserCloseEndsTheProcess(t *testing.T) {
	requireBrowser(t)
	ctx := testContext(t)
	options := BrowserOptions{UserDataDir: testProfileDir(t), LaunchTimeout: 20 * time.Second}
	browser, err := LaunchBrowser(ctx, options)
	if err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	if browser.Mode() != BrowserHeadless {
		t.Errorf("mode = %q, want %q", browser.Mode(), BrowserHeadless)
	}
	if browser.PID() == 0 {
		t.Errorf("a launched browser has no process id")
	}
	if browser.Address() == "" || browser.Product() == "" {
		t.Errorf("address = %q, product = %q, want both filled in", browser.Address(), browser.Product())
	}
	if version := browser.ProtocolVersion(); version == "" {
		t.Errorf("protocol version = %q, want the version of the protocol", version)
	}
	if got := browser.Options(); got.LaunchTimeout != options.LaunchTimeout {
		t.Errorf("Options().LaunchTimeout = %v, want %v", got.LaunchTimeout, options.LaunchTimeout)
	}
	if err := browser.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if browser.Alive() {
		t.Errorf("the browser is still alive after Close")
	}
	select {
	case <-browser.exited:
	case <-time.After(10 * time.Second):
		t.Errorf("the browser process was not reaped")
	}
	if err := browser.Close(); err != nil {
		t.Errorf("Close is not idempotent: %v", err)
	}
}

// notABrowserExecutable returns an executable that is not a browser and exits
// right away, which is what a wrong ExecPath behaves like.
func notABrowserExecutable(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("SystemRoot"), "System32", "findstr.exe")
	}
	for _, candidate := range []string{"/bin/cat", "/usr/bin/cat", "/bin/false"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	t.Skip("no harmless executable to stand in for a wrong browser")
	return ""
}

// TestBrowserLaunchReportsAnExecutableThatIsNotABrowser checks the failure a
// wrong configuration produces: the process ends at once, and the launch has to
// say so instead of waiting for its timeout.
func TestBrowserLaunchReportsAnExecutableThatIsNotABrowser(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	fake := notABrowserExecutable(t)
	started := time.Now()
	_, err := LaunchBrowser(context.Background(), BrowserOptions{
		UserDataDir:   testProfileDir(t),
		ExecPath:      fake,
		LaunchTimeout: 15 * time.Second,
	})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatalf("LaunchBrowser(%s) = nil error, want the executable to be reported as unusable", fake)
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Errorf("error = %v, want it to name the exit of the process", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the launch took %s, want the failure as soon as the process ends", elapsed)
	}
	t.Logf("reported after %s: %v", elapsed.Round(time.Millisecond), err)
}

func TestBrowserSharesCookiesWithAJar(t *testing.T) {
	requireBrowser(t)
	ctx := testContext(t)
	browser, err := LaunchBrowser(ctx, BrowserOptions{UserDataDir: testProfileDir(t)})
	if err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	t.Cleanup(func() { _ = browser.Close() })

	origin, err := url.Parse("https://example.com/")
	if err != nil {
		t.Fatalf("parse origin: %v", err)
	}
	if err := browser.SetCookies(ctx, Cookie{
		Name: "session", Value: "browser-value", URL: origin.String(), Secure: true,
	}); err != nil {
		t.Fatalf("SetCookies: %v", err)
	}
	if !hasCookie(t, browserCookies(t, ctx, browser), "session", "browser-value") {
		t.Errorf("the cookie set on the browser did not come back")
	}

	// The browser cookie reaches an HTTP client through a jar ...
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	if err := browser.ExportCookies(ctx, jar, origin.String()); err != nil {
		t.Fatalf("ExportCookies: %v", err)
	}
	if !hasHTTPCookie(jar.Cookies(origin), "session", "browser-value") {
		t.Errorf("the browser cookie was not exported into the jar")
	}

	// ... and a cookie of the jar reaches the browser.
	other, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	other.SetCookies(origin, []*http.Cookie{{Name: "from-jar", Value: "jar-value", Path: "/", Secure: true}})
	if err := browser.ImportCookies(ctx, other, origin.String()); err != nil {
		t.Fatalf("ImportCookies: %v", err)
	}
	if !hasCookie(t, browserCookies(t, ctx, browser), "from-jar", "jar-value") {
		t.Errorf("the cookie of the jar was not imported into the browser")
	}

	// A cookie that was placed by domain travels without being asked for by
	// address: the export covers the origins of the cookies themselves.
	if err := browser.SetCookies(ctx, Cookie{Name: "domain-cookie", Value: "domain-value", Domain: "example.com", Path: "/"}); err != nil {
		t.Fatalf("SetCookies by domain: %v", err)
	}
	every, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	if err := browser.ExportCookies(ctx, every); err != nil {
		t.Fatalf("ExportCookies without URLs: %v", err)
	}
	if !hasHTTPCookie(every.Cookies(origin), "domain-cookie", "domain-value") {
		t.Errorf("the cookie placed by domain was not exported to its origin")
	}
}

// browserCookies reads the cookie store of a browser, failing the test when the
// browser refuses.
func browserCookies(t *testing.T, ctx context.Context, browser *Browser) []Cookie {
	t.Helper()
	cookies, err := browser.Cookies(ctx)
	if err != nil {
		t.Fatalf("Cookies: %v", err)
	}
	return cookies
}

// hasCookie reports whether the protocol cookies hold a name/value pair.
func hasCookie(t *testing.T, cookies []Cookie, name, value string) bool {
	t.Helper()
	for _, cookie := range cookies {
		if cookie.Name == name && cookie.Value == value {
			return true
		}
	}
	t.Logf("cookies seen: %+v", cookies)
	return false
}

// hasHTTPCookie reports whether an HTTP cookie list holds a name/value pair.
func hasHTTPCookie(cookies []*http.Cookie, name, value string) bool {
	for _, cookie := range cookies {
		if cookie.Name == name && cookie.Value == value {
			return true
		}
	}
	return false
}

func TestPageRendersAndReportsItsDocument(t *testing.T) {
	requireBrowser(t)
	server := newPageServer(t)
	ctx := testContext(t)
	browser, err := LaunchBrowser(ctx, BrowserOptions{UserDataDir: testProfileDir(t)})
	if err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	t.Cleanup(func() { _ = browser.Close() })

	page, err := browser.NewPage(ctx)
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	t.Cleanup(func() { closePage(page) })

	nav, err := page.Navigate(ctx, server.URL+"/page", 20*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	if nav.ErrorText != "" {
		t.Fatalf("navigate error text = %q", nav.ErrorText)
	}
	if !nav.Loaded {
		t.Errorf("loaded = false, want a finished load")
	}
	html, err := page.HTML(ctx)
	if err != nil {
		t.Fatalf("HTML: %v", err)
	}
	if !strings.Contains(html, "rendered-marker") || !hasDoctypePrefix(html) {
		t.Errorf("serialized DOM = %.200q, want the doctype and the script output", html)
	}
	if title, err := page.Title(ctx); err != nil || title != "fetch me" {
		t.Errorf("title = %q (err %v), want %q", title, err, "fetch me")
	}
	if current, err := page.URL(ctx); err != nil || !strings.HasSuffix(current, "/page") {
		t.Errorf("URL = %q (err %v), want the requested page", current, err)
	}
	if status := page.StatusCode(ctx); status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if contentType := page.ContentType(ctx); !strings.Contains(contentType, "html") {
		t.Errorf("content type = %q, want an HTML document", contentType)
	}
}

// TestPageCookies covers the page scoped cookie API, which is what a caller
// driving the user's own tabs uses: cookies for the address of a tab go in and
// come back out through the same session.
func TestPageCookies(t *testing.T) {
	requireBrowser(t)
	server := newPageServer(t)
	ctx := testContext(t)
	browser, err := LaunchBrowser(ctx, BrowserOptions{UserDataDir: testProfileDir(t)})
	if err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	t.Cleanup(func() { _ = browser.Close() })

	page, err := browser.NewPage(ctx)
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	t.Cleanup(func() { closePage(page) })
	if _, err := page.Navigate(ctx, server.URL+"/page", 20*time.Second, 0); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	if err := page.SetCookies(ctx, Cookie{Name: "page-cookie", Value: "page-value", URL: server.URL + "/page"}); err != nil {
		t.Fatalf("SetCookies: %v", err)
	}
	cookies, err := page.Cookies(ctx, server.URL+"/page")
	if err != nil {
		t.Fatalf("Cookies: %v", err)
	}
	if !hasCookie(t, cookies, "page-cookie", "page-value") {
		t.Errorf("the cookie set on the page did not come back")
	}
}

// TestPageWaitForLoad covers both ends of the wait: a page that is done returns
// at once, a page that never finishes reports the timeout instead of holding
// the caller.
func TestPageWaitForLoad(t *testing.T) {
	requireBrowser(t)
	server := newPageServer(t)
	hanging := newHangingServer(t)
	ctx := testContext(t)
	browser, err := LaunchBrowser(ctx, BrowserOptions{UserDataDir: testProfileDir(t)})
	if err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	t.Cleanup(func() { _ = browser.Close() })

	page, err := browser.NewPage(ctx)
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	t.Cleanup(func() { closePage(page) })

	if _, err := page.Navigate(ctx, server.URL+"/page", 20*time.Second, 0); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	started := time.Now()
	if err := page.WaitForLoad(ctx, 5*time.Second); err != nil {
		t.Fatalf("WaitForLoad on a finished page: %v", err)
	}
	if waited := time.Since(started); waited > 2*time.Second {
		t.Errorf("WaitForLoad on a finished page waited %s, want an immediate return", waited)
	}

	if _, err := page.Navigate(ctx, hanging.URL+"/hanging", 300*time.Millisecond, 0); err != nil {
		t.Fatalf("Navigate the hanging page: %v", err)
	}
	started = time.Now()
	err = page.WaitForLoad(ctx, 700*time.Millisecond)
	if err == nil {
		t.Fatalf("WaitForLoad on a page that never finishes = nil error, want the timeout")
	}
	if !strings.Contains(err.Error(), "did not finish loading") {
		t.Errorf("error = %v, want the missing load to be named", err)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Errorf("WaitForLoad waited %s, want the timeout to bound it", waited)
	}
}

// TestBrowserCrashIsReported checks what happens when the browser dies under
// the agent: a wait ends with an error instead of hanging, and the browser
// reports itself as gone.
func TestBrowserCrashIsReported(t *testing.T) {
	requireBrowser(t)
	hanging := newHangingServer(t)
	ctx := testContext(t)
	browser, err := LaunchBrowser(ctx, BrowserOptions{UserDataDir: testProfileDir(t)})
	if err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	t.Cleanup(func() { _ = browser.Close() })

	page, err := browser.NewPage(ctx)
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	t.Cleanup(func() { closePage(page) })
	if _, err := page.Navigate(ctx, hanging.URL+"/hanging", 300*time.Millisecond, 0); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// The browser goes away without a word, the way a crash does.
	killBrowserProcess(t, browser.PID())

	started := time.Now()
	err = page.WaitForLoad(ctx, 10*time.Second)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatalf("WaitForLoad survived the death of the browser, want a failure")
	}
	if !strings.Contains(err.Error(), "connection") {
		t.Errorf("error = %v, want the lost connection to be named", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the crash was reported after %s, want it as soon as the connection dies", elapsed)
	}
	deadline := time.Now().Add(10 * time.Second)
	for browser.Alive() {
		if time.Now().After(deadline) {
			t.Fatalf("the browser still reports itself as alive")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("reported after %s: %v", elapsed.Round(time.Millisecond), err)
}

// killBrowserProcess ends a launched browser without closing its connection, so
// that the crash path is what the caller sees.
func killBrowserProcess(t *testing.T, pid int) {
	t.Helper()
	if pid == 0 {
		t.Skip("there is no process to end")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find process %d: %v", pid, err)
	}
	if err := process.Kill(); err != nil {
		t.Fatalf("end process %d: %v", pid, err)
	}
}

// TestPageDetachLeavesTheTabAlone covers the other half of the page lifecycle:
// the session ends, the tab does not.
func TestPageDetachLeavesTheTabAlone(t *testing.T) {
	requireBrowser(t)
	ctx := testContext(t)
	browser, err := LaunchBrowser(ctx, BrowserOptions{UserDataDir: testProfileDir(t)})
	if err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	t.Cleanup(func() { _ = browser.Close() })

	pages, err := browser.Pages(ctx)
	if err != nil {
		t.Fatalf("Pages: %v", err)
	}
	if len(pages) == 0 {
		t.Skip("the browser has no tab to attach to")
	}
	page, err := browser.AttachPage(ctx, pages[0].TargetID)
	if err != nil {
		t.Fatalf("AttachPage: %v", err)
	}
	if err := page.Detach(ctx); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if _, err := page.Evaluate(ctx, "1 + 1"); err == nil {
		t.Errorf("a detached page still answers calls")
	}
	if err := page.Detach(ctx); err != nil {
		t.Errorf("Detach is not idempotent: %v", err)
	}
	after, err := browser.Pages(ctx)
	if err != nil {
		t.Fatalf("Pages after detaching: %v", err)
	}
	found := false
	for _, info := range after {
		if info.TargetID == pages[0].TargetID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("detaching from a page closed its tab")
	}
}

// TestEvaluateSurfacesErrorsAndValues covers both halves of Evaluate: values
// come back typed, and a throwing script is reported with its own message.
func TestEvaluateSurfacesErrorsAndValues(t *testing.T) {
	requireBrowser(t)
	server := newPageServer(t)
	ctx := testContext(t)
	browser, err := LaunchBrowser(ctx, BrowserOptions{UserDataDir: testProfileDir(t)})
	if err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	t.Cleanup(func() { _ = browser.Close() })

	page, err := browser.NewPage(ctx)
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	t.Cleanup(func() { closePage(page) })
	if _, err := page.Navigate(ctx, server.URL+"/page", 20*time.Second, 0); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	if value := evaluateNumber(t, ctx, page, "1 + 1"); value != 2 {
		t.Errorf("Evaluate(1 + 1) = %v, want 2", value)
	}
	if text := evaluateString(t, ctx, page, "'a' + 'b'"); text != "ab" {
		t.Errorf("Evaluate('a' + 'b') = %q, want %q", text, "ab")
	}
	if object := evaluateString(t, ctx, page, "JSON.stringify({a: 1, b: 'two'})"); object != `{"a":1,"b":"two"}` {
		t.Errorf("an object came back as %q, want its JSON text", object)
	}

	_, err = page.Evaluate(ctx, "throw new Error('boom' + '-js')")
	if err == nil {
		t.Fatalf("Evaluate of a throwing script = nil error, want a failure")
	}
	if !strings.Contains(err.Error(), "boom-js") {
		t.Errorf("error = %v, want the message of the JavaScript error", err)
	}
}

func TestAttachToRunningBrowser(t *testing.T) {
	requireBrowser(t)
	server := newPageServer(t)
	ctx := testContext(t)

	port, err := freePort()
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	launched, err := LaunchBrowser(ctx, BrowserOptions{UserDataDir: testProfileDir(t), DebugPort: port})
	if err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	t.Cleanup(func() { _ = launched.Close() })

	attached, err := AttachBrowser(ctx, "127.0.0.1:"+strconv.Itoa(port), BrowserOptions{})
	if err != nil {
		t.Fatalf("AttachBrowser: %v", err)
	}
	if attached.Mode() != BrowserAttached {
		t.Errorf("mode = %q, want %q", attached.Mode(), BrowserAttached)
	}
	if attached.Product() == "" {
		t.Errorf("an attached browser should still report its product")
	}

	page, err := attached.NewPage(ctx)
	if err != nil {
		t.Fatalf("NewPage through the attached connection: %v", err)
	}
	nav, err := page.Navigate(ctx, server.URL+"/page", 20*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	if !nav.Loaded {
		t.Errorf("loaded = false, want a finished load")
	}
	html, err := page.HTML(ctx)
	if err != nil {
		t.Fatalf("HTML: %v", err)
	}
	if !strings.Contains(html, "rendered-marker") {
		t.Errorf("serialized DOM = %.200q, want the script output", html)
	}
	closePage(page)

	// Detaching is not stopping: the browser we attached to keeps running.
	if err := attached.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !launched.Alive() {
		t.Errorf("closing an attached connection ended the browser itself")
	}
}

func TestAttachToExistingTabKeepsIt(t *testing.T) {
	requireBrowser(t)
	server := newPageServer(t)
	ctx := testContext(t)
	browser, err := LaunchBrowser(ctx, BrowserOptions{UserDataDir: testProfileDir(t)})
	if err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	t.Cleanup(func() { _ = browser.Close() })

	pages, err := browser.Pages(ctx)
	if err != nil {
		t.Fatalf("Pages: %v", err)
	}
	if len(pages) == 0 {
		t.Skip("the browser has no tab to attach to")
	}

	// A tab somebody else opened (here: the browser itself) is driven through
	// the page API but survives being closed by the caller.
	page, err := browser.AttachPage(ctx, pages[0].TargetID)
	if err != nil {
		t.Fatalf("AttachPage: %v", err)
	}
	nav, err := page.Navigate(ctx, server.URL+"/page", 20*time.Second, 0)
	if err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	if !nav.Loaded {
		t.Errorf("loaded = false, want a finished load")
	}
	if err := page.Close(ctx); err != nil {
		t.Errorf("Close of an attached page: %v", err)
	}
	after, err := browser.Pages(ctx)
	if err != nil {
		t.Fatalf("Pages after closing an attached page: %v", err)
	}
	found := false
	for _, info := range after {
		if info.TargetID == pages[0].TargetID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("closing an attached page closed the tab instead of detaching")
	}
}

func TestFindRunningBrowserIgnoresCandidatesThatDoNotAnswer(t *testing.T) {
	port, err := freePort()
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	dead := "127.0.0.1:" + strconv.Itoa(port)
	t.Setenv(ChromeDebugPortEnv, dead)

	// A candidate that does not answer on /json/version is not a browser: the
	// lookup either finds another one or reports that there is none.
	address, err := FindRunningBrowser(testContext(t))
	if err == nil && address == dead {
		t.Errorf("FindRunningBrowser = %q, want a candidate that answers", address)
	}
}

func TestWebFetchThroughAnAttachedBrowser(t *testing.T) {
	requireBrowser(t)
	server := newPageServer(t)
	ctx := testContext(t)

	port, err := freePort()
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	// A browser that is already running: standing in for the user's own, which
	// is reached through its DevTools port.
	launched, err := LaunchBrowser(ctx, BrowserOptions{UserDataDir: testProfileDir(t), DebugPort: port, KeepAlive: -1})
	if err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	t.Cleanup(func() { _ = launched.Close() })

	// The address of that browser is discovered the way an agent would find a
	// browser the user has open.
	address := "127.0.0.1:" + strconv.Itoa(port)
	t.Setenv(ChromeDebugPortEnv, address)
	found, err := FindRunningBrowser(ctx)
	if err != nil {
		t.Fatalf("FindRunningBrowser: %v", err)
	}
	if found != address {
		t.Errorf("FindRunningBrowser = %q, want %q", found, address)
	}

	result, err := WebFetch(ctx, server.URL+"/page",
		WithFetchBrowserAddress(address), WithFetchTimeout(45*time.Second))
	if err != nil {
		t.Fatalf("WebFetch through the attached browser: %v", err)
	}
	if result.Method != FetchMethodAttached {
		t.Errorf("method = %q, want %q", result.Method, FetchMethodAttached)
	}
	if !strings.Contains(result.HTML, "rendered-marker") {
		t.Errorf("the DOM does not carry the script output: %.200q", result.HTML)
	}
	if result.Browser == "" {
		t.Errorf("browser = %q, want the product of the running browser", result.Browser)
	}

	// Dropping the pool detaches; the browser somebody else runs stays.
	if err := CloseSharedBrowsers(); err != nil {
		t.Errorf("CloseSharedBrowsers: %v", err)
	}
	if !launched.Alive() {
		t.Errorf("the pool closed the browser it only attached to")
	}
}

func TestWebFetchSharesCookiesWithTheBrowser(t *testing.T) {
	requireBrowser(t)
	server := newPageServer(t)
	profile := testProfileDir(t)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	origin, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	jar.SetCookies(origin, []*http.Cookie{{Name: "session", Value: "jar-cookie", Path: "/"}})
	t.Cleanup(func() { _ = CloseSharedBrowsers() })

	headers := http.Header{}
	headers.Set("X-Test", "header-value")
	// The page of the browser sees the cookie of the jar, the headers and the
	// user agent the caller asked for.
	result, err := WebFetch(testContext(t), server.URL+"/echo",
		WithFetchBrowser(BrowserOptions{UserDataDir: profile}),
		WithFetchJar(jar),
		WithFetchHeaders(headers),
		WithFetchUserAgent("lightagent-test/1.0"),
		WithFetchTimeout(45*time.Second),
	)
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}
	if result.Method == FetchMethodHTTP {
		t.Error("the fetch fell back to the HTTP source, so nothing of the browser was exercised")
	}
	if !strings.Contains(result.HTML, "header-value|lightagent-test/1.0|true") {
		t.Errorf("the page saw %.200q, want the header, the user agent and the cookie of the jar", result.HTML)
	}
}

// tcpTestPage is the page the raw TCP server serves. Its script changes the DOM
// in three ways — an attribute, an element it creates and a change it applies
// from a timer — and computes a result the test verifies independently, so a
// serializer that lost the executed DOM would not pass. Every marker is
// assembled at runtime ("js" + "-added"), which keeps the source free of them:
// finding one in the source would mean the script never ran.
const tcpTestPage = `<!DOCTYPE html>
<html><head><title>tcp page</title></head>
<body>
<div id="app">static-marker</div>
<script>
  var sum = [1, 2, 3, 4].reduce(function (total, value) { return total + value; }, 0);
  var result = 'js' + '-result';
  document.body.setAttribute('data-js-result', String(sum));
  var added = document.createElement('p');
  added.id = 'js' + '-added';
  added.textContent = result + ':' + sum;
  document.body.appendChild(added);
  setTimeout(function () { added.setAttribute('data-late', 'late' + '-value'); }, 50);
</script>
</body></html>`

// rawTCPServer answers HTTP requests from a plain TCP listener: the smallest
// possible web server, so the whole path (accept, request, response) is what a
// real site would do. It returns the page address and a counter of the requests
// it served, which shows whether the browser fetched the page at all.
func rawTCPServer(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	requests := &atomic.Int32{}
	accepting := make(chan struct{})
	go func() {
		defer close(accepting)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // the listener was closed by the test
			}
			go serveRawConnection(conn, requests)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-accepting
	})
	return "http://" + listener.Addr().String() + "/", requests
}

// serveRawConnection reads one HTTP request and writes one HTTP response by
// hand. The connection deadline keeps a browser that opens a socket without
// finishing its request from pinning the goroutine.
func serveRawConnection(conn net.Conn, requests *atomic.Int32) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	request, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return
	}
	requests.Add(1)
	body, status := tcpTestPage, "200 OK"
	if request.URL.Path != "/" {
		body, status = "<html><body>not found</body></html>", "404 Not Found"
	}
	_, _ = io.WriteString(conn, fmt.Sprintf(
		"HTTP/1.1 %s\r\nContent-Type: text/html; charset=utf-8\r\n"+
			"Content-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, len(body), body))
}

// TestWebFetchAgainstRawTCPServer is the practical end-to-end check: a TCP
// listener serves a page whose script rewrites the DOM, and that page is
// fetched twice — once as the source, once through a real browser that has to
// run the script. The results the script produced are then read back out of the
// page (Execute, not just markup) and compared with the serialized DOM.
//
// It needs a Chromium-based browser and takes about a second:
//
//	go test ./internal/utils/ -run TestWebFetchAgainstRawTCPServer -v
func TestWebFetchAgainstRawTCPServer(t *testing.T) {
	requireBrowser(t)
	address, requests := rawTCPServer(t)
	ctx := testContext(t)
	profile := testProfileDir(t)
	t.Cleanup(func() { _ = CloseSharedBrowsers() })

	// The source: the script is text, nothing has run yet, so neither the
	// element it creates nor the attributes it sets exist.
	source, err := WebFetch(context.Background(), address, WithFetchMode(FetchModeHTTP))
	if err != nil {
		t.Fatalf("source fetch: %v", err)
	}
	if source.Method != FetchMethodHTTP {
		t.Errorf("source method = %q, want %q", source.Method, FetchMethodHTTP)
	}
	if !strings.Contains(source.HTML, "static-marker") {
		t.Errorf("the source lost its static content: %.200q", source.HTML)
	}
	for _, executed := range jsMarkers {
		if strings.Contains(source.HTML, executed) {
			t.Errorf("the source contains %q, which only a running script can produce: %.200q",
				executed, source.HTML)
		}
	}

	// The rendered DOM: the browser runs the script and the DOM tree it built
	// is serialized back to HTML — including the change the timer applied.
	rendered, err := WebFetch(ctx, address,
		WithFetchBrowser(BrowserOptions{UserDataDir: profile}),
		WithFetchTimeout(45*time.Second), WithFetchSettle(100*time.Millisecond))
	if err != nil {
		t.Fatalf("rendered fetch: %v", err)
	}
	if rendered.Method != FetchMethodHeadless {
		t.Errorf("rendered method = %q, want %q", rendered.Method, FetchMethodHeadless)
	}
	if !rendered.Loaded {
		t.Errorf("loaded = false, want a finished load (notes: %v)", rendered.Notes)
	}
	if !hasDoctypePrefix(rendered.HTML) || !strings.Contains(rendered.HTML, "static-marker") {
		t.Errorf("serialized DOM = %.200q, want the doctype and the page content", rendered.HTML)
	}
	for _, executed := range jsMarkers {
		if !strings.Contains(rendered.HTML, executed) {
			t.Errorf("the DOM misses %q, so the script did not run: %.300q", executed, rendered.HTML)
		}
	}
	if rendered.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", rendered.StatusCode)
	}

	// The results of the script are checked as values, not as markup: the page
	// of the shared browser is asked what the script computed.
	browser, err := SharedBrowser(ctx, BrowserOptions{UserDataDir: profile})
	if err != nil {
		t.Fatalf("SharedBrowser: %v", err)
	}
	page, err := browser.NewPage(ctx)
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	defer closePage(page)
	nav, err := page.Navigate(ctx, address, 20*time.Second, 100*time.Millisecond)
	if err != nil || !nav.Loaded {
		t.Fatalf("Navigate: %v (loaded: %v)", err, nav.Loaded)
	}

	text := evaluateString(t, ctx, page, "document.getElementById('js' + '-added').textContent")
	sum := evaluateNumber(t, ctx, page, "[1, 2, 3, 4].reduce(function (t, v) { return t + v; }, 0)")
	late := evaluateString(t, ctx, page, "document.querySelector('#js' + '-added').getAttribute('data-late')")
	async := evaluateString(t, ctx, page, "Promise.resolve('async' + '-value')")

	if text != "js-result:10" {
		t.Errorf("the element the script created says %q, want %q", text, "js-result:10")
	}
	if sum != 10 {
		t.Errorf("the script computed %v, want 10", sum)
	}
	if late != "late-value" {
		t.Errorf("the timer of the page produced %q, want %q", late, "late-value")
	}
	if async != "async-value" {
		t.Errorf("a promise resolved to %q, want %q", async, "async-value")
	}

	// What the script computed is what the serialized DOM contains.
	html, err := page.HTML(ctx)
	if err != nil {
		t.Fatalf("HTML: %v", err)
	}
	if !strings.Contains(html, ">"+text+"</p>") {
		t.Errorf("the serialized DOM does not carry the text of the element: %.300q", html)
	}
	if got := requests.Load(); got == 0 {
		t.Errorf("the TCP server served no request")
	}
	t.Logf("served by TCP, rendered %d bytes in %s via %s (%s, %d request(s) seen); "+
		"script results: text=%q sum=%v timer=%q promise=%q",
		len(rendered.HTML), rendered.Elapsed.Round(time.Millisecond),
		rendered.Method, rendered.Browser, requests.Load(), text, sum, late, async)
}

// jsMarkers are the strings only the script of tcpTestPage can produce.
var jsMarkers = []string{"js-added", "js-result:10", "data-js-result=\"10\"", "late-value"}

// evaluateString runs an expression and returns its string result.
func evaluateString(t *testing.T, ctx context.Context, page *Page, expression string) string {
	t.Helper()
	raw, err := page.Evaluate(ctx, expression)
	if err != nil {
		t.Fatalf("Evaluate(%s): %v", expression, err)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("Evaluate(%s) = %s, want a string: %v", expression, raw, err)
	}
	return value
}

// evaluateNumber runs an expression and returns its numeric result.
func evaluateNumber(t *testing.T, ctx context.Context, page *Page, expression string) float64 {
	t.Helper()
	raw, err := page.Evaluate(ctx, expression)
	if err != nil {
		t.Fatalf("Evaluate(%s): %v", expression, err)
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("Evaluate(%s) = %s, want a number: %v", expression, raw, err)
	}
	return value
}
