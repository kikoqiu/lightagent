package web

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"lightagent/internal/tools"
)

// UploadsDirName is the directory below the state directory (.lightagent) the
// browser's attachments are stored in.
const UploadsDirName = "uploads"

// uploadFileField is the multipart field the page posts its file in.
const uploadFileField = "file"

const (
	// maxUploadNameBytes bounds the original file name kept in an upload's
	// stored name: the name is only a label (it is what the model sees as the
	// attachment's name), so a very long one has no use.
	maxUploadNameBytes = 96
	// uploadIDAttempts bounds the search for a free stored name when several
	// uploads of the same file name pile up in one session.
	uploadIDAttempts = 100
	// multipartOverheadBytes is the slack the request body limit leaves for the
	// multipart envelope (boundaries and field headers) around a file of the
	// configured size.
	multipartOverheadBytes = 64 << 10
)

// SetMedia wires the multimedia capability behind the composer's attach control:
// the media types the model accepts and the directory an uploaded file is stored
// in. Without it (or with an empty type list) the page offers no attachment and
// /api/upload refuses everything. It is expected to be called before Start.
func (s *Server) SetMedia(cfg tools.MediaConfig, uploadsDir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.media = tools.NewMediaConfig(cfg.Types, cfg.MaxBytes)
	s.uploadsDir = strings.TrimSpace(uploadsDir)
}

// mediaLocked returns the capability and its upload directory. The caller must
// hold s.mu.
func (s *Server) mediaLocked() (tools.MediaConfig, string) {
	return s.media, s.uploadsDir
}

// uploadPayload is one stored upload as the page sees it: the id it sends back
// with the message, plus what the attachment chip displays.
type uploadPayload struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// handleUpload serves the attach control's API:
//
//	POST   /api/upload        multipart/form-data with one "file" part
//	DELETE /api/upload?id=... drop a stored upload the user cancelled
//
// An upload is stored as it arrives and only travels to the model once the page
// sends its id with a message (see handleClientMessage), so attaching a file
// costs nothing until it is used and can be cancelled in between.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	media, dir := s.mediaLocked()
	s.mu.Unlock()
	if !media.Enabled() || dir == "" {
		writeJSONError(w, http.StatusNotFound, "attachments are not enabled: set openai.media_types and restart")
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.storeUpload(w, r, media, dir)
	case http.MethodDelete:
		s.dropUpload(w, r, dir)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// storeUpload stores one uploaded file and answers with its descriptor. A file
// of a type the model does not accept is refused with the accepted list, exactly
// like the model's own upload tool refuses one.
func (s *Server) storeUpload(w http.ResponseWriter, r *http.Request, media tools.MediaConfig, dir string) {
	limit := media.Limit()
	r.Body = http.MaxBytesReader(w, r.Body, limit+multipartOverheadBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "expected a multipart/form-data body: "+err.Error())
		return
	}
	part, rawName, err := nextFilePart(reader)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer part.Close()

	name := uploadName(rawName)
	data, err := io.ReadAll(io.LimitReader(part, limit+1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	if int64(len(data)) > limit {
		writeJSONError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"%s is larger than the %d byte upload limit (tools.upload_media.max_bytes)", name, limit))
		return
	}
	if len(data) == 0 {
		writeJSONError(w, http.StatusBadRequest, name+" is empty; there is nothing to attach")
		return
	}
	mediaType, err := media.ResolveType(name, data)
	if err != nil {
		writeJSONError(w, http.StatusUnsupportedMediaType, err.Error())
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create upload directory: "+err.Error())
		return
	}
	id, err := uploadID(dir, name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(dir, id), data, 0o600); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store upload: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, uploadPayload{ID: id, Name: name, Type: mediaType, Size: int64(len(data))})
}

// dropUpload removes one stored upload (the user cancelled the attachment before
// sending it), so a cancelled file does not pile up in the upload directory.
func (s *Server) dropUpload(w http.ResponseWriter, r *http.Request, dir string) {
	path, err := uploadPath(dir, r.URL.Query().Get("id"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		writeJSONError(w, http.StatusInternalServerError, "remove upload: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": true})
}

// nextFilePart returns the first file of a multipart body and its client-supplied
// name. The page posts exactly one file, so anything else (a different field
// name, a body without a file) is reported instead of silently stored.
func nextFilePart(reader *multipart.Reader) (io.ReadCloser, string, error) {
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return nil, "", fmt.Errorf("the upload carried no %q part", uploadFileField)
		}
		if err != nil {
			return nil, "", fmt.Errorf("read upload: %v", err)
		}
		name := part.FileName()
		if part.FormName() != uploadFileField || name == "" {
			_ = part.Close()
			continue
		}
		return part, name, nil
	}
}

// uploadName reduces a client-supplied file name to a safe label: the base name
// only, with separators and control characters removed.
func uploadName(raw string) string {
	name := filepath.Base(strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`/\:*?"<>|`, r) {
			continue
		}
		b.WriteRune(r)
	}
	name = strings.Trim(b.String(), " .")
	if name == "" {
		return "file"
	}
	if len(name) > maxUploadNameBytes {
		// The extension is what the type sniffer reads, so it is kept whole;
		// the rest is cut on a rune boundary (a name is usually not ASCII).
		ext := filepath.Ext(name)
		if len(ext) > maxUploadNameBytes/4 {
			ext = ""
		}
		name = cutRunes(name[:len(name)-len(ext)], maxUploadNameBytes-len(ext)) + ext
	}
	return name
}

// cutRunes cuts s to at most n bytes without splitting a rune.
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := 0
	for i := range s {
		if i > n {
			break
		}
		cut = i
	}
	return s[:cut]
}

// uploadID picks the stored name of one upload: the sanitized original name, or
// that name with a counter before its extension when one is already stored (the
// same file attached twice, say). The id is both the file's name inside the
// upload directory and the token the page sends back, and it stays the name the
// model sees as the attachment's name.
func uploadID(dir, name string) (string, error) {
	if !uploadStored(dir, name) {
		return name, nil
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; i <= uploadIDAttempts; i++ {
		candidate := fmt.Sprintf("%s-%d%s", base, i, ext)
		if !uploadStored(dir, candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("too many uploads named %s", name)
}

// uploadStored reports whether a name is already taken in the upload directory.
func uploadStored(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// uploadPath resolves the id of an upload to its stored file, refusing anything
// that is not a plain file name inside the upload directory.
func uploadPath(dir, id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("no upload id given")
	}
	if id != filepath.Base(id) || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return "", fmt.Errorf("invalid upload id")
	}
	return filepath.Join(dir, id), nil
}
