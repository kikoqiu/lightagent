package llm

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMessageRendersMediaAsContentArray verifies a message with media is sent
// the way providers expect an attachment: content becomes an array whose first
// element is the message's text.
func TestMessageRendersMediaAsContentArray(t *testing.T) {
	msg := Message{
		Role:    "tool",
		Content: "Uploaded image/png attachment \"shot.png\" (12 B).",
		Media: []ContentPart{
			{Type: PartTypeImageURL, ImageURL: &ImageURLPart{URL: "data:image/png;base64,AAAA"}},
		},
		ToolCallID: "call_1",
		Name:       "upload_media",
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		Content    []map[string]any `json:"content"`
		Role       string           `json:"role"`
		ToolCallID string           `json:"tool_call_id"`
		Name       string           `json:"name"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("the content of a message with media must be an array: %v (%s)", err, data)
	}
	if len(wire.Content) != 2 {
		t.Fatalf("content parts = %d, want the text plus the media: %s", len(wire.Content), data)
	}
	if wire.Content[0]["type"] != PartTypeText || wire.Content[0]["text"] != msg.Content {
		t.Fatalf("the text part must come first: %s", data)
	}
	image, _ := wire.Content[1]["image_url"].(map[string]any)
	if wire.Content[1]["type"] != PartTypeImageURL || image["url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("the image part is wrong: %s", data)
	}
	if wire.Role != "tool" || wire.ToolCallID != "call_1" || wire.Name != "upload_media" {
		t.Fatalf("the rest of the message must survive: %s", data)
	}
}

// TestMessageWithoutMediaKeepsStringContent verifies every ordinary message is
// still a plain string content, so providers that take no content array (and the
// session files of the conversations before this feature) are unaffected.
func TestMessageWithoutMediaKeepsStringContent(t *testing.T) {
	data, err := json.Marshal(Message{Role: "user", Content: "hello"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"content":"hello"`) {
		t.Fatalf("content must stay a string: %s", data)
	}
}

// TestMessageRoundTripWithMedia verifies a message read back — from a session
// file, say — is restored with its text and its media apart again, and that
// writing it out a second time is stable.
func TestMessageRoundTripWithMedia(t *testing.T) {
	original := Message{
		Role:    "user",
		Content: "what is in this picture?",
		Media: []ContentPart{
			{Type: PartTypeImageURL, ImageURL: &ImageURLPart{URL: "data:image/png;base64,AAAA"}},
			{Type: PartTypeFile, File: &FilePart{Filename: "a.pdf", FileData: "data:application/pdf;base64,BBBB"}},
		},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var restored Message
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored.Role != "user" || restored.Content != original.Content {
		t.Fatalf("restored = %+v", restored)
	}
	if len(restored.Media) != 2 {
		t.Fatalf("media parts = %d, want 2: %s", len(restored.Media), data)
	}
	if restored.Media[0].ImageURL == nil || restored.Media[0].ImageURL.URL != "data:image/png;base64,AAAA" {
		t.Fatalf("image part = %+v", restored.Media[0])
	}
	if restored.Media[1].File == nil || restored.Media[1].File.Filename != "a.pdf" {
		t.Fatalf("file part = %+v", restored.Media[1])
	}
	again, err := json.Marshal(restored)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(again) != string(data) {
		t.Fatalf("the round trip is not stable:\n%s\n%s", data, again)
	}
}

// TestMessageUnmarshalReadsPlainContent verifies the string shape is still read
// (a session file written before the media feature, or a reply from a provider).
func TestMessageUnmarshalReadsPlainContent(t *testing.T) {
	var msg Message
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"answer"}`), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Role != "assistant" || msg.Content != "answer" || len(msg.Media) != 0 {
		t.Fatalf("message = %+v", msg)
	}
}

// TestMediaAttachmentsDescribeTheFiles pins what a front-end is told about the
// files a message carries: what to call each of them, what it is, and where it
// was read from — never the payload.
func TestMediaAttachmentsDescribeTheFiles(t *testing.T) {
	msg := Message{
		Role: "user",
		Media: []ContentPart{
			{Type: PartTypeImageURL, ImageURL: &ImageURLPart{URL: "data:image/png;base64,AAAA"}, Path: filepath.Join("state", "uploads", "shot.png")},
			{Type: PartTypeFile, File: &FilePart{Filename: "notes.pdf", FileData: "data:application/pdf;base64,BBBB"}},
			{Type: PartTypeInputAudio, Audio: &InputAudioPart{Data: "CCCC", Format: "wav"}},
		},
	}
	got := msg.Attachments()
	if len(got) != 3 {
		t.Fatalf("attachments = %+v, want one per media part", got)
	}
	if got[0].Name != "shot.png" || got[0].Type != "image/png" || got[0].Path == "" {
		t.Errorf("image attachment = %+v", got[0])
	}
	if got[1].Name != "notes.pdf" || got[1].Type != "application/pdf" || got[1].Path != "" {
		t.Errorf("file attachment = %+v", got[1])
	}
	if got[2].Type != "audio/wav" {
		t.Errorf("audio attachment = %+v", got[2])
	}
}

// TestReferenceMediaKeepsThePathNotTheBytes verifies the save direction: a media
// part whose payload came from a file is stored as that path (and its media type)
// with the payload dropped, the live message keeps its bytes, and a part with no
// file behind it is left as it is.
func TestReferenceMediaKeepsThePathNotTheBytes(t *testing.T) {
	image := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(image, []byte("PNGDATA"), 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}
	payload := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("PNGDATA"))
	msgs := []Message{
		{Role: "user", Content: "look", Media: []ContentPart{
			{Type: PartTypeImageURL, ImageURL: &ImageURLPart{URL: payload}, Path: image},
			{Type: PartTypeFile, File: &FilePart{Filename: "inline.pdf", FileData: "data:application/pdf;base64,BBBB"}},
		}},
		{Role: "assistant", Content: "answer"},
	}

	refs := ReferenceMedia(msgs)
	if len(refs) != 2 || refs[0].Role != "user" || refs[0].Content != "look" {
		t.Fatalf("referenced messages = %+v", refs)
	}
	part := refs[0].Media[0]
	if part.Path != image || part.Mime != "image/png" {
		t.Fatalf("referenced part = %+v, want the path and the media type", part)
	}
	if part.HasPayload() || part.ImageURL == nil || part.ImageURL.URL != "" {
		t.Fatalf("referenced part still carries bytes: %+v", part)
	}
	// A part with no file behind it has nowhere else to keep its bytes.
	if !refs[0].Media[1].HasPayload() {
		t.Fatalf("a part with no path must keep its payload: %+v", refs[0].Media[1])
	}
	// The live message is untouched: the conversation keeps sending its media.
	if msgs[0].Media[0].ImageURL.URL != payload {
		t.Fatalf("the input message was modified: %+v", msgs[0].Media[0])
	}

	// The stored form is what a session file holds: paths, no base64.
	data, err := json.Marshal(refs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "PNGDATA") || strings.Contains(string(data), "base64,AA") {
		t.Fatalf("the stored shape still carries the payload: %s", data)
	}
	if !strings.Contains(string(data), `"mime":"image/png"`) {
		t.Fatalf("the stored shape must keep the media type: %s", data)
	}
}

// TestResolveMediaReadsTheFilesBack verifies the resume direction: the payload of
// a referenced part is read from its file again, a reference whose file is gone
// stays a reference (and travels as a note instead of an invalid part), and a
// part that already carries its bytes is left alone.
func TestResolveMediaReadsTheFilesBack(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(image, []byte("PNGDATA"), 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}
	gone := filepath.Join(dir, "deleted.png")
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("PNGDATA"))
	msgs := []Message{{Role: "user", Content: "look", Media: []ContentPart{
		{Type: PartTypeImageURL, ImageURL: &ImageURLPart{}, Path: image, Mime: "image/png"},
		{Type: PartTypeImageURL, ImageURL: &ImageURLPart{}, Path: gone, Mime: "image/png"},
	}}}

	resolved := ResolveMedia(msgs)
	if got := resolved[0].Media[0].ImageURL.URL; got != want {
		t.Fatalf("resolved payload = %q, want %q", got, want)
	}
	if resolved[0].Media[1].HasPayload() {
		t.Fatalf("a file that is gone must stay a reference: %+v", resolved[0].Media[1])
	}
	if msgs[0].Media[0].HasPayload() {
		t.Fatalf("the input message was modified: %+v", msgs[0].Media[0])
	}

	// The request cannot carry a part without a payload: it names the file
	// instead, and the session bookkeeping stays behind.
	wire := requestMessages(resolved)
	if len(wire[0].Media) != 2 {
		t.Fatalf("wire media = %+v", wire[0].Media)
	}
	if wire[0].Media[0].Path != "" || wire[0].Media[0].Mime != "" {
		t.Fatalf("the wire part must not name a local file: %+v", wire[0].Media[0])
	}
	note := wire[0].Media[1]
	if note.Type != PartTypeText || !strings.Contains(note.Text, "deleted.png") {
		t.Fatalf("a missing attachment must travel as a note: %+v", note)
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), `"path"`) || strings.Contains(string(data), `"mime"`) {
		t.Fatalf("the request must not carry the session bookkeeping: %s", data)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("the request must carry the payload: %s", data)
	}
}
