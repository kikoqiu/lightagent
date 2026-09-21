package utils

import (
	"errors"
	"strings"
	"testing"
)

// convert is a small helper that runs the converter and fails the test on error.
func convert(t *testing.T, html string, opts ...Html2MdOptionFunc) Html2MdConvertResult {
	t.Helper()
	res, err := Html2MdConvert(html, opts...)
	if err != nil {
		t.Fatalf("Html2MdConvert(%q) returned error: %v", html, err)
	}
	return res
}

func TestHtml2MdConvert_Empty(t *testing.T) {
	res := convert(t, "")
	if res.Markdown != "" {
		t.Fatalf("expected empty markdown, got %q", res.Markdown)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", res.Warnings)
	}
}

func TestHtml2MdConvert_Headings(t *testing.T) {
	res := convert(t, "<h1>Top</h1><h2>Second</h2><h3>Third</h3>")
	for _, want := range []string{"# Top", "## Second", "### Third"} {
		if !strings.Contains(res.Markdown, want) {
			t.Fatalf("missing %q in output: %q", want, res.Markdown)
		}
	}
}

func TestHtml2MdConvert_Paragraphs(t *testing.T) {
	res := convert(t, "<p>Hello world.</p><p>Second para.</p>")
	if !strings.Contains(res.Markdown, "Hello world.") {
		t.Fatalf("missing first paragraph: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "Second para.") {
		t.Fatalf("missing second paragraph: %q", res.Markdown)
	}
}

func TestHtml2MdConvert_InlineEmphasis(t *testing.T) {
	res := convert(t, "<p>Some <strong>bold</strong> and <em>italic</em> text</p>")
	if !strings.Contains(res.Markdown, "**bold**") {
		t.Fatalf("missing bold marker: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "*italic*") {
		t.Fatalf("missing italic marker: %q", res.Markdown)
	}
}

func TestHtml2MdConvert_ShortQuotation(t *testing.T) {
	res := convert(t, `<p>He said <q>hello</q> once</p>`)
	if !strings.Contains(res.Markdown, `"hello"`) {
		t.Fatalf("expected plain double quotes around the quotation: %q", res.Markdown)
	}
	if strings.Contains(res.Markdown, "\u201c") || strings.Contains(res.Markdown, "\u201d") {
		t.Fatalf("the curved quotation marks of a page must not be carried over: %q", res.Markdown)
	}
}

func TestHtml2MdConvert_UnorderedList(t *testing.T) {
	res := convert(t, "<ul><li>one</li><li>two</li><li>three</li></ul>")
	for _, want := range []string{"- one", "- two", "- three"} {
		if !strings.Contains(res.Markdown, want) {
			t.Fatalf("missing list item %q in output: %q", want, res.Markdown)
		}
	}
}

func TestHtml2MdConvert_OrderedList(t *testing.T) {
	res := convert(t, "<ol><li>first</li><li>second</li></ol>")
	for _, want := range []string{"1. first", "2. second"} {
		if !strings.Contains(res.Markdown, want) {
			t.Fatalf("missing ordered item %q in output: %q", want, res.Markdown)
		}
	}
}

func TestHtml2MdConvert_Link(t *testing.T) {
	res := convert(t, `<p><a href="https://example.com">Example</a></p>`)
	if !strings.Contains(res.Markdown, "Example") {
		t.Fatalf("missing link text: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "https://example.com") {
		t.Fatalf("missing link url: %q", res.Markdown)
	}
}

func TestHtml2MdConvert_RelativeLinkWithBaseURL(t *testing.T) {
	res := convert(t, `<a href="/docs/page">Docs</a>`, WithBaseURL("https://example.com/base"))
	if !strings.Contains(res.Markdown, "https://example.com/docs/page") {
		t.Fatalf("relative link not resolved against base url: %q", res.Markdown)
	}
}

func TestHtml2MdConvert_InvalidBaseURL(t *testing.T) {
	res := convert(t, `<a href="x">link</a>`, WithBaseURL("://bad url"))
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "Invalid BaseURL") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected Invalid BaseURL warning, got %v", res.Warnings)
	}
}

func TestHtml2MdConvert_InlineCode(t *testing.T) {
	res := convert(t, "<p>Use <code>fmt.Println</code> here</p>")
	if !strings.Contains(res.Markdown, "`fmt.Println`") {
		t.Fatalf("missing inline code: %q", res.Markdown)
	}
}

func TestHtml2MdConvert_CodeBlock(t *testing.T) {
	res := convert(t, "<pre><code class=\"language-go\">func main() {}\n</code></pre>")
	if !strings.Contains(res.Markdown, "func main() {}") {
		t.Fatalf("missing code content: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "```go") {
		t.Fatalf("missing code fence language: %q", res.Markdown)
	}
}

func TestHtml2MdConvert_Blockquote(t *testing.T) {
	res := convert(t, "<blockquote><p>quoted</p></blockquote>")
	if !strings.Contains(res.Markdown, "> quoted") {
		t.Fatalf("missing blockquote marker: %q", res.Markdown)
	}
}

func TestHtml2MdConvert_HorizontalRule(t *testing.T) {
	res := convert(t, "<p>a</p><hr><p>b</p>")
	if !strings.Contains(res.Markdown, "---") {
		t.Fatalf("missing horizontal rule: %q", res.Markdown)
	}
}

func TestHtml2MdConvert_Image(t *testing.T) {
	res := convert(t, `<img src="pic.png" alt="A picture">`)
	if !strings.Contains(res.Markdown, "A picture") {
		t.Fatalf("missing image alt: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "pic.png") {
		t.Fatalf("missing image src: %q", res.Markdown)
	}
}

func TestHtml2MdConvert_Table(t *testing.T) {
	res := convert(t, "<table><tr><th>Head</th></tr><tr><td>cell</td></tr></table>")
	if !strings.Contains(res.Markdown, "Head") {
		t.Fatalf("missing table header: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "cell") {
		t.Fatalf("missing table cell: %q", res.Markdown)
	}
}

func TestHtml2MdConvert_DetailsPreserved(t *testing.T) {
	res := convert(t, "<details><summary>More</summary><p>body</p></details>")
	if !strings.Contains(res.Markdown, "<details>") {
		t.Fatalf("expected preserved <details> tag: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "<summary>") {
		t.Fatalf("expected preserved <summary> tag: %q", res.Markdown)
	}
}

func TestHtml2MdConvert_DetailsNotPreserved(t *testing.T) {
	res := convert(t, "<details><summary>More</summary><p>body</p></details>", WithPreserveDetails(false))
	if strings.Contains(res.Markdown, "<details>") {
		t.Fatalf("details tag should not be preserved: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "**More**") {
		t.Fatalf("expected summary rendered as bold: %q", res.Markdown)
	}
}

func TestHtml2MdConvert_CustomBulletMarker(t *testing.T) {
	res := convert(t, "<ul><li>a</li></ul>", WithBulletMarker("*"))
	if !strings.Contains(res.Markdown, "* a") {
		t.Fatalf("expected custom bullet marker: %q", res.Markdown)
	}
}

func TestHtml2MdConvertReader(t *testing.T) {
	res, err := Html2MdConvertReader(strings.NewReader("<p>from reader</p>"))
	if err != nil {
		t.Fatalf("Html2MdConvertReader error: %v", err)
	}
	if !strings.Contains(res.Markdown, "from reader") {
		t.Fatalf("missing content: %q", res.Markdown)
	}
}

func TestHtml2MdConvertReader_Error(t *testing.T) {
	r := errReader{}
	if _, err := Html2MdConvertReader(r); err == nil {
		t.Fatalf("expected error from failing reader")
	}
}

func TestWarningTracker_Summary(t *testing.T) {
	w := newWarningTracker()
	w.addTag("custom")
	w.addTag("custom")
	w.addMsg("Something happened")

	summary := w.summary()
	if len(summary) != 2 {
		t.Fatalf("expected 2 summary entries, got %v", summary)
	}
	joined := strings.Join(summary, "\n")
	if !strings.Contains(joined, "Unknown <custom> x2") {
		t.Fatalf("expected aggregated tag count, got %v", summary)
	}
	if !strings.Contains(joined, "Something happened") {
		t.Fatalf("expected message entry, got %v", summary)
	}
}

func TestWarningTracker_SummarySorted(t *testing.T) {
	w := newWarningTracker()
	w.addTag("zebra")
	w.addTag("alpha")
	w.addMsg("Message")

	summary := w.summary()
	if len(summary) != 3 {
		t.Fatalf("expected 3 entries, got %v", summary)
	}
	// summary() sorts results; "Message" sorts before "Unknown <...>" entries.
	if summary[0] != "Message" {
		t.Fatalf("expected sorted summary, got %v", summary)
	}
}

func TestHtml2MdConvert_UnknownTagWarning(t *testing.T) {
	res := convert(t, "<customtag>hello</customtag>")
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "customtag") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected unknown tag warning, got %v", res.Warnings)
	}
}

// errReader is an io.Reader that always returns an error.
type errReader struct{}

func (errReader) Read(p []byte) (int, error) { return 0, errors.New("read fail") }