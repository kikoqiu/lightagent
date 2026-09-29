package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding/htmlindex"
)

// FetchMethod reports how the HTML of a result was obtained.
type FetchMethod string

const (
	// FetchMethodHTTP is the page source exactly as the server sent it.
	FetchMethodHTTP FetchMethod = "http"
	// FetchMethodHeadless is the DOM of a page rendered by a headless browser
	// started for the request.
	FetchMethodHeadless FetchMethod = "chrome-headless"
	// FetchMethodHeadful is the DOM of a page rendered by a visible browser
	// started for the request (on the profile of the user when one is given).
	FetchMethodHeadful FetchMethod = "chrome-headful"
	// FetchMethodAttached is the DOM of a page rendered by a browser that was
	// already running, typically the user's own.
	FetchMethodAttached FetchMethod = "chrome-attached"
)

// FetchMode selects how WebFetch obtains a page.
type FetchMode string

const (
	// FetchModeAuto renders the page with the browser the options describe (see
	// BrowserOptions.Headful) and falls back to the HTTP source when no browser
	// can be used. This is the default. The webfetch tool pairs it with a
	// visible window, since a browser with one is far less likely to be blocked
	// than a headless one.
	FetchModeAuto FetchMode = "auto"
	// FetchModeChromeHeadful renders the page in a visible browser window,
	// whatever the options say about the window: the fetch fails when no
	// browser can be started.
	FetchModeChromeHeadful FetchMode = "chrome-headful"
	// FetchModeChromeHeadless renders the page in a headless browser, whatever
	// the options say about the window.
	FetchModeChromeHeadless FetchMode = "chrome-headless"
	// FetchModeChromeAttached renders the page through a browser that is
	// already running, reached over its DevTools endpoint
	// (WebFetchOptions.Browser.Address): the user's own browser, with their
	// logins and cookies.
	FetchModeChromeAttached FetchMode = "chrome-attached"
	// FetchModeHTTP downloads the source and never starts a browser.
	FetchModeHTTP FetchMode = "http"
)

// ErrNotPage reports that the address does not serve a web page: a PDF, an
// image, an archive or any other content WebFetch refuses. The fetcher reads
// HTML documents only, so the caller can turn this into a message that points
// the model at a command instead of another fetch.
var ErrNotPage = errors.New("not a web page")

// Defaults of WebFetchOptions.
const (
	// WebFetchTimeoutDefault bounds a whole fetch, loading and serializing
	// included.
	WebFetchTimeoutDefault = 30 * time.Second
	// WebFetchMaxBytesDefault caps the body of the HTTP source.
	WebFetchMaxBytesDefault = 8 << 20
	// WebFetchNetworkIdleDefault is the quiet window of the wait for the content
	// a page loads after its document: the fetcher waits this long for the
	// network to go quiet (no request starting, none finishing) before it
	// captures the DOM, which catches the AJAX/XHR content that arrives after
	// the initial HTML. The same value is the grace period in front of the check
	// (some sites start their requests a moment after their load) and the settle
	// after it (the DOM update the last response triggers).
	WebFetchNetworkIdleDefault = 500 * time.Millisecond
	// WebFetchNetworkIdleLimitDefault is the smallest cap on the checking phase
	// of that wait: a page whose network never goes quiet (a chat, a live page,
	// a stream of beacons) is captured after it, with a note, instead of eating
	// the whole fetch and returning nothing.
	WebFetchNetworkIdleLimitDefault = 5 * time.Second
	// networkIdleCaptureMargin is the time the fetch keeps back for serializing
	// the DOM: the checking phase never runs into the deadline of the fetch
	// itself, so a fetch whose budget is almost spent still returns its page.
	networkIdleCaptureMargin = 2 * time.Second
	// pageCloseTimeout bounds dropping a page, so a request that already ran
	// out of time does not wait for its tab to close.
	pageCloseTimeout = 5 * time.Second
)

// WebFetchOptions configures WebFetch. The zero value is the usual default:
// render with a headless browser, fall back to the HTTP source, 30 seconds.
type WebFetchOptions struct {
	// Mode selects the fetching strategy (default FetchModeAuto: a visible
	// browser, else the HTTP source). Every other mode asks for exactly one way
	// of obtaining the page and fails when that way is not available.
	Mode FetchMode
	// Timeout bounds the whole operation (default WebFetchTimeoutDefault).
	Timeout time.Duration
	// LoadTimeout bounds waiting for the page to finish loading. A page that
	// does not get there in time still yields the DOM it has built so far, with
	// the result saying Loaded false (default: Timeout).
	LoadTimeout time.Duration
	// Settle is a quiet period after the load event, for pages that render
	// their content asynchronously (default: none).
	Settle time.Duration
	// NetworkIdle is the quiet window of the wait for the content a page loads
	// after its own document. When non-zero, the fetcher waits this long for
	// the network to go quiet (no request starting, none finishing) before it
	// captures the DOM, which catches AJAX/XHR content that arrives after the
	// initial HTML. The same value is the grace period in front of the check
	// (some sites start their requests a moment after their load) and the
	// settle after it (the DOM update the last response triggers). The check
	// is capped (see WebFetchNetworkIdleLimitDefault), so a page whose network
	// never goes quiet still yields its DOM, with a note. Default: 500ms.
	NetworkIdle time.Duration
	// UserAgent overrides the user agent of both paths. Without it the
	// browser uses its own default, and the HTTP source sends none.
	UserAgent string
	// Headers are extra request headers; a browser applies them to every
	// request the page makes.
	Headers http.Header
	// Jar shares cookies in both directions: its cookies are imported into the
	// browser before the page loads and the cookies of the page are exported
	// into it afterwards. With the HTTP path it is simply the jar of the
	// request.
	Jar http.CookieJar
	// MaxBytes caps the body read over HTTP (default WebFetchMaxBytesDefault).
	MaxBytes int64
	// Browser configures the browser: executable, profile, attach address,
	// headless or not, keep-alive, ...
	Browser BrowserOptions
}

// WebFetchOptionFunc configures WebFetchOptions.
type WebFetchOptionFunc func(*WebFetchOptions)

func defaultWebFetchOptions() WebFetchOptions {
	return WebFetchOptions{
		Mode:        FetchModeAuto,
		Timeout:     WebFetchTimeoutDefault,
		MaxBytes:    WebFetchMaxBytesDefault,
		NetworkIdle: WebFetchNetworkIdleDefault,
	}
}

// WithFetchMode selects the fetching strategy.
func WithFetchMode(mode FetchMode) WebFetchOptionFunc {
	return func(o *WebFetchOptions) { o.Mode = mode }
}

// WithFetchTimeout bounds the whole fetch.
func WithFetchTimeout(timeout time.Duration) WebFetchOptionFunc {
	return func(o *WebFetchOptions) { o.Timeout = timeout }
}

// WithFetchLoadTimeout bounds waiting for the page to finish loading.
func WithFetchLoadTimeout(timeout time.Duration) WebFetchOptionFunc {
	return func(o *WebFetchOptions) { o.LoadTimeout = timeout }
}

// WithFetchSettle adds a quiet period after the load event.
func WithFetchSettle(settle time.Duration) WebFetchOptionFunc {
	return func(o *WebFetchOptions) { o.Settle = settle }
}

// WithFetchNetworkIdle sets the network idle duration for detecting when the
// page's network has gone quiet after the initial load. A zero value disables
// the check entirely.
func WithFetchNetworkIdle(idle time.Duration) WebFetchOptionFunc {
	return func(o *WebFetchOptions) { o.NetworkIdle = idle }
}

// WithFetchUserAgent overrides the user agent of both paths.
func WithFetchUserAgent(userAgent string) WebFetchOptionFunc {
	return func(o *WebFetchOptions) { o.UserAgent = userAgent }
}

// WithFetchHeaders adds request headers (used by both paths).
func WithFetchHeaders(headers http.Header) WebFetchOptionFunc {
	return func(o *WebFetchOptions) { o.Headers = headers }
}

// WithFetchJar shares cookies between the caller and the browser.
func WithFetchJar(jar http.CookieJar) WebFetchOptionFunc {
	return func(o *WebFetchOptions) { o.Jar = jar }
}

// WithFetchMaxBytes caps the body read over HTTP.
func WithFetchMaxBytes(maxBytes int64) WebFetchOptionFunc {
	return func(o *WebFetchOptions) { o.MaxBytes = maxBytes }
}

// WithFetchBrowser replaces the browser configuration.
func WithFetchBrowser(opts BrowserOptions) WebFetchOptionFunc {
	return func(o *WebFetchOptions) { o.Browser = opts }
}

// WithFetchBrowserAddress renders through a browser that is already running
// instead of starting one: "127.0.0.1:9222", the ws:// browser socket. A browser
// the user started with --remote-debugging-port is used as it is, with their
// logins and cookies; FindRunningBrowser locates such a browser.
func WithFetchBrowserAddress(address string) WebFetchOptionFunc {
	return func(o *WebFetchOptions) { o.Browser.Address = address }
}

// WithFetchChromePath selects the browser executable (an empty value goes back
// to auto-detection).
func WithFetchChromePath(path string) WebFetchOptionFunc {
	return func(o *WebFetchOptions) { o.Browser.ExecPath = path }
}

// WebFetchResult is the outcome of WebFetch. Method, Browser and Loaded say how
// the HTML was obtained, which is what a caller needs to judge it: a rendered
// DOM can carry content the source never had, while a source returned because
// no browser was available is not the rendered page.
type WebFetchResult struct {
	// URL is the address that was requested.
	URL string
	// FinalURL is the address the page ended up on (after redirects, after a
	// script replaced the location).
	FinalURL string
	// HTML is the page: the serialized DOM tree for the browser methods, the
	// response body for FetchMethodHTTP.
	HTML string
	// Method says how the HTML was obtained.
	Method FetchMethod
	// StatusCode is the status of the response: of the main document for the
	// browser methods (0 when the browser did not report one), of the HTTP
	// response otherwise.
	StatusCode int
	// ContentType is the media type of the document or of the response.
	ContentType string
	// Browser identifies the browser that rendered the page, e.g.
	// "Chrome/153.0.8010.52" (empty for FetchMethodHTTP).
	Browser string
	// Loaded reports whether the page finished loading before the timeout. A
	// page that did not is still returned, with the DOM as far as it got.
	Loaded bool
	// Notes are the remarks that belong to the result: a page that was cut
	// short, a browser that was skipped, a browser that failed before the HTTP
	// source was used.
	Notes []string
	// Elapsed is how long the whole fetch took.
	Elapsed time.Duration
}

// WebFetch returns the HTML of a page. A Chromium-based browser is detected
// first: when one is found, an instance with a visible window (or a headless
// one, or the browser the options point at — see FetchMode) loads the page,
// waits for it to finish loading — or for the timeout to expire — and the
// resulting DOM tree is serialized back to HTML. In FetchModeAuto without a
// usable browser the HTTP source is returned instead, and the result says so in
// Method and Notes.
//
// Only pages are read: an address that serves anything else is refused with an
// error wrapping ErrNotPage.
//
// The browser stays alive between calls (see SharedBrowser) and is closed after
// ten minutes without use or when the program ends, so the cookie state of a
// site survives a sequence of fetches.
func WebFetch(ctx context.Context, rawURL string, opts ...WebFetchOptionFunc) (WebFetchResult, error) {
	cfg := defaultWebFetchOptions()
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.Mode == "" {
		cfg.Mode = FetchModeAuto
	}
	target, err := normalizeFetchURL(rawURL)
	if err != nil {
		return WebFetchResult{}, err
	}
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}
	started := time.Now()

	if cfg.Mode == FetchModeHTTP {
		result, err := fetchSource(ctx, target, cfg)
		result.Elapsed = time.Since(started)
		return result, err
	}
	// The mode selects how the page is rendered, so it wins over the window
	// flag of the options: an explicit chrome-headful is a visible browser and
	// an explicit chrome-headless is not, whatever the caller passed. Auto
	// leaves the choice to the options, and chrome-attached uses the browser it
	// was pointed at.
	switch cfg.Mode {
	case FetchModeChromeHeadful:
		cfg.Browser.Headful = true
	case FetchModeChromeHeadless, FetchModeChromeAttached:
		cfg.Browser.Headful = false
	}
	// Attaching is the one mode that needs something the caller has to supply:
	// there is no endpoint to guess, so a missing one is reported as such
	// instead of ending up as a connection error.
	if cfg.Mode == FetchModeChromeAttached && strings.TrimSpace(cfg.Browser.Address) == "" {
		return WebFetchResult{}, errors.New("chrome-attached needs the DevTools address of the browser to drive " +
			"(set it next to the mode; see WithFetchBrowserAddress)")
	}

	result, err := fetchRendered(ctx, target, cfg)
	if err == nil {
		result.Elapsed = time.Since(started)
		return result, nil
	}
	if cfg.Mode != FetchModeAuto {
		// Every other mode asks for one way and nothing else, so the error is
		// the answer: it says what that way was missing.
		return WebFetchResult{}, err
	}

	source, sourceErr := fetchSource(ctx, target, cfg)
	if sourceErr != nil {
		// Both ways failed: the browser error explains what rendering was
		// missing, the source error what the request itself hit. The source
		// error is the outer one, so a refusal of the content itself (a PDF,
		// say) still reads as ErrNotPage.
		return WebFetchResult{}, fmt.Errorf("%w (rendering failed as well: %v)", sourceErr, err)
	}
	source.Notes = append(source.Notes, "returned the HTTP source: "+err.Error())
	source.Elapsed = time.Since(started)
	return source, nil
}

// IsPageContentType reports whether a response media type is one WebFetch reads:
// a document a browser shows as text — HTML for the sites, plain text for what
// is served as a text file (a robot file, a markdown document). Binary content
// (a PDF, an image, an archive) is not a page. A response that says nothing
// about its type counts as a page too, so a server that keeps quiet is not
// refused for it.
func IsPageContentType(contentType string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(contentType))
	if mediaType == "" {
		return true
	}
	if parsed, _, err := mime.ParseMediaType(contentType); err == nil {
		mediaType = strings.ToLower(strings.TrimSpace(parsed))
	}
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	return mediaType == "application/xhtml+xml"
}

// notPageError turns a media type into the refusal of content that is not a
// page.
func notPageError(contentType string) error {
	mediaType := strings.TrimSpace(contentType)
	if mediaType == "" {
		mediaType = "an unidentified type"
	}
	return fmt.Errorf("%w: the response is %s, not an HTML page", ErrNotPage, mediaType)
}

// WebFetchHTML is WebFetch for callers that only want the markup.
func WebFetchHTML(ctx context.Context, rawURL string, opts ...WebFetchOptionFunc) (string, error) {
	result, err := WebFetch(ctx, rawURL, opts...)
	return result.HTML, err
}

// fetchRendered loads a page in a browser and serializes its DOM tree.
func fetchRendered(ctx context.Context, target string, cfg WebFetchOptions) (WebFetchResult, error) {
	browser, err := SharedBrowser(ctx, cfg.Browser)
	if err != nil {
		return WebFetchResult{}, err
	}
	page, err := browser.NewPage(ctx)
	if err != nil {
		return WebFetchResult{}, err
	}
	defer closePage(page)

	var notes []string
	if cfg.UserAgent != "" {
		if err := page.SetUserAgent(ctx, cfg.UserAgent); err != nil {
			return WebFetchResult{}, err
		}
	}
	if len(cfg.Headers) > 0 {
		if err := page.SetHeaders(ctx, cfg.Headers); err != nil {
			return WebFetchResult{}, err
		}
	}
	if cfg.Jar != nil {
		if err := browser.ImportCookies(ctx, cfg.Jar, target); err != nil {
			notes = append(notes, "the cookies of the jar were not imported: "+err.Error())
		}
	}

	loadTimeout := cfg.LoadTimeout
	if loadTimeout <= 0 {
		loadTimeout = cfg.Timeout
	}
	nav, err := page.Navigate(ctx, target, loadTimeout, cfg.Settle)
	if err != nil {
		return WebFetchResult{}, err
	}
	if nav.ErrorText != "" {
		return WebFetchResult{}, fmt.Errorf("the browser could not load %s: %s", target, nav.ErrorText)
	}
	if !nav.Loaded {
		notes = append(notes, fmt.Sprintf(
			"the page had not finished loading after %s; the DOM was captured as it was", loadTimeout))
	} else {
		notes = append(notes, waitForNetworkIdle(ctx, page, cfg.NetworkIdle)...)
	}
	html, err := page.HTML(ctx)
	if err != nil {
		return WebFetchResult{}, err
	}

	finalURL := target
	if current, err := page.URL(ctx); err == nil && current != "" {
		finalURL = current
	}
	if cfg.Jar != nil {
		urls := []string{target}
		if finalURL != target {
			urls = append(urls, finalURL)
		}
		if err := browser.ExportCookies(ctx, cfg.Jar, urls...); err != nil {
			notes = append(notes, "the cookies of the page were not exported: "+err.Error())
		}
	}
	return WebFetchResult{
		URL:         target,
		FinalURL:    finalURL,
		HTML:        html,
		Method:      fetchMethodOf(browser.Mode()),
		StatusCode:  page.StatusCode(ctx),
		ContentType: page.ContentType(ctx),
		Browser:     browser.Product(),
		Loaded:      nav.Loaded,
		Notes:       notes,
	}, nil
}

// waitForNetworkIdle lets the content a page fetches after its own document
// arrive, for a page that has finished loading: a grace period first, so that
// the requests a site starts late begin, then a wait for the network to go quiet,
// then a settle for the DOM update the last response triggers. It must only be
// called for a page whose load is over, since the events it reads mean "the page
// is still working" (see Page.WaitNetworkIdle).
//
// The notes it returns explain a wait that had to be cut short: the DOM may then
// be missing what was still on its way, and the caller has to say so. A wait cut
// short by the deadline of the fetch itself returns no note here — serializing
// the page is about to fail for the same reason, and the caller reports that.
func waitForNetworkIdle(ctx context.Context, page *Page, idle time.Duration) []string {
	if idle <= 0 {
		return nil
	}
	limit := networkIdleLimit(ctx, idle)
	if limit <= 0 {
		return []string{"the fetch was out of time to wait for content loaded in the background; " +
			"the DOM was captured as it was"}
	}
	if !page.WaitNetworkIdle(ctx, idle, idle, limit) {
		if ctx.Err() != nil {
			return nil
		}
		return []string{fmt.Sprintf(
			"the network was still busy %s after the load; the DOM was captured as it was", idle+limit)}
	}
	if err := sleepContext(ctx, idle); err != nil {
		return nil
	}
	return nil
}

// networkIdleLimit bounds the checking phase of the network idle wait (see
// waitForNetworkIdle): a few quiet windows, never less than
// WebFetchNetworkIdleLimitDefault, and never so long that the serialization of
// the DOM runs into the deadline of the fetch. A zero answer means there is no
// time left to watch the network at all.
func networkIdleLimit(ctx context.Context, idle time.Duration) time.Duration {
	limit := 4 * idle
	if limit < WebFetchNetworkIdleLimitDefault {
		limit = WebFetchNetworkIdleLimitDefault
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline) - networkIdleCaptureMargin
		if remaining <= 0 {
			return 0
		}
		if remaining < limit {
			limit = remaining
		}
	}
	return limit
}

// fetchMethodOf names the method behind a browser mode.
func fetchMethodOf(mode BrowserMode) FetchMethod {
	switch mode {
	case BrowserHeadful:
		return FetchMethodHeadful
	case BrowserAttached:
		return FetchMethodAttached
	default:
		return FetchMethodHeadless
	}
}

// closePage drops a page with a context of its own, so a request that ran out
// of time still leaves no tab behind.
func closePage(page *Page) {
	ctx, cancel := context.WithTimeout(context.Background(), pageCloseTimeout)
	defer cancel()
	_ = page.Close(ctx)
}

// fetchSource downloads a page over HTTP: the source as the server sent it,
// decoded to UTF-8 and capped at MaxBytes.
func fetchSource(ctx context.Context, target string, cfg WebFetchOptions) (WebFetchResult, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return WebFetchResult{}, err
	}
	if cfg.UserAgent != "" {
		request.Header.Set("User-Agent", cfg.UserAgent)
	}
	for name, values := range cfg.Headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	// No Accept-Encoding is set: the transport then negotiates gzip itself and
	// hands back the decoded body, which keeps the source readable.
	client := &http.Client{Jar: cfg.Jar}
	response, err := client.Do(request)
	if err != nil {
		return WebFetchResult{}, err
	}
	defer response.Body.Close()

	// A page is an HTML document. Anything else — a PDF, an image, an archive —
	// is refused here, before its body is read, so a download cannot flood the
	// caller with bytes nobody asked for.
	contentType := response.Header.Get("Content-Type")
	if !IsPageContentType(contentType) {
		return WebFetchResult{}, notPageError(contentType)
	}

	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = WebFetchMaxBytesDefault
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return WebFetchResult{}, fmt.Errorf("read %s: %w", target, err)
	}
	var notes []string
	truncated := int64(len(body)) > maxBytes
	if truncated {
		body = body[:maxBytes]
		notes = append(notes, fmt.Sprintf("the source was truncated at %d bytes", maxBytes))
	}
	text, note, err := decodeBody(body, contentType)
	if err != nil {
		return WebFetchResult{}, err
	}
	if note != "" {
		notes = append(notes, note)
	}
	if truncated {
		// The cut may have gone through the last character.
		text = strings.ToValidUTF8(text, "")
	}
	finalURL := target
	if response.Request != nil && response.Request.URL != nil {
		finalURL = response.Request.URL.String()
	}
	return WebFetchResult{
		URL:         target,
		FinalURL:    finalURL,
		HTML:        text,
		Method:      FetchMethodHTTP,
		StatusCode:  response.StatusCode,
		ContentType: contentType,
		Loaded:      true,
		Notes:       notes,
	}, nil
}

// decodeBody converts a response body into UTF-8. The charset of the
// Content-Type decides first, then a byte order mark or the declaration of an
// HTML document. A body that is already valid UTF-8 without saying so is taken
// as UTF-8 — the sniffing would read it as windows-1252, which mangles every
// page that is modern but quiet about it. The returned note is a remark for the
// caller (an unknown charset, for instance).
func decodeBody(body []byte, contentType string) (string, string, error) {
	if label := mimeCharset(contentType); label != "" {
		text, note, err := decodeWithCharset(body, label)
		return text, note, err
	}
	if isMarkupContentType(contentType) {
		if enc, name, certain := charset.DetermineEncoding(body, contentType); certain {
			decoded, err := enc.NewDecoder().Bytes(body)
			if err != nil {
				return "", "", fmt.Errorf("cannot decode the response as %s: %w", name, err)
			}
			return string(decoded), "", nil
		}
	}
	if utf8.Valid(body) {
		return string(body), "", nil
	}
	return decodeWithCharset(body, "")
}

// decodeWithCharset decodes a body through an IANA/WHATWG charset label. An
// empty label means the charset is unknown, in which case the body is decoded
// per its byte order mark or left alone when it is already UTF-8.
func decodeWithCharset(body []byte, label string) (string, string, error) {
	name := strings.ToLower(strings.TrimSpace(label))
	switch name {
	case "utf-8", "utf8", "us-ascii", "ascii":
		return string(body), "", nil
	case "":
		if utf8.Valid(body) {
			return string(body), "", nil
		}
		// The HTML fallback of the WHATWG: an unlabelled legacy body.
		name = "windows-1252"
	}
	enc, err := htmlindex.Get(name)
	if err != nil || enc == nil {
		return string(body), fmt.Sprintf("unknown charset %q; the body was kept as it is", label), nil
	}
	decoded, err := enc.NewDecoder().Bytes(body)
	if err != nil {
		return "", "", fmt.Errorf("cannot decode the response as %s: %w", label, err)
	}
	return string(decoded), "", nil
}

// mimeCharset extracts the charset parameter of a Content-Type.
func mimeCharset(contentType string) string {
	if contentType == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(params["charset"])
}

// isMarkupContentType reports whether the response is a document whose charset
// may be declared inside the content.
func isMarkupContentType(contentType string) bool {
	mediaType := contentType
	if parsed, _, err := mime.ParseMediaType(contentType); err == nil {
		mediaType = parsed
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	return strings.Contains(mediaType, "html") || strings.Contains(mediaType, "xml")
}

// normalizeFetchURL validates an address and fills in a missing scheme.
func normalizeFetchURL(rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", errors.New("empty URL")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported URL scheme %q: use http or https", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("invalid URL %q: no host", rawURL)
	}
	return parsed.String(), nil
}
