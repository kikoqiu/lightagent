package utils

import (
	"strings"
	"testing"
)

// TestHtml2MdConvert_RelativeLinks pins how a page links into its own site once
// the caller asks for relative references: an address of the site is written as
// the path an anchor inside the site would use, while other sites and other
// schemes keep their full address (and a bare fragment stays a fragment).
func TestHtml2MdConvert_RelativeLinks(t *testing.T) {
	page := `<h1>Guide</h1>` +
		`<p><a href="https://example.com/docs/other">Same site absolute</a>` +
		`<a href="/docs/page">Root relative</a>` +
		`<a href="page2.html">Sibling</a>` +
		`<a href="../up.html?x=1#top">Parent with query</a>` +
		`<a href="#section">Fragment</a>` +
		`<a href="https://other.example.org/x">Other site</a>` +
		`<a href="mailto:a@b.c">Mail</a></p>`

	res := convert(t, page, WithBaseURL("https://example.com/docs/guide"), WithRelativeLinks(true))
	for _, want := range []string{
		"[Same site absolute](/docs/other)",
		"[Root relative](/docs/page)",
		"[Sibling](/docs/page2.html)",
		"[Parent with query](/up.html?x=1#top)",
		"[Fragment](#section)",
		"[Other site](https://other.example.org/x)",
		"[Mail](mailto:a@b.c)",
	} {
		if !strings.Contains(res.Markdown, want) {
			t.Errorf("missing %q in output:\n%s", want, res.Markdown)
		}
	}
	if strings.Contains(res.Markdown, "https://example.com") {
		t.Errorf("a link into the page's own site must not repeat its address:\n%s", res.Markdown)
	}

	// Without the option the absolute form the base URL produces is kept, which
	// is what the CLI and every other caller gets.
	absolute := convert(t, page, WithBaseURL("https://example.com/docs/guide"))
	if !strings.Contains(absolute.Markdown, "https://example.com/docs/page") {
		t.Errorf("relative references must be off by default:\n%s", absolute.Markdown)
	}
}

// TestHtml2MdConvert_MainContentSelection pins the region a page's content is
// read from: the article inside the main element rather than the navigation,
// sidebar and footer around it, with the lines it occupies in the whole
// markdown reported so a caller can point into the saved page.
func TestHtml2MdConvert_MainContentSelection(t *testing.T) {
	page := `<header class="site-header"><nav><ul>` +
		`<li><a href="/">Home</a></li><li><a href="/docs">Docs</a></li></ul></nav></header>` +
		`<aside class="sidebar"><ul><li><a href="/a">Alpha</a></li></ul></aside>` +
		`<main><h1>Getting started</h1><article class="post-content">` +
		`<p>Install the tool and run it.</p>` +
		`<p>Then read the guide that comes with it.</p>` +
		`<p>The guide explains every setting of the tool, and it is worth reading end to end before the first run.</p>` +
		`<p>Afterwards the reference lists each tool, its arguments and what it does, one section per tool.</p>` +
		`</article></main>` +
		`<footer class="site-footer"><p>Copyright 2026 Example.</p></footer>`

	res := convert(t, page, WithMainContentSelection(true))
	if !res.Content.Located {
		t.Fatalf("no main content was located:\n%s", res.Markdown)
	}
	if res.Content.Label != "main" {
		t.Errorf("label = %q, want the main element: it only adds the title around the article", res.Content.Label)
	}
	for _, want := range []string{"Install the tool and run it.", "Then read the guide that comes with it."} {
		if !strings.Contains(res.Content.Markdown, want) {
			t.Errorf("the region is missing %q:\n%s", want, res.Content.Markdown)
		}
	}
	for _, unwanted := range []string{"Home", "Alpha", "Copyright"} {
		if strings.Contains(res.Content.Markdown, unwanted) {
			t.Errorf("the region must leave the chrome out, found %q:\n%s", unwanted, res.Content.Markdown)
		}
	}
	// The reported lines describe the markdown exactly: the first line of the
	// region is the heading of the article's main element.
	lines := strings.Split(res.Markdown, "\n")
	if res.Content.StartLine < 1 || res.Content.StartLine > len(lines) {
		t.Fatalf("start line %d is outside the %d lines of markdown", res.Content.StartLine, len(lines))
	}
	if got := lines[res.Content.StartLine-1]; got != "# Getting started" {
		t.Errorf("line %d = %q, want the start of the content", res.Content.StartLine, got)
	}
	if res.Content.LineCount != len(strings.Split(res.Content.Markdown, "\n")) {
		t.Errorf("line count %d does not match the region's %d lines",
			res.Content.LineCount, len(strings.Split(res.Content.Markdown, "\n")))
	}
	if strings.Join(lines[res.Content.StartLine-1:res.Content.StartLine-1+res.Content.LineCount], "\n") != res.Content.Markdown {
		t.Errorf("the region is not the markdown of the lines it reports:\n%s", res.Content.Markdown)
	}

	// Nothing is reported unless the selection is asked for.
	if plain := convert(t, page); plain.Content.Located {
		t.Errorf("a conversion that asked for no selection reported %+v", plain.Content)
	}
}

// TestHtml2MdConvert_MainContentNarrowedToTheCore pins the other half of the
// narrowing rule: a wrapper that holds a navigation of its own around the
// article is a shell, and the article is what is read.
func TestHtml2MdConvert_MainContentNarrowedToTheCore(t *testing.T) {
	page := `<main><div class="docs-nav"><ul>` +
		`<li><a href="/a">Alpha</a></li><li><a href="/b">Beta</a></li>` +
		`<li><a href="/c">Gamma</a></li><li><a href="/d">Delta</a></li>` +
		`<li><a href="/e">Epsilon</a></li><li><a href="/f">Zeta</a></li>` +
		`<li><a href="/g">Eta</a></li><li><a href="/h">Theta</a></li></ul></div>` +
		`<article class="post-content"><h1>Deep dive</h1>` +
		`<p>The article explains the tool, its settings and the way it reads a page.</p>` +
		`<p>It is long enough to be the content a reader came for, and long enough for the pick to trust it.</p>` +
		`<p>Every paragraph adds to the text the page's content is measured by.</p></article></main>`

	res := convert(t, page, WithMainContentSelection(true))
	if !res.Content.Located {
		t.Fatalf("no main content was located:\n%s", res.Markdown)
	}
	if res.Content.Label != "article.post-content" {
		t.Errorf("label = %q, want the article inside the shell", res.Content.Label)
	}
	if !strings.Contains(res.Content.Markdown, "# Deep dive") {
		t.Errorf("the region must hold the article:\n%s", res.Content.Markdown)
	}
	for _, unwanted := range []string{"Alpha", "Theta"} {
		if strings.Contains(res.Content.Markdown, unwanted) {
			t.Errorf("the region must leave the shell's navigation out, found %q:\n%s",
				unwanted, res.Content.Markdown)
		}
	}
}

// TestHtml2MdConvert_MainContentNotFound pins the other side of the selection:
// a page that only holds chrome, or whose content is a caption, leaves the
// region empty so the caller keeps its own way of cutting the markdown.
func TestHtml2MdConvert_MainContentNotFound(t *testing.T) {
	cases := []struct {
		name string
		page string
	}{
		{"chrome only", `<nav><ul><li><a href="/a">Alpha</a></li><li><a href="/b">Beta</a></li></ul></nav>` +
			`<footer><p>Copyright 2026 Example.</p></footer>`},
		{"content too small", `<main><article><p>Just a caption.</p></article></main>`},
		{"content named as chrome", `<div class="comment-content"><p>` +
			strings.Repeat("A comment that is long enough to look like content. ", 20) +
			`</p></div>`},
	}
	for _, tc := range cases {
		res := convert(t, tc.page, WithMainContentSelection(true))
		if res.Content.Located {
			t.Errorf("%s: located %+v, want no region", tc.name, res.Content)
		}
	}
}
