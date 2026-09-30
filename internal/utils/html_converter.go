package utils

import (
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"unicode"

	nethtml "golang.org/x/net/html"
)

// Html2MdConvertResult is the outcome of a conversion: the markdown it produced
// and the diagnostics it collected.
type Html2MdConvertResult struct {
	Markdown string        // the converted markdown
	Warnings []string      // the aggregated diagnostics and warnings
	Content  ContentRegion // the part of Markdown read as the page's main content
}

// ContentRegion describes the part of a converted page the converter read as
// the page's main content: an <article>, a <main>, a container whose class or
// id names it as the content of the page. A caller can carry that part of the
// markdown instead of the navigation, sidebars and footer around it. It is
// empty unless the conversion asked for the selection (WithMainContentSelection).
type ContentRegion struct {
	// Located reports whether a main content region was found; false leaves
	// every other field zero.
	Located bool
	// Label names the element the region came from, e.g. `div.post-content`.
	Label string
	// StartLine is the 1-based line of the conversion's Markdown the region
	// starts on.
	StartLine int
	// LineCount is how many lines of Markdown the region holds.
	LineCount int
	// Markdown is the region on its own, trimmed. Its first line is
	// StartLine of the conversion's Markdown.
	Markdown string
}

// Html2MdOptions carries the settings of a conversion.
type Html2MdOptions struct {
	BaseURL         string // the base URL relative addresses are resolved against
	BulletMarker    string // the bullet of an unordered list item (default "-")
	CodeFence       string // the fence of a code block (default "```")
	PreserveDetails bool   // whether <details>/<summary> keep their raw HTML tags
	// RelativeLinks writes references that stay on the base URL's host as
	// root-relative paths (the path, query and fragment alone, such as
	// /docs/page) instead of absolute addresses, so the markdown of a page
	// does not repeat its own address on every link. References to another
	// host keep their absolute form, as do references to a bare fragment.
	RelativeLinks bool
	// SelectMainContent asks the converter to pick the element that holds the
	// page's main content and report it in Html2MdConvertResult.Content.
	SelectMainContent bool
}

// Html2MdOptionFunc configures a conversion.
type Html2MdOptionFunc func(*Html2MdOptions)

func WithBaseURL(u string) Html2MdOptionFunc { return func(o *Html2MdOptions) { o.BaseURL = u } }
func WithBulletMarker(m string) Html2MdOptionFunc {
	return func(o *Html2MdOptions) { o.BulletMarker = m }
}
func WithCodeFence(f string) Html2MdOptionFunc { return func(o *Html2MdOptions) { o.CodeFence = f } }
func WithPreserveDetails(p bool) Html2MdOptionFunc {
	return func(o *Html2MdOptions) { o.PreserveDetails = p }
}

// WithRelativeLinks makes the converter write every reference that stays on the
// base URL's host as a root-relative path (see Html2MdOptions.RelativeLinks).
func WithRelativeLinks(on bool) Html2MdOptionFunc {
	return func(o *Html2MdOptions) { o.RelativeLinks = on }
}

// WithMainContentSelection makes the converter locate the element that holds
// the page's main content and report it in Html2MdConvertResult.Content (see
// ContentRegion).
func WithMainContentSelection(on bool) Html2MdOptionFunc {
	return func(o *Html2MdOptions) { o.SelectMainContent = on }
}

func defaultHtml2MdOptions() Html2MdOptions {
	return Html2MdOptions{
		BulletMarker:    "-",
		CodeFence:       "```",
		PreserveDetails: true,
	}
}

// Html2MdConverter is the state of one conversion.
type Html2MdConverter struct {
	opts     Html2MdOptions
	baseURL  *url.URL
	warnings *warningTracker

	// contentNode is the element the conversion reads as the page's main
	// content (nil when none was asked for or none was found), and the offsets
	// are where its markdown begins and ends in the buffer while it is walked.
	// contentOpen and contentClose record that each offset was taken once.
	contentNode  *nethtml.Node
	contentStart int
	contentEnd   int
	contentOpen  bool
	contentClose bool
}

// walkContext is the state carried down the tree while it is walked.
type walkContext struct {
	depth          int
	inPre          bool
	inHeading      bool
	atListItemHead bool // the first paragraph of a list item, which must not be pushed away from its bullet
	listDepth      int
	isOrdered      bool
	listCounter    int
}

// warningTracker collects the warnings and diagnostics of a conversion.
type warningTracker struct {
	tagCounts map[string]int
	messages  map[string]int
}

func newWarningTracker() *warningTracker {
	return &warningTracker{
		tagCounts: make(map[string]int),
		messages:  make(map[string]int),
	}
}

func (w *warningTracker) addTag(tag string) {
	w.tagCounts[strings.ToLower(tag)]++
}

func (w *warningTracker) addMsg(msg string) {
	w.messages[msg]++
}

func (w *warningTracker) summary() []string {
	var res []string
	for tag, count := range w.tagCounts {
		if count == 1 {
			res = append(res, fmt.Sprintf("Unknown <%s>", tag))
		} else {
			res = append(res, fmt.Sprintf("Unknown <%s> x%d", tag, count))
		}
	}
	for msg, count := range w.messages {
		if count == 1 {
			res = append(res, msg)
		} else {
			res = append(res, fmt.Sprintf("%s x%d", msg, count))
		}
	}
	sort.Strings(res)
	return res
}

// Html2MdConvert converts a string of HTML; it is the public entry point.
func Html2MdConvert(htmlStr string, opts ...Html2MdOptionFunc) (res Html2MdConvertResult, err error) {
	return Html2MdConvertReader(strings.NewReader(htmlStr), opts...)
}

// Html2MdConvertReader converts an HTML stream; it is the core, public entry
// point.
func Html2MdConvertReader(r io.Reader, opts ...Html2MdOptionFunc) (res Html2MdConvertResult, err error) {
	cfg := defaultHtml2MdOptions()
	for _, opt := range opts {
		opt(&cfg)
	}

	tracker := newWarningTracker()
	var parsedBase *url.URL
	if cfg.BaseURL != "" {
		if u, parseErr := url.Parse(cfg.BaseURL); parseErr == nil {
			parsedBase = u
		} else {
			tracker.addMsg(fmt.Sprintf("Invalid BaseURL: %s", cfg.BaseURL))
		}
	}

	c := &Html2MdConverter{
		opts:     cfg,
		baseURL:  parsedBase,
		warnings: tracker,
	}

	// Top-level failsafe: a panic raised anywhere below becomes an error
	// instead of taking the process down.
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic recovered: %v", rec)
		}
	}()

	rawBytes, readErr := io.ReadAll(r)
	if readErr != nil {
		return Html2MdConvertResult{}, readErr
	}

	// Wrap a bare innerHTML fragment so the parse tree comes out well formed.
	preparedHTML, wrappedType := wrapFragmentIfRequired(string(rawBytes))
	if wrappedType != "" {
		tracker.addMsg(fmt.Sprintf("Fragment <%s> wrapped", wrappedType))
	}

	doc, parseErr := nethtml.Parse(strings.NewReader(preparedHTML))
	if parseErr != nil {
		return Html2MdConvertResult{}, fmt.Errorf("HTML parsing failed: %w", parseErr)
	}

	buf := newMdBuffer()
	ctx := &walkContext{depth: 0}

	// The main content is picked before the walk, so the offsets of that
	// element can be recorded while the markdown that comes from it is written.
	if cfg.SelectMainContent {
		c.contentNode = selectMainContent(doc)
	}

	c.walk(doc, buf, ctx)

	raw := buf.String()
	res.Markdown = strings.TrimSpace(raw)
	res.Warnings = tracker.summary()
	res.Content = c.contentRegion(raw, res.Markdown)
	return res, nil
}

// contentRegion maps the offsets recorded while walking onto the converted
// markdown. The raw buffer differs from the markdown only in the blank space
// trimming takes off its two ends, so a leading offset has to be moved by the
// space the trim removed in front of the text and nothing else.
func (c *Html2MdConverter) contentRegion(raw, markdown string) ContentRegion {
	if c.contentNode == nil || !c.contentOpen || !c.contentClose || markdown == "" {
		return ContentRegion{}
	}
	lead := len(raw) - len(strings.TrimLeftFunc(raw, unicode.IsSpace))
	start := clampInt(c.contentStart-lead, 0, len(markdown))
	end := clampInt(c.contentEnd-lead, start, len(markdown))

	text := markdown[start:end]
	// The region is trimmed like the markdown itself, so the lines it reports
	// are the lines it holds.
	start += len(text) - len(strings.TrimLeftFunc(text, unicode.IsSpace))
	text = strings.TrimSpace(text)
	if text == "" {
		return ContentRegion{}
	}
	return ContentRegion{
		Located:   true,
		Label:     contentNodeLabel(c.contentNode),
		StartLine: strings.Count(markdown[:start], "\n") + 1,
		LineCount: strings.Count(text, "\n") + 1,
		Markdown:  text,
	}
}

// clampInt confines a value to a range.
func clampInt(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}
