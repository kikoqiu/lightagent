package web

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestCopyMenuIsServed checks the copy menu reaches the browser with the page:
// it is part of the skeleton, so a page built by the server must carry it and
// the three flavours a message can leave as.
func TestCopyMenuIsServed(t *testing.T) {
	srv := newTestServer(t, "")
	resp, err := http.Get(baseURL(srv) + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	page, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read /: %v", readErr)
	}
	for _, want := range []string{
		`id="copyPop"`,
		`data-copy="markdown"`,
		`data-copy="html"`,
		`data-copy="text"`,
	} {
		if !strings.Contains(string(page), want) {
			t.Errorf("the served page is missing %q", want)
		}
	}
}

// TestCopyControlOnMessageRows pins the copy feature: every user message, every
// agent reply and the compressed-context summary carries a copy control, the menu
// offers markdown, HTML and plain text, and the row's source text is remembered so
// the markdown flavour is the message as it was written rather than what the
// current rendering shows. The summary is the only copy of the messages that were
// cut out of the context, so it has to be copyable too.
func TestCopyControlOnMessageRows(t *testing.T) {
	src := pageSource()
	for _, want := range []string{
		// The row kinds that carry the control, and the hook that adds it.
		"function copyableRow(cls)",
		"return cls === 'user' || cls === 'user pending' || cls === 'assistant' || cls === 'summary';",
		"if (copyableRow(cls)) {",
		"(label || row).appendChild(copyControl(row))",
		// The source text of a row, kept for the markdown flavour.
		"var rowSource = new WeakMap()",
		"rowSource.set(span, text)",
		"rowSource.get(span)",
		// The control opens the shared menu on its own row.
		"function copyControl(row)",
		"btn.setAttribute('aria-haspopup', 'menu')",
		"openCopyMenu(row, btn)",
		"function placeCopyMenu(btn)",
		// The flavours: markdown as written, the rendered HTML, the plain text.
		"function copyRow(row, mode, item)",
		"function rowHTML(span, source)",
		"function rowPlainText(span, source)",
		"item.getAttribute('data-copy')",
		// The menu closes when the row moves out from under it, like the modals.
		"copyPop.contains(e.target)",
		"(e.key === 'Escape' || e.keyCode === 27)",
		"log.addEventListener('scroll', closeCopyMenu, { passive: true })",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the copy feature is missing %q", want)
		}
	}
}

// TestCopyWorksWithoutTheClipboardAPI pins the two sides of the write. The async
// Clipboard API only exists in a secure context, and the mirror is usually
// reached over plain http on a LAN address, so the selection path (a hidden
// textarea, its selection copied through execCommand, the HTML flavour riding on
// the copy event) is what a phone actually takes; it must stay.
func TestCopyWorksWithoutTheClipboardAPI(t *testing.T) {
	src := pageSource()
	for _, want := range []string{
		"function writeClipboard(text, html)",
		"window.isSecureContext && api && api.write && typeof ClipboardItem === 'function'",
		"function legacyCopy(text, html)",
		"document.execCommand('copy')",
		"e.clipboardData.setData('text/plain', text)",
		"e.clipboardData.setData('text/html', html)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the clipboard write is missing %q", want)
		}
	}
	if strings.Contains(src, "position:fixed") {
		t.Error("the clipboard helper must not use fixed positioning: the page keeps to absolute overlays")
	}
	// The helper must not live inside an editable field of the page, and its
	// holder is a textarea because iOS Safari only copies out of a form field.
	if !strings.Contains(src, "holder.setSelectionRange(0, text.length)") {
		t.Error("the fallback should select the whole holder, which is what iOS Safari copies")
	}
}

// TestCopyMarkIsTemporary pins the green entry. It says "this is the flavour you
// just copied" and must not outlive the menu that copied it: otherwise the next
// message's menu would open with one entry still green, which reads as the state
// of the page rather than as a remark about a copy that is over.
func TestCopyMarkIsTemporary(t *testing.T) {
	src := pageSource()
	for _, want := range []string{
		"function clearCopyMarks()",
		"var done = copyPop.querySelectorAll('button.done');",
		"done[i].classList.remove('done')",
		// The mark itself is still what a successful copy sets.
		"if (item && item.classList) { item.classList.add('done'); }",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the copied-flavour mark is missing %q", want)
		}
	}
	// It has to be cleared again both when a menu opens (the entries always start
	// in their idle colour) and when it closes.
	if got := strings.Count(src, "clearCopyMarks();"); got < 2 {
		t.Errorf("clearCopyMarks() is called %d times, want it on open and on close", got)
	}
}
