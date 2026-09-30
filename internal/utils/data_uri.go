package utils

import (
	"fmt"
	"regexp"
	"strings"
)

// A data URI carries its whole payload inside the address: `data:image/png;base64,…`
// is how a page inlines an image, and a single screenshot is hundreds of
// kilobytes of base64 — far more text than the page around it. Nothing outside
// that page can use such an address, so the payload never travels: what is
// written instead is the short marker below, which says what was there and how
// much of it there was, and leaves the reader with the text the page is made of.

// dataURIPattern matches a data URI inside a piece of HTML: from `data:` to the
// first character that cannot be part of one (a quote, a bracket, white space —
// which is where an attribute value or a CSS url() ends). The media type and the
// comma are part of the pattern, so prose that merely mentions `data:` is left
// as it is.
var dataURIPattern = regexp.MustCompile(
	`data:[a-zA-Z0-9!#$&^_\-.+]+/[a-zA-Z0-9!#$&^_\-.+]*(?:;[a-zA-Z0-9=_-]+)*,[^"'()<>[:space:]]+`)

// DataURIPlaceholder renders a data URI as the short marker that stands for it:
// `data:image/png;base64,omitted-1.2MiB`. The marker holds no spaces and no
// brackets, so it can be written into a markdown link target or an HTML
// attribute; the size is the payload the reader is not being given, counted the
// way it was written (base64 text, not the bytes it decodes to).
func DataURIPlaceholder(raw string) string {
	marker, _ := dataURIMarker(raw)
	return marker
}

// OmitDataURIs replaces every data URI in a piece of HTML with the marker that
// stands for it, and reports how many were replaced and how much payload left the
// text. HTML without a data URI in it is returned as it is. The text is not
// parsed: a data URI cannot contain the quote, bracket or space that ends an
// attribute value, so the pattern finds the whole of one.
func OmitDataURIs(html string) (omitted string, count int, bytes int) {
	out := dataURIPattern.ReplaceAllStringFunc(html, func(match string) string {
		marker, payload := dataURIMarker(match)
		count++
		bytes += payload
		return marker
	})
	if count == 0 {
		return html, 0, 0
	}
	return out, count, bytes
}

// dataURIMarker renders the marker of one data URI and reports how many
// characters of payload it stands for. An address that is not a data URI is
// returned as it is: the caller asks about addresses it has already recognized
// as one, and a marker must never be invented for something else.
func dataURIMarker(raw string) (marker string, payload int) {
	body := strings.TrimSpace(raw)
	if len(body) < len("data:") || !strings.EqualFold(body[:len("data:")], "data:") {
		return body, 0
	}
	head, data := splitDataURI(body[len("data:"):])
	return "data:" + head + ",omitted-" + humanSize(len(data)), len(data)
}

// splitDataURI splits the part of a data URI after `data:` into its head (the
// media type and its parameters) and its payload, which is everything after the
// first comma. A URI without a comma is all head: there is nothing after it to
// stand in for.
func splitDataURI(body string) (head, payload string) {
	comma := strings.IndexByte(body, ',')
	if comma < 0 {
		return body, ""
	}
	return body[:comma], body[comma+1:]
}

// humanSize renders a length the way a marker reads it: 820B, 12.3KiB, 1.2MiB.
// It has no space in it, so the marker stays a single markdown-link target.
func humanSize(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f%s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1fPiB", value)
}
