package web

import (
	"net/http"
	"os"

	"lightagent/internal/workdir"
)

// completeResponse is the body of GET /api/complete: the head the page keeps in
// front of the picked name (the typed directory part, exactly as the user
// spelled it), the separator to append when the picked entry is a directory, and
// the matching children of that directory.
type completeResponse struct {
	Head  string              `json:"head"`
	Sep   string              `json:"sep"`
	Items []workdir.Candidate `json:"items"`
}

// handleComplete serves the composer's "@" picker: the fragment typed after the
// '@' (the query's path) is resolved one level deep — relative to the working
// directory, an absolute or ~-headed fragment under itself — and the matching
// children come back. Only one level is ever listed: the page inserts what it
// picked (a directory with the separator, so the next fragment is already the
// next level). An unreadable directory is an empty list rather than an error,
// because a half-typed name is the normal case here.
func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	head, candidates := workdir.Complete(r.URL.Query().Get("path"))
	if candidates == nil {
		candidates = []workdir.Candidate{}
	}
	writeJSON(w, http.StatusOK, completeResponse{
		Head:  head,
		Sep:   string(os.PathSeparator),
		Items: candidates,
	})
}
