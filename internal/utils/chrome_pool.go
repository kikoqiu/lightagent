package utils

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

// browserPool keeps one browser alive between requests. A browser start costs
// hundreds of milliseconds and a profile, and pages that were fetched a moment
// ago left cookies behind, so WebFetch reuses what is already there and closes
// it once it has been idle long enough.
type browserPool struct {
	// launchMu serializes opens: two callers that need a different browser must
	// not race for a profile directory, and one of them must not leave the
	// browser of the other unclosed.
	launchMu sync.Mutex

	mu         sync.Mutex
	key        string
	browser    *Browser
	timer      *time.Timer
	generation uint64
}

// sharedBrowsers is the pool WebFetch draws from.
var sharedBrowsers = &browserPool{}

// SharedBrowser returns a browser that outlives a single request: the same
// options hand back the same instance, so consecutive fetches share the process
// and its cookies. The browser is closed after KeepAlive of inactivity
// (BrowserKeepAliveDefault when the options leave it at zero).
//
// The pool holds one browser at a time: options that ask for a different
// browser close the previous one first. Launches are serialized, so two callers
// that need a different browser cannot race for the same profile directory.
func SharedBrowser(ctx context.Context, opts BrowserOptions) (*Browser, error) {
	return sharedBrowsers.acquire(ctx, opts)
}

// SharedBrowserInstance returns the browser the pool currently holds, or nil
// when there is none. It lets a caller work on the same browser the fetches are
// using, which is what makes the cookies of a fetch available afterwards.
func SharedBrowserInstance() *Browser {
	sharedBrowsers.mu.Lock()
	defer sharedBrowsers.mu.Unlock()
	return sharedBrowsers.browser
}

// CloseSharedBrowsers closes the browser of the pool and cancels its keep-alive
// timer. The browser is closed on an idle timeout in any case, and a launched
// one dies with this process even without this call (it belongs to the process
// tree), so calling it is tidiness rather than a necessity.
func CloseSharedBrowsers() error {
	return sharedBrowsers.closeAll()
}

// acquire hands out the pooled browser, launching or attaching one when the
// pool is empty or holds a different browser.
func (p *browserPool) acquire(ctx context.Context, opts BrowserOptions) (*Browser, error) {
	key := browserPoolKey(opts)
	keepAlive := opts.keepAlive()
	if browser, ok := p.take(key, keepAlive); ok {
		return browser, nil
	}

	// Opens are serialized: the caller that loses the race finds the browser of
	// the winner instead of starting a second one.
	p.launchMu.Lock()
	defer p.launchMu.Unlock()
	if browser, ok := p.take(key, keepAlive); ok {
		return browser, nil
	}
	if stale := p.detach(); stale != nil {
		_ = stale.Close()
	}

	browser, err := OpenBrowser(ctx, opts)
	if err != nil {
		return nil, err
	}
	p.store(key, browser, keepAlive)
	return browser, nil
}

// take returns the pooled browser when it is alive and matches the key, pushing
// its idle deadline out. Without a match the pool is left alone: detach and
// store replace its content.
func (p *browserPool) take(key string, keepAlive time.Duration) (*Browser, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.browser == nil || p.key != key || !p.browser.Alive() {
		return nil, false
	}
	p.scheduleLocked(keepAlive)
	return p.browser, true
}

// detach removes the pooled browser and cancels its timer.
func (p *browserPool) detach() *Browser {
	p.mu.Lock()
	defer p.mu.Unlock()
	browser := p.browser
	p.browser = nil
	p.key = ""
	p.generation++
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	return browser
}

// store makes a browser the pooled one and arms its idle timer.
func (p *browserPool) store(key string, browser *Browser, keepAlive time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.browser = browser
	p.key = key
	p.scheduleLocked(keepAlive)
}

// scheduleLocked arms the idle timer of the pooled browser (a keep-alive of
// zero means it stays until the program ends).
func (p *browserPool) scheduleLocked(keepAlive time.Duration) {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	if keepAlive <= 0 {
		return
	}
	p.generation++
	generation := p.generation
	p.timer = time.AfterFunc(keepAlive, func() { p.expire(generation) })
}

// expire closes the pooled browser once it has been idle for its keep-alive.
// The generation guards against a browser that was replaced (or reused) while
// the timer was running.
func (p *browserPool) expire(generation uint64) {
	p.mu.Lock()
	if p.generation != generation || p.browser == nil {
		p.mu.Unlock()
		return
	}
	browser := p.browser
	p.browser = nil
	p.key = ""
	p.timer = nil
	p.mu.Unlock()

	_ = browser.Close()
}

// closeAll closes the pooled browser and stops the keep-alive timer.
func (p *browserPool) closeAll() error {
	browser := p.detach()
	if browser == nil {
		return nil
	}
	return browser.Close()
}

// browserPoolKey identifies a browser configuration: two requests with the same
// key may share one browser, two with different keys may not.
func browserPoolKey(opts BrowserOptions) string {
	parts := []string{
		strings.TrimSpace(opts.Address),
		strings.TrimSpace(opts.ExecPath),
		strings.TrimSpace(opts.UserDataDir),
		strings.Join(opts.ExtraArgs, " "),
	}
	if opts.Headful {
		parts = append(parts, "headful")
	}
	if opts.DebugPort != 0 {
		parts = append(parts, "port="+strconv.Itoa(opts.DebugPort))
	}
	return strings.Join(parts, "|")
}
