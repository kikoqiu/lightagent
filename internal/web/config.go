package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"lightagent/internal/config"
	"lightagent/internal/passwd"
)

// maxConfigBytes caps the document the editor may submit. The file describes
// the program, so anything this large is a mistake (a pasted log, most likely)
// rather than a configuration.
const maxConfigBytes = 1 << 20

// SetConfigPath wires the config file behind the /api/config endpoints. Without
// it those endpoints report that editing is unavailable, which is what tests
// and embedders that run without a config file want. It is expected to be
// called before Start, next to the address and password.
func (s *Server) SetConfigPath(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configPath = strings.TrimSpace(path)
}

// configFilePath returns the wired config file, or "" when there is none.
func (s *Server) configFilePath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.configPath
}

// setRestartPending records whether a saved document is waiting for a restart.
func (s *Server) setRestartPending(pending bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configPendingRestart = pending
}

// restartPending reports whether a document was saved through the editor that
// the running process has not picked up. Configuration is read once at startup,
// so this stays true until the process is restarted.
func (s *Server) restartPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.configPendingRestart
}

// handleConfig serves the config editor's API:
//
//	GET /api/config -> the stored document, decoded with the rules the next
//	                   start will apply and with the api key masked
//	PUT /api/config -> validate the submitted document and write it back
//
// A save deliberately leaves the running process alone: config.json is read
// once at startup, so the file takes effect on the next run. That is what makes
// the endpoint safe to call at any time, including while a turn is running, and
// it is why the reply always reports pending_restart.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleConfigGet(w)
	case http.MethodPut, http.MethodPost:
		s.handleConfigPut(w, r)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleConfigGet returns the config file as the page should show it: the
// document as stored, with defaults filled in for omitted fields, and with the
// api key masked (the CLI masks it the same way for --print-config). The file is
// read on every request, so an edit made elsewhere shows up on reload.
func (s *Server) handleConfigGet(w http.ResponseWriter) {
	path := s.configFilePath()
	if path == "" {
		writeJSONError(w, http.StatusNotFound, "config editing is not available in this process")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "read config: "+err.Error())
		return
	}
	cfg, err := config.Parse(data)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("parse %s: %v", path, err))
		return
	}
	writeJSON(w, http.StatusOK, configPayload(path, cfg, s.restartPending()))
}

// handleConfigPut validates a document and writes it to the config file. A
// rejected document leaves the file untouched, and a saved one never changes the
// running process, so the file always describes something the next start can
// actually load.
func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	path := s.configFilePath()
	if path == "" {
		writeJSONError(w, http.StatusNotFound, "config editing is not available in this process")
		return
	}
	body, err := readConfigBody(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Strict parsing: in an edited document an unknown field is a typo, and
	// dropping it silently would hide the mistake.
	cfg, err := config.ParseStrict(body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("parse config: %v", err))
		return
	}
	if err := keepStoredSecrets(path, cfg); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Apply the same check the next start performs (api key present, MCP servers
	// declared correctly, ...) so a saved file is always startable.
	if err := cfg.Validate(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := config.Save(path, cfg); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "write config: "+err.Error())
		return
	}
	// A password written here takes effect at once, unlike the rest of the
	// document: a login change that waited for a restart would be confusing, and
	// nothing else depends on the credential. A save that leaves it alone keeps
	// the running credential — and every session — as it is.
	if password, salt := s.auth.credential(); cfg.Web.Password != password || cfg.Web.PasswordSalt != salt {
		s.SetPassword(cfg.Web.Password, cfg.Web.PasswordSalt)
	}
	s.setRestartPending(true)
	if saved, err := readConfigDocument(path); err == nil {
		cfg = saved
	}
	writeJSON(w, http.StatusOK, configPayload(path, cfg, true))
}

// keepStoredSecrets carries the secrets stored in the file over when the client
// echoes a masked placeholder back (or leaves the field empty). The page only
// ever sees masks, so editing anything else must not require retyping them; a
// real value in the submitted document wins.
//
// A new web password always gets a fresh salt: it pairs the credential with the
// exact value it belongs to, and it tells a browser that a digest it stored
// belongs to an older password (the page drops it instead of replaying it).
func keepStoredSecrets(path string, cfg *config.Config) error {
	key := strings.TrimSpace(cfg.OpenAI.APIKey)
	password := strings.TrimSpace(cfg.Web.Password)
	keepKey := key == "" || key == config.MaskedSecret
	keepPassword := password == "" || password == config.MaskedSecret
	if !keepKey && !keepPassword {
		return nil
	}
	stored, err := readConfigDocument(path)
	if err != nil {
		return err
	}
	if keepKey {
		cfg.OpenAI.APIKey = stored.OpenAI.APIKey
	}
	if keepPassword {
		cfg.Web.Password = stored.Web.Password
		if strings.TrimSpace(cfg.Web.PasswordSalt) == "" {
			cfg.Web.PasswordSalt = stored.Web.PasswordSalt
		}
		return nil
	}
	salt, err := passwd.NewSalt()
	if err != nil {
		return err
	}
	cfg.Web.PasswordSalt = salt
	return nil
}

// readConfigBody reads the submitted document, refusing an oversized one.
func readConfigBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read request body: %v", err)
	}
	if len(body) > maxConfigBytes {
		return nil, fmt.Errorf("the config document is larger than %d bytes", maxConfigBytes)
	}
	return body, nil
}

// configPayload is the body of both config endpoints: the document, the file it
// belongs to, and whether a save is waiting for the next start.
func configPayload(path string, cfg *config.Config, pendingRestart bool) map[string]any {
	return map[string]any{
		"path":            path,
		"config":          cfg.MaskSecrets(),
		"pending_restart": pendingRestart,
	}
}

// writeJSON writes a JSON response body. The mirror's own responses are never
// cacheable: they describe live state.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeJSONError reports a problem as {"error": "..."} so the page can show the
// server's wording: validation messages are meant for the user.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}
