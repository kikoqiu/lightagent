package utils

import (
	"strings"
	"unicode/utf8"

	nethtml "golang.org/x/net/html"
)

// The heuristic that picks the main content of a page. A page is read from the
// outside in: it wraps its text in containers, and one of them is what the page
// is about — the article, the documentation body, the post — while the rest is
// the chrome around it (navigation, sidebars, banners, footers, comments).
//
// The pick is made in three steps: every element that declares itself to hold
// content becomes a candidate (a semantic tag, an ARIA role, or a class or id
// that names the content), the candidate with the most text wins, and the
// winner is narrowed down into the core it wraps when that core holds most of
// the text and the wrapper adds a real amount of its own around it (a <main>
// that wraps an article together with a navigation of its own is a shell; the
// article is what the page is about). A page that declares no content at all
// leaves the pick empty, and the caller falls back to its own way of cutting
// the markdown.

const (
	// The bonuses are what the markup of an element is worth, in runes, on top
	// of the text it holds: they decide between candidates of a similar size
	// (a <main> and a <div class="content"> of the same length), and lose to
	// any candidate that is markedly longer.
	contentBonusRole    = 500 // role="main": the page states it in ARIA
	contentBonusMain    = 450 // <main>
	contentBonusArticle = 400 // <article>
	contentBonusNamed   = 300 // a class or id that names the content

	// minContentRunes is how much text a main content region has to hold: a
	// page's content is not a caption.
	minContentRunes = 200
	// minContentShare is the share of the page's text a region has to hold: a
	// small block of a page that is mostly chrome is not its content.
	minContentShare = 0.02
	// innerContentShare is the share of an outer candidate's text an inner one
	// has to hold to be preferred to it (the shell is dropped for its core),
	// and shellExtraShare is how much text of its own the outer has to hold
	// beyond it: a wrapper that adds a title above an article is not a shell to
	// drop, since the title is part of what a reader wants, while one that adds
	// a navigation of its own is.
	innerContentShare = 0.6
	shellExtraShare   = 0.1
	// maxContentLinkDensity is the share of link text a candidate may hold and
	// still count as content: past it the element is a list of anchors.
	maxContentLinkDensity = 0.5
)

// contentContainerTags are the elements a page wraps its text in. A tag outside
// this set never becomes a candidate on a class or id name alone (<a>, <span>,
// <li>, ... are parts of a text, not containers of it).
var contentContainerTags = map[string]bool{
	"article": true,
	"main":    true,
	"section": true,
	"div":     true,
	"td":      true,
}

// contentSkipTags are elements whose subtrees are chrome by definition, so no
// candidate is collected inside them (the element itself stays measurable as
// part of its parent).
var contentSkipTags = map[string]bool{
	"nav": true, "aside": true, "header": true, "footer": true,
}

// contentPositiveTokens are the class or id tokens pages name their main
// content with. Names are split into tokens on every character that is neither
// a letter nor a digit, so "article-body", "articleBody" (lowered) and
// "article.body" all come out as "article" + "body"; the composite spellings
// below cover the names written without a separator.
var contentPositiveTokens = map[string]bool{
	"article": true, "articlebody": true, "articlecontent": true,
	"articledetail": true, "articletext": true, "articlearea": true,
	"content": true, "contentbody": true, "contentmain": true,
	"contenttext": true, "contentarea": true, "contentcontainer": true,
	"contentwrapper": true, "contentcolumn": true, "contentdetail": true,
	"entry": true, "entrycontent": true, "entrytext": true,
	"main": true, "maincontent": true, "mainbody": true, "maintext": true,
	"mainarea": true, "maincontainer": true,
	"pagecontent": true, "pagebody": true,
	"post": true, "postcontent": true, "postbody": true, "posttext": true,
	"story": true, "storycontent": true, "storybody": true,
	"markdown": true, "markdownbody": true, "markdowncontent": true,
	"md": true, "mdcontent": true,
	"doc": true, "docs": true, "doccontent": true, "docscontent": true,
	"documentation": true, "readme": true,
	"richmedia": true, "notion": true, "wysiwyg": true,
}

// contentNegativeTokens are the class or id tokens that mark an element as
// chrome. A name that carries one of them is never a candidate, however it is
// spelled around it ("sidebar-content", "main-nav", "post-meta").
var contentNegativeTokens = map[string]bool{
	"nav": true, "navbar": true, "navigation": true, "menu": true, "menubar": true,
	"sidebar": true, "aside": true, "header": true, "footer": true,
	"topbar": true, "topnav": true, "bottombar": true,
	"breadcrumb": true, "breadcrumbs": true, "toc": true, "tableofcontents": true,
	"comment": true, "comments": true, "commentlist": true, "disqus": true,
	"related": true, "recommend": true, "recommended": true, "trending": true,
	"popular": true, "share": true, "sharing": true, "social": true, "socials": true,
	"banner": true, "ad": true, "ads": true, "advert": true, "advertisement": true,
	"promo": true, "promotion": true, "sponsor": true, "sponsored": true,
	"popup": true, "modal": true, "overlay": true, "dialog": true,
	"cookie": true, "consent": true, "gdpr": true,
	"subscribe": true, "newsletter": true, "signup": true, "signin": true,
	"login": true, "register": true, "account": true, "profile": true,
	"pagination": true, "pager": true, "paginator": true, "toolbar": true,
	"search": true, "searchbox": true, "searchbar": true,
	"meta": true, "metadata": true, "tags": true, "tag": true, "taglist": true,
	"author": true, "byline": true, "timestamp": true, "readingtime": true,
	"progress": true, "tooltip": true, "announcement": true, "notice": true,
	"alert": true, "donate": true, "widget": true, "skip": true,
	"sticky": true, "affix": true, "carousel": true, "slider": true,
	"cart": true, "checkout": true, "userinfo": true,
}

// contentCandidate is one element that declares itself to hold content, with
// the measurements the pick is made on.
type contentCandidate struct {
	node    *nethtml.Node
	textLen int // the element's visible text, in runes
	linkLen int // how much of that text sits inside links
	bonus   int // how strongly the markup declares the element to be content
}

// score weighs the text a candidate holds against the part of it that is links
// — a stretch of anchors is navigation dressed as a container — and adds the
// bonus its markup earned.
func (cand contentCandidate) score() float64 {
	return float64(cand.textLen-cand.linkLen) + float64(cand.bonus)
}

// linkDensity is the share of a candidate's text that is links (1 for an
// element without text, which is nothing but anchors or empty).
func (cand contentCandidate) linkDensity() float64 {
	if cand.textLen <= 0 {
		return 1
	}
	return float64(cand.linkLen) / float64(cand.textLen)
}

// selectMainContent picks the element that holds the main content of a parsed
// document, or nil when the page declares no content to pick (a page of
// navigation, or one whose containers are too small or too link-ridden).
func selectMainContent(root *nethtml.Node) *nethtml.Node {
	candidates, pageText := collectContentCandidates(root)
	best, ok := pickBestCandidate(candidates)
	if !ok {
		return nil
	}
	// A region has to hold enough text to be a page's content, and to be a
	// real part of the page rather than a stray block of it.
	if best.textLen < minContentRunes || float64(best.textLen) < minContentShare*float64(pageText) {
		return nil
	}
	return tightenContentRegion(candidates, best).node
}

// collectContentCandidates walks a document and gathers every element that
// declares itself to hold content, together with the amount of text the whole
// document holds (the yardstick a region's share is measured against).
func collectContentCandidates(root *nethtml.Node) (candidates []contentCandidate, pageText int) {
	pageText, _ = contentTextStats(root)

	ctx := &walkContext{}
	var walk func(n *nethtml.Node)
	walk = func(n *nethtml.Node) {
		if n == nil || isIgnoredNode(n, ctx) {
			return
		}
		if n.Type == nethtml.ElementNode {
			if bonus := contentCandidateBonus(n); bonus > 0 {
				textLen, linkLen := contentTextStats(n)
				candidates = append(candidates, contentCandidate{
					node:    n,
					textLen: textLen,
					linkLen: linkLen,
					bonus:   bonus,
				})
			}
			if contentSkipTags[strings.ToLower(n.Data)] {
				return
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return candidates, pageText
}

// pickBestCandidate returns the candidate with the highest score; a tie keeps
// the first one, which is the outermost in document order.
func pickBestCandidate(candidates []contentCandidate) (contentCandidate, bool) {
	var best contentCandidate
	found := false
	for _, cand := range candidates {
		if !found || cand.score() > best.score() {
			best, found = cand, true
		}
	}
	return best, found
}

// tightenContentRegion descends into an outer candidate while one of the
// elements inside it is itself a candidate holding most of its text: a <main>
// that wraps an <article> is the shell of the page rather than what it is
// about, and the reader wants the article. Descending strictly narrows the
// region, so the loop ends.
func tightenContentRegion(candidates []contentCandidate, outer contentCandidate) contentCandidate {
	for {
		inner, ok := largestInnerCandidate(candidates, outer)
		if !ok {
			return outer
		}
		outer = inner
	}
}

// largestInnerCandidate returns the strongest candidate inside outer whose text
// is most of outer's text and which is not a list of anchors. A wrapper that
// adds little of its own around its core is not narrowed down: that little is
// the title a reader wants to read first.
func largestInnerCandidate(candidates []contentCandidate, outer contentCandidate) (contentCandidate, bool) {
	var best contentCandidate
	found := false
	for _, cand := range candidates {
		if cand.node == outer.node || !isDescendantOf(cand.node, outer.node) {
			continue
		}
		if float64(cand.textLen) < innerContentShare*float64(outer.textLen) ||
			float64(outer.textLen-cand.textLen) < shellExtraShare*float64(outer.textLen) ||
			cand.linkDensity() > maxContentLinkDensity {
			continue
		}
		if !found || cand.score() > best.score() {
			best, found = cand, true
		}
	}
	return best, found
}

// isDescendantOf reports whether n sits inside ancestor.
func isDescendantOf(n, ancestor *nethtml.Node) bool {
	for parent := n.Parent; parent != nil; parent = parent.Parent {
		if parent == ancestor {
			return true
		}
	}
	return false
}

// contentCandidateBonus reports how strongly an element declares itself to be
// the page's main content; 0 means it declares nothing.
func contentCandidateBonus(n *nethtml.Node) int {
	if n.Type != nethtml.ElementNode {
		return 0
	}
	tag := strings.ToLower(n.Data)
	if tag == "main" {
		// <main> states what the ARIA role states, and unlike a name it
		// cannot be about anything else.
		return contentBonusMain
	}
	if strings.EqualFold(strings.TrimSpace(getAttr(n, "role")), "main") {
		return contentBonusRole
	}
	// A name is a claim the page makes about an element, and pages name the
	// parts they do not want read as content as well; a name that says both
	// (the "comment-content" of a comment thread) is not content.
	if contentNameHasToken(n, contentNegativeTokens) {
		return 0
	}
	if tag == "article" {
		return contentBonusArticle
	}
	if contentContainerTags[tag] && contentNameHasToken(n, contentPositiveTokens) {
		return contentBonusNamed
	}
	return 0
}

// contentNameHasToken reports whether the class or id of an element carries one
// of the given tokens.
func contentNameHasToken(n *nethtml.Node, tokens map[string]bool) bool {
	for _, token := range contentNameTokens(n) {
		if tokens[token] {
			return true
		}
	}
	return false
}

// contentNameTokens splits the class and id of an element into lowercase
// tokens, on every character that is neither a letter nor a digit.
func contentNameTokens(n *nethtml.Node) []string {
	raw := strings.ToLower(getAttr(n, "class") + " " + getAttr(n, "id"))
	return strings.FieldsFunc(raw, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
}

// contentTextStats measures an element the way a reader sees it: the runes of
// text it holds, and how many of them are inside links. Scripts, styles and
// hidden elements count for nothing, since a reader never sees them.
func contentTextStats(n *nethtml.Node) (textLen, linkLen int) {
	ctx := &walkContext{}
	var walk func(node *nethtml.Node, inLink bool)
	walk = func(node *nethtml.Node, inLink bool) {
		if node == nil || isIgnoredNode(node, ctx) {
			return
		}
		if node.Type == nethtml.TextNode {
			runes := textRuneCount(node.Data)
			textLen += runes
			if inLink {
				linkLen += runes
			}
			return
		}
		if node.Type == nethtml.ElementNode && strings.EqualFold(node.Data, "a") {
			inLink = true
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, inLink)
		}
	}
	walk(n, false)
	return textLen, linkLen
}

// textRuneCount counts the runes of a text node with every run of whitespace
// collapsed to a single space, so the measurement follows what a reader sees
// rather than the indentation of the markup.
func textRuneCount(s string) int {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	return utf8.RuneCountInString(strings.Join(fields, " "))
}

// contentNodeLabel names an element for a report: its tag, plus the id it
// carries or failing that its first class.
func contentNodeLabel(n *nethtml.Node) string {
	tag := strings.ToLower(n.Data)
	if id := strings.TrimSpace(getAttr(n, "id")); id != "" {
		return tag + "#" + id
	}
	if class := strings.Fields(getAttr(n, "class")); len(class) > 0 {
		return tag + "." + class[0]
	}
	if role := strings.TrimSpace(getAttr(n, "role")); role != "" {
		return tag + "[role=" + strings.ToLower(role) + "]"
	}
	return tag
}
