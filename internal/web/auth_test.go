package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"lightagent/internal/config"
	"lightagent/internal/passwd"
)

// loginReply is the /api/login response.
type loginReply struct {
	OK            bool   `json:"ok"`
	Authenticated bool   `json:"authenticated"`
	Remember      bool   `json:"remember"`
	Error         string `json:"error"`
}

// sessionReply is the /api/session response.
type sessionReply struct {
	AuthRequired  bool   `json:"auth_required"`
	Authenticated bool   `json:"authenticated"`
	Salt          string `json:"salt"`
	Error         string `json:"error"`
}

// getJSON performs a GET and decodes the reply into target (which may be nil).
func getJSON(t *testing.T, client *http.Client, url string, target any) int {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if target != nil && len(body) > 0 {
		if err := json.Unmarshal(body, target); err != nil {
			t.Fatalf("decode %s: %v (%s)", url, err, body)
		}
	}
	return resp.StatusCode
}

// postLogin posts a digest and returns the status, the reply and the cookies the
// server handed out.
func postLogin(t *testing.T, client *http.Client, url, body string) (int, loginReply, []*http.Cookie) {
	t.Helper()
	resp, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var reply loginReply
	if len(data) > 0 {
		if err := json.Unmarshal(data, &reply); err != nil {
			t.Fatalf("decode login reply: %v (%s)", err, data)
		}
	}
	return resp.StatusCode, reply, resp.Cookies()
}

// TestSessionReportsTheSalt checks the endpoint the page starts with: it says
// whether a login is needed and hands out the public salt a browser digests
// with.
func TestSessionReportsTheSalt(t *testing.T) {
	srv := newTestServer(t, "hunter2")

	var open sessionReply
	if status := getJSON(t, http.DefaultClient, baseURL(srv)+"/api/session", &open); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !open.AuthRequired || open.Authenticated {
		t.Fatalf("session = %+v, want auth required and not signed in", open)
	}
	if open.Salt != srv.auth.saltValue() || len(open.Salt) != 32 {
		t.Fatalf("salt = %q, want the mirror's public salt", open.Salt)
	}

	// A signed-in browser is reported as such.
	client := signIn(t, srv, "hunter2")
	var signed sessionReply
	if status := getJSON(t, client, baseURL(srv)+"/api/session", &signed); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !signed.Authenticated {
		t.Fatalf("session = %+v, want authenticated", signed)
	}

	// Without a password the mirror is open and hands out no salt.
	plain := newTestServer(t, "")
	var none sessionReply
	if status := getJSON(t, http.DefaultClient, baseURL(plain)+"/api/session", &none); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if none.AuthRequired || !none.Authenticated || none.Salt != "" {
		t.Fatalf("session = %+v, want an open mirror", none)
	}
}

// TestProtectedEndpointsNeedASession checks the endpoints that can see or steer
// the agent: they answer 401 until the browser has signed in.
func TestProtectedEndpointsNeedASession(t *testing.T) {
	srv := newTestServer(t, "hunter2")
	for _, path := range []string{"/api/config", "/api/password", "/api/logout"} {
		if status := getJSON(t, http.DefaultClient, baseURL(srv)+path, nil); status != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", path, status)
		}
	}
	// The WebSocket handshake is refused before it is upgraded.
	req, err := http.NewRequest(http.MethodGet, baseURL(srv)+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /ws: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/ws status = %d, want 401", resp.StatusCode)
	}
}

// TestLoginAndLogout walks the sign-in a browser performs: a wrong digest is
// rejected, the right one sets a session cookie, and signing out drops it.
func TestLoginAndLogout(t *testing.T) {
	srv := newTestServer(t, "hunter2")
	seedConfigFile(t, srv, `{"openai":{"api_key":"sk-secret"}}`)
	salt := srv.auth.saltValue()

	// A wrong digest: 401, no cookie.
	status, reply, cookies := postLogin(t, http.DefaultClient, baseURL(srv)+"/api/login",
		fmt.Sprintf(`{"digest":%q}`, passwd.Digest(salt, "wrong")))
	if status != http.StatusUnauthorized || len(cookies) != 0 {
		t.Fatalf("wrong digest: status = %d, cookies = %v, reply = %+v", status, cookies, reply)
	}

	// The digest the page computes signs the browser in.
	client := signIn(t, srv, "hunter2")
	if status := getJSON(t, client, baseURL(srv)+"/api/config", nil); status != http.StatusOK {
		t.Fatalf("/api/config status = %d, want 200 after signing in", status)
	}

	// Signing out drops the session.
	resp, err := client.Post(baseURL(srv)+"/api/logout", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/logout: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", resp.StatusCode)
	}
	if status := getJSON(t, client, baseURL(srv)+"/api/config", nil); status != http.StatusUnauthorized {
		t.Fatalf("/api/config status = %d, want 401 after signing out", status)
	}
}

// TestRememberMeSetsAPersistentCookie checks "stay signed in": the cookie carries
// Max-Age, which is what makes the browser keep it across a restart.
func TestRememberMeSetsAPersistentCookie(t *testing.T) {
	srv := newTestServer(t, "hunter2")
	url := baseURL(srv) + "/api/login"
	body := fmt.Sprintf(`{"digest":%q,"remember":true}`, passwd.Digest(srv.auth.saltValue(), "hunter2"))
	status, reply, cookies := postLogin(t, http.DefaultClient, url, body)
	if status != http.StatusOK || !reply.Remember {
		t.Fatalf("status = %d, reply = %+v", status, reply)
	}
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v, want one session cookie", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != sessionCookie || !cookie.HttpOnly {
		t.Fatalf("cookie = %+v, want an HttpOnly %s", cookie, sessionCookie)
	}
	if cookie.MaxAge <= 0 || int64(cookie.MaxAge) > int64(rememberTTL/time.Second) {
		t.Fatalf("MaxAge = %d, want a persistent cookie bounded by rememberTTL", cookie.MaxAge)
	}

	// Without "remember" it is a session cookie: no Max-Age, so the browser drops
	// it when it closes.
	plain := fmt.Sprintf(`{"digest":%q}`, passwd.Digest(srv.auth.saltValue(), "hunter2"))
	_, _, sessionCookies := postLogin(t, http.DefaultClient, url, plain)
	if len(sessionCookies) != 1 || sessionCookies[0].MaxAge != 0 {
		t.Fatalf("cookies = %+v, want a session cookie", sessionCookies)
	}
}

// TestLoginThrottle checks the lockout: repeated wrong digests are answered with
// 429 instead of being free to guess.
func TestLoginThrottle(t *testing.T) {
	srv := newTestServer(t, "hunter2")
	url := baseURL(srv) + "/api/login"
	bad := fmt.Sprintf(`{"digest":%q}`, passwd.Digest(srv.auth.saltValue(), "nope"))

	var last int
	for i := 0; i < loginFailures+2; i++ {
		last, _, _ = postLogin(t, http.DefaultClient, url, bad)
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once the allowance is spent", last)
	}

	// Even the right digest waits out the lockout: it is per address, not per
	// attempt.
	good := fmt.Sprintf(`{"digest":%q}`, passwd.Digest(srv.auth.saltValue(), "hunter2"))
	status, reply, _ := postLogin(t, http.DefaultClient, url, good)
	if status != http.StatusTooManyRequests || !strings.Contains(reply.Error, "too many attempts") {
		t.Fatalf("status = %d, reply = %+v", status, reply)
	}
}

// TestLoginRejectsNonsense checks a malformed request is refused without hashing
// anything.
func TestLoginRejectsNonsense(t *testing.T) {
	srv := newTestServer(t, "hunter2")
	url := baseURL(srv) + "/api/login"
	for _, body := range []string{
		`{}`,
		`{"digest":"short"}`,
		`{"digest":""}`,
		`{"digest":123}`,
		`not json`,
		``,
	} {
		status, reply, _ := postLogin(t, http.DefaultClient, url, body)
		if status != http.StatusBadRequest && status != http.StatusUnauthorized {
			t.Fatalf("body %q: status = %d (%s), want 400/401", body, status, reply.Error)
		}
	}
}

// TestCrossOriginWritesAreRefused checks the CSRF guard: a state-changing
// request that says it came from another site is rejected even with a session.
func TestCrossOriginWritesAreRefused(t *testing.T) {
	srv := newTestServer(t, "hunter2")
	client := signIn(t, srv, "hunter2")

	req, err := http.NewRequest(http.MethodPost, baseURL(srv)+"/api/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://evil.example")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a cross-origin write", resp.StatusCode)
	}

	// A same-origin request (what the page sends) goes through.
	req, err = http.NewRequest(http.MethodPost, baseURL(srv)+"/api/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", baseURL(srv))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a same-origin write", resp.StatusCode)
	}
}

// TestSetPasswordAppliesAtOnce checks the editor's password control: the file is
// rewritten, the new credential works immediately (no restart), other sessions
// are dropped, and clearing the password opens the mirror again while the file
// keeps an empty web.password (its salt is dropped).
func TestSetPasswordAppliesAtOnce(t *testing.T) {
	srv := newTestServer(t, "hunter2")
	path := seedConfigFile(t, srv, `{"openai":{"api_key":"sk-secret"},"web":{"host":"127.0.0.1","port":8791,"password":"hunter2","password_salt":"0123456789abcdef0123456789abcdef"}}`)
	client := signIn(t, srv, "hunter2")
	other := signIn(t, srv, "hunter2") // a second browser

	status, reply := callConfigAs(t, srv, client, http.MethodPost, "/api/password", `{"password":"s3cret"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, reply.Error)
	}
	if reply.Config.Web.Password != config.MaskedSecret {
		t.Fatalf("the reply leaks the password: %q", reply.Config.Web.Password)
	}

	saved, err := config.Parse([]byte(readConfigString(t, path)))
	if err != nil {
		t.Fatalf("parse the saved config: %v", err)
	}
	if saved.Web.Password != "s3cret" || len(saved.Web.PasswordSalt) != 32 {
		t.Fatalf("stored credential = %q / %q, want the new password and a fresh salt",
			saved.Web.Password, saved.Web.PasswordSalt)
	}

	// The new password works right away; the old one does not.
	if status := getJSON(t, signIn(t, srv, "s3cret"), baseURL(srv)+"/api/config", nil); status != http.StatusOK {
		t.Fatalf("status = %d, want 200 with the new password", status)
	}
	oldDigest := fmt.Sprintf(`{"digest":%q}`, passwd.Digest(srv.auth.saltValue(), "hunter2"))
	if status, _, _ := postLogin(t, http.DefaultClient, baseURL(srv)+"/api/login", oldDigest); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for the old password", status)
	}

	// The caller keeps a session; the other browser loses theirs.
	if status := getJSON(t, client, baseURL(srv)+"/api/config", nil); status != http.StatusOK {
		t.Fatalf("status = %d, want the caller to stay signed in", status)
	}
	if status := getJSON(t, other, baseURL(srv)+"/api/config", nil); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want the other browser signed out", status)
	}

	// Clearing the password turns the login off.
	if status, reply := callConfigAs(t, srv, client, http.MethodPost, "/api/password", `{"password":""}`); status != http.StatusOK {
		t.Fatalf("clear status = %d, want 200 (%s)", status, reply.Error)
	}
	if srv.auth.enabled() {
		t.Fatal("the mirror should stop asking for a login")
	}
	if status := getJSON(t, http.DefaultClient, baseURL(srv)+"/api/config", nil); status != http.StatusOK {
		t.Fatalf("status = %d, want an open mirror", status)
	}
	// The file is written back the way the config schema serializes it:
	// web.password is always present (empty here, which turns the login off),
	// while web.password_salt drops out because it is empty.
	cleared := readConfigString(t, path)
	clearedCfg, err := config.Parse([]byte(cleared))
	if err != nil {
		t.Fatalf("parse the cleared config: %v", err)
	}
	if clearedCfg.Web.Password != "" || clearedCfg.Web.PasswordSalt != "" {
		t.Fatalf("stored credential = %q / %q, want it cleared",
			clearedCfg.Web.Password, clearedCfg.Web.PasswordSalt)
	}
	if !strings.Contains(cleared, `"password": ""`) {
		t.Fatalf("the cleared password is not in the file:\n%s", cleared)
	}
	if strings.Contains(cleared, "password_salt") {
		t.Fatalf("the leftover salt is still in the file:\n%s", cleared)
	}
}

// TestConfigPutKeepsTheStoredPassword checks the editor round trip: the masked
// password in the document does not overwrite the stored one, and the login keeps
// working after a save that changes something else.
func TestConfigPutKeepsTheStoredPassword(t *testing.T) {
	srv := newTestServer(t, "hunter2")
	path := seedConfigFile(t, srv, `{"openai":{"api_key":"sk-secret"},"web":{"host":"127.0.0.1","port":8791,"password":"hunter2","password_salt":"0123456789abcdef0123456789abcdef"}}`)
	client := signIn(t, srv, "hunter2")

	status, reply := callConfigAs(t, srv, client, http.MethodGet, "/api/config", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, reply.Error)
	}
	if reply.Config.Web.Password != config.MaskedSecret {
		t.Fatalf("password = %q, want the mask", reply.Config.Web.Password)
	}

	// Echo the document back (as the page does) with another model.
	document := fmt.Sprintf(
		`{"openai":{"api_key":%q,"model":"m2"},"web":{"port":8791,"password":%q,"password_salt":%q}}`,
		config.MaskedSecret, config.MaskedSecret, reply.Config.Web.PasswordSalt)
	if status, reply := callConfigAs(t, srv, client, http.MethodPut, "/api/config", document); status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, reply.Error)
	}
	saved, err := config.Parse([]byte(readConfigString(t, path)))
	if err != nil {
		t.Fatalf("parse the saved config: %v", err)
	}
	if saved.Web.Password != "hunter2" || saved.Web.PasswordSalt != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("stored credential = %q / %q, want it untouched", saved.Web.Password, saved.Web.PasswordSalt)
	}
	if saved.OpenAI.Model != "m2" {
		t.Fatalf("model = %q, want the edit to have landed", saved.OpenAI.Model)
	}
	if status := getJSON(t, signIn(t, srv, "hunter2"), baseURL(srv)+"/api/config", nil); status != http.StatusOK {
		t.Fatalf("status = %d, want the password to keep working", status)
	}
}
