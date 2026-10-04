package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"lightagent/internal/llm"
)

// UploadMediaToolName is the tool identifier of the media uploader.
const UploadMediaToolName = "upload_media"

const (
	// MediaMaxBytesDefault caps one uploaded file when nothing else is
	// configured (20 MiB, the size limit OpenAI applies to an image).
	MediaMaxBytesDefault = 20 << 20
	// mediaTypeWildcardSuffix marks a whole family: "image/*" accepts every
	// image type.
	mediaTypeWildcardSuffix = "/*"
	// mediaSniffBytes is how much of a file the type sniffer looks at.
	mediaSniffBytes = 512
)

// MediaConfig is the multimedia capability of the model: the media types
// (openai.media_types) it accepts as attachments and the size one uploaded file
// may have.
//
// It is shared by the upload_media tool and by the web mirror's attach control,
// so both offer exactly the types the model is configured to read.
type MediaConfig struct {
	// Types are the accepted media types, lower case, in the order they were
	// configured. A wildcard subtype ("image/*", or the family name "image"
	// which normalizes to it) accepts every subtype of its top-level type. An
	// empty list means the capability is off: no tool, no attach control.
	Types []string
	// MaxBytes caps one uploaded file. 0 (or a negative value) keeps
	// MediaMaxBytesDefault.
	MaxBytes int64
}

// NewMediaConfig builds the capability from a configured type list.
func NewMediaConfig(types []string, maxBytes int64) MediaConfig {
	return MediaConfig{Types: NormalizeMediaTypes(types), MaxBytes: maxBytes}
}

// NormalizeMediaTypes cleans a configured type list: entries are trimmed and
// lower cased, an empty entry is dropped, and a bare family name turns into its
// wildcard form ("image" → "image/*"). The first occurrence of a type wins, so
// the list the model is told about reads the way it was written.
func NormalizeMediaTypes(types []string) []string {
	out := make([]string, 0, len(types))
	seen := make(map[string]bool, len(types))
	for _, raw := range types {
		mediaType := strings.ToLower(strings.TrimSpace(raw))
		if mediaType == "" {
			continue
		}
		if !strings.Contains(mediaType, "/") {
			mediaType += mediaTypeWildcardSuffix
		}
		if seen[mediaType] {
			continue
		}
		seen[mediaType] = true
		out = append(out, mediaType)
	}
	return out
}

// Enabled reports whether any media type is configured.
func (c MediaConfig) Enabled() bool { return len(c.Types) > 0 }

// List renders the accepted types as one readable list, for the tool
// description, the parameter hint and the error a refused file returns.
func (c MediaConfig) List() string { return strings.Join(c.Types, ", ") }

// Accept renders the accepted types as a comma separated list, the value an
// <input type="file"> accept attribute takes.
func (c MediaConfig) Accept() string { return strings.Join(c.Types, ",") }

// Limit returns the effective per-file byte cap.
func (c MediaConfig) Limit() int64 {
	if c.MaxBytes <= 0 {
		return MediaMaxBytesDefault
	}
	return c.MaxBytes
}

// Supports reports whether a media type is accepted. A wildcard entry matches
// every subtype of its family.
func (c MediaConfig) Supports(mediaType string) bool {
	mediaType = normalizeMediaType(mediaType)
	if mediaType == "" {
		return false
	}
	for _, allowed := range c.Types {
		if allowed == mediaType {
			return true
		}
		if strings.HasSuffix(allowed, mediaTypeWildcardSuffix) &&
			strings.HasPrefix(mediaType, strings.TrimSuffix(allowed, "*")) {
			return true
		}
	}
	return false
}

// MediaFile is one local file an upload turns into content parts for the model.
type MediaFile struct {
	// Path is the file's absolute path.
	Path string
	// Name is its base name, which the file part carries.
	Name string
	// Type is the detected media type.
	Type string
	// Size is the file size in bytes.
	Size int64
	// Parts carries the file to the model.
	Parts []llm.ContentPart
}

// ReadMedia reads one file and turns it into the content parts that hand it to
// the model. It refuses a directory, an empty file, a file above the size cap
// and one of a type the model does not accept (naming the accepted types).
func (c MediaConfig) ReadMedia(path string) (*MediaFile, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("no file path given")
	}
	// Everything downstream works on the absolute path: it is what the part
	// remembers (see below) and what an error reports, and a session file
	// referring to it stays readable from any working directory.
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not a file", path)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("%s is empty; there is nothing to upload", path)
	}
	limit := c.Limit()
	if info.Size() > limit {
		return nil, fmt.Errorf("%s is %s, over the %s upload limit (tools.upload_media.max_bytes)",
			path, humanBytes(info.Size()), humanBytes(limit))
	}
	// The sniffing window is read first, so a file of an unsupported type is
	// refused without loading all of it.
	head, err := readHead(path, mediaSniffBytes)
	if err != nil {
		return nil, err
	}
	mediaType, err := c.ResolveType(filepath.Base(path), head)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(filepath.Base(path))
	if name == "" || name == "." {
		name = "file"
	}
	part := mediaPart(mediaType, name, data)
	// The part remembers the file it came from. That path is what a session file
	// keeps instead of the payload (see llm.ReferenceMedia), and what lets a
	// front-end name the attachment — or the web mirror serve its picture.
	part.Path = path
	part.Mime = mediaType
	return &MediaFile{
		Path:  path,
		Name:  name,
		Type:  mediaType,
		Size:  info.Size(),
		Parts: []llm.ContentPart{part},
	}, nil
}

// ResolveType resolves the media type of a file from its name and its first
// bytes and reports whether the model accepts it: it returns the accepted type,
// or an error saying what the file looks like and which types are accepted. Both
// the upload tool and the web mirror's attach control validate a file with it,
// so a file is refused (or accepted) exactly the same way on either path.
func (c MediaConfig) ResolveType(name string, head []byte) (string, error) {
	if len(head) > mediaSniffBytes {
		head = head[:mediaSniffBytes]
	}
	candidates := mediaTypeCandidates(filepath.Base(name), head)
	for _, candidate := range candidates {
		if c.Supports(candidate) {
			return candidate, nil
		}
	}
	detected := "an unknown type"
	if len(candidates) > 0 {
		detected = candidates[0]
	}
	return "", fmt.Errorf("%q looks like %s, which this model does not accept; upload one of: %s",
		filepath.Base(name), detected, c.List())
}

// mediaTypeCandidates lists the types a file may be, most likely first: the one
// its extension declares and the one its content looks like. Both are returned,
// so a file whose name says nothing (or lies) still matches an accepted type.
func mediaTypeCandidates(name string, head []byte) []string {
	var out []string
	if byExt := normalizeMediaType(mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))); byExt != "" {
		out = append(out, byExt)
	}
	sniffed := normalizeMediaType(http.DetectContentType(head))
	if sniffed == "" {
		return out
	}
	for _, known := range out {
		if known == sniffed {
			return out
		}
	}
	return append(out, sniffed)
}

// normalizeMediaType lower cases a media type and drops its parameters
// ("text/plain; charset=utf-8" → "text/plain").
func normalizeMediaType(mediaType string) string {
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = mediaType[:i]
	}
	return strings.ToLower(strings.TrimSpace(mediaType))
}

// mediaPart renders one file as the content part its media type calls for: an
// image_url part carrying a data URI for an image, an input_audio part for the
// audio containers chat completions accepts (wav, mp3), and a file part
// (filename + data URI) for everything else — a PDF, a video, other audio.
func mediaPart(mediaType, name string, data []byte) llm.ContentPart {
	encoded := base64.StdEncoding.EncodeToString(data)
	switch {
	case strings.HasPrefix(mediaType, "image/"):
		return llm.ContentPart{
			Type:     llm.PartTypeImageURL,
			ImageURL: &llm.ImageURLPart{URL: "data:" + mediaType + ";base64," + encoded},
		}
	case audioContainer(mediaType) != "":
		return llm.ContentPart{
			Type:  llm.PartTypeInputAudio,
			Audio: &llm.InputAudioPart{Data: encoded, Format: audioContainer(mediaType)},
		}
	default:
		return llm.ContentPart{
			Type: llm.PartTypeFile,
			File: &llm.FilePart{Filename: name, FileData: "data:" + mediaType + ";base64," + encoded},
		}
	}
}

// audioContainer maps an audio type to the container an input_audio part names,
// and returns "" for every type that part cannot carry.
func audioContainer(mediaType string) string {
	switch mediaType {
	case "audio/wav", "audio/wave", "audio/x-wav", "audio/vnd.wave":
		return "wav"
	case "audio/mpeg", "audio/mp3", "audio/x-mp3":
		return "mp3"
	}
	return ""
}

// readHead reads at most n bytes from the start of a file.
func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	read, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return buf[:read], nil
}

// humanBytes renders a byte count the way a status line reads it.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value)
}

// UploadMediaTool reads one local file and uploads it as an attachment of the
// conversation: the file travels with the tool result (see Result.Media), so the
// model receives its content instead of its bytes.
//
// It is registered only when the model declares the media types it accepts
// (openai.media_types) and the tool's own switch (tools.upload_media.enabled) is
// on: both have to be true for the tool to exist.
type UploadMediaTool struct {
	cfg MediaConfig
}

// NewUploadMediaTool creates the media uploader for the configured types.
func NewUploadMediaTool(cfg MediaConfig) *UploadMediaTool {
	cfg.Types = NormalizeMediaTypes(cfg.Types)
	return &UploadMediaTool{cfg: cfg}
}

// Name implements Tool.
func (t *UploadMediaTool) Name() string { return UploadMediaToolName }

// Description implements Tool.
func (t *UploadMediaTool) Description() string {
	return fmt.Sprintf(
		"Read a local file whose media type this model accepts and upload it as an attachment of this conversation, "+
			"so you receive its content (the picture, the audio, the document) instead of having to read its bytes. "+
			"Accepted types: %s. A file of any other type is refused with an error naming the accepted types; "+
			"convert it first, or read it as text with read_file when it really is text.",
		t.cfg.List())
}

// Parameters implements Tool.
func (t *UploadMediaTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type": "string",
				"description": fmt.Sprintf(
					"Path of the file to upload. Accepted media types: %s; a file of any other type is refused.",
					t.cfg.List()),
			},
		},
		"required": []string{"path"},
	}
}

// Execute implements Tool.
func (t *UploadMediaTool) Execute(ctx context.Context, args map[string]any) *Result {
	path, _ := stringArg(args, "path")
	if strings.TrimSpace(path) == "" {
		return Fail(`missing required argument "path"`)
	}
	file, err := t.cfg.ReadMedia(path)
	if err != nil {
		return Fail(err.Error())
	}
	note := fmt.Sprintf("Uploaded %s attachment %q (%s); its content is attached to this tool result.",
		file.Type, file.Name, humanBytes(file.Size))
	return &Result{ForLLM: note, ForUser: note, Media: file.Parts}
}
