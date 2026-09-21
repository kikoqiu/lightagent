package utils

import (
	"net/http"
	"testing"
)

// TestCookieMatchingRules covers the RFC 6265 matching an export relies on: a
// cookie may only reach the requests it belongs to.
func TestCookieMatchingRules(t *testing.T) {
	domains := []struct {
		cookieDomain string
		host         string
		want         bool
	}{
		{"example.com", "example.com", true},
		{".example.com", "example.com", true},
		{".example.com", "www.example.com", true},
		{"example.com", "www.example.com", true},
		{"EXAMPLE.com", "www.example.com", true},
		{"example.com", "notexample.com", false},
		{"example.com", "example.com.evil.test", false},
		{"www.example.com", "example.com", false},
		{"", "anything.test", true},
	}
	for _, testCase := range domains {
		if got := cookieDomainMatches(testCase.cookieDomain, testCase.host); got != testCase.want {
			t.Errorf("cookieDomainMatches(%q, %q) = %v, want %v",
				testCase.cookieDomain, testCase.host, got, testCase.want)
		}
	}

	paths := []struct {
		cookiePath string
		request    string
		want       bool
	}{
		{"/", "/", true},
		{"/", "/deep/page", true},
		{"/deep", "/deep", true},
		{"/deep", "/deep/page", true},
		{"/deep/", "/deep/page", true},
		{"/deep", "/deeper/page", false},
		{"/deep", "/other", false},
		{"/deep/", "/deep", false},
	}
	for _, testCase := range paths {
		if got := cookiePathMatches(testCase.cookiePath, testCase.request); got != testCase.want {
			t.Errorf("cookiePathMatches(%q, %q) = %v, want %v",
				testCase.cookiePath, testCase.request, got, testCase.want)
		}
	}

	if got := cookiePath(""); got != "/" {
		t.Errorf("cookiePath(\"\") = %q, want %q", got, "/")
	}
	if got := cookiePath("relative"); got != "/" {
		t.Errorf("cookiePath(%q) = %q, want %q", "relative", got, "/")
	}
	if got := cookiePath("/api"); got != "/api" {
		t.Errorf("cookiePath(%q) = %q, want %q", "/api", got, "/api")
	}
}

// TestSameSiteMapping checks the translation in both directions, which is what
// keeps a cookie's policy intact while it travels between the browser and an
// HTTP client.
func TestSameSiteMapping(t *testing.T) {
	modes := []struct {
		mode   http.SameSite
		value  string
		usable bool
	}{
		{http.SameSiteDefaultMode, "", false},
		{http.SameSiteLaxMode, "Lax", true},
		{http.SameSiteStrictMode, "Strict", true},
		{http.SameSiteNoneMode, "None", true},
	}
	for _, testCase := range modes {
		value := protocolSameSite(testCase.mode)
		if value != testCase.value {
			t.Errorf("protocolSameSite(%v) = %q, want %q", testCase.mode, value, testCase.value)
		}
		if !testCase.usable {
			continue
		}
		if back := sameSiteFromProtocol(value); back != testCase.mode {
			t.Errorf("sameSiteFromProtocol(%q) = %v, want %v", value, back, testCase.mode)
		}
	}
	if got := sameSiteFromProtocol("lax"); got != http.SameSiteLaxMode {
		t.Errorf("sameSiteFromProtocol(%q) = %v, want %v", "lax", got, http.SameSiteLaxMode)
	}
	if got := sameSiteFromProtocol("unknown"); got != http.SameSiteDefaultMode {
		t.Errorf("sameSiteFromProtocol(%q) = %v, want the default", "unknown", got)
	}
}
