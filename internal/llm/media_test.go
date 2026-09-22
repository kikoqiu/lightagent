package llm

import (
	"encoding/json"
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
