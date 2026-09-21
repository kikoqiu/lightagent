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

// browserProfileDir decides which profile a launch uses. A headless launch
// without an explicit directory gets a private temporary one (owned, removed
// with the browser) so that concurrent runs never fight over a profile; a
// headful launch falls back to the default profile of the user, which is what
// makes their cookies and logins visible.
func browserProfileDir(opts BrowserOptions) (dir string, owned bool, err error) {
	if trimmed := strings.TrimSpace(opts.UserDataDir); trimmed != "" {
		if err := os.MkdirAll(trimmed, 0o700); err != nil {
			return "", false, fmt.Errorf("browser profile %s: %w", trimmed, err)
		}
		return trimmed, false, nil
	}
	if opts.Headful {
		return "", false, nil
	}
	dir, err = os.MkdirTemp("", "lightagent-browser-")
	if err != nil {
		return "", false, fmt.Errorf("cannot create a browser profile: %w", err)
	}
	return dir, true, nil
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

// browserLaunchArgs builds the command line of a launch. The trailing
// about:blank gives a fresh browser a defined first tab instead of one that
// restores history.
func browserLaunchArgs(profileDir string, port int, opts BrowserOptions) []string {
	args := []string{
		"--remote-debugging-port=" + strconv.Itoa(port),
		"--no-first-run",
		"--no-default-browser-check",
		// A profile whose last session did not end cleanly (a browser that was
		// killed, a machine that went down) makes Chrome offer to restore that
		// session: the bubble has no place in an unattended run, and its pages
		// would compete with the page being fetched.
		"--hide-crash-restore-bubble",
		"--disable-session-crashed-bubble",
		"--noerrdialogs",
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
		"--password-store=basic",
		"--use-mock-keychain",
	}
	if !opts.Headful {
		// "new" selects the current headless implementation; a browser that
		// predates the value reads the switch all the same. The rest keeps an
		// unattended run quiet and windowless.
		args = append(args,
			"--headless=new",
			"--disable-gpu",
			"--disable-dev-shm-usage",
			"--hide-scrollbars",
			"--window-size=1280,900",
		)
	}
	if profileDir != "" {
		args = append(args, "--user-data-dir="+profileDir)
	}
	args = append(args, opts.ExtraArgs...)
	return append(args, "about:blank")
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
