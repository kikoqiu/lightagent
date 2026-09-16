package web

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"lightagent/internal/config"
	"lightagent/internal/passwd"
)

// The mirror signs browsers in with a session cookie and a salted digest of the
// password:
//
//   - the page asks /api/session for the public salt, digests the password it
//     typed (salted SHA-256, in JavaScript) and posts only the digest, so the
//     plaintext never travels;
//   - the server derives the same digest from the stored password and compares;
//   - on success it hands out an HttpOnly session cookie, which every protected
//     endpoint (the WebSocket handshake included) then rides on.
//
// "Remember me" does two things: the cookie becomes persistent (Max-Age) so the
// browser keeps the session across restarts, and the page stores the digest it
// already computed, so a browser whose cookie is gone can sign in again without
// asking. Rotating the salt (a password change) invalidates those stored
// digests.
const (
	// sessionCookie is the name of the mirror's session cookie.
	sessionCookie = "lightagent_session"
	// sessionTTL is how long a session stays valid. Without "remember me" the
	// cookie is a session cookie, so closing the browser ends it sooner.
	sessionTTL = 12 * time.Hour
	// rememberTTL is the lifetime of a session whose browser asked to stay
	// signed in; the cookie carries Max-Age so it survives a restart.
	rememberTTL = 30 * 24 * time.Hour
	// loginFailures is how many wrong digests one address may send before it is
	// locked out for a growing delay (5s, 10s, 20s …
	loginFailures = 5
	lockoutBase   = 5 * time.Second
	lockoutMax    = 5 * time.Minute
	// maxLoginBytes caps a login or password request body.
	maxLoginBytes = 4 << 10
)

// session is one signed-in browser.
type session struct {
	expires  time.Time
	remember bool
}

// lockout counts the failed logins of one address.
type lockout struct {
	count int
	until time.Time
}

// auth holds the credential and the live sessions. It has its own mutex so a
// login never waits behind the event stream.
type auth struct {
	mu       sync.Mutex
	password string
	salt     string
	sessions map[string]session
	failures map[string]*lockout
}

func newAuth() *auth {
	return &auth{
		sessions: make(map[string]session),
		failures: make(map[string]*lockout),
	}
}

// enabled reports whether a login is required.
func (a *auth) enabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.password != "" && a.salt != ""
}

// saltValue returns the public salt handed to browsers; empty when no password
// is configured.
func (a *auth) saltValue() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.password == "" {
		return ""
	}
	return a.salt
}

// credential returns the stored password and salt (for the config editor, which
// needs to know whether the login is on).
func (a *auth) credential() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.password, a.salt
}

// set installs a new credential and drops every session: a password change must
// sign other browsers out.
func (a *auth) set(password, salt string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.password, a.salt = password, salt
	a.sessions = make(map[string]session)
}

// accepts reports whether digest is the salted digest of the stored password.
func (a *auth) accepts(digest string) bool {
	a.mu.Lock()
	password, salt := a.password, a.salt
	a.mu.Unlock()
	return passwd.Matches(salt, password, digest)
}

// open starts a session and returns its token.
func (a *auth) open(remember bool) (string, time.Time, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, fmt.Errorf("web: read session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	ttl := sessionTTL
	if remember {
		ttl = rememberTTL
	}
	expires := time.Now().Add(ttl)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked(time.Now())
	a.sessions[token] = session{expires: expires, remember: remember}
	return token, expires, nil
}

// valid looks a session up, dropping it when it has expired.
func (a *auth) valid(token string) (session, bool) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.sessions[token]
	if !ok {
		return session{}, false
	}
	if now.After(entry.expires) {
		delete(a.sessions, token)
		return session{}, false
	}
	return entry, true
}

// close ends one session (sign out).
func (a *auth) close(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

// blocked reports how long an address must wait before trying again.
func (a *auth) blocked(key string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.failures[key]
	if !ok {
		return 0
	}
	if wait := time.Until(entry.until); wait > 0 {
		return wait
	}
	return 0
}

// fail records a wrong digest and arms the lockout once the allowance is spent.
func (a *auth) fail(key string) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.failures) > 256 {
		a.pruneLocked(now)
	}
	entry, ok := a.failures[key]
	if !ok {
		entry = &lockout{}
		a.failures[key] = entry
	}
	entry.count++
	if shift := entry.count - loginFailures; shift > 0 {
		if shift > 8 {
			shift = 8
		}
		delay := lockoutBase << uint(shift)
		if delay > lockoutMax {
			delay = lockoutMax
		}
		entry.until = now.Add(delay)
	}
}

// succeed forgets the failures of an address that signed in.
func (a *auth) succeed(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.failures, key)
}

// pruneLocked drops expired sessions and lockouts. The caller must hold a.mu.
func (a *auth) pruneLocked(now time.Time) {
	for token, entry := range a.sessions {
		if now.After(entry.expires) {
			delete(a.sessions, token)
		}
	}
	for key, entry := range a.failures {
		if !entry.until.After(now) && entry.count < loginFailures {
			delete(a.failures, key)
		}
	}
}

// SetPassword installs the login credential: the plaintext the server keeps and
// the public salt a browser digests it with. Both empty means the mirror runs
// without a login, which is only sane on loopback. It is called at startup and
// again by the editor's password control, which applies immediately.
func (s *Server) SetPassword(password, salt string) {
	s.auth.set(strings.TrimSpace(password), strings.TrimSpace(salt))
}

// currentSession returns the session of the calling browser, if any.
func (s *Server) currentSession(r *http.Request) (session, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return session{}, false
	}
	return s.auth.valid(cookie.Value)
}

// requireSession wraps everything that can see or steer the agent: without a
// session, anyone who can reach the port could read the conversation and run
// tools. When no password is configured the mirror stays open (loopback
// default).
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.enabled() {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := s.currentSession(r); !ok {
			writeJSONError(w, http.StatusUnauthorized, "sign in to continue")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin guards state-changing requests against cross-site posts. The
// session cookie is SameSite=Lax already; this additionally rejects a request
// whose Origin (or Fetch metadata) says it came from elsewhere, while leaving
// clients that send neither header (curl, tests) alone.
func sameOrigin(r *http.Request) bool {
	if site := strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

// clientKey identifies the caller for login throttling.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// handleSession tells the page whether a login is needed, whether the caller
// already has one, and the public salt used to digest the password. It answers
// without a session because the page needs it before it can ask for anything
// else.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	required := s.auth.enabled()
	_, signed := s.currentSession(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_required": required,
		"authenticated": !required || signed,
		"salt":          s.auth.saltValue(),
	})
}

// handleLogin verifies the salted digest a browser computed and starts a
// session. The body is {"digest": "...", "remember": bool}; the password itself
// never travels.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "cross-origin sign-in is not allowed")
		return
	}
	var req struct {
		Digest   string `json:"digest"`
		Remember bool   `json:"remember"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.auth.enabled() {
		// No password configured: nothing to verify, the mirror is open.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "authenticated": true})
		return
	}
	key := clientKey(r)
	if wait := s.auth.blocked(key); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSONError(w, http.StatusTooManyRequests,
			fmt.Sprintf("too many attempts; try again in %s", wait.Round(time.Second)))
		return
	}
	digest := strings.TrimSpace(req.Digest)
	if !passwd.LooksLikeDigest(digest) || !s.auth.accepts(digest) {
		s.auth.fail(key)
		writeJSONError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	s.auth.succeed(key)
	if err := s.startSession(w, req.Remember); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"authenticated": true,
		"remember":      req.Remember,
	})
}

// handleLogout ends the session of the calling browser.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "cross-origin sign-out is not allowed")
		return
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		s.auth.close(cookie.Value)
	}
	expireSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "authenticated": false})
}

// handlePassword sets or clears the login password. It is the write path behind
// the editor's password control and, unlike a config.json edit, it applies at
// once: the file is written first, so a file that cannot be written leaves the
// running credential untouched.
//
// The plaintext is sent to the server, which has to keep it (the browser's
// digest is derived from it on every sign-in). Over plain HTTP on an untrusted
// network, prefer editing config.json directly.
func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "cross-origin password change is not allowed")
		return
	}
	path := s.configFilePath()
	if path == "" {
		writeJSONError(w, http.StatusNotFound, "config editing is not available in this process")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	password := req.Password
	salt := ""
	if password != "" {
		generated, err := passwd.NewSalt()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		salt = generated
	}
	if err := storeWebCredential(path, password, salt); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	previous, signedIn := s.currentSession(r)
	s.SetPassword(password, salt) // drops every session, including this one
	if password != "" {
		// Keep the caller signed in with their previous lifetime preference; the
		// other browsers now have to sign in again.
		if err := s.startSession(w, signedIn && previous.remember); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	cfg, err := readConfigDocument(path)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	payload := configPayload(path, cfg, true)
	payload["salt"] = salt
	payload["message"] = "password updated; it is active now"
	writeJSON(w, http.StatusOK, payload)
}

// storeWebCredential rewrites config.json with a new web credential, keeping the
// rest of the document as it is on disk. An empty password (and salt) turns the
// login off.
func storeWebCredential(path, password, salt string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.Web.Password, cfg.Web.PasswordSalt = password, salt
	if err := config.Save(path, cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// readConfigDocument reads the config file for a response body.
func readConfigDocument(path string) (*config.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// startSession issues the session cookie. "Remember me" adds Max-Age, which is
// what makes the browser keep it after a restart; without it the cookie lives
// until the browser closes.
func (s *Server) startSession(w http.ResponseWriter, remember bool) error {
	token, expires, err := s.auth.open(remember)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	cookie := &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	if remember {
		cookie.MaxAge = int(time.Until(expires).Seconds())
	}
	http.SetCookie(w, cookie)
	return nil
}

// expireSessionCookie clears the session cookie in the browser.
func expireSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// decodeBody reads a small JSON request body, reporting a readable problem.
func decodeBody(r *http.Request, target any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLoginBytes))
	if err != nil {
		return fmt.Errorf("read request body: %v", err)
	}
	if len(body) == 0 {
		return fmt.Errorf("the request body is empty")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("invalid request body: %v", err)
	}
	return nil
}
