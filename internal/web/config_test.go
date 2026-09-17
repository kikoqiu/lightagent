package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lightagent/internal/config"
)

// seedConfigFile points the mirror's config editor at a temporary config.json
// seeded with body and returns its path.
func seedConfigFile(t *testing.T, srv *Server, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	srv.SetConfigPath(path)
	return path
}

// configReply is one /api/config response as the page consumes it.
type configReply struct {
	Path           string        `json:"path"`
	Config         config.Config `json:"config"`
	PendingRestart bool          `json:"pending_restart"`
	Error          string        `json:"error"`
}

// callConfig performs one /api/config request with an unauthenticated client.
func callConfig(t *testing.T, srv *Server, method, body string) (int, configReply) {
	t.Helper()
	return callConfigAs(t, srv, http.DefaultClient, method, "/api/config", body)
}

// callConfigAs performs one API request with a specific client (which may carry
// a session) and decodes the JSON reply.
func callConfigAs(t *testing.T, srv *Server, client *http.Client, method, path, body string) (int, configReply) {
	t.Helper()
	var payload io.Reader
	if body != "" {
		payload = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, baseURL(srv)+path, payload)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	var reply configReply
	if len(data) > 0 {
		if err := json.Unmarshal(data, &reply); err != nil {
			t.Fatalf("%s %s returned %q: %v", method, path, data, err)
		}
	}
	return resp.StatusCode, reply
}

// readConfigString reads a file for assertions.
func readConfigString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestConfigGetMasksTheKeyAndFillsDefaults covers the read side of the editor:
// the document comes from the file, omitted fields show their default, and the
// api key never reaches the browser.
func TestConfigGetMasksTheKeyAndFillsDefaults(t *testing.T) {
	srv := newTestServer(t, "")
	path := seedConfigFile(t, srv, `{"openai":{"api_key":"sk-secret","model":"m1"}}`)

	status, reply := callConfig(t, srv, http.MethodGet, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, reply.Error)
	}
	if reply.Path != path {
		t.Fatalf("path = %q, want %q", reply.Path, path)
	}
	if reply.Config.OpenAI.Model != "m1" {
		t.Fatalf("model = %q, want the stored value", reply.Config.OpenAI.Model)
	}
	if reply.Config.OpenAI.APIKey != config.MaskedSecret {
		t.Fatalf("api_key = %q, want %q", reply.Config.OpenAI.APIKey, config.MaskedSecret)
	}
	if got, want := reply.Config.Context.ContextWindow, config.Default().Context.ContextWindow; got != want {
		t.Fatalf("context_window = %d, want the default %d", got, want)
	}
	if reply.PendingRestart {
		t.Fatal("nothing was saved, so pending_restart must be false")
	}
}

// TestConfigPutWritesTheFileWithoutApplyingIt covers the write side: the edit
// lands in config.json, the echoed mask keeps the stored key, and the save is
// reported as waiting for a restart (the running agent keeps the settings it
// started with).
func TestConfigPutWritesTheFileWithoutApplyingIt(t *testing.T) {
	srv := newTestServer(t, "")
	seed := `{"openai":{"api_key":"sk-secret","model":"m1"}}`
	path := seedConfigFile(t, srv, seed)

	// Edit the way the page does: a new model, with the masked key sent back
	// untouched.
	body := fmt.Sprintf(`{"openai":{"api_key":%q,"model":"m2"}}`, config.MaskedSecret)
	status, reply := callConfig(t, srv, http.MethodPut, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, reply.Error)
	}
	if !reply.PendingRestart {
		t.Fatal("a saved document must be reported as pending a restart")
	}
	if reply.Config.OpenAI.APIKey != config.MaskedSecret {
		t.Fatalf("the reply leaks the api key: %q", reply.Config.OpenAI.APIKey)
	}
	if reply.Config.OpenAI.Model != "m2" {
		t.Fatalf("reply model = %q, want the saved value", reply.Config.OpenAI.Model)
	}

	saved, err := config.Parse([]byte(readConfigString(t, path)))
	if err != nil {
		t.Fatalf("parse the saved config: %v", err)
	}
	if saved.OpenAI.Model != "m2" {
		t.Fatalf("file model = %q, want m2", saved.OpenAI.Model)
	}
	if saved.OpenAI.APIKey != "sk-secret" {
		t.Fatalf("file api_key = %q, want the stored key (the mask is never written)", saved.OpenAI.APIKey)
	}
	if readConfigString(t, path) == seed {
		t.Fatal("the file was not rewritten")
	}

	// A later read still reports the pending restart, so a browser that reloads
	// the panel keeps warning about it.
	if _, again := callConfig(t, srv, http.MethodGet, ""); !again.PendingRestart {
		t.Fatal("pending_restart must survive a later read")
	}
}

// TestConfigPutRejectsBadDocuments pins that a document the next start could not
// use is refused: the file must stay startable, and a rejection must not touch
// it.
func TestConfigPutRejectsBadDocuments(t *testing.T) {
	withKey := `{"openai":{"api_key":"sk-secret","model":"m1"}}`
	noKey := `{"openai":{"model":"m1"}}`
	cases := []struct {
		name string
		seed string
		body string
		want string
	}{
		{"unknown field", withKey, fmt.Sprintf(`{"openai":{"api_key":%q,"modle":"typo"}}`, config.MaskedSecret), "unknown field"},
		{"malformed json", withKey, `{"openai":`, "parse config"},
		{"empty document", withKey, "  \n ", "empty"},
		{"two documents", withKey, withKey + withKey, "single JSON object"},
		{"missing api key", noKey, noKey, "api_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, "")
			path := seedConfigFile(t, srv, tc.seed)
			status, reply := callConfig(t, srv, http.MethodPut, tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (error %q)", status, reply.Error)
			}
			if !strings.Contains(reply.Error, tc.want) {
				t.Fatalf("error = %q, want it to mention %q", reply.Error, tc.want)
			}
			if got := readConfigString(t, path); got != tc.seed {
				t.Fatalf("a rejected document changed the file:\n%s", got)
			}
		})
	}
}

// TestConfigPutAcceptsARealKey checks a pasted key replaces the stored one
// instead of being treated as an echo of the mask.
func TestConfigPutAcceptsARealKey(t *testing.T) {
	srv := newTestServer(t, "")
	path := seedConfigFile(t, srv, `{"openai":{"api_key":"sk-old"}}`)

	status, reply := callConfig(t, srv, http.MethodPut, `{"openai":{"api_key":"sk-new"}}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", status, reply.Error)
	}
	saved, err := config.Parse([]byte(readConfigString(t, path)))
	if err != nil {
		t.Fatalf("parse the saved config: %v", err)
	}
	if saved.OpenAI.APIKey != "sk-new" {
		t.Fatalf("file api_key = %q, want the submitted key", saved.OpenAI.APIKey)
	}
}

// TestConfigEndpointsWithoutAConfigFile checks a mirror started without a config
// file reports that editing is unavailable instead of failing on a read, and
// that only GET/PUT are accepted.
func TestConfigEndpointsWithoutAConfigFile(t *testing.T) {
	srv := newTestServer(t, "") // no SetConfigPath
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		status, reply := callConfig(t, srv, method, `{"openai":{"api_key":"sk-x"}}`)
		if status != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404 (%s)", method, status, reply.Error)
		}
	}
}

func TestConfigRejectsOtherMethods(t *testing.T) {
	srv := newTestServer(t, "")
	seedConfigFile(t, srv, `{"openai":{"api_key":"sk-x"}}`)
	status, _ := callConfig(t, srv, http.MethodDelete, "")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", status)
	}
}

// TestConfigEndpointsRequireAuth pins that the editor sits behind the session
// cookie: without a login nobody can rewrite the config file, and signing in
// unlocks it.
func TestConfigEndpointsRequireAuth(t *testing.T) {
	srv := newTestServer(t, "secret")
	seedConfigFile(t, srv, `{"openai":{"api_key":"sk-secret"}}`)

	status, reply := callConfig(t, srv, http.MethodGet, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a session (%s)", status, reply.Error)
	}

	client := signIn(t, srv, "secret")
	if status, reply := callConfigAs(t, srv, client, http.MethodGet, "/api/config", ""); status != http.StatusOK {
		t.Fatalf("status = %d, want 200 after signing in (%s)", status, reply.Error)
	}
}

// TestConfigEditorWiring keeps the page side of the editor wired: both triggers,
// the two modes, the control widgets and the endpoint they talk to.
func TestConfigEditorWiring(t *testing.T) {
	for _, want := range []string{
		"data-config-open",      // header gear and side-rail card
		`id="configModal"`,      // the panel
		`id="configForm"`,       // form mode
		`id="configText"`,       // raw JSON mode
		`id="modeForm"`,         // mode tabs
		`id="modeJSON"`,         //
		"config.js",             // the editor script
		"/api/config",           // the endpoint it talks to
		"restart lightagent",    // the notice the panel always carries
		".modal {",              // overlay styles
		"input.switch",          // switches
		"input[type=range]",     // sliders
		"grid-template-columns", // label/control rows
	} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	// Every option of the config document must be reachable from the form: the
	// list also documents what the form covers, so a new config field is noticed.
	for _, path := range []string{
		"openai.api_base", "openai.api_key", "openai.model", "openai.stream",
		"openai.temperature", "openai.max_tokens", "openai.timeout_seconds", "openai.extra_body",
		"context.context_window", "context.summarize_token_percent",
		"web.host", "web.port", "web.password",
		"tools.exec.enabled", "tools.exec.timeout_seconds", "tools.exec.wait_seconds", "tools.exec.use_utf8",
		"tools.read_file_lines.enabled", "tools.read_file_lines.max_read_file_size", "tools.read_file_lines.max_read_file_lines",
		"tools.write_file.enabled", "tools.write_file.max_lines", "tools.write_file.auto_split", "tools.edit_file.enabled",
		"tools.discovery.enabled", "tools.discovery.mode", "tools.discovery.ttl",
		"tools.discovery.max_search_results", "tools.discovery.min_match_rate", "tools.discovery.use_bm25",
		"tools.mcp.enabled", "tools.mcp.servers",
		"agent.max_tool_iterations", "agent.system_prompt", "agent.include_working_dir",
		"ui.markdown",
	} {
		if !strings.Contains(configJS, "'"+path+"'") {
			t.Errorf("the form does not cover the %s option", path)
		}
	}
	if strings.Contains(pageSource(), "position:fixed") {
		t.Error("the config panel must be absolutely positioned, not fixed: a fixed overlay breaks the mobile viewport")
	}
}

// TestConfigScriptIsServed checks the page scripts are embedded and served like
// the rest of the UI (they carry no data, so they are public: the sign-in dialog
// has to load before there is a session).
func TestConfigScriptIsServed(t *testing.T) {
	srv := newTestServer(t, "secret")
	for _, script := range []string{"/config.js", "/auth.js", "/app.js", "/tts.js"} {
		resp, err := http.Get(baseURL(srv) + script)
		if err != nil {
			t.Fatalf("GET %s: %v", script, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", script, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200 (the UI is public)", script, resp.StatusCode)
		}
		if len(body) == 0 {
			t.Fatalf("%s served an empty body", script)
		}
		if script == "/config.js" && !strings.Contains(string(body), "/api/config") {
			t.Fatalf("config.js does not talk to /api/config: %s", body)
		}
		if script == "/auth.js" && !strings.Contains(string(body), "/api/login") {
			t.Fatalf("auth.js does not talk to /api/login: %s", body)
		}
	}
}
