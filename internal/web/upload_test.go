package web

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"lightagent/internal/agent"
	"lightagent/internal/config"
	"lightagent/internal/llm"
	"lightagent/internal/tools"
)

// pngBytes is a minimal real PNG (signature plus IHDR): enough for the type
// sniffer to recognize it.
var pngBytes = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00,
	0x1f, 0x15, 0xc4, 0x89,
}

// newMediaTestServer builds a mirror with the attachment capability wired to a
// temporary upload directory. The llm endpoint points at a dead loopback port,
// so a turn the test starts fails at once instead of reaching the network.
func newMediaTestServer(t *testing.T, types []string, maxBytes int64) (*Server, string) {
	t.Helper()

	cfg := config.Default()
	cfg.OpenAI.APIBase = "http://127.0.0.1:1/v1"
	client := llm.NewClient(cfg.OpenAI)
	bus := agent.NewBus()
	ag := agent.New(cfg, client, tools.NewRegistry(), bus)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	srv, err := New(ag, "127.0.0.1", port, true)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	uploads := filepath.Join(t.TempDir(), UploadsDirName)
	srv.SetMedia(tools.NewMediaConfig(types, maxBytes), uploads)
	srv.Start()
	t.Cleanup(func() { _ = srv.Close() })
	return srv, uploads
}

// postUpload sends one file to /api/upload and returns the response with its
// decoded body.
func postUpload(t *testing.T, base, name string, data []byte) (*http.Response, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(uploadFileField, name)
	if err != nil {
		t.Fatalf("build upload: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("build upload: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close upload: %v", err)
	}
	resp, err := http.Post(base+"/api/upload", writer.FormDataContentType(), &body)
	if err != nil {
		t.Fatalf("POST /api/upload: %v", err)
	}
	defer resp.Body.Close()
	payload := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &payload)
	if payload == nil {
		payload = map[string]any{"raw": string(raw)}
	}
	return resp, payload
}

// quote renders one JSON string literal.
func quote(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

// waitForUserMessage waits for the turn a submitted message started to record its
// user message in the history.
func waitForUserMessage(t *testing.T, ag *agent.Agent) llm.Message {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, msg := range ag.History() {
			if msg.Role == "user" {
				return msg
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the submitted message never reached the history")
	return llm.Message{}
}

// TestUploadDisabledWithoutMediaTypes verifies the attach endpoint does not
// exist for a run without the multimedia capability: no media types means no
// attachments, whatever a client posts.
func TestUploadDisabledWithoutMediaTypes(t *testing.T) {
	srv := newTestServer(t, "")
	resp, payload := postUpload(t, baseURL(srv), "shot.png", pngBytes)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%v)", resp.StatusCode, payload)
	}
	if msg, _ := payload["error"].(string); !strings.Contains(msg, "media_types") {
		t.Fatalf("the refusal must say how to enable attachments: %v", payload)
	}
}

// TestUploadStoresAndDropsAttachment covers the attach control's round trip: the
// file lands in .lightagent/uploads under the id the page sends back, and the
// cancel path deletes it again.
func TestUploadStoresAndDropsAttachment(t *testing.T) {
	srv, uploads := newMediaTestServer(t, []string{"image/png"}, 0)
	base := baseURL(srv)

	resp, payload := postUpload(t, base, "shot.png", pngBytes)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", resp.StatusCode, payload)
	}
	id, _ := payload["id"].(string)
	if id == "" || payload["name"] != "shot.png" || payload["type"] != "image/png" {
		t.Fatalf("upload payload = %v", payload)
	}
	if got, ok := payload["size"].(float64); !ok || int(got) != len(pngBytes) {
		t.Fatalf("size = %v, want %d", payload["size"], len(pngBytes))
	}
	if strings.ContainsAny(id, `/\`) {
		t.Fatalf("id %q must be a plain file name inside the upload directory", id)
	}
	stored, err := os.ReadFile(filepath.Join(uploads, id))
	if err != nil {
		t.Fatalf("the upload was not stored: %v", err)
	}
	if !bytes.Equal(stored, pngBytes) {
		t.Fatal("the stored file does not match what was uploaded")
	}

	// Cancel: the page drops the chip and the stored file goes with it.
	req, err := http.NewRequest(http.MethodDelete, base+"/api/upload?id="+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /api/upload: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", delResp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(uploads, id)); !os.IsNotExist(err) {
		t.Fatalf("the cancelled upload is still there (stat err = %v)", err)
	}
}

// TestUploadRefusesOtherAndOversizedFiles verifies the two refusals the page
// shows: a type the model does not accept (naming the accepted ones) and a file
// over the configured cap. Neither may be stored.
func TestUploadRefusesOtherAndOversizedFiles(t *testing.T) {
	srv, uploads := newMediaTestServer(t, []string{"image/png"}, 16)
	base := baseURL(srv)

	resp, payload := postUpload(t, base, "notes.txt", []byte("just text"))
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415 (%v)", resp.StatusCode, payload)
	}
	if msg, _ := payload["error"].(string); !strings.Contains(msg, "image/png") {
		t.Fatalf("the refusal must name the accepted types: %v", payload)
	}

	over := append(append([]byte{}, pngBytes...), make([]byte, 64)...)
	resp, payload = postUpload(t, base, "big.png", over)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (%v)", resp.StatusCode, payload)
	}
	if entries, err := os.ReadDir(uploads); err == nil && len(entries) > 0 {
		t.Fatalf("a refused upload must not be stored: %v", entries)
	}
}

// turned into the content parts the model receives, in order, and that an id
// outside the upload directory is refused.
func TestReadAttachmentsBuildsTheMediaParts(t *testing.T) {
	srv, _ := newMediaTestServer(t, []string{"image/png", "application/pdf"}, 0)
	base := baseURL(srv)

	_, first := postUpload(t, base, "one.png", pngBytes)
	_, second := postUpload(t, base, "two.pdf", []byte("%PDF-1.7\nbody"))
	ids := []string{first["id"].(string), second["id"].(string)}

	parts, names := srv.readAttachments(ids)
	if len(parts) != 2 || len(names) != 2 {
		t.Fatalf("parts = %d, names = %v", len(parts), names)
	}
	if parts[0].Type != llm.PartTypeImageURL || parts[0].ImageURL == nil {
		t.Fatalf("first part = %+v, want an image", parts[0])
	}
	if parts[1].Type != llm.PartTypeFile || parts[1].File == nil || parts[1].File.Filename != "two.pdf" {
		t.Fatalf("second part = %+v, want a file", parts[1])
	}
	if strings.Join(names, ",") != "one.png,two.pdf" {
		t.Fatalf("names = %v, want the order they were attached in", names)
	}

	for _, bad := range []string{"../../session.json", "a/b.png", ""} {
		if parts, _ := srv.readAttachments([]string{bad}); len(parts) != 0 {
			t.Fatalf("id %q must be refused", bad)
		}
	}
}

// TestAttachmentTravelsWithTheNextMessage verifies the whole web path: a file
// uploaded from the page rides with the next prompt, so the agent's user message
// carries both the text and the media.
func TestAttachmentTravelsWithTheNextMessage(t *testing.T) {
	srv, _ := newMediaTestServer(t, []string{"image/png"}, 0)
	_, payload := postUpload(t, baseURL(srv), "shot.png", pngBytes)

	srv.handleClientMessage([]byte(`{"text":"what is this?","attachments":[` +
		quote(payload["id"].(string)) + `]}`))

	msg := waitForUserMessage(t, srv.agent)
	if msg.Content != "what is this?" {
		t.Fatalf("content = %q, want the prompt", msg.Content)
	}
	if len(msg.Media) != 1 || msg.Media[0].Type != llm.PartTypeImageURL {
		t.Fatalf("user message = %+v, want the attached image", msg)
	}
}

// TestAttachmentOnlyMessageGetsAPlaceholder verifies a message that is nothing
// but an attachment still reads as one: the row and the model see a line naming
// the file instead of an empty prompt.
func TestAttachmentOnlyMessageGetsAPlaceholder(t *testing.T) {
	srv, _ := newMediaTestServer(t, []string{"image/png"}, 0)
	_, payload := postUpload(t, baseURL(srv), "shot.png", pngBytes)

	srv.handleClientMessage([]byte(`{"text":"","attachments":[` + quote(payload["id"].(string)) + `]}`))

	msg := waitForUserMessage(t, srv.agent)
	if !strings.Contains(msg.Content, "shot.png") || len(msg.Media) != 1 {
		t.Fatalf("user message = %+v, want a placeholder naming the file", msg)
	}
}

// TestPageCarriesTheMediaCapability verifies the page is told what it may attach:
// the capability flag, the accepted types and the size cap, with every injected
// placeholder resolved. A run without the capability injects it as off.
func TestPageCarriesTheMediaCapability(t *testing.T) {
	srv, _ := newMediaTestServer(t, []string{"image/png", "image/jpeg"}, 1024)
	resp, err := http.Get(baseURL(srv) + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	page := string(body)
	for _, want := range []string{
		`"enabled":true`,
		`"accept":"image/png,image/jpeg"`,
		`"max_bytes":1024`,
		`id="attach"`,
		`id="attachFile"`,
		`id="attachments"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
	if strings.Contains(page, "__LIGHTAGENT_MEDIA__") {
		t.Fatal("the media placeholder was not resolved")
	}

	plain := newTestServer(t, "")
	plainResp, err := http.Get(baseURL(plain) + "/")
	if err != nil {
		t.Fatal(err)
	}
	plainBody, _ := io.ReadAll(plainResp.Body)
	_ = plainResp.Body.Close()
	if !strings.Contains(string(plainBody), `"enabled":false`) {
		t.Fatal("a run without media types must inject the capability as off")
	}
}

// TestUploadNamingKeepsTheFileName verifies the stored id is the file's own name
// (it is what the model sees as the attachment's name), that a second upload of
// the same name gets a counter instead of overwriting the first, and that a name
// carrying a path is reduced to its base name.
func TestUploadNamingKeepsTheFileName(t *testing.T) {
	srv, _ := newMediaTestServer(t, []string{"image/png"}, 0)
	base := baseURL(srv)

	_, first := postUpload(t, base, "shot.png", pngBytes)
	if first["id"] != "shot.png" {
		t.Fatalf("id = %v, want the file's own name", first["id"])
	}
	_, second := postUpload(t, base, "shot.png", pngBytes)
	if second["id"] != "shot-2.png" {
		t.Fatalf("the second upload of one name got %v, want a counter", second["id"])
	}
	_, nested := postUpload(t, base, `..\evil/shot.png`, pngBytes)
	if nested["id"] != "shot-3.png" {
		t.Fatalf("a name carrying a path got %v, want its base name with a counter", nested["id"])
	}

	// A very long name is cut, and the cut does not split a multi-byte rune
	// (the extension is what the type sniffer reads, so it survives whole).
	long := strings.Repeat("图片", 60) + ".png"
	_, trimmed := postUpload(t, base, long, pngBytes)
	id, _ := trimmed["id"].(string)
	if !strings.HasPrefix(id, "图片") || !strings.HasSuffix(id, ".png") {
		t.Fatalf("trimmed id = %q, want a readable name keeping its extension", id)
	}
	if len(id) > maxUploadNameBytes || !utf8.ValidString(id) {
		t.Fatalf("trimmed id = %q (%d bytes), want a valid name of at most %d bytes", id, len(id), maxUploadNameBytes)
	}
}

// TestUserRowShowsTheAttachmentsOfItsMessage verifies the page is told which files
// a message brought along: the row names each of them and, for a file the mirror
// stores itself, carries the URL its picture is drawn from.
func TestUserRowShowsTheAttachmentsOfItsMessage(t *testing.T) {
	srv, uploads := newMediaTestServer(t, []string{"image/png"}, 0)
	_, payload := postUpload(t, baseURL(srv), "shot.png", pngBytes)
	id := payload["id"].(string)

	srv.handleClientMessage([]byte(`{"text":"what is this?","attachments":[` + quote(id) + `]}`))

	rows := waitForHistory(t, srv, func(rows []historyRow) bool {
		return len(rows) > 0 && rows[0].Role == "user"
	})
	row := rows[0]
	if row.Content != "what is this?" {
		t.Fatalf("row = %+v, want the user message", row)
	}
	if len(row.Attachments) != 1 {
		t.Fatalf("attachments = %+v, want the file the message carried", row.Attachments)
	}
	att := row.Attachments[0]
	if att.Name != "shot.png" || att.Type != "image/png" {
		t.Errorf("attachment = %+v, want the uploaded file", att)
	}
	if want := mediaPath + "?id=" + id; att.URL != want {
		t.Errorf("url = %q, want %q", att.URL, want)
	}
	// The stored file is what the URL serves, so the page can draw the picture.
	if _, err := os.Stat(filepath.Join(uploads, id)); err != nil {
		t.Fatalf("the attachment is not where the URL points: %v", err)
	}
}

// TestMediaEndpointServesStoredUploads verifies a row's picture can be fetched
// back: the endpoint answers with the stored file itself, and refuses anything
// that is not a file of the upload directory (or that is gone).
func TestMediaEndpointServesStoredUploads(t *testing.T) {
	srv, uploads := newMediaTestServer(t, []string{"image/png"}, 0)
	base := baseURL(srv)
	_, payload := postUpload(t, base, "shot.png", pngBytes)
	id := payload["id"].(string)

	resp, err := http.Get(base + mediaURL(id))
	if err != nil {
		t.Fatalf("GET %s: %v", mediaURL(id), err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, body)
	}
	if !bytes.Equal(body, pngBytes) {
		t.Fatalf("body = %d bytes, want the stored file (%d)", len(body), len(pngBytes))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Fatalf("Content-Type = %q, want the file's own type", ct)
	}

	// A name the upload directory does not hold, a path, and an empty id.
	cases := []struct {
		id     string
		status int
	}{
		{"nope.png", http.StatusNotFound},
		{"../session.json", http.StatusBadRequest},
		{"", http.StatusBadRequest},
	}
	for _, tc := range cases {
		endpoint := mediaPath + "?id=" + url.QueryEscape(tc.id)
		resp, err := http.Get(base + endpoint)
		if err != nil {
			t.Fatalf("GET %s: %v", endpoint, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Errorf("GET %s = %d, want %d", endpoint, resp.StatusCode, tc.status)
		}
	}

	// A name that is a directory (a folder someone dropped in the upload
	// directory by hand) is not a file to serve.
	if err := os.Mkdir(filepath.Join(uploads, "folder"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	resp, err = http.Get(base + mediaPath + "?id=folder")
	if err != nil {
		t.Fatalf("GET %s?id=folder: %v", mediaPath, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d for a directory, want 404", resp.StatusCode)
	}

	// A file that was dropped (the user cancelled the attachment) stops being
	// served, which is what makes the page fall back to the file's name.
	if err := os.Remove(filepath.Join(uploads, id)); err != nil {
		t.Fatalf("remove upload: %v", err)
	}
	resp, err = http.Get(base + mediaURL(id))
	if err != nil {
		t.Fatalf("GET %s: %v", mediaURL(id), err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d after the file was dropped, want 404", resp.StatusCode)
	}
}

// TestMediaEndpointNeedsTheCapability verifies the endpoint stays closed in a run
// without attachments: no media types means no upload directory and nothing to
// serve.
func TestMediaEndpointNeedsTheCapability(t *testing.T) {
	srv := newTestServer(t, "")
	resp, err := http.Get(baseURL(srv) + mediaPath + "?id=shot.png")
	if err != nil {
		t.Fatalf("GET %s: %v", mediaPath, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestPageDrawsTheFilesOfAMessage pins the page side of an attachment: a row that
// carried files draws them under its text — the picture itself when the mirror
// serves it, a chip with the file's name otherwise (a type the browser cannot
// render, a file the mirror does not store, or a picture that cannot be loaded
// any more).
func TestPageDrawsTheFilesOfAMessage(t *testing.T) {
	page := pageSource()
	for _, want := range []string{
		"function mediaList(attachments)",
		"function mediaItem(item)",
		"function mediaChip(item)",
		"function isImageAttachment(item)",
		// A picture the URL does not answer for falls back to naming the file.
		"img.onerror = function ()",
		// Both row paths carry the files: the replayed row and the live event.
		"render('user', { text: m.content, attachments: m.attachments })",
		"addRow('user', 'you', ev.text || '', false, ev.attachments)",
		".row .attachments .media-image img",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
}
