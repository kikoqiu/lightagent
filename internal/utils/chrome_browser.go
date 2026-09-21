package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"lightagent/internal/proc"
)

// BrowserMode reports how a Browser connection was obtained.
type BrowserMode string

const (
	// BrowserHeadless is a browser that was launched without a visible window.
	BrowserHeadless BrowserMode = "headless"
	// BrowserHeadful is a browser that was launched with a visible window;
	// with a profile directory of the user it works on the user's cookies.
	BrowserHeadful BrowserMode = "headful"
	// BrowserAttached is a browser somebody else runs, reached through its
	// DevTools endpoint: the actual browser of the user, with its tabs, its
	// logins and its cookies.
	BrowserAttached BrowserMode = "attached"
)

// BrowserKeepAliveDefault is how long a browser started by SharedBrowser stays
// alive after its last use before it is closed again.
const BrowserKeepAliveDefault = 10 * time.Minute

const (
	// browserLaunchTimeoutDefault bounds waiting for a launched browser to
	// answer on its DevTools endpoint.
	browserLaunchTimeoutDefault = 30 * time.Second
	// browserStopTimeout bounds the teardown of a browser (closing the pages
	// and waiting for the process to be gone).
	browserStopTimeout = 10 * time.Second
	// browserQuitTimeout bounds waiting for a launched browser to end after it
	// was asked to quit, before its process tree is killed.
	browserQuitTimeout = 5 * time.Second
)

// BrowserOptions configures how a browser is launched or attached to. A launch
// names the profile to use (UserDataDir); only a launch that asks for a window
// may leave it empty, which means the default profile of the user.
type BrowserOptions struct {
	// ExecPath is the browser executable; empty means auto-detection (see
	// FindChrome).
	ExecPath string
	// UserDataDir is the profile directory: it is created when it is missing and
	// reused exactly as it stands, so the cookies and logins one fetch leaves in
	// it are there for the next one. A headless launch needs it. A launch with a
	// window may leave it empty, which means the browser's default profile —
	// the one the user's own browser has open — which is how their cookies and
	// logins become reachable.
	UserDataDir string
	// Headful launches a visible window instead of a headless instance.
	Headful bool
	// Address attaches to the DevTools endpoint of an already running browser:
	// "127.0.0.1:9222", "9222", a URL or a ws:// browser socket. A browser the
	// user started with --remote-debugging-port is fully usable this way, which
	// is what shares their cookies with the agent.
	Address string
	// DebugPort is the DevTools port a launch must use; 0 lets the browser pick
	// one.
	DebugPort int
	// ExtraArgs appends raw command line switches to a launch.
	ExtraArgs []string
	// LaunchTimeout bounds waiting for the DevTools endpoint (default 30s).
	LaunchTimeout time.Duration
	// Stderr receives the browser's error output (default: discarded).
	Stderr io.Writer
	// KeepAlive is how long a browser of the shared pool survives without
	// being used (default BrowserKeepAliveDefault). A negative value keeps it
	// forever.
	KeepAlive time.Duration
}

// launchTimeout is the effective launch deadline.
func (o BrowserOptions) launchTimeout() time.Duration {
	if o.LaunchTimeout > 0 {
		return o.LaunchTimeout
	}
	return browserLaunchTimeoutDefault
}

// keepAlive is the effective keep-alive of the shared pool: zero means the
// default, a negative value means "no timeout".
func (o BrowserOptions) keepAlive() time.Duration {
	switch {
	case o.KeepAlive == 0:
		return BrowserKeepAliveDefault
	case o.KeepAlive < 0:
		return 0
	default:
		return o.KeepAlive
	}
}

// Browser is a connection to one browser, plus the process behind it when it
// was launched here. Pages are attached through it (see Page): the same
// connection serves the whole browser, so the caller reaches the user's own
// tabs and cookies exactly like the tabs the agent opened.
type Browser struct {
	mode     BrowserMode
	opts     BrowserOptions
	execPath string
	endpoint cdpEndpoint
	conn     *cdpConn

	cmd        *exec.Cmd
	exited     chan struct{} // closed once a launched process has been reaped
	profileDir string

	mu      sync.Mutex
	closed  bool
	created map[string]struct{} // page targets opened through this connection
	exitErr error
}

// OpenBrowser attaches to a running browser when opts.Address is set and
// launches one otherwise.
func OpenBrowser(ctx context.Context, opts BrowserOptions) (*Browser, error) {
	if strings.TrimSpace(opts.Address) != "" {
		return AttachBrowser(ctx, opts.Address, opts)
	}
	return LaunchBrowser(ctx, opts)
}

// LaunchBrowser starts a browser on the profile of the options and connects to
// its DevTools endpoint. The profile is named by the caller (UserDataDir); a
// headful launch without one uses the default profile of the current user, which
// only works while no other browser uses that profile — a running one has to be
// attached to instead. A headless launch without a profile is refused.
func LaunchBrowser(ctx context.Context, opts BrowserOptions) (*Browser, error) {
	execPath, err := FindChrome(opts.ExecPath)
	if err != nil {
		return nil, err
	}
	profileDir, err := browserProfileDir(opts)
	if err != nil {
		return nil, err
	}
	// A profile that outlives our browser carries the leftovers of the previous
	// run, and they would spoil this launch (see prepareProfileDir).
	prepareProfileDir(profileDir)
	port, awaitPortFile := browserDebugPort(opts, profileDir)
	cmd := exec.Command(execPath, browserLaunchArgs(profileDir, port, opts)...)
	cmd.Stdout = io.Discard
	if opts.Stderr != nil {
		cmd.Stderr = opts.Stderr
	} else {
		cmd.Stderr = io.Discard
	}
	// The browser and everything it spawns belong to this process: the tree is
	// terminated on shutdown even when the agent dies without running its
	// cleanup.
	if err := proc.Start(cmd); err != nil {
		return nil, fmt.Errorf("start %s: %w", execPath, err)
	}

	mode := BrowserHeadless
	if opts.Headful {
		mode = BrowserHeadful
	}
	browser := &Browser{
		mode:       mode,
		opts:       opts,
		execPath:   execPath,
		cmd:        cmd,
		exited:     make(chan struct{}),
		profileDir: profileDir,
		created:    make(map[string]struct{}),
	}
	go browser.waitProcess()

	launchCtx, cancel := context.WithTimeout(ctx, opts.launchTimeout())
	defer cancel()
	address, err := browser.waitForDevTools(launchCtx, port, awaitPortFile)
	if err != nil {
		_ = browser.Close()
		if opts.Headful && strings.TrimSpace(opts.UserDataDir) == "" {
			// The default profile belongs to the browser the user has open: a
			// second start only hands the request over to it and returns.
			return nil, fmt.Errorf("%w (the default profile is probably in use; "+
				"attach to the running browser with BrowserOptions.Address instead)", err)
		}
		return nil, err
	}
	endpoint, err := waitEndpoint(launchCtx, address, "")
	if err != nil {
		_ = browser.Close()
		return nil, err
	}
	if err := browser.connect(ctx, endpoint); err != nil {
		_ = browser.Close()
		return nil, err
	}
	return browser, nil
}

// AttachBrowser connects to the DevTools endpoint of a browser that is already
// running, which is how the user's own browser (with its cookies, its sessions
// and its tabs) is driven. Nothing is launched and nothing of the user's is
// closed on Close, except the tabs this connection opened.
func AttachBrowser(ctx context.Context, address string, opts BrowserOptions) (*Browser, error) {
	normalized, wsURL, err := normalizeAddress(address)
	if err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, opts.launchTimeout())
	defer cancel()
	endpoint, err := waitEndpoint(probeCtx, normalized, wsURL)
	if err != nil {
		return nil, fmt.Errorf("attach to %s: %w", address, err)
	}
	browser := &Browser{
		mode:     BrowserAttached,
		opts:     opts,
		endpoint: endpoint,
		created:  make(map[string]struct{}),
	}
	if err := browser.connect(ctx, endpoint); err != nil {
		return nil, fmt.Errorf("attach to %s: %w", address, err)
	}
	return browser, nil
}

// connect opens the protocol connection of a fresh browser.
func (b *Browser) connect(ctx context.Context, endpoint cdpEndpoint) error {
	conn, err := dialCDP(ctx, endpoint.WSURL, cdpHandshakeTimeout)
	if err != nil {
		return err
	}
	b.conn = conn
	b.endpoint = endpoint
	b.refreshVersion(ctx)
	return nil
}

// refreshVersion fills in the browser identification, which a WebSocket URL
// given by the caller does not carry.
func (b *Browser) refreshVersion(ctx context.Context) {
	var version struct {
		ProtocolVersion string `json:"protocolVersion"`
		Product         string `json:"product"`
		UserAgent       string `json:"userAgent"`
	}
	callCtx, cancel := context.WithTimeout(ctx, cdpHandshakeTimeout)
	defer cancel()
	if err := b.conn.call(callCtx, "", "Browser.getVersion", nil, &version); err != nil {
		return
	}
	if version.Product != "" {
		b.endpoint.Product = version.Product
	}
	if version.ProtocolVersion != "" {
		b.endpoint.ProtocolVersion = version.ProtocolVersion
	}
}

// Mode reports how this browser was obtained.
func (b *Browser) Mode() BrowserMode { return b.mode }

// Address reports the DevTools address of the browser.
func (b *Browser) Address() string { return b.endpoint.Address }

// Product reports the browser identification, e.g. "Chrome/153.0.8010.52".
func (b *Browser) Product() string { return b.endpoint.Product }

// ProtocolVersion reports the DevTools protocol version, e.g. "1.3".
func (b *Browser) ProtocolVersion() string { return b.endpoint.ProtocolVersion }

// PID reports the process id of a launched browser (0 when attached). It is
// mostly useful to diagnostics and tests.
func (b *Browser) PID() int {
	if b.cmd == nil || b.cmd.Process == nil {
		return 0
	}
	return b.cmd.Process.Pid
}

// Alive reports whether the browser can still be used.
func (b *Browser) Alive() bool {
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return false
	}
	if b.exited != nil && channelClosed(b.exited) {
		return false
	}
	return b.conn != nil && !channelClosed(b.conn.done)
}

// call is the session scoped protocol call of this browser; an empty session
// addresses the browser itself.
func (b *Browser) call(ctx context.Context, session, method string, params any, out any) error {
	if b.conn == nil {
		return errors.New("browser is not connected")
	}
	return b.conn.call(ctx, session, method, params, out)
}

// Options returns the configuration the browser was opened with, which is what
// a caller needs to open another one on the same profile.
func (b *Browser) Options() BrowserOptions { return b.opts }

// Close releases the browser: the pages this connection opened, the WebSocket
// and — for a browser that was launched here — the browser itself. The profile
// stays: it belongs to the caller, and its cookies and logins are what the next
// launch picks up.
//
// A launched browser is first asked to quit over the protocol and given a moment
// to end on its own; only a browser that does not go away is killed with its
// process tree. That matters beyond tidiness: a browser that ends by itself
// leaves a clean profile behind — no "did not end cleanly" marker, no leftover
// DevTools port file — so the next launch on the same profile starts straight
// away instead of working around the leftovers of this one (see
// prepareProfileDir, which cleans up after a browser that never got the chance).
// A browser that was attached to keeps running; nothing of the user's is closed.
func (b *Browser) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	conn := b.conn
	created := make([]string, 0, len(b.created))
	for targetID := range b.created {
		created = append(created, targetID)
	}
	cmd := b.cmd
	b.mu.Unlock()

	if conn != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), browserStopTimeout)
		for _, targetID := range created {
			_ = conn.call(closeCtx, "", "Target.closeTarget", map[string]any{"targetId": targetID}, nil)
		}
		cancel()
		if cmd != nil {
			// An empty session addresses the browser itself, which is where
			// Browser.close lives. The call fails once the browser goes away,
			// which is the point of it.
			quitCtx, cancel := context.WithTimeout(context.Background(), browserQuitTimeout)
			_ = conn.call(quitCtx, "", "Browser.close", nil, nil)
			cancel()
			select {
			case <-b.exited:
			case <-time.After(browserQuitTimeout):
			}
		}
		_ = conn.close()
	}
	if cmd != nil {
		_ = proc.Kill(cmd)
		select {
		case <-b.exited:
		case <-time.After(browserStopTimeout):
		}
	}
	return nil
}

// waitProcess reaps a launched browser process and releases the tree
// bookkeeping, so a browser that exits on its own leaves nothing behind.
func (b *Browser) waitProcess() {
	err := b.cmd.Wait()
	proc.Reap(b.cmd)
	b.mu.Lock()
	b.exitErr = err
	b.mu.Unlock()
	close(b.exited)
}

// exitError reports why a launched browser ended (nil while it runs).
func (b *Browser) exitError() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exitErr
}
