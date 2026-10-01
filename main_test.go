package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"lightagent/internal/config"
	"lightagent/internal/utils"
)

func TestParseOptionsDefaults(t *testing.T) {
	o, rest, err := parseOptions(nil)
	if err != nil {
		t.Fatalf("parseOptions(nil): %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("rest = %v, want empty", rest)
	}
	if o.resume || o.prompt != "" || o.json || o.quiet || o.webPort != -1 {
		t.Fatalf("unexpected defaults: %+v", o)
	}
	if o.stream != nil || o.markdown != nil || o.result != nil {
		t.Fatalf("on|off overrides should default to auto (nil): %+v", o)
	}
}

func TestParseOptionsShortAndLong(t *testing.T) {
	o, rest, err := parseOptions([]string{
		"-r", "--prompt", "hi", "--json", "-q",
		"--web-port", "8080", "--markdown=off", "--result", "on",
		"a", "b",
	})
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if !o.resume || o.prompt != "hi" || !o.json || !o.quiet || o.webPort != 8080 {
		t.Fatalf("options = %+v", o)
	}
	if o.markdown == nil || *o.markdown {
		t.Fatalf("markdown = %v, want false", o.markdown)
	}
	if o.result == nil || !*o.result {
		t.Fatalf("result = %v, want true", o.result)
	}
	if len(rest) != 2 || rest[0] != "a" || rest[1] != "b" {
		t.Fatalf("rest = %v", rest)
	}

	// Long forms bind the same fields as the short ones.
	o2, _, err := parseOptions([]string{"--resume", "--config", "x.json", "--dir", "sub"})
	if err != nil {
		t.Fatalf("parseOptions long: %v", err)
	}
	if !o2.resume || o2.configPath != "x.json" || o2.dir != "sub" {
		t.Fatalf("long options = %+v", o2)
	}
}

func TestParseOptionsErrors(t *testing.T) {
	cases := [][]string{
		{"--nope"},
		{"-X"},
		{"--json"},
		{"--save", "--no-save"},
		{"--stream", "maybe"},
	}
	for _, args := range cases {
		if _, _, err := parseOptions(args); err == nil {
			t.Fatalf("parseOptions(%v) = nil error, want an error", args)
		}
	}
}

func TestParseOnOff(t *testing.T) {
	for _, v := range []string{"", "auto", "AUTO"} {
		got, err := parseOnOff(v)
		if err != nil || got != nil {
			t.Fatalf("parseOnOff(%q) = (%v, %v), want (nil, nil)", v, got, err)
		}
	}
	for _, v := range []string{"on", "true", "1", "yes"} {
		got, err := parseOnOff(v)
		if err != nil || got == nil || !*got {
			t.Fatalf("parseOnOff(%q) = (%v, %v), want true", v, got, err)
		}
	}
	for _, v := range []string{"off", "false", "0", "no"} {
		got, err := parseOnOff(v)
		if err != nil || got == nil || *got {
			t.Fatalf("parseOnOff(%q) = (%v, %v), want false", v, got, err)
		}
	}
	if _, err := parseOnOff("maybe"); err == nil {
		t.Fatal("parseOnOff(maybe) should fail")
	}
}

func TestHelpText(t *testing.T) {
	if text := helpText(nil); !strings.Contains(text, "usage:") || !strings.Contains(text, "--print-config") {
		t.Fatalf("top-level help is incomplete:\n%s", text)
	}
	if text := helpText([]string{"sessions"}); !strings.Contains(text, "prune") {
		t.Fatalf("sessions help is incomplete:\n%s", text)
	}
	if text := helpText([]string{"nope"}); !strings.Contains(text, "no help for") {
		t.Fatalf("unknown topic should say so:\n%s", text)
	}
}

func TestVersionString(t *testing.T) {
	if got := versionString(); !strings.HasPrefix(got, "lightagent ") {
		t.Fatalf("versionString = %q", got)
	}
}

func TestRunHelpVersionAndUnknown(t *testing.T) {
	var out, errBuf bytes.Buffer

	if code := run([]string{"--help"}, &out, &errBuf); code != 0 {
		t.Fatalf("--help code = %d", code)
	}
	if !strings.Contains(out.String(), "usage:") {
		t.Fatalf("--help output = %q", out.String())
	}

	out.Reset()
	if code := run([]string{"help", "completion"}, &out, &errBuf); code != 0 {
		t.Fatalf("help completion code = %d", code)
	}
	if !strings.Contains(out.String(), "bash|zsh|powershell") {
		t.Fatalf("help completion output = %q", out.String())
	}

	out.Reset()
	if code := run([]string{"--version"}, &out, &errBuf); code != 0 {
		t.Fatalf("--version code = %d", code)
	}
	if !strings.HasPrefix(out.String(), "lightagent ") {
		t.Fatalf("--version output = %q", out.String())
	}

	out.Reset()
	errBuf.Reset()
	if code := run([]string{"--bogus"}, &out, &errBuf); code != 2 {
		t.Fatalf("unknown flag code = %d, want 2", code)
	}
	if !strings.Contains(errBuf.String(), "--help") {
		t.Fatalf("unknown flag stderr = %q", errBuf.String())
	}

	out.Reset()
	errBuf.Reset()
	if code := run([]string{"nope"}, &out, &errBuf); code != 2 {
		t.Fatalf("unknown command code = %d, want 2", code)
	}
}

// TestRunOneShotJSON drives the whole -p/--json path against a closed port so
// the turn fails fast without any real API call.
func TestRunOneShotJSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := `{"openai":{"api_base":"http://127.0.0.1:1/v1","api_key":"x","model":"m","stream":false},"web":{"port":0}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()

	var out, errBuf bytes.Buffer
	code := run([]string{"-c", cfgPath, "-C", dir, "-p", "hello", "--json"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errBuf.String())
	}
	var res struct {
		Assistant string `json:"assistant"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("stdout is not JSON: %v (out=%q)", err, out.String())
	}
	if res.Error == "" {
		t.Fatalf("expected an error field, got %q", out.String())
	}
}

// TestPrintConfig checks the effective configuration is dumped with the API key
// masked.
func TestPrintConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"openai":{"api_key":"secret","model":"base"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	code := run([]string{"-c", cfgPath, "--model", "override", "--print-config"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errBuf.String())
	}
	body := out.String()
	if !strings.Contains(body, cfgPath) || !strings.Contains(body, `"model": "override"`) {
		t.Fatalf("print-config output = %q", body)
	}
	if strings.Contains(body, "secret") {
		t.Fatalf("api key leaked: %q", body)
	}
	if !strings.Contains(body, `"api_key": "***"`) {
		t.Fatalf("api key should be masked: %q", body)
	}
}

// TestSignalStatus pins the exit status a signal-triggered exit reports: the
// conventional 128+N form shells use.
func TestSignalStatus(t *testing.T) {
	if got := signalStatus(os.Interrupt); got != 130 {
		t.Fatalf("signalStatus(os.Interrupt) = %d, want 130", got)
	}
	if got := signalStatus(syscall.SIGTERM); got != 143 {
		t.Fatalf("signalStatus(SIGTERM) = %d, want 143", got)
	}
}

// TestShutdownHooksRunNewestFirst pins the order the signal exit uses: the
// hooks mirror the LIFO order of the deferred teardown they stand in for.
func TestShutdownHooksRunNewestFirst(t *testing.T) {
	shutdownHooksMu.Lock()
	saved := shutdownHooks
	shutdownHooks = nil
	shutdownHooksMu.Unlock()
	t.Cleanup(func() {
		shutdownHooksMu.Lock()
		shutdownHooks = saved
		shutdownHooksMu.Unlock()
	})

	var order []int
	onShutdown(func() { order = append(order, 1) })
	onShutdown(nil) // must be ignored
	onShutdown(func() { order = append(order, 2) })

	runShutdownHooks()
	if len(order) != 2 || order[0] != 2 || order[1] != 1 {
		t.Fatalf("hook order = %v, want [2 1]", order)
	}
}

// testProfileDir returns the profile directory the browser of this test runs on,
// and takes it away once the browsers are gone: a launch always names a profile,
// and a browser that was just closed can still hold its files for a moment, so
// the removal is retried instead of being left to the test framework (which would
// fail to remove the temporary directory it lives in).
func testProfileDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "browser-profile")
	t.Cleanup(func() {
		_ = utils.CloseSharedBrowsers()
		for attempt := 0; attempt < 20; attempt++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	return dir
}

// TestRunMainClosesTheFetcherBrowser checks the cleanup of the exit path: the
// browser the fetcher keeps in the background does not survive a run. Its profile
// does, which is what the next run picks up. It is skipped where no browser can
// be started, exactly like the fetcher itself skipping the browser.
func TestRunMainClosesTheFetcherBrowser(t *testing.T) {
	if os.Getenv("LIGHTAGENT_TEST_BROWSER") != "1" {
		t.Skip("browser tests are opt-in: set LIGHTAGENT_TEST_BROWSER=1 to run them")
	}
	if testing.Short() {
		t.Skip("short mode: no browser is started")
	}
	if !utils.ChromeAvailable("") {
		t.Skip("no Chromium-based browser available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!DOCTYPE html><html><body>pooled</body></html>")
	}))
	defer server.Close()

	// A fetch leaves its browser in the pool for the next one, on a profile of
	// its own (a launch always names one).
	profile := testProfileDir(t)
	if _, err := utils.WebFetch(ctx, server.URL, utils.WithFetchBrowser(utils.BrowserOptions{UserDataDir: profile})); err != nil {
		t.Fatalf("WebFetch: %v", err)
	}
	browser := utils.SharedBrowserInstance()
	if browser == nil || !browser.Alive() {
		t.Skip("the fetch did not keep a browser, so there is nothing to release")
	}

	// The exit path of the program releases it.
	if code := runMain([]string{"--version"}); code != 0 {
		t.Errorf("runMain(--version) = %d, want 0", code)
	}
	if left := utils.SharedBrowserInstance(); left != nil {
		t.Errorf("the pooled browser survived the exit path: %+v", left)
	}
	deadline := time.Now().Add(10 * time.Second)
	for browser.Alive() {
		if time.Now().After(deadline) {
			t.Fatalf("the browser is still alive after the exit path")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestMediaCapabilityRequiresBothSettings pins the rule the multimedia
// capability follows: configuring the model's accepted types turns the
// capability (and so the web attach control) on, while the model only gets the
// upload_media tool when the tool's own switch is on as well.
func TestMediaCapabilityRequiresBothSettings(t *testing.T) {
	cases := []struct {
		name       string
		types      []string
		toolOn     bool
		wantEnable bool
		wantTool   bool
	}{
		{"neither", nil, false, false, false},
		{"switch only", nil, true, false, false},
		{"types only", []string{"image/png"}, false, true, false},
		{"both", []string{"image/png"}, true, true, true},
	}
	for _, tc := range cases {
		cfg := config.Default()
		cfg.OpenAI.MediaTypes = tc.types
		cfg.Tools.UploadMedia.Enabled = tc.toolOn
		media, tool := mediaCapability(cfg)
		if media.Enabled() != tc.wantEnable {
			t.Errorf("%s: capability = %v, want %v", tc.name, media.Enabled(), tc.wantEnable)
		}
		if tool != tc.wantTool {
			t.Errorf("%s: upload_media registered = %v, want %v", tc.name, tool, tc.wantTool)
		}
	}

	// The byte cap travels from the tool's setting into the capability.
	cfg := config.Default()
	cfg.OpenAI.MediaTypes = []string{"image"}
	cfg.Tools.UploadMedia.MaxBytes = 2048
	media, _ := mediaCapability(cfg)
	if media.Limit() != 2048 {
		t.Fatalf("limit = %d, want the configured cap", media.Limit())
	}
	if got := media.List(); got != "image/*" {
		t.Fatalf("types = %q, want the normalized family", got)
	}
}
