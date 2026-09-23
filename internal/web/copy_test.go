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

// TestCopyControlOnMessageRows pins the copy feature: every user message and
// every agent reply carries a copy control, the menu offers markdown, HTML and
// plain text, and the row's source text is remembered so the markdown flavour is
// the message as it was written rather than what the current rendering shows.
func TestCopyControlOnMessageRows(t *testing.T) {
	src := pageSource()
	for _, want := range []string{
		// The two row kinds that carry the control, and the hook that adds it.
		"function copyableRow(cls)",
		"return cls === 'user' || cls === 'user pending' || cls === 'assistant';",
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

// TestCopyIconAppearsOnDemand pins how the control is shown, because the two
// layouts ask for different things: the icon is a transparent button tucked into
// the row's role line next to the label (right of "agent", left of "you", the
// same line height whatever the icon does), a desktop brings it up on hover and
// a touch screen — which has no hover — on a long press or a tap on the message.
func TestCopyIconAppearsOnDemand(t *testing.T) {
	src := pageSource()
	for _, want := range []string{
		// The icon lives in the role line without changing it.
		"if (label) { label.classList.add('with-actions'); }",
		"(label || row).appendChild(copyControl(row))",
		".role.with-actions { display:flex; align-items:center; }",
		".user .role.with-actions { justify-content:flex-end; }",
		"position:relative; flex:0 0 auto; width:0; height:0;",
		// ...mirrored so a user message keeps its icon on the message's side.
		".user .role .row-actions { order:-1; }",
		".user .role .row-actions button.copy { left:auto; right:6px; }",
		// Transparent, out of the way until it is wanted.
		"opacity:0; pointer-events:none; transform:scale(.8);",
		"background:transparent; color:var(--fg-dim); box-shadow:none;",
		"transform:translateY(-50%);",
		// Shown by a hover (desktop), the keyboard, or the long press below.
		".row:hover .row-actions,",
		".row:focus-within .row-actions,",
		".row.show-actions .row-actions { opacity:1; pointer-events:auto; transform:none; }",
		// The icon is a glyph, not a labelled button.
		"function copyGlyph()",
		"document.createElementNS(NS, 'svg')",
		"btn.appendChild(copyGlyph())",
		// A touch screen: long press (with drift cancelling it) or a tap.
		"var LONG_PRESS_MS = 450",
		"function onCopyTouchStart(e)",
		"function onCopyTouchEnd(e)",
		"log.addEventListener('touchstart', onCopyTouchStart, { passive: true })",
		"log.addEventListener('touchmove', onCopyTouchMove, { passive: true })",
		"log.addEventListener('touchend', onCopyTouchEnd)",
		"log.addEventListener('touchcancel', cancelLongPress, { passive: true })",
		"function revealCopyControl(row)",
		"function hideCopyControl()",
		"row.classList.add('show-actions')",
		"copyRowOf(e.target)",
		// The finger target grows on a phone while the glyph stays on the line.
		".row-actions button.copy { width:32px; height:32px; }",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the copy control is missing %q", want)
		}
	}
	for _, avoid := range []string{
		"btn.textContent = 'Copy'",     // the control carries an icon, not a word
		".row-actions { display:block", // ...and it is not part of the text flow
		"position:absolute; top:-4px;", // ...or a box pinned to the row's corner
		"<span>Markdown</span>",        // ...and the menu names its flavours in words
		"<span>HTML</span>",
		"<span>Text</span>",
	} {
		if strings.Contains(src, avoid) {
			t.Errorf("the copy control should no longer be %q", avoid)
		}
	}
}

// TestCopyMenuLooksAndArrives pins the menu itself: one shared panel of three
// named flavours (each with its own small glyph), translucent enough to keep the
// transcript visible behind it, and animated into place — including the
// reduced-motion escape hatch the rest of the page respects.
func TestCopyMenuLooksAndArrives(t *testing.T) {
	src := pageSource()
	for _, want := range []string{
		// The three flavours, each with its glyph (no words on the buttons: the
		// tooltip and the note after a copy name them).
		`data-copy="markdown"`,
		`data-copy="html"`,
		`data-copy="text"`,
		`aria-label="copy as markdown"`,
		`aria-label="copy as HTML"`,
		`aria-label="copy as plain text"`,
		`title="copy as markdown (the message as it was written)"`,
		// A translucent panel...
		"background:rgba(16,23,34,.72);",
		"backdrop-filter:blur(12px) saturate(150%);",
		// ...that rises into place, from the right side (it flips when there is
		// no room below the icon).
		"animation:copy-down .16s cubic-bezier(.2,.85,.3,1.25) both;",
		"@keyframes copy-down {",
		"@keyframes copy-up {",
		".copy-pop.flip { transform-origin:bottom center; animation-name:copy-up; }",
		"copyPop.classList.toggle('flip', flip);",
		// A reader who asked for less motion gets the panel without the rise.
		".copy-pop { animation:none; }",
		// The entries stay small: three icon buttons, not fat labelled pills.
		"width:30px; height:28px; min-height:0; padding:0; border:0; border-radius:8px;",
		// The outcome is one short word in a pill that floats beside the panel:
		// absolute, so its text can never widen the menu, and with the long
		// advice kept in its tooltip.
		"copyNote.textContent = 'copied';",
		"position:absolute; left:0; top:100%; margin-top:6px;",
		".copy-pop.flip .copy-note { top:auto; bottom:100%; margin:0 0 6px; }",
		"copyNote.title = 'select the text and press Ctrl+C';",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the copy menu is missing %q", want)
		}
	}
	for _, avoid := range []string{
		"COPY_FLAVORS",  // the note says "copied", not the flavour's name...
		"+ ' copied'",   // ...so it never grows with a longer word
		"flex:1 1 100%", // ...and it is not a flex item of the panel
	} {
		if strings.Contains(src, avoid) {
			t.Errorf("the copied-note should no longer use %q", avoid)
		}
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
