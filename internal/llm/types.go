// Package llm implements a minimal OpenAI-compatible chat client.
//
// It only depends on the standard library and speaks the /chat/completions
// protocol, including tool calls, SSE streaming and multimodal content.
package llm

import (
	"encoding/json"
	"strings"
)

// Content part types of a multimodal content array.
const (
	// PartTypeText is a plain text part.
	PartTypeText = "text"
	// PartTypeImageURL is an image part carrying a URL or a data URI.
	PartTypeImageURL = "image_url"
	// PartTypeInputAudio is an audio part carrying base64 data plus its format
	// (wav or mp3), the shape chat completions uses for audio input.
	PartTypeInputAudio = "input_audio"
	// PartTypeFile is a file part carrying a file name plus a data URI. It is
	// the shape OpenAI-compatible servers use for attachments that are not
	// images or audio (a PDF, a video, ...).
	PartTypeFile = "file"
)

// ContentPart is one element of a multimodal message content.
type ContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *ImageURLPart   `json:"image_url,omitempty"`
	Audio    *InputAudioPart `json:"input_audio,omitempty"`
	File     *FilePart       `json:"file,omitempty"`
	// Path is the file this part's payload was read from, and Mime its media
	// type. They are what a session file keeps instead of the payload itself,
	// so a saved conversation names the pictures it carries instead of
	// embedding them as base64 (see media.go). Neither ever reaches the
	// provider: the request leaves them behind.
	Path string `json:"path,omitempty"`
	Mime string `json:"mime,omitempty"`
}

// ImageURLPart is the payload of an image_url content part.
type ImageURLPart struct {
	URL string `json:"url"`
	// Detail asks the provider for a rendering cost ("low", "high", "auto").
	// It is empty for every part lightagent builds: the provider default is
	// what a caller wants.
	Detail string `json:"detail,omitempty"`
}

// InputAudioPart is the payload of an input_audio content part.
type InputAudioPart struct {
	// Data is the audio, base64 encoded.
	Data string `json:"data"`
	// Format is the audio container: "wav" or "mp3".
	Format string `json:"format"`
}

// FilePart is the payload of a file content part.
type FilePart struct {
	Filename string `json:"filename"`
	// FileData is the file as a data URI (data:<type>;base64,<data>).
	FileData string `json:"file_data"`
}

// TextPart builds a text content part.
func TextPart(text string) ContentPart {
	return ContentPart{Type: PartTypeText, Text: text}
}

// Message is a single chat message in the OpenAI schema.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Media holds the multimodal parts appended to Content (images, audio,
	// files). A message with media travels as a content array — the text part
	// first, then the media — which is how every provider expects an attachment
	// to be delivered; a message without it stays a plain string content.
	//
	// The field is only a rendering detail: Content keeps the message's text,
	// so context estimation, display, logging and compaction work on every
	// message the same way. It marshals into content (see MarshalJSON), so a
	// message saved to a session file is restored with its media intact.
	Media      []ContentPart `json:"-"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
	// ReasoningContent is the model's thinking text for providers that expose
	// it (reasoning_content). It is stored with the assistant message and sent
	// back on later requests, so a provider with a preserve-thinking chat
	// template can see the model's own earlier reasoning.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// MarshalJSON encodes the message the way the API expects it: content is a
// plain string, or a content array once the message carries media parts.
func (m Message) MarshalJSON() ([]byte, error) {
	type plain Message // without the methods, so this cannot recurse
	if len(m.Media) == 0 {
		return json.Marshal(plain(m))
	}
	return json.Marshal(struct {
		plain
		Content any `json:"content"`
	}{plain: plain(m), Content: m.contentArray()})
}

// UnmarshalJSON reads both content shapes: a plain string (every message
// without media) and a content array (a message with media parts, e.g. one
// restored from a session file). Text parts become Content and every other
// part becomes Media, which is exactly the layout MarshalJSON writes.
func (m *Message) UnmarshalJSON(data []byte) error {
	type plain Message
	var wire struct {
		plain
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	text, media, err := decodeContent(wire.Content)
	if err != nil {
		return err
	}
	*m = Message(wire.plain)
	m.Content = text
	m.Media = media
	return nil
}

// contentArray renders Content followed by the media parts.
func (m Message) contentArray() []ContentPart {
	parts := make([]ContentPart, 0, len(m.Media)+1)
	if m.Content != "" {
		parts = append(parts, TextPart(m.Content))
	}
	return append(parts, m.Media...)
}

// decodeContent splits a raw content value into its text and its media parts.
func decodeContent(raw json.RawMessage) (string, []ContentPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", nil, err
		}
		return text, nil, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, err
	}
	texts := make([]string, 0, len(parts))
	media := make([]ContentPart, 0, len(parts))
	for _, part := range parts {
		if part.Type == PartTypeText || part.Type == "" {
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
			continue
		}
		media = append(media, part)
	}
	return strings.Join(texts, "\n"), media, nil
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction carries the tool name and its raw JSON arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDef is a tool definition advertised to the model.
type ToolDef struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction describes one callable function.
type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// Usage reports token accounting from the provider.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response is the assembled result of one chat completion call.
type Response struct {
	Content string
	// Reasoning is the model's "thinking" text, when the provider emits it
	// (e.g. reasoning_content or reasoning). It is carried onto the assistant
	// message (see Message.ReasoningContent) so it is preserved and sent back.
	Reasoning string
	ToolCalls []ToolCall
	Usage     Usage
	Finish    string
}

// ChatRequest is the request body sent to /chat/completions.
type ChatRequest struct {
	Model      string    `json:"model"`
	Messages   []Message `json:"messages"`
	Tools      []ToolDef `json:"tools,omitempty"`
	ToolChoice string    `json:"tool_choice,omitempty"`
	// Temperature is a pointer so the "unset" state (a negative value) can be
	// left out of the body while 0 and up are sent verbatim; the caller only
	// fills it for non-negative temperatures (see Client.Chat).
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Stream      bool     `json:"stream,omitempty"`
}
