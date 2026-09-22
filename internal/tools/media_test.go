package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lightagent/internal/llm"
)

// pngBytes is a minimal but real PNG: signature plus IHDR, which is what the
// type sniffer looks at.
var pngBytes = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00,
	0x1f, 0x15, 0xc4, 0x89,
}

// wavBytes is the header of a WAVE file: what the sniffer recognizes as audio.
var wavBytes = append([]byte("RIFF\x00\x00\x00\x00WAVEfmt "), make([]byte, 16)...)

// writeMediaFile writes bytes into a temporary directory under a given name.
func writeMediaFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestNormalizeMediaTypes(t *testing.T) {
	got := NormalizeMediaTypes([]string{" image/png ", "IMAGE/PNG", "image", "", "audio/WAV"})
	want := []string{"image/png", "image/*", "audio/wav"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("NormalizeMediaTypes = %v, want %v", got, want)
	}
}

func TestMediaConfigSupports(t *testing.T) {
	cfg := NewMediaConfig([]string{"image/png", "audio", "application/pdf"}, 0)
	cases := map[string]bool{
		"image/png":            true,
		"IMAGE/PNG":            true,
		"image/png; charset=x": true,
		"image/jpeg":           false, // only one image type is configured
		"audio/wav":            true,  // the family name accepts every audio type
		"audio/mpeg":           true,
		"application/pdf":      true,
		"application/zip":      false,
		"":                     false,
	}
	for mediaType, want := range cases {
		if got := cfg.Supports(mediaType); got != want {
			t.Errorf("Supports(%q) = %v, want %v", mediaType, got, want)
		}
	}
	if !cfg.Enabled() {
		t.Fatal("a configured type list must enable the capability")
	}
	if NewMediaConfig(nil, 0).Enabled() {
		t.Fatal("an empty type list must not enable the capability")
	}
}

func TestMediaConfigLimit(t *testing.T) {
	if got := (MediaConfig{}).Limit(); got != MediaMaxBytesDefault {
		t.Fatalf("Limit() = %d, want the built-in %d", got, MediaMaxBytesDefault)
	}
	if got := (MediaConfig{MaxBytes: 1024}).Limit(); got != 1024 {
		t.Fatalf("Limit() = %d, want the configured 1024", got)
	}
	if got := (MediaConfig{MaxBytes: -5}).Limit(); got != MediaMaxBytesDefault {
		t.Fatalf("a negative cap must fall back: %d", got)
	}
}

// TestUploadMediaToolUploadsAnImage verifies the happy path: the file travels as
// an image_url part carrying a data URI, the answer names what was uploaded and
// the parameter hint lists the accepted types.
func TestUploadMediaToolUploadsAnImage(t *testing.T) {
	path := writeMediaFile(t, "shot.png", pngBytes)
	tool := NewUploadMediaTool(NewMediaConfig([]string{"image/png", "application/pdf"}, 0))

	if tool.Name() != UploadMediaToolName {
		t.Fatalf("Name() = %q", tool.Name())
	}
	desc := tool.Description()
	if !strings.Contains(desc, "image/png, application/pdf") {
		t.Fatalf("description does not list the accepted types: %s", desc)
	}
	params, err := json.Marshal(tool.Parameters())
	if err != nil {
		t.Fatalf("marshal parameters: %v", err)
	}
	if !strings.Contains(string(params), "image/png, application/pdf") {
		t.Fatalf("the path hint does not carry the accepted types: %s", params)
	}

	res := tool.Execute(context.Background(), map[string]any{"path": path})
	if res.IsError {
		t.Fatalf("upload failed: %s", res.ForLLM)
	}
	if len(res.Media) != 1 {
		t.Fatalf("media parts = %d, want 1", len(res.Media))
	}
	part := res.Media[0]
	if part.Type != llm.PartTypeImageURL || part.ImageURL == nil {
		t.Fatalf("part = %+v, want an image_url part", part)
	}
	wantData := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
	if part.ImageURL.URL != wantData {
		t.Fatalf("data URI = %q, want the file's bytes", part.ImageURL.URL)
	}
	if !strings.Contains(res.ForLLM, "image/png") || !strings.Contains(res.ForLLM, "shot.png") {
		t.Fatalf("answer does not describe the upload: %s", res.ForLLM)
	}
}

// TestUploadMediaToolRefusesOtherTypes verifies a file of an unaccepted type is
// an error naming the accepted ones, and that a failed call carries no media:
// a refused file must never reach the model.
func TestUploadMediaToolRefusesOtherTypes(t *testing.T) {
	path := writeMediaFile(t, "archive.zip", []byte("PK\x03\x04payload"))
	tool := NewUploadMediaTool(NewMediaConfig([]string{"image/png"}, 0))

	res := tool.Execute(context.Background(), map[string]any{"path": path})
	if !res.IsError {
		t.Fatalf("a zip must be refused, got %q", res.ForLLM)
	}
	if len(res.Media) != 0 {
		t.Fatalf("a refused upload must carry no media: %+v", res.Media)
	}
	if !strings.Contains(res.ForLLM, "archive.zip") || !strings.Contains(res.ForLLM, "image/png") {
		t.Fatalf("the error must name the file and the accepted types: %s", res.ForLLM)
	}
}

// TestUploadMediaToolErrors covers the remaining refusals: a missing argument,
// a file that is not there, a directory and one over the size cap.
func TestUploadMediaToolErrors(t *testing.T) {
	dir := t.TempDir()
	small := NewMediaConfig([]string{"image/*"}, 8)

	cases := []struct {
		name string
		cfg  MediaConfig
		args map[string]any
		want string
	}{
		{"missing path", small, map[string]any{}, "path"},
		{"blank path", small, map[string]any{"path": "   "}, "path"},
		{"absent file", small, map[string]any{"path": filepath.Join(dir, "nope.png")}, "nope.png"},
		{"directory", small, map[string]any{"path": dir}, "directory"},
		{"over the cap", small, map[string]any{"path": writeMediaFile(t, "big.png", append(pngBytes, make([]byte, 64)...))}, "upload limit"},
	}
	for _, tc := range cases {
		res := NewUploadMediaTool(tc.cfg).Execute(context.Background(), tc.args)
		if !res.IsError {
			t.Errorf("%s: expected an error, got %q", tc.name, res.ForLLM)
			continue
		}
		if !strings.Contains(res.ForLLM, tc.want) {
			t.Errorf("%s: error = %q, want it to mention %q", tc.name, res.ForLLM, tc.want)
		}
		if len(res.Media) != 0 {
			t.Errorf("%s: an error result must carry no media", tc.name)
		}
	}
}

// TestReadMediaPartShapes verifies the part each media family travels in: an
// input_audio part for the audio containers chat completions accepts, and a file
// part (name + data URI) for everything else.
func TestReadMediaPartShapes(t *testing.T) {
	cfg := NewMediaConfig([]string{"audio/*", "application/pdf", "video/mp4"}, 0)

	wav, err := cfg.ReadMedia(writeMediaFile(t, "clip.wav", wavBytes))
	if err != nil {
		t.Fatalf("read wav: %v", err)
	}
	if len(wav.Parts) != 1 || wav.Parts[0].Type != llm.PartTypeInputAudio || wav.Parts[0].Audio == nil {
		t.Fatalf("wav part = %+v, want an input_audio part", wav.Parts)
	}
	if wav.Parts[0].Audio.Format != "wav" {
		t.Fatalf("audio format = %q, want wav", wav.Parts[0].Audio.Format)
	}
	if wav.Parts[0].Audio.Data != base64.StdEncoding.EncodeToString(wavBytes) {
		t.Fatal("the audio part must carry the file's bytes, base64 encoded")
	}

	pdf, err := cfg.ReadMedia(writeMediaFile(t, "doc.pdf", []byte("%PDF-1.7\nbody")))
	if err != nil {
		t.Fatalf("read pdf: %v", err)
	}
	if len(pdf.Parts) != 1 || pdf.Parts[0].Type != llm.PartTypeFile || pdf.Parts[0].File == nil {
		t.Fatalf("pdf part = %+v, want a file part", pdf.Parts)
	}
	if pdf.Parts[0].File.Filename != "doc.pdf" {
		t.Fatalf("file part name = %q", pdf.Parts[0].File.Filename)
	}
	if !strings.HasPrefix(pdf.Parts[0].File.FileData, "data:application/pdf;base64,") {
		t.Fatalf("file part data = %q", pdf.Parts[0].File.FileData)
	}
}

// TestReadMediaSniffsTheContent verifies a file whose name says nothing is typed
// by what is inside it: an extensionless PNG still matches an image type.
func TestReadMediaSniffsTheContent(t *testing.T) {
	cfg := NewMediaConfig([]string{"image/png"}, 0)
	file, err := cfg.ReadMedia(writeMediaFile(t, "picture", pngBytes))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if file.Type != "image/png" {
		t.Fatalf("type = %q, want image/png", file.Type)
	}
	if file.Name != "picture" || file.Size != int64(len(pngBytes)) {
		t.Fatalf("file = %+v", file)
	}
}
