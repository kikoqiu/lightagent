package utils

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// browserProfileDir decides which profile a launch uses. The caller names it: an
// explicit directory is created when it is missing and reused exactly as it
// stands, which is what carries the cookies and logins of a project from one run
// to the next. Only a launch with a window may leave it empty, which means the
// default profile of the user — the one their own browser has open, and the only
// way to their cookies. A headless launch without a directory is refused instead
// of inventing one: an invisible browser has no business writing into a profile
// nobody asked for.
func browserProfileDir(opts BrowserOptions) (dir string, err error) {
	if trimmed := strings.TrimSpace(opts.UserDataDir); trimmed != "" {
		if err := os.MkdirAll(trimmed, 0o700); err != nil {
			return "", fmt.Errorf("browser profile %s: %w", trimmed, err)
		}
		return trimmed, nil
	}
	if opts.Headful {
		return "", nil
	}
	return "", errors.New("a headless browser needs a profile directory (BrowserOptions.UserDataDir)")
}

// browserDebugPort returns the DevTools port of a launch and whether the port
// has to be read back from the DevToolsActivePort file: a port of 0 means the
// browser picks a free one, which it then publishes in the profile directory.
// A headful launch on the user's default profile has no directory of ours to
// read, so it is given a concrete free port instead.
func browserDebugPort(opts BrowserOptions, profileDir string) (port int, awaitPortFile bool) {
	if opts.DebugPort > 0 {
		return opts.DebugPort, false
	}
	if profileDir != "" {
		return 0, true
	}
	chosen, err := freePort()
	if err != nil {
		// Falling back to the well-known port is still better than refusing the
		// launch; a collision shows up as a timeout with a clear message.
		return 9222, false
	}
	return chosen, false
}

// freePort asks the operating system for a port that is free right now.
func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("cannot determine a free port")
	}
	return addr.Port, nil
}

// browserLaunchArgs builds the command line of a launch.
//
// A launch with a window has to pass for the browser the user starts
// themselves: the page that is rendered in it is read by the site's own scripts
// too, and the switches an automation setup carries — background networking and
// extensions off, a muted and unsized window, phishing detection and component
// updates off, and so on — are exactly what anti-bot code looks for. So a
// headful launch takes nothing but the switches that only silence prompts (see
// quietLaunchArgs). A headless launch is recognized as one whatever the command
// line says, so it keeps the whole automation set (see headlessLaunchArgs),
// which is what makes an unattended run quiet and light.
//
// One switch that such a command line usually carries is deliberately missing,
// because Chrome answers it with a bar across the window — a visible sign of an
// automation setup, and of a browser nobody started by hand: --disable-blink-features=AutomationControlled
// would hide the automation flag natively, and Chrome shows its "unsupported
// command-line flag" warning for it (checked on Chrome 153, where the bar took
// 56px off the content area). The flag is turned off from inside the page
// instead (see automationFlagScript).
//
// The trailing about:blank gives a fresh browser a defined first tab instead of
// one that restores history.
func browserLaunchArgs(profileDir string, port int, opts BrowserOptions) []string {
	args := append([]string{"--remote-debugging-port=" + strconv.Itoa(port)}, quietLaunchArgs()...)
	if !opts.Headful {
		args = append(args, headlessLaunchArgs()...)
	}
	if profileDir != "" {
		args = append(args, "--user-data-dir="+profileDir)
	}
	// The caller's arguments come last: for a switch that is passed once, like
	// --window-size, Chrome reads the value of the last occurrence, so the
	// caller overrules what we pass.
	args = append(args, opts.ExtraArgs...)
	return append(args, "about:blank")
}

// quietLaunchArgs are the switches that only keep the browser from asking
// questions of somebody who is not there. No page can observe them — they
// decide what the browser itself does about its profile and its dialogs, not
// what it does as a web client — so both launch kinds carry them.
func quietLaunchArgs() []string {
	return []string{
		"--no-default-browser-check",
		// A profile that has never been used makes Chrome open its first-run
		// flow, which ends in the sign-in dialog; the switch keeps that out of
		// the way (without it the first launch of a fresh profile opens that
		// dialog). It belongs to the quiet set rather than to the automation
		// set: a page cannot observe it, and what the window shows instead is
		// Chrome's own UI (a profile that is already in use shows neither).
		"--no-first-run",
		// A profile whose last session did not end cleanly (a browser that was
		// killed, a machine that went down) makes Chrome offer to restore that
		// session: the bubble has no place in an unattended run, and its pages
		// would compete with the page being fetched.
		"--hide-crash-restore-bubble",
		"--disable-session-crashed-bubble",
		"--noerrdialogs",
		// The profile keeps the cookies and logins of the previous runs, and the
		// store they are encrypted with stays the one the profile was created
		// with: a keyring that is locked would ask for a password nobody is
		// there to type, and it would make those cookies unreadable besides.
		"--password-store=basic",
		"--use-mock-keychain",
	}
}

// headlessLaunchArgs are the switches of a launch without a window: an invisible
// browser is recognized as one whatever the command line says (its user agent
// says "HeadlessChrome"), so looking like an ordinary browser gains nothing
// there, while this set keeps an unattended run quiet and cheap — no background
// traffic, no extensions, no updater, no crash reporting, no audio, and a window
// size that is not the size of a screen it cannot see.
func headlessLaunchArgs() []string {
	return []string{
		// "new" selects the current headless implementation; a browser that
		// predates the value reads the switch all the same.
		"--headless=new",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--hide-scrollbars",
		"--window-size=1280,900",
		"--disable-background-networking",
		"--disable-background-timer-throttling",
		"--disable-breakpad",
		"--disable-client-side-phishing-detection",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-extensions",
		"--disable-hang-monitor",
		"--disable-popup-blocking",
		"--disable-prompt-on-repost",
		"--disable-sync",
		"--metrics-recording-only",
		"--mute-audio",
		"--no-service-autorun",
	}
}

// waitForDevTools waits for a launched browser to publish its DevTools
// endpoint: probed over TCP for a port that was chosen for the launch, read back
// from the port file when the browser picked the port itself (a port of 0).
//
// A browser that was killed leaves its port file behind, naming a port nobody
// listens on any more, so an address read from the file is only accepted once it
// answers: the file keeps being read, and an address that did not answer is not
// tried again while the file keeps naming it. Without that, a leftover file
// would pin the wait on a dead port until the whole launch timed out — which is
// exactly what a browser killed by a timeout or by the agent exiting leaves
// behind. A browser that dies in the meantime ends the wait right away, with its
// exit status as the reason.
func (b *Browser) waitForDevTools(ctx context.Context, port int, awaitPortFile bool) (string, error) {
	address := "127.0.0.1:" + strconv.Itoa(port)
	// stale is the address a leftover port file named that did not answer.
	stale := ""
	var lastErr error
	for {
		if awaitPortFile {
			address = ""
			if found, err := readActivePort(b.profileDir); err == nil {
				if found != stale {
					address = found
				}
			} else {
				lastErr = err
			}
		}
		if address != "" {
			if err := probePort(ctx, address); err == nil {
				return address, nil
			} else {
				lastErr = err
				// Try the file again next round: the browser that is starting
				// rewrites it as soon as it knows its own port.
				stale = address
			}
		}
		if channelClosed(b.exited) {
			reason := "the browser exited"
			if err := b.exitError(); err != nil {
				reason = fmt.Sprintf("the browser exited: %v", err)
			}
			if lastErr != nil {
				return "", fmt.Errorf("%s before its DevTools endpoint was ready (%v)", reason, lastErr)
			}
			return "", fmt.Errorf("%s before its DevTools endpoint was ready", reason)
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return "", fmt.Errorf("browser DevTools endpoint did not become ready: %w (%v)", ctx.Err(), lastErr)
			}
			return "", fmt.Errorf("browser DevTools endpoint did not become ready: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// readActivePort reads the port a browser published in its profile directory
// (the first line of DevToolsActivePort).
func readActivePort(profileDir string) (string, error) {
	if profileDir == "" {
		return "", errors.New("no profile directory to read the DevTools port from")
	}
	raw, err := os.ReadFile(ActivePortFile(profileDir))
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 0 {
		return "", errors.New("empty DevToolsActivePort file")
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || port <= 0 {
		return "", fmt.Errorf("invalid DevTools port %q", strings.TrimSpace(lines[0]))
	}
	return "127.0.0.1:" + strconv.Itoa(port), nil
}

// probePort reports whether something accepts connections on a DevTools port.
func probePort(ctx context.Context, address string) error {
	dialCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", address)
	if err != nil {
		return err
	}
	return conn.Close()
}

// channelClosed reports whether a channel is already closed.
func channelClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
