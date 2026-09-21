package utils

import (
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	nethtml "golang.org/x/net/html"
)

// Html2MdConvertResult is the outcome of a conversion: the markdown it produced
// and the diagnostics it collected.
type Html2MdConvertResult struct {
	Markdown string   // the converted markdown
	Warnings []string // the aggregated diagnostics and warnings
}

// Html2MdOptions carries the settings of a conversion.
type Html2MdOptions struct {
	BaseURL         string // the base URL relative addresses are resolved against
	BulletMarker    string // the bullet of an unordered list item (default "-")
	CodeFence       string // the fence of a code block (default "```")
	PreserveDetails bool   // whether <details>/<summary> keep their raw HTML tags
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

	c.walk(doc, buf, ctx)

	res.Markdown = strings.TrimSpace(buf.String())
	res.Warnings = tracker.summary()
	return res, nil
}
