package utils

import (
	"strings"
	"testing"
)

// TestDataURIPlaceholder pins the marker that stands in for one data URI: the
// media type as the page wrote it, the size of the payload, and nothing of the
// payload itself.
func TestDataURIPlaceholder(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"base64 image", "data:image/png;base64," + strings.Repeat("A", 4096),
			"data:image/png;base64,omitted-4.0KiB"},
		{"large image", "data:image/jpeg;base64," + strings.Repeat("A", 1258291),
			"data:image/jpeg;base64,omitted-1.2MiB"},
		{"plain text", "data:text/plain,hi", "data:text/plain,omitted-2B"},
		{"svg", "data:image/svg+xml,<svg/>", "data:image/svg+xml,omitted-6B"},
		{"no media type", "data:,hello", "data:,omitted-5B"},
		{"upper case", "DATA:IMAGE/PNG;BASE64,AAAA", "data:IMAGE/PNG;BASE64,omitted-4B"},
		{"nothing after the comma", "data:image/png;base64,", "data:image/png;base64,omitted-0B"},
		{"not a data URI", "https://example.com/x.png", "https://example.com/x.png"},
	}
	for _, tc := range cases {
		if got := DataURIPlaceholder(tc.in); got != tc.want {
			t.Errorf("%s: DataURIPlaceholder(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestOmitDataURIsRewritesHTMLText pins the rewriting of a piece of HTML: every
// data URI in it becomes its marker, prose that merely mentions the form of one
// is left alone, and what was left out is reported.
func TestOmitDataURIsRewritesHTMLText(t *testing.T) {
	image := "data:image/png;base64," + strings.Repeat("A", 2048)
	html := `<p>plain</p><img src="` + image + `">` +
		`<a href="data:text/markdown,hello">doc</a>` +
		`<span style="background:url(data:image/gif;base64,AAAA)"></span>` +
		`<p>the data:image/png form is mentioned in prose</p>`

	out, count, bytes := OmitDataURIs(html)
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
	if want := 2048 + 5 + 4; bytes != want {
		t.Errorf("bytes = %d, want %d", bytes, want)
	}
	if strings.Contains(out, strings.Repeat("A", 64)) {
		t.Errorf("a payload remained in the text:\n%s", out)
	}
	for _, want := range []string{
		`<img src="data:image/png;base64,omitted-2.0KiB">`,
		`<a href="data:text/markdown,omitted-5B">`,
		`url(data:image/gif;base64,omitted-4B)`,
		`<p>plain</p>`,
		`<p>the data:image/png form is mentioned in prose</p>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the rewritten HTML is missing %q:\n%s", want, out)
		}
	}

	// Text without a data URI in it comes back as it is.
	const plain = `<p>nothing to omit</p>`
	same, count, bytes := OmitDataURIs(plain)
	if same != plain || count != 0 || bytes != 0 {
		t.Errorf("OmitDataURIs(%q) = (%q, %d, %d), want it unchanged", plain, same, count, bytes)
	}
}
