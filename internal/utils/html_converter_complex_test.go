package utils

import (
	"strings"
	"testing"
)

// This file holds complex integration-style tests for Html2MdConvert that
// exercise the heuristic / modern-component handling paths which are not
// covered by the basic tests in html_converter_test.go.

// ---------------------------------------------------------------------------
// Lists: nesting, ordering, start attribute, and task / checkbox lists
// ---------------------------------------------------------------------------

func TestComplex_NestedUnorderedList(t *testing.T) {
	res := convert(t, "<ul><li>one<ul><li>two</li><li>three</li></ul></li></ul>")
	if !strings.Contains(res.Markdown, "- one") {
		t.Fatalf("missing top item: %q", res.Markdown)
	}
	// Nested items must be indented by two spaces per level.
	if !strings.Contains(res.Markdown, "  - two") || !strings.Contains(res.Markdown, "  - three") {
		t.Fatalf("missing indented nested items: %q", res.Markdown)
	}
}

func TestComplex_NestedOrderedList(t *testing.T) {
	res := convert(t, "<ol><li>first<ol><li>sub</li></ol></li><li>second</li></ol>")
	if !strings.Contains(res.Markdown, "1. first") {
		t.Fatalf("missing '1. first': %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "2. second") {
		t.Fatalf("missing '2. second': %q", res.Markdown)
	}
	// The nested ordered list resets its own counter and is indented.
	if !strings.Contains(res.Markdown, "  1. sub") {
		t.Fatalf("missing indented nested ordered item: %q", res.Markdown)
	}
}

func TestComplex_OrderedListStartAttribute(t *testing.T) {
	res := convert(t, `<ol start="3"><li>a</li><li>b</li></ol>`)
	if !strings.Contains(res.Markdown, "3. a") || !strings.Contains(res.Markdown, "4. b") {
		t.Fatalf("start attribute not honoured: %q", res.Markdown)
	}
}

func TestComplex_TaskListChecked(t *testing.T) {
	res := convert(t, `<ul><li><input type="checkbox" checked> done</li><li><input type="checkbox"> todo</li></ul>`)
	if !strings.Contains(res.Markdown, "- [x] done") {
		t.Fatalf("missing checked task: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "- [ ] todo") {
		t.Fatalf("missing unchecked task: %q", res.Markdown)
	}
}

func TestComplex_TaskListDataChecked(t *testing.T) {
	res := convert(t, `<ul><li data-checked="true">yes</li><li data-checked="false">no</li></ul>`)
	if !strings.Contains(res.Markdown, "- [x] yes") {
		t.Fatalf("missing data-checked true task: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "- [ ] no") {
		t.Fatalf("missing data-checked false task: %q", res.Markdown)
	}
}

// ---------------------------------------------------------------------------
// Code blocks: language sniffing and fence escaping
// ---------------------------------------------------------------------------

func TestComplex_CodeBlockLanguageSniffing(t *testing.T) {
	cases := []struct {
		html string
		want string
	}{
		{`<pre><code class="language-go">func main() {}</code></pre>`, "```go"},
		{`<pre data-lang="python"><code>print(1)</code></pre>`, "```python"},
		{`<pre><code data-language="rust">fn main() {}</code></pre>`, "```rust"},
		{`<pre><code data-code-language="c++">int main() {}</code></pre>`, "```c++"},
		{`<pre><code class="highlight-source-java">int x;</code></pre>`, "```java"},
	}
	for _, c := range cases {
		res := convert(t, c.html)
		if !strings.Contains(res.Markdown, c.want) {
			t.Fatalf("expected fence %q in output, got %q", c.want, res.Markdown)
		}
	}
}

func TestComplex_CodeBlockFenceEscaping(t *testing.T) {
	// The code body itself contains a triple backtick, so the fence must grow.
	res := convert(t, "<pre><code>line1\n```\nline2</code></pre>")
	if !strings.Contains(res.Markdown, "````") {
		t.Fatalf("expected fence to be escaped to 4 backticks, got %q", res.Markdown)
	}
}

func TestComplex_CustomCodeFence(t *testing.T) {
	res := convert(t, "<pre><code>code</code></pre>", WithCodeFence("~~~"))
	if !strings.Contains(res.Markdown, "~~~") {
		t.Fatalf("expected custom fence, got %q", res.Markdown)
	}
}

// ---------------------------------------------------------------------------
// Math: KaTeX / MathJax / MathML, inline vs block
// ---------------------------------------------------------------------------

func TestComplex_MathKaTeXInline(t *testing.T) {
	res := convert(t, `<span class="katex" data-tex="E = mc^2"></span>`)
	if !strings.Contains(res.Markdown, "$E = mc^2$") {
		t.Fatalf("missing inline math: %q", res.Markdown)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "Math formula extracted") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected math warning, got %v", res.Warnings)
	}
}

func TestComplex_MathKaTeXBlock(t *testing.T) {
	res := convert(t, `<span class="katex katex-display" data-tex="x^2"></span>`)
	if !strings.Contains(res.Markdown, "$$\nx^2\n$$") {
		t.Fatalf("missing block math: %q", res.Markdown)
	}
}

func TestComplex_MathMathMLAnnotation(t *testing.T) {
	res := convert(t, `<math><annotation encoding="application/x-tex">a+b</annotation></math>`)
	if !strings.Contains(res.Markdown, "$a+b$") {
		t.Fatalf("missing mathml inline math: %q", res.Markdown)
	}
}

func TestComplex_MathMathJax(t *testing.T) {
	res := convert(t, `<span class="MathJax" data-tex="a+b"></span>`)
	if !strings.Contains(res.Markdown, "$a+b$") {
		t.Fatalf("missing mathjax math: %q", res.Markdown)
	}
}

// ---------------------------------------------------------------------------
// Callouts / Admonitions
// ---------------------------------------------------------------------------

func TestComplex_CalloutTypes(t *testing.T) {
	cases := []struct {
		class string
		want  string
	}{
		{"callout tip", "> [!TIP]"},
		{"admonition warning", "> [!WARNING]"},
		{"alert danger", "> [!CAUTION]"},
		{"admonition important", "> [!IMPORTANT]"},
		{"callout", "> [!NOTE]"},
	}
	for _, c := range cases {
		res := convert(t, `<div class="`+c.class+`"><p>msg</p></div>`)
		if !strings.Contains(res.Markdown, c.want) {
			t.Fatalf("expected %q for class %q, got %q", c.want, c.class, res.Markdown)
		}
	}
}

func TestComplex_CalloutSkipsTypeHeadingLine(t *testing.T) {
	// A leading line that merely repeats the type label is dropped.
	res := convert(t, `<div class="callout tip"><p>TIP</p><p>real body</p></div>`)
	if !strings.Contains(res.Markdown, "> real body") {
		t.Fatalf("missing callout body: %q", res.Markdown)
	}
}

// ---------------------------------------------------------------------------
// Breadcrumbs and ARIA lists
// ---------------------------------------------------------------------------

func TestComplex_BreadcrumbNav(t *testing.T) {
	res := convert(t, `<nav class="breadcrumb"><a href="/">Home</a><a href="/docs">Docs</a></nav>`)
	if !strings.Contains(res.Markdown, "Home > Docs") {
		t.Fatalf("breadcrumb not linearized: %q", res.Markdown)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "Breadcrumb") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected breadcrumb warning, got %v", res.Warnings)
	}
}

func TestComplex_ARIAList(t *testing.T) {
	res := convert(t, `<div role="list"><div role="listitem">alpha</div><div role="listitem">beta</div></div>`)
	if !strings.Contains(res.Markdown, "- alpha") || !strings.Contains(res.Markdown, "- beta") {
		t.Fatalf("ARIA list not converted: %q", res.Markdown)
	}
}

// ---------------------------------------------------------------------------
// Tables: alignment, colspan, escaping, ARIA tables
// ---------------------------------------------------------------------------

func TestComplex_TableColumnAlignment(t *testing.T) {
	res := convert(t, `<table><tr><th style="text-align:center">A</th><th align="right">B</th><th>C</th></tr><tr><td>1</td><td>2</td><td>3</td></tr></table>`)
	// center -> :---:, right -> ---:
	if !strings.Contains(res.Markdown, ":---:") {
		t.Fatalf("missing center alignment marker: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "---:") {
		t.Fatalf("missing right alignment marker: %q", res.Markdown)
	}
}

func TestComplex_TableColspan(t *testing.T) {
	res := convert(t, `<table><tr><th colspan="2">Span</th><th>C</th></tr><tr><td>1</td><td>2</td><td>3</td></tr></table>`)
	if !strings.Contains(res.Markdown, "Span") || !strings.Contains(res.Markdown, "3") {
		t.Fatalf("colspan table malformed: %q", res.Markdown)
	}
}

func TestComplex_TablePipeEscaping(t *testing.T) {
	res := convert(t, "<table><tr><td>a|b</td></tr></table>")
	if !strings.Contains(res.Markdown, `a\|b`) {
		t.Fatalf("pipe not escaped: %q", res.Markdown)
	}
}

func TestComplex_ARIAStubTable(t *testing.T) {
	res := convert(t, `<div role="table"><div role="row"><div role="columnheader">H</div><div role="cell">c</div></div></div>`)
	if !strings.Contains(res.Markdown, "H") || !strings.Contains(res.Markdown, "c") {
		t.Fatalf("ARIA table not rendered: %q", res.Markdown)
	}
}

// ---------------------------------------------------------------------------
// Modern layout heuristics: grid -> table and flex key/value
// ---------------------------------------------------------------------------

func TestComplex_GridToTable(t *testing.T) {
	res := convert(t, `<div class="grid grid-cols-2"><span>a</span><span>b</span><span>c</span><span>d</span></div>`)
	if !strings.Contains(res.Markdown, "|") {
		t.Fatalf("grid not converted to table: %q", res.Markdown)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "Grid (2 cols) -> table") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected grid->table warning, got %v", res.Warnings)
	}
}

func TestComplex_FlexKeyValueRow(t *testing.T) {
	res := convert(t, `<div class="flex"><span>Name</span><span>Value</span></div>`)
	if !strings.Contains(res.Markdown, "- **Name**: Value") {
		t.Fatalf("flex key/value not converted: %q", res.Markdown)
	}
}

func TestComplex_MultiColumnCards(t *testing.T) {
	res := convert(t, `<div class="grid grid-cols-2"><div><p>card1</p></div><div><p>card2</p></div></div>`)
	if !strings.Contains(res.Markdown, "card1") || !strings.Contains(res.Markdown, "card2") {
		t.Fatalf("multi-column cards lost content: %q", res.Markdown)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "Multi-col") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected multi-col warning, got %v", res.Warnings)
	}
}

// ---------------------------------------------------------------------------
// URL sanitisation and safe schemes
// ---------------------------------------------------------------------------

func TestComplex_URLJavascriptFiltered(t *testing.T) {
	res := convert(t, `<a href="javascript:alert(1)">x</a>`)
	if !strings.Contains(res.Markdown, "x") {
		t.Fatalf("link text lost: %q", res.Markdown)
	}
	if strings.Contains(res.Markdown, "javascript:") {
		t.Fatalf("javascript URL leaked: %q", res.Markdown)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "Dangerous URL filtered") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected dangerous URL warning, got %v", res.Warnings)
	}
}

func TestComplex_URLVbscriptFiltered(t *testing.T) {
	res := convert(t, `<a href="vbscript:msgbox(1)">x</a>`)
	if strings.Contains(res.Markdown, "vbscript:") {
		t.Fatalf("vbscript URL leaked: %q", res.Markdown)
	}
}

func TestComplex_URLCustomSchemeFiltered(t *testing.T) {
	res := convert(t, `<a href="ftp://files.example.com/x">x</a>`)
	if strings.Contains(res.Markdown, "ftp://") {
		t.Fatalf("ftp URL leaked: %q", res.Markdown)
	}
}

func TestComplex_URLParenEscaping(t *testing.T) {
	res := convert(t, `<a href="http://example.com/a(b)">x</a>`)
	if !strings.Contains(res.Markdown, "%28") || !strings.Contains(res.Markdown, "%29") {
		t.Fatalf("parens not percent-encoded: %q", res.Markdown)
	}
}

func TestComplex_ImageDataURImageAllowed(t *testing.T) {
	res := convert(t, `<img src="data:image/png;base64,abc" alt="d">`)
	if !strings.Contains(res.Markdown, "data:image/png") {
		t.Fatalf("data:image not allowed: %q", res.Markdown)
	}
}

func TestComplex_ImageDataURTextBlocked(t *testing.T) {
	res := convert(t, `<img src="data:text/plain;base64,abc" alt="d">`)
	if strings.Contains(res.Markdown, "data:text/plain") {
		t.Fatalf("data:text image should be blocked: %q", res.Markdown)
	}
}

func TestComplex_ImageWithTitle(t *testing.T) {
	res := convert(t, `<img src="pic.png" alt="pic" title="A title">`)
	if !strings.Contains(res.Markdown, `![pic](pic.png "A title")`) {
		t.Fatalf("image title not rendered: %q", res.Markdown)
	}
}

func TestComplex_MediaFallbackLabel(t *testing.T) {
	res := convert(t, `<video src="movie.mp4" aria-label="My Video"></video>`)
	if !strings.Contains(res.Markdown, "[VIDEO: My Video](movie.mp4)") {
		t.Fatalf("media fallback not rendered: %q", res.Markdown)
	}
}

// ---------------------------------------------------------------------------
// Fragment wrapping of isolated table/list fragments
// ---------------------------------------------------------------------------

func TestComplex_FragmentTableRowWrapped(t *testing.T) {
	res := convert(t, "<tr><td>cell</td></tr>")
	if !strings.Contains(res.Markdown, "cell") {
		t.Fatalf("fragment cell lost: %q", res.Markdown)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "Fragment <tr> wrapped") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected tr wrap warning, got %v", res.Warnings)
	}
}

func TestComplex_FragmentLiWrapped(t *testing.T) {
	res := convert(t, "<li>item</li>")
	if !strings.Contains(res.Markdown, "- item") {
		t.Fatalf("li fragment not wrapped into list: %q", res.Markdown)
	}
}

func TestComplex_FragmentTheadWrapped(t *testing.T) {
	res := convert(t, "<thead><tr><th>H</th></tr></thead>")
	if !strings.Contains(res.Markdown, "H") {
		t.Fatalf("thead fragment lost header: %q", res.Markdown)
	}
}

// ---------------------------------------------------------------------------
// Warnings: aggregation and recursion depth limit
// ---------------------------------------------------------------------------

func TestComplex_WarningAggregation(t *testing.T) {
	res := convert(t, "<customtag>a</customtag><customtag>b</customtag>")
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "Unknown <customtag> x2") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected aggregated unknown tag warning, got %v", res.Warnings)
	}
}

func TestComplex_RecursionDepthLimit(t *testing.T) {
	inner := strings.Repeat("<div>", 300) + "deep" + strings.Repeat("</div>", 300)
	res := convert(t, inner)
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "Depth limit") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected depth limit warning, got %v", res.Warnings)
	}
}

// ---------------------------------------------------------------------------
// Text normalisation and inline elements
// ---------------------------------------------------------------------------

func TestComplex_WhitespaceCollapse(t *testing.T) {
	res := convert(t, "<p>a&nbsp;&nbsp;&nbsp;b</p>")
	if !strings.Contains(res.Markdown, "a b") {
		t.Fatalf("nbsp not collapsed: %q", res.Markdown)
	}
	if strings.Contains(res.Markdown, "a   b") {
		t.Fatalf("multiple spaces not collapsed: %q", res.Markdown)
	}
}

func TestComplex_ZeroWidthSpaceStripped(t *testing.T) {
	res := convert(t, "<p>a\u200Bb</p>")
	if !strings.Contains(res.Markdown, "ab") {
		t.Fatalf("zero-width space not stripped: %q", res.Markdown)
	}
}

func TestComplex_LineBreakHardBreak(t *testing.T) {
	res := convert(t, "<p>line1<br>line2</p>")
	if !strings.Contains(res.Markdown, "line1  \nline2") {
		t.Fatalf("hard line break not produced: %q", res.Markdown)
	}
}

func TestComplex_InlineMarkDelKbd(t *testing.T) {
	res := convert(t, "<p><mark>hi</mark> <del>gone</del> <kbd>Ctrl</kbd></p>")
	if !strings.Contains(res.Markdown, "==hi==") {
		t.Fatalf("mark not rendered: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "~~gone~~") {
		t.Fatalf("del not rendered: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "<kbd>Ctrl</kbd>") {
		t.Fatalf("kbd not rendered: %q", res.Markdown)
	}
}

func TestComplex_AbbreviationTitle(t *testing.T) {
	res := convert(t, `<p><abbr title="HyperText">HT</abbr></p>`)
	if !strings.Contains(res.Markdown, "HT (HyperText)") {
		t.Fatalf("abbr title not expanded: %q", res.Markdown)
	}
}

func TestComplex_DefinitionList(t *testing.T) {
	res := convert(t, "<dl><dt>Term</dt><dd>Def</dd></dl>")
	if !strings.Contains(res.Markdown, "**Term**") {
		t.Fatalf("dt not bolded: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, ": Def") {
		t.Fatalf("dd not rendered: %q", res.Markdown)
	}
}

func TestComplex_Figcaption(t *testing.T) {
	res := convert(t, `<figure><img src="a.png" alt="x"><figcaption>Caption</figcaption></figure>`)
	if !strings.Contains(res.Markdown, "![x](a.png)") {
		t.Fatalf("figure image lost: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "*Caption*") {
		t.Fatalf("figcaption not italicised: %q", res.Markdown)
	}
}

func TestComplex_TextareaToCodeBlock(t *testing.T) {
	res := convert(t, "<textarea>some text</textarea>")
	if !strings.Contains(res.Markdown, "```") || !strings.Contains(res.Markdown, "some text") {
		t.Fatalf("textarea not fenced: %q", res.Markdown)
	}
}

func TestComplex_InputTextToInlineCode(t *testing.T) {
	res := convert(t, `<input type="text" value="hello">`)
	if !strings.Contains(res.Markdown, "`hello`") {
		t.Fatalf("input value not rendered as inline code: %q", res.Markdown)
	}
}

// ---------------------------------------------------------------------------
// Heading anchor stripping
// ---------------------------------------------------------------------------

func TestComplex_HeadingAnchorClassStripped(t *testing.T) {
	res := convert(t, `<h1>Title <a class="header-anchor" href="#title">#</a></h1>`)
	if !strings.Contains(res.Markdown, "# Title") {
		t.Fatalf("heading lost: %q", res.Markdown)
	}
	if strings.Contains(res.Markdown, "Title #") {
		t.Fatalf("anchor text not stripped: %q", res.Markdown)
	}
}

func TestComplex_HeadingAnchorGlyphStripped(t *testing.T) {
	res := convert(t, `<h2>Sec <a href="#sec">¶</a></h2>`)
	if !strings.Contains(res.Markdown, "## Sec") {
		t.Fatalf("heading lost: %q", res.Markdown)
	}
	if strings.Contains(res.Markdown, "¶") {
		t.Fatalf("anchor glyph not stripped: %q", res.Markdown)
	}
}


// ---------------------------------------------------------------------------
// Anchor handling: links must stay on a single line even when the anchor
// wraps block-level content
// ---------------------------------------------------------------------------

func TestComplex_AnchorInlineStaysSingleLine(t *testing.T) {
	res := convert(t, `<a href="https://example.com">plain text</a>`)
	if !strings.Contains(res.Markdown, "[plain text](https://example.com)") {
		t.Fatalf("inline link malformed: %q", res.Markdown)
	}
	// The link must not be split across lines.
	if strings.Contains(res.Markdown, "\n]") {
		t.Fatalf("link split across lines: %q", res.Markdown)
	}
}

func TestComplex_AnchorWithBrCollapsesNewline(t *testing.T) {
	res := convert(t, `<a href="https://example.com">line1<br>line2</a>`)
	if !strings.Contains(res.Markdown, "[line1 line2](https://example.com)") {
		t.Fatalf("br not collapsed in link text: %q", res.Markdown)
	}
}

func TestComplex_AnchorHeadingWrapped(t *testing.T) {
	// <h2><a>...</a></h2> must keep the heading and link on one line.
	res := convert(t, `<h2><a href="https://example.com">Title</a></h2>`)
	if !strings.Contains(res.Markdown, "## [Title](https://example.com)") {
		t.Fatalf("heading link malformed: %q", res.Markdown)
	}
}

func TestComplex_AnchorWrapsHeadingAndParagraph(t *testing.T) {
	// The URL attaches to the heading at the top of the block; the paragraph
	// is rendered normally without the link.
	res := convert(t, `<a href="https://example.com"><h3>Head</h3><p>Body text</p></a>`)
	if !strings.Contains(res.Markdown, "### [Head](https://example.com)") {
		t.Fatalf("heading link missing: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "Body text") {
		t.Fatalf("paragraph lost: %q", res.Markdown)
	}
	// The paragraph must not be wrapped in the link.
	if strings.Contains(res.Markdown, "[Body text](https://example.com)") {
		t.Fatalf("paragraph should not be linked: %q", res.Markdown)
	}
}

func TestComplex_AnchorWrapsImage(t *testing.T) {
	// <a><img></a> must produce a valid image-inside-link, not an escaped or
	// broken image.
	res := convert(t, `<a href="https://example.com"><img src="pic.png" alt="pic"></a>`)
	if !strings.Contains(res.Markdown, "[![pic](pic.png)](https://example.com)") {
		t.Fatalf("image link malformed: %q", res.Markdown)
	}
	if strings.Contains(res.Markdown, `\[`) {
		t.Fatalf("image brackets were escaped: %q", res.Markdown)
	}
}

func TestComplex_AnchorEmptyAltImage(t *testing.T) {
	res := convert(t, `<a href="https://example.com"><img src="pic.png" alt=""></a>`)
	if !strings.Contains(res.Markdown, "[![](pic.png)](https://example.com)") {
		t.Fatalf("empty-alt image link malformed: %q", res.Markdown)
	}
}

// ---------------------------------------------------------------------------
// Inline elements must not introduce block-level line breaks
// ---------------------------------------------------------------------------

func TestComplex_ListItemInlineSpanStaysOnBulletLine(t *testing.T) {
	// <span> is inline: it must not split the bullet from the item content.
	// This mirrors the DuckDuckGo nav pattern
	// <li><button><span><strong>More</strong></span><svg></svg></button></li>.
	res := convert(t, `<ul><li><button><span><strong>more</strong></span></button></li></ul>`)
	if !strings.Contains(res.Markdown, "- **more**") {
		t.Fatalf("bullet split from content: %q", res.Markdown)
	}
	if strings.Contains(res.Markdown, "-\n") {
		t.Fatalf("unexpected newline after bullet: %q", res.Markdown)
	}
}

func TestComplex_InlineSpanInsideParagraphStaysInline(t *testing.T) {
	res := convert(t, "<p>a<span>b</span>c</p>")
	if !strings.Contains(res.Markdown, "abc") {
		t.Fatalf("inline span split the text: %q", res.Markdown)
	}
}


func TestComplex_AnchorWrapsHeadingInsideListItem(t *testing.T) {
	// Inside a list item the bullet provides the block structure, so the
	// heading marker is dropped and the bullet shares its line with the link.
	res := convert(t, `<ul><li><a href="https://example.com"><h3>Head</h3><p>Body text</p></a></li></ul>`)
	if !strings.Contains(res.Markdown, "- [Head](https://example.com)") {
		t.Fatalf("bullet and link not on one line: %q", res.Markdown)
	}
	if strings.Contains(res.Markdown, "-\n") {
		t.Fatalf("dangling bullet: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "Body text") {
		t.Fatalf("body lost: %q", res.Markdown)
	}
}


func TestComplex_OrderedListItemBulletStaysWithNestedBlocks(t *testing.T) {
	// A result item wraps its content in nested block elements; the bullet
	// must still share its line with the first piece of content.
	res := convert(t, `<ol><li><div><div><span><img src="i.png" alt=""></span></div></div><div><p>site</p></div></li></ol>`)
	if !strings.Contains(res.Markdown, "1. ![](i.png)") {
		t.Fatalf("bullet dangling from nested block: %q", res.Markdown)
	}
	if strings.Contains(res.Markdown, "1.\n") {
		t.Fatalf("dangling bullet: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "site") {
		t.Fatalf("item body lost: %q", res.Markdown)
	}
}

func TestComplex_UnorderedListItemBulletStaysWithBlockChild(t *testing.T) {
	res := convert(t, `<ul><li><div>text</div></li></ul>`)
	if !strings.Contains(res.Markdown, "- text") {
		t.Fatalf("bullet dangling from block child: %q", res.Markdown)
	}
}

