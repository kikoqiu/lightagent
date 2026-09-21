package utils

import (
	"strings"
	"testing"
)

// TestNormalizeAddress covers the address parsing an attach depends on: what a
// caller may pass and what has to be rejected.
func TestNormalizeAddress(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		address string
		wsURL   string
		wantErr bool
	}{
		{name: "host and port", in: "127.0.0.1:9222", address: "127.0.0.1:9222"},
		{name: "port only", in: "9222", address: "127.0.0.1:9222"},
		{name: "host only", in: "127.0.0.1", address: "127.0.0.1:9222"},
		{name: "host name only", in: "localhost", address: "localhost:9222"},
		{name: "http URL", in: "http://127.0.0.1:9333/json", address: "127.0.0.1:9333"},
		{name: "surrounding spaces", in: "  127.0.0.1:9222 ", address: "127.0.0.1:9222"},
		{
			name:    "browser socket",
			in:      "ws://127.0.0.1:9222/devtools/browser/abc",
			address: "127.0.0.1:9222",
			wsURL:   "ws://127.0.0.1:9222/devtools/browser/abc",
		},
		{name: "empty", in: "   ", wantErr: true},
		{name: "no host", in: "http://", wantErr: true},
	}
	for _, testCase := range cases {
		address, wsURL, err := normalizeAddress(testCase.in)
		if testCase.wantErr {
			if err == nil {
				t.Errorf("%s: normalizeAddress(%q) = nil error, want a failure", testCase.name, testCase.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: normalizeAddress(%q): %v", testCase.name, testCase.in, err)
			continue
		}
		if address != testCase.address || wsURL != testCase.wsURL {
			t.Errorf("%s: normalizeAddress(%q) = (%q, %q), want (%q, %q)",
				testCase.name, testCase.in, address, wsURL, testCase.address, testCase.wsURL)
		}
	}
}

// TestCallReportsProtocolAndConnectionErrors checks that a refused command and
// a dead connection both reach the caller as an error instead of a block.
func TestCallReportsProtocolAndConnectionErrors(t *testing.T) {
	requireBrowser(t)
	ctx := testContext(t)
	browser, err := LaunchBrowser(ctx, BrowserOptions{})
	if err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	t.Cleanup(func() { _ = browser.Close() })

	err = browser.call(ctx, "", "Target.attachToTarget",
		map[string]any{"targetId": "no-such-target", "flatten": true}, nil)
	if err == nil {
		t.Fatalf("a call for an unknown target = nil error, want the protocol error")
	}
	if !strings.Contains(err.Error(), "cdp error") {
		t.Errorf("error = %v, want the protocol error to be reported", err)
	}

	_ = browser.conn.close()
	err = browser.call(ctx, "", "Browser.getVersion", nil, nil)
	if err == nil {
		t.Fatalf("a call on a closed connection = nil error, want a failure")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("error = %v, want the closed connection to be named", err)
	}
}
