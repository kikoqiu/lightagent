package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lightagent/internal/utils"
)

// webfetchPageServer serves one page with a heading, a paragraph, a script and
// an ad-like block, so the conversion has something to simplify.
func webfetchPageServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html><html><head><title>Doc</title></head><body>
<h1>Getting started</h1><p>Install it and run it.</p>
<div class="ad">Buy now!</div>
<script>document.title = 'changed';</script>
</body></html>`)
	}))
	t.Cleanup(server.Close)
	return server
}

// newMarkdownWebFetchTool builds the tool in HTTP mode, so a test never depends
// on a browser being installed on the machine that runs it.
func newMarkdownWebFetchTool(cfg WebFetchConfig) *WebFetchTool {
	cfg.Mode = utils.FetchModeHTTP
	return NewWebFetchTool(cfg)
}

// TestWebFetchToolConvertsPageToMarkdown pins the answer the tool returns: the
// status line, the separator and the markdown the converter produced, plus the
// compression request the agent honours.
func TestWebFetchToolConvertsPageToMarkdown(t *testing.T) {
	server := webfetchPageServer(t)
	tool := newMarkdownWebFetchTool(WebFetchConfig{Timeout: 10 * time.Second, Compress: true, CompressRetries: 2})

	res := tool.Execute(context.Background(), map[string]any{"url": server.URL + "/docs"})
	if res.IsError {
		t.Fatalf("Execute reported an error: %s", res.ForLLM)
	}
	if !strings.HasPrefix(res.ForLLM, "Conversion succeeded. Converter warnings (if any): ") {
		t.Errorf("the answer is missing the status line:\n%s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "\n---\n\n") {
		t.Errorf("the answer is missing the body separator:\n%s", res.ForLLM)
	}
	// The answer carries no wrapper of its own: it is the tool's return value,
	// recorded as the tool message of the call.
	if got := strings.TrimSpace(res.ForLLM); strings.HasPrefix(got, "<") && strings.HasSuffix(got, ">") {
		t.Errorf("the tool wrapped its own answer:\n%s", res.ForLLM)
	}
	for _, want := range []string{"# Getting started", "Install it and run it."} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("the markdown is missing %q:\n%s", want, res.ForLLM)
		}
	}
	if strings.Contains(res.ForLLM, "document.title") {
		t.Errorf("the script should not reach the model:\n%s", res.ForLLM)
	}
	if !strings.Contains(res.ForUser, server.URL+"/docs") {
		t.Errorf("ForUser = %q, want the fetched address", res.ForUser)
	}
	// The tool asks for the self-compression pass, with the configured retries
	// and the timeout the caller gave it.
	if !res.Compress || res.CompressRetries != 2 {
		t.Errorf("compression request = %v/%d, want true/2", res.Compress, res.CompressRetries)
	}
}

func TestWebFetchToolKeepsCompressionOffWhenDisabled(t *testing.T) {
	server := webfetchPageServer(t)
	tool := newMarkdownWebFetchTool(WebFetchConfig{Timeout: 10 * time.Second})
	res := tool.Execute(context.Background(), map[string]any{"url": server.URL})
	if res.IsError {
		t.Fatalf("Execute reported an error: %s", res.ForLLM)
	}
	if res.Compress {
		t.Error("compression must stay off when the tool was built without it")
	}
}

// TestWebFetchToolKeepsTheDefaultForANullTimeout pins that an explicit null is
// read as "unset" (the configured default applies) rather than as an unusable
// value.
func TestWebFetchToolKeepsTheDefaultForANullTimeout(t *testing.T) {
	server := webfetchPageServer(t)
	tool := newMarkdownWebFetchTool(WebFetchConfig{Timeout: 10 * time.Second})
	res := tool.Execute(context.Background(), map[string]any{"url": server.URL, "timeout": nil})
	if res.IsError {
		t.Fatalf("a null timeout should keep the configured default: %s", res.ForLLM)
	}
}

func TestWebFetchToolRejectsBadArguments(t *testing.T) {
	tool := newMarkdownWebFetchTool(WebFetchConfig{Timeout: 10 * time.Second})
	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing url", map[string]any{}},
		{"empty url", map[string]any{"url": "   "}},
		{"non-string url", map[string]any{"url": 42}},
		{"zero timeout", map[string]any{"url": "https://example.com", "timeout": 0}},
		{"negative timeout", map[string]any{"url": "https://example.com", "timeout": -1}},
	}
	for _, testCase := range cases {
		res := tool.Execute(context.Background(), testCase.args)
		if !res.IsError {
			t.Errorf("%s: Execute = %+v, want an error result", testCase.name, res)
		}
	}
}

// TestWebFetchToolDefaults pins the fallbacks of a tool built from a config that
// leaves everything out.
func TestWebFetchToolDefaults(t *testing.T) {
	tool := NewWebFetchTool(WebFetchConfig{})
	if tool.cfg.Mode != utils.FetchModeAuto {
		t.Errorf("mode = %q, want %q", tool.cfg.Mode, utils.FetchModeAuto)
	}
	if tool.cfg.Timeout != webFetchTimeoutDefault {
		t.Errorf("timeout = %s, want %s", tool.cfg.Timeout, webFetchTimeoutDefault)
	}
}

// TestWebFetchToolDescriptionFollowsTheMode pins that the model is told how the
// pages are actually obtained: a tool pinned to the source must not advertise a
// browser.
func TestWebFetchToolDescriptionFollowsTheMode(t *testing.T) {
	const automatic = "rendered with a browser when one is available and downloaded over HTTP otherwise"
	cases := []struct {
		mode utils.FetchMode
		want string
	}{
		{"", automatic},
		{utils.FetchModeAuto, automatic},
		{utils.FetchModeBrowser, "rendered with a browser, and the fetch fails when none is available"},
		{utils.FetchModeHTTP, "downloaded over HTTP, without a browser"},
	}
	for _, tc := range cases {
		tool := NewWebFetchTool(WebFetchConfig{Mode: tc.mode})
		if !strings.Contains(tool.Description(), tc.want) {
			t.Errorf("Description() for mode %q = %q, want it to contain %q",
				tc.mode, tool.Description(), tc.want)
		}
	}
}

// TestWebFetchToolAppliesTheConfiguredWebSettings pins the settings a fetch
// carries: the configured user agent reaches the server and an oversized source
// is read only up to the configured byte cap.
func TestWebFetchToolAppliesTheConfiguredWebSettings(t *testing.T) {
	var agent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent = r.UserAgent()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html><html><body><h1>Headline</h1><p>Keep me.</p>`+
			strings.Repeat("<p>filler that the cap should cut away</p>", 200)+`</body></html>`)
	}))
	t.Cleanup(server.Close)

	tool := newMarkdownWebFetchTool(WebFetchConfig{
		Timeout:   10 * time.Second,
		UserAgent: "lightagent-test/1.0",
		MaxBytes:  512,
	})
	res := tool.Execute(context.Background(), map[string]any{"url": server.URL})
	if res.IsError {
		t.Fatalf("Execute reported an error: %s", res.ForLLM)
	}
	if agent != "lightagent-test/1.0" {
		t.Errorf("the server saw user agent %q, want the configured one", agent)
	}
	if !strings.Contains(res.ForUser, "truncated at 512 bytes") {
		t.Errorf("ForUser = %q, want the configured byte cap reported", res.ForUser)
	}
	if !strings.Contains(res.ForLLM, "Headline") {
		t.Errorf("the content before the cap is missing:\n%s", res.ForLLM)
	}
}

func TestWebFetchToolReportsFetchFailures(t *testing.T) {
	tool := newMarkdownWebFetchTool(WebFetchConfig{Timeout: 3 * time.Second})
	res := tool.Execute(context.Background(), map[string]any{"url": "127.0.0.1:1/unreachable"})
	if !res.IsError || !strings.Contains(res.ForLLM, "Fetch failed:") {
		t.Fatalf("Execute = %+v, want a fetch failure", res)
	}
}
