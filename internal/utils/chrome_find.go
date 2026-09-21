package utils

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Environment variables that override the browser lookup. The first one wins.
const (
	// ChromePathEnv points at the browser executable to use.
	ChromePathEnv = "LIGHTAGENT_CHROME_PATH"
	// chromePathEnvAlt is the generic name other tools use as well.
	chromePathEnvAlt = "CHROME_PATH"
	// ChromeDebugPortEnv points at the DevTools endpoint of a running browser
	// ("127.0.0.1:9222"), which is how the user's own browser is reached. See
	// FindRunningBrowser.
	ChromeDebugPortEnv = "LIGHTAGENT_CHROME_DEBUG_PORT"
	// chromeDebugPortEnvAlt is the generic name other tools use as well.
	chromeDebugPortEnvAlt = "CHROME_DEBUG_PORT"
)

// ErrChromeNotFound reports that no Chromium-based browser could be located.
// Callers that only need the page source treat it as a reason to fetch over
// HTTP instead.
var ErrChromeNotFound = errors.New("no chromium-based browser found")

// FindChrome returns the executable of a Chromium-based browser. override (may
// be empty) takes precedence, then ChromePathEnv/CHROME_PATH, then the usual
// names on PATH and finally the standard install locations of Chrome, Chromium,
// Brave and Edge — anything that speaks the DevTools protocol will do.
func FindChrome(override string) (string, error) {
	if path := strings.TrimSpace(override); path != "" {
		if isExecutableFile(path) {
			return path, nil
		}
		return "", fmt.Errorf("%w: %s is not an executable", ErrChromeNotFound, path)
	}
	for _, env := range []string{ChromePathEnv, chromePathEnvAlt} {
		if path := strings.TrimSpace(os.Getenv(env)); path != "" {
			if isExecutableFile(path) {
				return path, nil
			}
		}
	}
	for _, name := range chromeExecNames() {
		if path, err := exec.LookPath(name); err == nil && isExecutableFile(path) {
			return path, nil
		}
	}
	for _, path := range chromeCandidatePaths() {
		if isExecutableFile(path) {
			return path, nil
		}
	}
	return "", ErrChromeNotFound
}

// ChromeAvailable reports whether a browser can be started at all, without
// distinguishing a missing browser from an override that does not exist.
func ChromeAvailable(override string) bool {
	_, err := FindChrome(override)
	return err == nil
}

// DefaultUserDataDirs returns the profile directories the installed browsers
// use for the current user, most likely first. They matter when an already
// running browser has to be found (it advertises its DevTools port there) or
// when the user's own cookies are wanted.
func DefaultUserDataDirs() []string {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return nil
		}
		return []string{
			filepath.Join(base, "Google", "Chrome", "User Data"),
			filepath.Join(base, "Chromium", "User Data"),
			filepath.Join(base, "BraveSoftware", "Brave-Browser", "User Data"),
			filepath.Join(base, "Microsoft", "Edge", "User Data"),
		}
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		support := filepath.Join(home, "Library", "Application Support")
		return []string{
			filepath.Join(support, "Google", "Chrome"),
			filepath.Join(support, "Chromium"),
			filepath.Join(support, "BraveSoftware", "Brave-Browser"),
			filepath.Join(support, "Microsoft Edge"),
		}
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		config := filepath.Join(home, ".config")
		return []string{
			filepath.Join(config, "google-chrome"),
			filepath.Join(config, "chromium"),
			filepath.Join(config, "BraveSoftware", "Brave-Browser"),
			filepath.Join(config, "microsoft-edge"),
		}
	}
}

// FindRunningBrowser returns the DevTools address of a browser that is already
// running, which is what makes the user's own browser (its logins, its cookies,
// its tabs) available to the agent. Candidates are tried in order: extra and
// the addresses named by ChromeDebugPortEnv/chromeDebugPortEnvAlt first, then
// the DevToolsActivePort of the default profile directories (a browser started
// with --remote-debugging-port=0 publishes it there) and finally the usual port
// 9222. Every candidate has to answer on /json/version, so a stale port file or
// an unrelated service on 9222 is not mistaken for a browser.
func FindRunningBrowser(ctx context.Context, extra ...string) (string, error) {
	var candidates []string
	candidates = append(candidates, extra...)
	for _, env := range []string{ChromeDebugPortEnv, chromeDebugPortEnvAlt} {
		if value := strings.TrimSpace(os.Getenv(env)); value != "" {
			candidates = append(candidates, value)
		}
	}
	for _, dir := range DefaultUserDataDirs() {
		if address, err := readActivePort(dir); err == nil {
			candidates = append(candidates, address)
		}
	}
	candidates = append(candidates, "127.0.0.1:9222")

	seen := make(map[string]struct{}, len(candidates))
	var lastErr error
	for _, candidate := range candidates {
		address, wsURL, err := normalizeAddress(candidate)
		if err != nil {
			lastErr = err
			continue
		}
		if _, ok := seen[address]; ok {
			continue
		}
		seen[address] = struct{}{}
		if _, err := probeEndpoint(ctx, address, wsURL); err != nil {
			lastErr = err
			continue
		}
		return address, nil
	}
	if lastErr != nil {
		return "", fmt.Errorf("no running browser found: %w", lastErr)
	}
	return "", errors.New("no running browser found")
}

// ActivePortFile returns the path of the DevToolsActivePort file of a profile
// directory. A browser started with --remote-debugging-port=0 writes the port
// it picked there.
func ActivePortFile(profileDir string) string {
	return filepath.Join(profileDir, "DevToolsActivePort")
}

// chromeExecNames lists the executable names tried through PATH.
func chromeExecNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"chrome.exe", "chromium.exe", "msedge.exe", "brave.exe"}
	}
	return []string{
		"google-chrome", "google-chrome-stable", "chromium", "chromium-browser",
		"chrome", "brave-browser", "microsoft-edge", "microsoft-edge-stable",
	}
}

// chromeCandidatePaths lists the standard install locations.
func chromeCandidatePaths() []string {
	switch runtime.GOOS {
	case "windows":
		return windowsChromeCandidates()
	case "darwin":
		return darwinChromeCandidates()
	default:
		return linuxChromeCandidates()
	}
}

// windowsChromeCandidates joins every Chrome-like relative path with every
// standard Windows install root.
func windowsChromeCandidates() []string {
	var roots []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA", "APPDATA"} {
		if dir := os.Getenv(env); dir != "" {
			roots = append(roots, dir)
		}
	}
	relatives := []string{
		filepath.Join("Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join("Google", "Chrome Beta", "Application", "chrome.exe"),
		filepath.Join("Google", "Chrome SxS", "Application", "chrome.exe"),
		filepath.Join("Chromium", "Application", "chrome.exe"),
		filepath.Join("BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
		filepath.Join("Microsoft", "Edge", "Application", "msedge.exe"),
	}
	paths := make([]string, 0, len(roots)*len(relatives))
	for _, root := range roots {
		for _, rel := range relatives {
			paths = append(paths, filepath.Join(root, rel))
		}
	}
	return paths
}

// darwinChromeCandidates lists the bundles of the macOS installs.
func darwinChromeCandidates() []string {
	bundles := [][2]string{
		{"Google Chrome.app", "Google Chrome"},
		{"Chromium.app", "Chromium"},
		{"Brave Browser.app", "Brave Browser"},
		{"Microsoft Edge.app", "Microsoft Edge"},
	}
	roots := []string{"/Applications"}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, "Applications"))
	}
	paths := make([]string, 0, len(roots)*len(bundles))
	for _, root := range roots {
		for _, bundle := range bundles {
			paths = append(paths, filepath.Join(root, bundle[0], "Contents", "MacOS", bundle[1]))
		}
	}
	return paths
}

// linuxChromeCandidates lists the usual Linux prefixes; the binaries are also
// looked up on PATH (see chromeExecNames).
func linuxChromeCandidates() []string {
	names := []string{
		"google-chrome", "google-chrome-stable", "chromium", "chromium-browser",
		"brave-browser",
	}
	prefixes := []string{"/usr/bin", "/usr/local/bin", "/snap/bin", "/opt/google/chrome"}
	paths := make([]string, 0, len(prefixes)*len(names))
	for _, prefix := range prefixes {
		for _, name := range names {
			paths = append(paths, filepath.Join(prefix, name))
		}
	}
	return paths
}

// isExecutableFile reports whether path is a regular file we could run. The
// execute bit is not checked: on Windows it carries no meaning, and elsewhere
// the candidate list holds real install locations.
func isExecutableFile(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
