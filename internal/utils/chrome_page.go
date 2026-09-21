package utils

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TargetInfo describes one target of a browser (a tab, an iframe holder, an
// extension page, ...).
type TargetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Attached bool   `json:"attached"`
}

// Page is one tab of a browser, attached through a flattened protocol session
// of the browser connection. Everything Page does goes through that session, so
// the same page type drives a tab the agent opened and a tab of the user's own
// browser.
type Page struct {
	browser   *Browser
	targetID  string
	sessionID string
	owned     bool // the tab was opened here (Close closes it)

	mu       sync.Mutex
	prepared bool
	closed   bool
}

// Targets lists every target of the browser, the tabs of the user included.
func (b *Browser) Targets(ctx context.Context) ([]TargetInfo, error) {
	var result struct {
		TargetInfos []TargetInfo `json:"targetInfos"`
	}
	if err := b.call(ctx, "", "Target.getTargets", nil, &result); err != nil {
		return nil, err
	}
	return result.TargetInfos, nil
}

// Pages lists the page targets (the tabs) of the browser.
func (b *Browser) Pages(ctx context.Context) ([]TargetInfo, error) {
	infos, err := b.Targets(ctx)
	if err != nil {
		return nil, err
	}
	pages := make([]TargetInfo, 0, len(infos))
	for _, info := range infos {
		if info.Type == "page" {
			pages = append(pages, info)
		}
	}
	return pages, nil
}

// NewPage opens a new tab and attaches to it. The tab belongs to the caller:
// Page.Close closes it, and so does Browser.Close.
func (b *Browser) NewPage(ctx context.Context) (*Page, error) {
	var created struct {
		TargetID string `json:"targetId"`
	}
	params := map[string]any{"url": "about:blank", "background": false}
	if err := b.call(ctx, "", "Target.createTarget", params, &created); err != nil {
		return nil, err
	}
	if created.TargetID == "" {
		return nil, errors.New("browser did not return a target id")
	}
	b.mu.Lock()
	b.created[created.TargetID] = struct{}{}
	b.mu.Unlock()

	page, err := b.AttachPage(ctx, created.TargetID)
	if err != nil {
		return nil, err
	}
	page.owned = true
	return page, nil
}

// AttachPage attaches to an existing tab; Page.Close then detaches instead of
// closing it, so a tab of the user's browser survives the agent.
func (b *Browser) AttachPage(ctx context.Context, targetID string) (*Page, error) {
	if strings.TrimSpace(targetID) == "" {
		return nil, errors.New("empty target id")
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	err := b.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &attached)
	if err != nil {
		return nil, err
	}
	if attached.SessionID == "" {
		return nil, errors.New("browser did not return a session id")
	}
	return &Page{browser: b, targetID: targetID, sessionID: attached.SessionID}, nil
}

// Browser returns the browser the page belongs to.
func (p *Page) Browser() *Browser { return p.browser }

// TargetID returns the protocol id of the tab.
func (p *Page) TargetID() string { return p.targetID }

// SessionID returns the protocol session the page calls travel on.
func (p *Page) SessionID() string { return p.sessionID }

// call runs one session scoped protocol command.
func (p *Page) call(ctx context.Context, method string, params any, out any) error {
	if err := p.usable(); err != nil {
		return err
	}
	return p.browser.call(ctx, p.sessionID, method, params, out)
}

// usable reports whether the page can still take calls.
func (p *Page) usable() error {
	if p == nil || p.browser == nil {
		return errors.New("page is not attached to a browser")
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return errors.New("page is closed")
	}
	if !p.browser.Alive() {
		return errors.New("browser is gone")
	}
	return nil
}

// prepare enables the protocol domains a page uses. It happens once per page
// and is called by every operation that needs them, so a caller that only
// attaches to a tab (the user's own, for instance) does not have to know about
// them.
//
// The Runtime domain is deliberately not among them: nothing here listens to its
// events, and evaluating JavaScript (Runtime.evaluate) needs no enabling. That
// matters beyond tidiness — a page can tell whether Runtime was enabled (an
// anti-bot script reads an object it logs through a getter, which a browser with
// the domain on looks at while a browser without it does not), so the domain
// stays off.
func (p *Page) prepare(ctx context.Context) error {
	p.mu.Lock()
	prepared := p.prepared
	p.mu.Unlock()
	if prepared {
		return nil
	}
	for _, domain := range []string{"Page", "DOM", "Network"} {
		if err := p.call(ctx, domain+".enable", nil, nil); err != nil {
			return err
		}
	}
	if err := p.hideAutomation(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	p.prepared = true
	p.mu.Unlock()
	return nil
}

// Detach releases the session. The tab keeps running, which is what happens to
// a tab of the user's browser when the agent is done with it.
func (p *Page) Detach(ctx context.Context) error {
	p.mu.Lock()
	already := p.closed
	p.closed = true
	p.mu.Unlock()
	if already || p.browser == nil || p.browser.conn == nil {
		return nil
	}
	return p.browser.call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": p.sessionID}, nil)
}

// Close closes the tab when this connection opened it and detaches from it
// otherwise.
func (p *Page) Close(ctx context.Context) error {
	p.mu.Lock()
	owned := p.owned
	already := p.closed
	p.closed = true
	session, target := p.sessionID, p.targetID
	p.mu.Unlock()
	if already || p.browser == nil || p.browser.conn == nil {
		return nil
	}
	p.browser.forgetTarget(target)
	if !owned {
		return p.browser.call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": session}, nil)
	}
	return p.browser.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": target}, nil)
}

// forgetTarget drops a tab from the bookkeeping of a browser.
func (b *Browser) forgetTarget(targetID string) {
	b.mu.Lock()
	delete(b.created, targetID)
	b.mu.Unlock()
}

// Navigation reports the outcome of a navigation. ErrorText holds the reason a
// navigation never got a document (a DNS failure, a refused connection) in the
// protocol's wording ("net::ERR_NAME_NOT_RESOLVED"). Loaded is false when the
// timeout expired before the load finished: the DOM is then whatever the page
// managed to build so far, which is still worth capturing.
type Navigation struct {
	URL       string
	FrameID   string
	ErrorText string
	Loaded    bool
	Waited    time.Duration
}

// Navigate loads a URL and waits for the page to finish loading, at most
// timeout. A timeout is not an error: the result says Loaded false and the
// caller keeps the DOM as it is. settle adds a quiet period after the load,
// which Single-Page applications usually need to render their content.
func (p *Page) Navigate(ctx context.Context, rawURL string, timeout, settle time.Duration) (Navigation, error) {
	nav := Navigation{URL: rawURL}
	if strings.TrimSpace(rawURL) == "" {
		return nav, errors.New("empty URL")
	}
	if err := p.prepare(ctx); err != nil {
		return nav, err
	}
	// The subscription only wakes the wait up; whether the load is really over
	// is decided by the document itself (see settled).
	loads := p.browser.conn.subscribe(p.sessionID, "Page.loadEventFired")
	defer loads.cancel()

	var result struct {
		FrameID   string `json:"frameId"`
		ErrorText string `json:"errorText"`
	}
	if err := p.call(ctx, "Page.navigate", map[string]any{"url": rawURL}, &result); err != nil {
		return nav, err
	}
	nav.FrameID, nav.ErrorText = result.FrameID, result.ErrorText
	if nav.ErrorText != "" {
		return nav, nil
	}

	waitCtx, cancel := withTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	loaded, _, err := p.waitSettled(waitCtx, rawURL, loads)
	nav.Waited = time.Since(start)
	if err != nil {
		return nav, err
	}
	if !loaded && ctx.Err() != nil {
		// The caller gave up before the page was done, which is different from
		// the load timeout: the page never got its chance.
		return nav, ctx.Err()
	}
	nav.Loaded = loaded
	if settle > 0 {
		if err := sleepContext(ctx, settle); err != nil {
			return nav, err
		}
	}
	return nav, nil
}

// WaitForLoad waits for a page that is loading on its own (a navigation the
// page started, a form it submitted) to become complete.
func (p *Page) WaitForLoad(ctx context.Context, timeout time.Duration) error {
	if err := p.prepare(ctx); err != nil {
		return err
	}
	waitCtx, cancel := withTimeout(ctx, timeout)
	defer cancel()
	loaded, href, err := p.waitSettled(waitCtx, "", nil)
	if err != nil {
		return err
	}
	if !loaded {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("page %s: %w", href, err)
		}
		return fmt.Errorf("page %s did not finish loading", href)
	}
	return nil
}

// waitSettled waits until the document is completely loaded and the URL has
// moved on from about:blank, or until the context ends (which returns false
// without an error). extra, when given, wakes the wait up on every load event.
func (p *Page) waitSettled(ctx context.Context, wantURL string, extra *cdpSubscription) (bool, string, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	href := ""
	var wake <-chan cdpEvent
	if extra != nil {
		wake = extra.ch
	}
	for {
		settled, current, err := p.settled(ctx, wantURL)
		if err == nil {
			href = current
			if settled {
				return true, href, nil
			}
		} else if ctx.Err() != nil {
			// The wait itself ran out, or was cancelled, while asking the page
			// how far it got: that is the timeout the caller handles, not a
			// failure of the page.
			return false, href, nil
		}
		select {
		case <-ctx.Done():
			return false, href, nil
		case <-ticker.C:
		case <-wake:
		case <-p.browser.conn.closeDone():
			return false, href, p.browser.conn.closeError()
		}
	}
}

// settledScript reports whether the document has finished loading and where it
// currently is; the URL tells a still untouched about:blank apart from a
// document that has begun to load.
const settledScript = `(function(){return JSON.stringify({ready:document.readyState,href:document.location.href})})()`

// settled asks the document whether it is done.
func (p *Page) settled(ctx context.Context, wantURL string) (bool, string, error) {
	raw, err := p.evaluateString(ctx, settledScript)
	if err != nil {
		return false, "", err
	}
	var state struct {
		Ready string `json:"ready"`
		Href  string `json:"href"`
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return false, "", err
	}
	if state.Ready != "complete" {
		return false, state.Href, nil
	}
	if isBlankURL(wantURL) {
		return true, state.Href, nil
	}
	return !isBlankURL(state.Href), state.Href, nil
}

// isBlankURL reports whether a URL is the empty starting page.
func isBlankURL(raw string) bool {
	trimmed := strings.TrimSpace(strings.ToLower(raw))
	return trimmed == "" || trimmed == "about:blank"
}

// withTimeout bounds a wait; a non-positive timeout leaves the context alone.
func withTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// sleepContext waits for a duration, giving up when the context ends.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SetUserAgent overrides the user agent of the page (an empty value restores
// the browser default).
func (p *Page) SetUserAgent(ctx context.Context, userAgent string) error {
	if err := p.prepare(ctx); err != nil {
		return err
	}
	return p.call(ctx, "Emulation.setUserAgentOverride", map[string]any{"userAgent": userAgent}, nil)
}

// SetHeaders adds HTTP headers to every request the page makes.
func (p *Page) SetHeaders(ctx context.Context, headers http.Header) error {
	if len(headers) == 0 {
		return nil
	}
	if err := p.prepare(ctx); err != nil {
		return err
	}
	flat := make(map[string]string, len(headers))
	for name, values := range headers {
		if len(values) > 0 {
			flat[name] = strings.Join(values, ", ")
		}
	}
	return p.call(ctx, "Network.setExtraHTTPHeaders", map[string]any{"headers": flat}, nil)
}

// SetCookies adds or replaces cookies for the page's current address.
func (p *Page) SetCookies(ctx context.Context, cookies ...Cookie) error {
	if len(cookies) == 0 {
		return nil
	}
	if err := p.prepare(ctx); err != nil {
		return err
	}
	return p.call(ctx, "Network.setCookies", map[string]any{"cookies": cookies}, nil)
}

// Cookies returns the cookies that apply to the URLs (to the current address
// when none are named), which is the page scoped view of the cookie store.
func (p *Page) Cookies(ctx context.Context, urls ...string) ([]Cookie, error) {
	if err := p.prepare(ctx); err != nil {
		return nil, err
	}
	params := map[string]any{}
	if len(urls) > 0 {
		params["urls"] = urls
	}
	var result struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err := p.call(ctx, "Network.getCookies", params, &result); err != nil {
		return nil, err
	}
	return result.Cookies, nil
}

// Evaluate runs a JavaScript expression in the page and returns its value as
// JSON; promises are awaited and the value is transferred by value.
func (p *Page) Evaluate(ctx context.Context, expression string) (json.RawMessage, error) {
	if err := p.prepare(ctx); err != nil {
		return nil, err
	}
	var result struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	params := map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	}
	if err := p.call(ctx, "Runtime.evaluate", params, &result); err != nil {
		return nil, err
	}
	if result.ExceptionDetails != nil {
		message := strings.TrimSpace(result.ExceptionDetails.Text)
		if details := result.ExceptionDetails.Exception; details != nil && details.Description != "" {
			message = firstLine(details.Description)
		}
		if message == "" {
			message = "javascript error"
		}
		return nil, fmt.Errorf("javascript evaluation failed: %s", message)
	}
	return result.Result.Value, nil
}

// evaluateString runs a JavaScript expression that yields a string.
func (p *Page) evaluateString(ctx context.Context, expression string) (string, error) {
	raw, err := p.Evaluate(ctx, expression)
	if err != nil {
		return "", err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("javascript result is not a string: %w", err)
	}
	return value, nil
}

// URL returns the address the page currently shows.
func (p *Page) URL(ctx context.Context) (string, error) {
	return p.evaluateString(ctx, "document.location.href")
}

// Title returns the title of the document.
func (p *Page) Title(ctx context.Context) (string, error) {
	return p.evaluateString(ctx, "document.title")
}

// StatusCode returns the status of the main document when the browser reports
// it (it does since Chrome 109) and 0 when there was no network response.
func (p *Page) StatusCode(ctx context.Context) int {
	raw, err := p.Evaluate(ctx, statusScript)
	if err != nil {
		return 0
	}
	var code int
	if err := json.Unmarshal(raw, &code); err != nil {
		return 0
	}
	return code
}

// ContentType returns the MIME type the browser assigned to the document.
func (p *Page) ContentType(ctx context.Context) string {
	value, err := p.evaluateString(ctx, "document.contentType")
	if err != nil {
		return ""
	}
	return value
}

// statusScript reads the status of the navigation response out of the
// performance timeline.
const statusScript = `(function(){try{var entries=performance.getEntriesByType("navigation");` +
	`if(!entries.length){return 0}return entries[0].responseStatus||0}catch(error){return 0}})()`

// domNode is the part of the protocol's DOM.Node that the serializer needs.
type domNode struct {
	NodeID    int64     `json:"nodeId"`
	NodeType  int       `json:"nodeType"`
	NodeName  string    `json:"nodeName"`
	LocalName string    `json:"localName"`
	Children  []domNode `json:"children"`
}

// Node types, used to pick the nodes of the serialization.
const (
	domElementNode      = 1
	domDocumentNode     = 9
	domDocumentTypeNode = 10
)

// HTML returns the DOM tree of the page serialized as HTML: the document as the
// browser built it — scripts that ran, content that was inserted, nodes that
// were removed — not the source it was served from. Frames are serialized as
// their <iframe> element: the content of a child frame belongs to its own
// document, which is attached separately (browser.AttachPage on its target).
//
// A document that replaces itself while it is read — a site that answers with a
// script challenge and then writes the real page, a page that reloads itself —
// leaves the node ids of the reading behind. The reading is then simply taken
// again (see staleDocumentAttempts): the caller cannot do that itself, and the
// alternative is a fetch that reports an error although the page is right there.
func (p *Page) HTML(ctx context.Context) (string, error) {
	if err := p.prepare(ctx); err != nil {
		return "", err
	}
	var (
		html string
		err  error
	)
	for attempt := 0; attempt < staleDocumentAttempts; attempt++ {
		html, err = p.documentHTML(ctx)
		if err == nil || !isStaleNodeError(err) {
			break
		}
		if err := sleepContext(ctx, staleDocumentPause); err != nil {
			return "", err
		}
	}
	if err != nil {
		return "", err
	}
	if !hasDoctypePrefix(html) {
		// The serializer leaves the doctype out, so it is read from the
		// document itself and put back in front.
		if doctype, err := p.evaluateString(ctx, doctypeScript); err == nil && doctype != "" {
			html = doctype + "\n" + html
		}
	}
	return html, nil
}

const (
	// staleDocumentAttempts is how often the document of a page is read before a
	// page that keeps replacing it is reported as unreadable.
	staleDocumentAttempts = 3
	// staleDocumentPause is the moment between two readings of such a document.
	staleDocumentPause = 50 * time.Millisecond
)

// isStaleNodeError reports whether a protocol error names a node that is gone,
// which is what reading a document that replaced itself produces. The protocol
// code is the generic one, so the message is what tells it apart.
func isStaleNodeError(err error) bool {
	var rpc *cdpRPCError
	if !errors.As(err, &rpc) {
		return false
	}
	return strings.Contains(rpc.Message, "Could not find node with given id") ||
		strings.Contains(rpc.Message, "does not belong to the document")
}

// documentHTML reads the document of the page and serializes it.
func (p *Page) documentHTML(ctx context.Context) (string, error) {
	var document struct {
		Root domNode `json:"root"`
	}
	params := map[string]any{"depth": -1, "pierce": true}
	if err := p.call(ctx, "DOM.getDocument", params, &document); err != nil {
		return "", err
	}
	if document.Root.NodeID == 0 {
		return "", errors.New("browser returned an empty document")
	}

	// The document node carries the whole tree; a node that is not a document
	// (a fragment of the user's browser, a detached node) is serialized as it
	// is.
	html, err := p.outerHTML(ctx, document.Root.NodeID)
	if err != nil || strings.TrimSpace(html) == "" {
		element := document.Root.findElement()
		if element == nil {
			if err != nil {
				return "", err
			}
			return "", errors.New("browser returned an empty document")
		}
		html, err = p.outerHTML(ctx, element.NodeID)
		if err != nil {
			return "", err
		}
	}
	return html, nil
}

// outerHTML serializes a single node with everything below it.
func (p *Page) outerHTML(ctx context.Context, nodeID int64) (string, error) {
	var result struct {
		OuterHTML string `json:"outerHTML"`
	}
	if err := p.call(ctx, "DOM.getOuterHTML", map[string]any{"nodeId": nodeID}, &result); err != nil {
		return "", err
	}
	return result.OuterHTML, nil
}

// findElement returns the first child element of a node, which is the document
// element of a document.
func (n *domNode) findElement() *domNode {
	for i := range n.Children {
		if n.Children[i].NodeType == domElementNode {
			return &n.Children[i]
		}
	}
	return nil
}

// doctypeScript rebuilds the doctype of a document.
const doctypeScript = `(function(){var doctype=document.doctype;if(!doctype){return ""}` +
	`var out="<!DOCTYPE "+doctype.name;` +
	`if(doctype.publicId){out+=' PUBLIC "'+doctype.publicId+'"'}` +
	`if(doctype.systemId){if(!doctype.publicId){out+=" SYSTEM"}out+=' "'+doctype.systemId+'"'}` +
	`return out+">"})()`

// hasDoctypePrefix reports whether serialized HTML already begins with a
// doctype declaration.
func hasDoctypePrefix(html string) bool {
	trimmed := strings.TrimLeft(html, " \t\r\n")
	const prefix = "<!doctype"
	return len(trimmed) >= len(prefix) && strings.EqualFold(trimmed[:len(prefix)], prefix)
}

// firstLine returns the first line of a message, which is the part of a
// JavaScript error that names the problem.
func firstLine(text string) string {
	trimmed := strings.TrimSpace(text)
	if index := strings.IndexByte(trimmed, '\n'); index >= 0 {
		return strings.TrimSpace(trimmed[:index])
	}
	return trimmed
}
