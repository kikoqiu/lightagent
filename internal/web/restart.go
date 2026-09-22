package web

import (
	"net/http"
)

// SetRestarter wires the callback behind the page's Restart button
// (POST /api/restart): the program registers a function that starts a replacement
// of itself and hands the run over to it. The mirror saves the conversation
// before calling it and the replacement resumes that session, so the page
// reconnects to a process that continues the same conversation with the
// config.json that was just written in effect.
//
// Without it the endpoint reports that restarting is unavailable and the page
// hides the control, which is what an embedder that runs the mirror inside its
// own process wants. It is expected to be called before Start, next to
// SetSessionSaver.
func (s *Server) SetRestarter(fn func() error) { s.restart = fn }

// canRestart reports whether the mirror can restart the program, so the page
// knows whether to offer the control at all.
func (s *Server) canRestart() bool { return s.restart != nil }

// ReleaseListener gives up the listening address without touching the live
// connections. A restart needs that before the replacement starts: the port has
// to be free for it, while the reply of the request that asked for the restart
// still travels on a connection that was already accepted — a Close, which drops
// every client, would cut that reply off. The mirror keeps serving the accepted
// connections until the process ends.
func (s *Server) ReleaseListener() error {
	if s.listener == nil {
		return nil
	}
	return s.listener.Close()
}

// handleRestart is the page's Restart button:
//
//	POST /api/restart -> save the conversation, then start a replacement process
//
// The order is the policy. The replacement resumes the session file, so the
// conversation has to be on disk first, and everything that can go wrong is
// answered before anything of this run is given up: a turn in progress is refused
// (its text is not in the history yet, and the terminal does not leave mid-turn
// either), a failed save is refused (a restart must not lose the conversation it
// was asked to keep), and a restart that cannot start the replacement is refused
// too — the run then continues as if nothing had been asked.
//
// The reply carries the path the session was written to. The page does not have
// to do anything with it: the replacement binds the same address, so the page
// reconnects and rebuilds its transcript from the resumed conversation (see
// seedHistory).
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.canRestart() {
		writeJSONError(w, http.StatusNotImplemented, "restarting is not available in this process")
		return
	}
	if s.agent.Busy() {
		writeJSONError(w, http.StatusConflict, "a turn is running; stop it (or wait) before restarting")
		return
	}
	saved := ""
	if s.save != nil {
		path, err := s.save()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "save the session: "+err.Error())
			return
		}
		saved = path
	}
	if err := s.restart(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "restart: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restarting": true, "saved": saved})
}
