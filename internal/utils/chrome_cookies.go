package utils

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Cookie is a browser cookie in the shape the DevTools protocol uses. URL is
// the alternative to Domain/Path when a cookie is set for one address; it is
// filled in when cookies travel between an http.CookieJar and the browser.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain,omitempty"`
	Path     string  `json:"path,omitempty"`
	URL      string  `json:"url,omitempty"`
	Expires  float64 `json:"expires,omitempty"`
	HTTPOnly bool    `json:"httpOnly,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	SameSite string  `json:"sameSite,omitempty"`
	Session  bool    `json:"session,omitempty"`
}

// Cookies returns every cookie of the browser profile, which is what makes the
// state of a real browser (a logged-in session, for instance) available to the
// caller.
func (b *Browser) Cookies(ctx context.Context) ([]Cookie, error) {
	var result struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err := b.call(ctx, "", "Storage.getCookies", nil, &result); err != nil {
		return nil, err
	}
	return result.Cookies, nil
}

// SetCookies adds or replaces cookies in the browser profile. A cookie needs
// either URL or Domain (and usually Path) to be placed.
func (b *Browser) SetCookies(ctx context.Context, cookies ...Cookie) error {
	if len(cookies) == 0 {
		return nil
	}
	return b.call(ctx, "", "Storage.setCookies", map[string]any{"cookies": cookies}, nil)
}

// ImportCookies copies the cookies an HTTP client holds for the URLs into the
// browser, so a page the browser loads is as authenticated as the HTTP requests
// were. It is the other half of sharing a session with a request that went over
// plain HTTP (see WebFetch, which does this with the jar it is given).
func (b *Browser) ImportCookies(ctx context.Context, jar http.CookieJar, urls ...string) error {
	if jar == nil {
		return errors.New("nil cookie jar")
	}
	var importable []Cookie
	for _, raw := range urls {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			continue
		}
		for _, cookie := range jar.Cookies(parsed) {
			importable = append(importable, Cookie{
				Name:     cookie.Name,
				Value:    cookie.Value,
				URL:      parsed.String(),
				Secure:   cookie.Secure,
				HTTPOnly: cookie.HttpOnly,
				SameSite: protocolSameSite(cookie.SameSite),
			})
		}
	}
	if len(importable) == 0 {
		return nil
	}
	return b.SetCookies(ctx, importable...)
}

// ExportCookies copies the browser cookies into an http.CookieJar, so a later
// HTTP request carries the session the browser built. Without explicit URLs the
// cookies themselves decide which origins are exported.
func (b *Browser) ExportCookies(ctx context.Context, jar http.CookieJar, urls ...string) error {
	if jar == nil {
		return errors.New("nil cookie jar")
	}
	cookies, err := b.Cookies(ctx)
	if err != nil {
		return err
	}
	if len(cookies) == 0 {
		return nil
	}
	if len(urls) == 0 {
		urls = cookieOriginURLs(cookies)
	}
	for _, raw := range urls {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			continue
		}
		host := parsed.Hostname()
		requestPath := cookiePath(parsed.EscapedPath())
		var applicable []*http.Cookie
		for _, cookie := range cookies {
			if !cookieDomainMatches(cookie.Domain, host) {
				continue
			}
			if !cookiePathMatches(cookiePath(cookie.Path), requestPath) {
				continue
			}
			if cookie.Secure && parsed.Scheme != "https" {
				continue
			}
			applicable = append(applicable, cookie.httpCookie())
		}
		if len(applicable) == 0 {
			continue
		}
		jar.SetCookies(parsed, applicable)
	}
	return nil
}

// httpCookie converts a browser cookie into the HTTP representation.
func (c Cookie) httpCookie() *http.Cookie {
	cookie := &http.Cookie{
		Name:     c.Name,
		Value:    c.Value,
		Domain:   strings.TrimPrefix(c.Domain, "."),
		Path:     cookiePath(c.Path),
		Secure:   c.Secure,
		HttpOnly: c.HTTPOnly,
		SameSite: sameSiteFromProtocol(c.SameSite),
	}
	if c.Expires > 0 {
		cookie.Expires = time.Unix(int64(c.Expires), 0)
	}
	return cookie
}

// cookieOriginURLs builds the origins an export should cover when the caller
// named none.
func cookieOriginURLs(cookies []Cookie) []string {
	seen := make(map[string]struct{}, len(cookies))
	var urls []string
	for _, cookie := range cookies {
		if cookie.URL != "" {
			if _, ok := seen[cookie.URL]; !ok {
				seen[cookie.URL] = struct{}{}
				urls = append(urls, cookie.URL)
			}
			continue
		}
		host := strings.TrimPrefix(cookie.Domain, ".")
		if host == "" {
			continue
		}
		scheme := "http"
		if cookie.Secure {
			scheme = "https"
		}
		origin := scheme + "://" + host + "/"
		if _, ok := seen[origin]; ok {
			continue
		}
		seen[origin] = struct{}{}
		urls = append(urls, origin)
	}
	return urls
}

// cookieDomainMatches reports whether a cookie for cookieDomain applies to host
// (the RFC 6265 domain match).
func cookieDomainMatches(cookieDomain, host string) bool {
	domain := strings.TrimPrefix(strings.ToLower(cookieDomain), ".")
	host = strings.ToLower(host)
	if domain == "" {
		return true
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// cookiePathMatches reports whether a cookie for cookiePath applies to a
// request path (the RFC 6265 path match).
func cookiePathMatches(cookiePath, requestPath string) bool {
	if cookiePath == requestPath {
		return true
	}
	if !strings.HasPrefix(requestPath, cookiePath) {
		return false
	}
	if strings.HasSuffix(cookiePath, "/") {
		return true
	}
	if len(requestPath) > len(cookiePath) {
		return requestPath[len(cookiePath)] == '/'
	}
	return false
}

// cookiePath is the effective path of a cookie: RFC 6265 defaults to "/".
func cookiePath(path string) string {
	if path == "" || !strings.HasPrefix(path, "/") {
		return "/"
	}
	return path
}

// protocolSameSite maps an HTTP same-site policy onto the protocol value.
func protocolSameSite(mode http.SameSite) string {
	switch mode {
	case http.SameSiteLaxMode:
		return "Lax"
	case http.SameSiteStrictMode:
		return "Strict"
	case http.SameSiteNoneMode:
		return "None"
	}
	return ""
}

// sameSiteFromProtocol maps a protocol same-site value onto the HTTP policy.
func sameSiteFromProtocol(value string) http.SameSite {
	switch strings.ToLower(value) {
	case "lax":
		return http.SameSiteLaxMode
	case "strict":
		return http.SameSiteStrictMode
	case "none":
		return http.SameSiteNoneMode
	}
	return http.SameSiteDefaultMode
}
