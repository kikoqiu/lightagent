package llm

import (
	"encoding/base64"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
)

// A media part carries its payload inline: a picture travels as a data URI, audio
// as base64 bytes, a document as a data URI plus its name. That is the shape the
// API takes, but it is not the shape a session file should keep — a saved
// conversation holding a handful of pictures would be megabytes of base64, and
// every save would rewrite them. The two shapes are therefore kept apart:
//
//   - ReferenceMedia is the save direction: a part whose bytes came from a file
//     keeps the file's path (ContentPart.Path) and its media type
//     (ContentPart.Mime) and gives up the payload (HasPayload is false for such
//     a reference).
//   - ResolveMedia is the resume direction: the file is read again and the
//     payload restored, so the conversation can be sent to the model as it was.
//   - requestMessages is the wire direction: the bookkeeping stays home (a path
//     is a local detail the provider has no business seeing) and a part whose
//     file is gone travels as a short note instead of an invalid part.
//
// A part that has no file behind it (its payload was built in memory) is left
// alone by both directions: its bytes have nowhere else to live.

// Attachment is one file a message carries, as a front-end draws it: what to call
// it, what it is, and where it can be read from. The bytes are never part of it,
// so a row description can travel over the event bus (and into the mirror's
// scrollback) without dragging a picture with it.
type Attachment struct {
	Name string `json:"name"`
	// Type is the media type ("image/png"), empty when even that is unknown.
	Type string `json:"type,omitempty"`
	// Path is the file the payload was read from, on the machine the agent runs
	// on. A front-end names the attachment from it.
	Path string `json:"path,omitempty"`
	// URL is where a front-end can fetch the file, when something serves it. The
	// web mirror fills it for the files it stores itself (see its
	// pageAttachments) and leaves it empty for every other file; the agent never
	// sets it.
	URL string `json:"url,omitempty"`
}

// Attachments lists the files a message carries, in the order they were attached.
func (m Message) Attachments() []Attachment { return MediaAttachments(m.Media) }

// MediaAttachments describes a set of media parts for a front-end.
func MediaAttachments(parts []ContentPart) []Attachment {
	if len(parts) == 0 {
		return nil
	}
	out := make([]Attachment, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.Attachment())
	}
	return out
}

// Attachment describes one media part.
func (p ContentPart) Attachment() Attachment {
	return Attachment{Name: p.FileName(), Type: p.MediaType(), Path: p.Path}
}

// FileName is the name an attachment is drawn (and reported to the model) under:
// the name a file part carries, else the base name of the file it was read from,
// else its media type (a part whose payload was built in memory, or one restored
// from a session file written before the parts carried a path).
func (p ContentPart) FileName() string {
	if p.File != nil && strings.TrimSpace(p.File.Filename) != "" {
		return p.File.Filename
	}
	if p.Path != "" {
		if name := filepath.Base(p.Path); name != "" && name != "." {
			return name
		}
	}
	if mt := p.MediaType(); mt != "" {
		return mt
	}
	return "attachment"
}

// MediaType returns the part's media type: the one stored with it, else the one
// its payload names (the prefix of a data URI, the audio container).
func (p ContentPart) MediaType() string {
	if p.Mime != "" {
		return p.Mime
	}
	switch {
	case p.ImageURL != nil:
		return dataURIMime(p.ImageURL.URL)
	case p.Audio != nil:
		if p.Audio.Format != "" {
			return "audio/" + p.Audio.Format
		}
	case p.File != nil:
		return dataURIMime(p.File.FileData)
	}
	return ""
}

// HasPayload reports whether the part still carries its bytes. A part read from a
// session file carries only the name of the file it came from (see
// ReferenceMedia) until ResolveMedia reads it back.
func (p ContentPart) HasPayload() bool {
	switch {
	case p.ImageURL != nil:
		return p.ImageURL.URL != ""
	case p.Audio != nil:
		return p.Audio.Data != ""
	case p.File != nil:
		return p.File.FileData != ""
	}
	// A text part — or one that carries no payload field at all — has nothing
	// to restore.
	return true
}

// Referenced returns the part as a session file keeps it: the payload dropped,
// the file it was read from and its media type kept. It is a no-op for a part
// with no file behind it, whose bytes have nowhere else to live.
func (p ContentPart) Referenced() ContentPart {
	if !p.referable() {
		return p
	}
	if p.Mime == "" {
		p.Mime = p.MediaType()
	}
	switch {
	case p.ImageURL != nil:
		p.ImageURL = &ImageURLPart{Detail: p.ImageURL.Detail}
	case p.Audio != nil:
		p.Audio = &InputAudioPart{Format: p.Audio.Format}
	case p.File != nil:
		p.File = &FilePart{Filename: p.File.Filename}
	}
	return p
}

// referable reports whether a part can be stored as a reference: it must carry
// its payload (a reference has nothing left to drop) and name the file it came
// from (which is where the payload is read back from).
func (p ContentPart) referable() bool {
	return p.Path != "" && p.HasPayload()
}

// ReferenceMedia returns the messages as a session file stores them: every media
// part that has a file behind it gives up its bytes. The input is never modified —
// the media of the live conversation is shared with the agent's history, which
// keeps sending it.
func ReferenceMedia(msgs []Message) []Message {
	return transformMedia(msgs, func(p ContentPart) (ContentPart, bool) {
		if !p.referable() {
			return p, false
		}
		return p.Referenced(), true
	})
}

// ResolveMedia returns the messages with the payload of every referenced media
// part read back from its file, so a conversation described by a session file can
// be sent to the model again. A reference whose file is gone is left as it is: it
// still names the attachment for every front-end, and requestMessages turns it
// into a note instead of an invalid part. The input is never modified.
func ResolveMedia(msgs []Message) []Message {
	return transformMedia(msgs, func(p ContentPart) (ContentPart, bool) {
		if p.Path == "" || p.HasPayload() {
			return p, false
		}
		return p.load()
	})
}

// transformMedia maps the media parts of every message through fn and returns
// fresh messages for the ones that changed, each with a fresh slice of parts.
// Messages that did not change — and so the input itself — are shared, which makes
// the common case (no media at all) free.
func transformMedia(msgs []Message, fn func(ContentPart) (ContentPart, bool)) []Message {
	out := msgs
	copied := false
	for i, m := range msgs {
		var parts []ContentPart
		for j, p := range m.Media {
			next, changed := fn(p)
			if !changed {
				continue
			}
			if parts == nil {
				parts = append([]ContentPart(nil), m.Media...)
			}
			parts[j] = next
		}
		if parts == nil {
			continue
		}
		if !copied {
			out = append([]Message(nil), msgs...)
			copied = true
		}
		out[i].Media = parts
	}
	return out
}

// load reads the file a referenced part points at and returns the part with its
// payload restored. It reports false when there is nothing to read (the file was
// deleted, it is unreadable or empty), leaving the part a reference.
func (p ContentPart) load() (ContentPart, bool) {
	data, err := os.ReadFile(p.Path)
	if err != nil || len(data) == 0 {
		return p, false
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	switch {
	case p.Type == PartTypeInputAudio || p.Audio != nil:
		p.Audio = &InputAudioPart{Data: encoded, Format: audioFormat(p)}
	case p.Type == PartTypeFile || p.File != nil:
		p.File = &FilePart{Filename: p.FileName(), FileData: dataURI(payloadType(p), encoded)}
	default:
		detail := ""
		if p.ImageURL != nil {
			detail = p.ImageURL.Detail
		}
		p.ImageURL = &ImageURLPart{URL: dataURI(payloadType(p), encoded), Detail: detail}
	}
	return p, true
}

// audioFormat returns the container an input_audio part names: the one the
// reference kept, else the one the media type implies (wav is the container the
// API takes for anything that is not MPEG audio).
func audioFormat(p ContentPart) string {
	if p.Audio != nil && p.Audio.Format != "" {
		return p.Audio.Format
	}
	if mt := p.MediaType(); strings.Contains(mt, "mp") {
		return "mp3"
	}
	return "wav"
}

// payloadType is the media type a restored data URI is written with: the stored
// (or derived) one, else the one the file's extension declares, else the generic
// binary type.
func payloadType(p ContentPart) string {
	if mt := p.MediaType(); mt != "" {
		return mt
	}
	if ext := filepath.Ext(p.Path); ext != "" {
		if mt := mime.TypeByExtension(strings.ToLower(ext)); mt != "" {
			return mt
		}
	}
	return "application/octet-stream"
}

// dataURI renders one payload the way the API takes it.
func dataURI(mediaType, encoded string) string {
	return "data:" + mediaType + ";base64," + encoded
}

// dataURIMime returns the media type a data URI names ("data:image/png;base64,…"
// → "image/png"), and "" for anything that is not a data URI (a plain URL, an
// empty payload).
func dataURIMime(uri string) string {
	const prefix = "data:"
	if !strings.HasPrefix(uri, prefix) {
		return ""
	}
	rest := uri[len(prefix):]
	end := strings.IndexAny(rest, ";,")
	if end <= 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(rest[:end]))
}

// requestMessages renders the messages for the provider: the session-file
// bookkeeping stays behind (Path and Mime describe a local file, which is none of
// the provider's business), and a part whose payload was never loaded cannot
// travel as media, so it travels as a short note naming it. A message that is
// already wire-shaped comes back unchanged.
func requestMessages(msgs []Message) []Message {
	return transformMedia(msgs, func(p ContentPart) (ContentPart, bool) {
		if !p.HasPayload() {
			return TextPart(missingMediaNote(p)), true
		}
		if p.Path == "" && p.Mime == "" {
			return p, false
		}
		p.Path = ""
		p.Mime = ""
		return p, true
	})
}

// missingMediaNote tells the model about an attachment whose content is gone: its
// file was deleted between the save and the resume, so there is nothing left to
// send and the part would be invalid on the wire.
func missingMediaNote(p ContentPart) string {
	return fmt.Sprintf("[attachment %s is no longer available: %s]", p.FileName(), p.Path)
}
