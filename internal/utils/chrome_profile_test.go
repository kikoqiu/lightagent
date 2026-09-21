package utils

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writePortFile writes a DevTools port file the way a browser does: the port on
// the first line, the browser path below it.
func writePortFile(t *testing.T, dir string, port int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create the profile directory: %v", err)
	}
	content := fmt.Sprintf("%d\n/devtools/browser/test\n", port)
	if err := os.WriteFile(ActivePortFile(dir), []byte(content), 0o600); err != nil {
		t.Fatalf("write the port file: %v", err)
	}
}

// deadPort returns a port nothing listens on: a listener is bound to get a free
// one and closed again.
func deadPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind a port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return port
}

// TestDevToolsPortFileIsRereadWhenThePortIsStale pins what a browser killed by a
// timeout, by the idle close or by the agent exiting leaves behind: a port file
// naming a port nobody listens on. A launch must not pin its wait on that port —
// it used to keep probing it, and the whole fetch ran out of time while the
// browser that was starting had its own port in the file all along.
func TestDevToolsPortFileIsRereadWhenThePortIsStale(t *testing.T) {
	dir := t.TempDir()
	writePortFile(t, dir, deadPort(t))

	// A browser that is starting: it listens on its own port and rewrites the
	// file, a moment after the launch began.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the browser port: %v", err)
	}
	defer listener.Close()
	live := listener.Addr().(*net.TCPAddr).Port
	go func() {
		time.Sleep(200 * time.Millisecond)
		writePortFile(t, dir, live)
	}()

	browser := &Browser{profileDir: dir}
	address, err := browser.waitForDevTools(testContext(t), 0, true)
	if err != nil {
		t.Fatalf("waitForDevTools: %v", err)
	}
	if want := fmt.Sprintf("127.0.0.1:%d", live); address != want {
		t.Errorf("address = %q, want the port the browser published (%s)", address, want)
	}
}

// TestPrepareProfileDirClearsTheLeftoversOfAKilledBrowser pins the cleanup a
// profile gets before a launch: the port file of the browser that was killed is
// removed, and the "did not end cleanly" marker that would make Chrome offer to
// restore that session is rewritten, without touching the rest of the file.
func TestPrepareProfileDirClearsTheLeftoversOfAKilledBrowser(t *testing.T) {
	dir := t.TempDir()
	writePortFile(t, dir, deadPort(t))

	preferences := filepath.Join(dir, "Default", "Preferences")
	if err := os.MkdirAll(filepath.Dir(preferences), 0o700); err != nil {
		t.Fatal(err)
	}
	blob := `{"profile":{"exit_type":"Crashed","exited_cleanly":false,"name":"Person 1"},"intl":{"app_locale":"en-US"}}`
	if err := os.WriteFile(preferences, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}

	// An empty directory is the default profile of the user: nothing to clear.
	prepareProfileDir("")

	prepareProfileDir(dir)
	if _, err := os.Stat(ActivePortFile(dir)); !os.IsNotExist(err) {
		t.Errorf("the port file of the killed browser is still there (stat: %v)", err)
	}
	raw, err := os.ReadFile(preferences)
	if err != nil {
		t.Fatalf("read the preferences: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("a rewritten preferences file must stay valid JSON: %v", err)
	}
	profile, _ := doc["profile"].(map[string]any)
	if profile["exit_type"] != "Normal" || profile["exited_cleanly"] != true {
		t.Errorf("the exit state was not cleared: %v", profile)
	}
	if profile["name"] != "Person 1" {
		t.Errorf("the rest of the profile section was lost: %v", profile)
	}
	if _, ok := doc["intl"]; !ok {
		t.Errorf("the rest of the preferences was lost: %v", doc)
	}

	// A profile that already ended cleanly is left alone.
	prepareProfileDir(dir)
	again, err := os.ReadFile(preferences)
	if err != nil {
		t.Fatalf("read the preferences again: %v", err)
	}
	if string(again) != string(raw) {
		t.Error("a clean profile must not be rewritten")
	}
}

// TestPrepareProfileKeepsThePortOfARunningBrowser pins the one thing the cleanup
// must not do: a port file naming an endpoint that answers belongs to a browser
// that is still running on the profile, and it is the only thing that says where
// that browser is.
func TestPrepareProfileKeepsThePortOfARunningBrowser(t *testing.T) {
	dir := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer listener.Close()
	writePortFile(t, dir, listener.Addr().(*net.TCPAddr).Port)

	prepareProfileDir(dir)
	if _, err := os.Stat(ActivePortFile(dir)); err != nil {
		t.Errorf("the port file of a running browser was removed: %v", err)
	}

	// A file that cannot be read is not worth keeping.
	if err := os.WriteFile(ActivePortFile(dir), []byte("not a port\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepareProfileDir(dir)
	if _, err := os.Stat(ActivePortFile(dir)); !os.IsNotExist(err) {
		t.Errorf("an unusable port file was kept (stat: %v)", err)
	}
}

// TestWebFetchWorksAfterTheBrowserWasKilled covers the loop the leftovers used to
// produce: a browser is killed (as a fetch timeout, the idle close or the agent
// exiting did), and the next fetch — which has to start a browser on the very
// same profile — must still work instead of running into its timeout.
func TestWebFetchWorksAfterTheBrowserWasKilled(t *testing.T) {
	requireBrowser(t)
	server := newPageServer(t)
	profile := testProfileDir(t)
	t.Cleanup(func() { _ = CloseSharedBrowsers() })

	options := []WebFetchOptionFunc{
		WithFetchMode(FetchModeChromeHeadless),
		WithFetchBrowser(BrowserOptions{UserDataDir: profile}),
		WithFetchTimeout(45 * time.Second),
	}
	if _, err := WebFetch(testContext(t), server.URL+"/page", options...); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	browser := SharedBrowserInstance()
	if browser == nil {
		t.Fatal("no pooled browser to kill")
	}

	process, err := os.FindProcess(browser.PID())
	if err != nil {
		t.Fatalf("find the browser process: %v", err)
	}
	if err := process.Kill(); err != nil {
		t.Fatalf("kill the browser: %v", err)
	}
	waitForUnusableBrowser(t, browser)
	// The leftovers of the kill, made deterministic: a port file naming a dead
	// port and a profile that did not end cleanly.
	writePortFile(t, profile, deadPort(t))
	markUncleanExit(t, profile)

	started := time.Now()
	result, err := WebFetch(testContext(t), server.URL+"/page", options...)
	if err != nil {
		t.Fatalf("fetch after a killed browser: %v (the leftovers pin the launch again)", err)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("the fetch took %s, want a launch that does not wait for a dead port", elapsed)
	}
	if result.Method == FetchMethodHTTP {
		t.Errorf("method = %q, want a browser method", result.Method)
	}
	if !strings.Contains(result.HTML, "rendered-marker") {
		t.Errorf("the DOM does not carry the script output: %.200q", result.HTML)
	}

	// A graceful close ends the browser and leaves a profile that does not claim
	// to have crashed.
	if err := CloseSharedBrowsers(); err != nil {
		t.Fatalf("CloseSharedBrowsers: %v", err)
	}
	if exitState(t, profile) != "Normal" {
		t.Errorf("exit state after a graceful close = %q, want Normal", exitState(t, profile))
	}
}

// TestLaunchBrowserClearsTheLeftoversBeforeItStarts pins the missing piece of the
// story: the cleanup happens on the way into a launch, so a browser started
// directly on a profile that was left behind still comes up.
func TestLaunchBrowserClearsTheLeftoversBeforeItStarts(t *testing.T) {
	requireBrowser(t)
	profile := testProfileDir(t)
	writePortFile(t, profile, deadPort(t))
	markUncleanExit(t, profile)

	browser, err := LaunchBrowser(testContext(t), BrowserOptions{UserDataDir: profile})
	if err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	if browser.Address() == "" {
		t.Error("the browser has no DevTools address")
	}
	if state := exitState(t, profile); state == "Crashed" {
		t.Errorf("the launch did not clear the crash marker (exit state %q)", state)
	}
	if err := browser.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !channelClosed(browser.exited) {
		t.Error("the browser is still running after a graceful close")
	}
}

// TestBrowserProfileDir pins which profile a launch uses: the caller names it,
// it is created when it is missing and reused exactly as it stands (that is what
// carries the cookies of one fetch to the next), and only a launch that asks for
// a window may leave the directory empty. A headless launch without one is
// refused: there is no profile of ours to fall back to, and an invisible browser
// must not end up in the profile of the user.
func TestBrowserProfileDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	got, err := browserProfileDir(BrowserOptions{UserDataDir: dir})
	if err != nil {
		t.Fatalf("browserProfileDir: %v", err)
	}
	if got != dir {
		t.Errorf("profile = %q, want %q", got, dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the profile directory was not created: %v", err)
	}

	// The same directory again: what is in it is left as it stands.
	cookie := filepath.Join(dir, "Cookies")
	if err := os.WriteFile(cookie, []byte("kept"), 0o600); err != nil {
		t.Fatalf("write into the profile: %v", err)
	}
	if got, err := browserProfileDir(BrowserOptions{UserDataDir: dir, Headful: true}); err != nil || got != dir {
		t.Errorf("browserProfileDir(headful, %q) = %q, %v, want the same directory", dir, got, err)
	}
	if _, err := os.Stat(cookie); err != nil {
		t.Errorf("an existing profile was not left as it was: %v", err)
	}

	// A launch with a window may fall back to the default profile of the user.
	if got, err := browserProfileDir(BrowserOptions{Headful: true}); err != nil || got != "" {
		t.Errorf("browserProfileDir(headful, no directory) = %q, %v, want the user's default profile", got, err)
	}
	// A headless launch has nowhere else to go, so it must name one.
	if got, err := browserProfileDir(BrowserOptions{}); err == nil || got != "" {
		t.Errorf("browserProfileDir(headless, no directory) = %q, %v, want a refusal", got, err)
	}
}

// TestExplicitProfileDirectorySurvivesAClose pins the profile the webfetch tool
// uses (an explicit directory below the working directory): closing the browser
// — on the idle timeout, when the program exits — ends the process but leaves the
// directory and the state in it alone, which is what lets the next fetch reuse
// the same profile (its cookies and logins included) instead of starting over.
func TestExplicitProfileDirectorySurvivesAClose(t *testing.T) {
	requireBrowser(t)
	profile := testProfileDir(t)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatalf("create the profile: %v", err)
	}
	// A file standing in for the state of a profile: cookies, logins, settings.
	cookie := filepath.Join(profile, "Cookies")
	if err := os.WriteFile(cookie, []byte("stays"), 0o600); err != nil {
		t.Fatalf("write into the profile: %v", err)
	}

	browser, err := LaunchBrowser(testContext(t), BrowserOptions{UserDataDir: profile, KeepAlive: -1})
	if err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	if browser.profileDir != profile {
		t.Errorf("the browser runs on %q, want the profile it was given", browser.profileDir)
	}
	if err := browser.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(profile); err != nil {
		t.Errorf("the profile directory is gone after a close: %v", err)
	}
	if _, err := os.Stat(cookie); err != nil {
		t.Errorf("the state of the profile is gone after a close: %v", err)
	}
}

// waitForUnusableBrowser waits until the pool notices that a browser is gone.
func waitForUnusableBrowser(t *testing.T, browser *Browser) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !browser.Alive() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the pool still considers a killed browser usable")
}

// markUncleanExit writes the marker Chrome writes for a session that did not end
// cleanly.
func markUncleanExit(t *testing.T, profile string) {
	t.Helper()
	preferences := filepath.Join(profile, "Default", "Preferences")
	if err := os.MkdirAll(filepath.Dir(preferences), 0o700); err != nil {
		t.Fatal(err)
	}
	blob := `{"profile":{"exit_type":"Crashed","exited_cleanly":false}}`
	if err := os.WriteFile(preferences, []byte(blob), 0o600); err != nil {
		t.Fatalf("write the crash marker: %v", err)
	}
}

// exitState reads the last-exit state a profile carries.
func exitState(t *testing.T, profile string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(profile, "Default", "Preferences"))
	if err != nil {
		return "no preferences: " + err.Error()
	}
	var doc struct {
		Profile struct {
			ExitType string `json:"exit_type"`
		} `json:"profile"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "unparsable"
	}
	return doc.Profile.ExitType
}
