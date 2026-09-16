// Package markdown renders the small markdown subset used by assistant replies
// into ANSI-styled terminal text for the CLI.
//
// The web mirror renders markdown in the browser instead (marked + DOMPurify),
// so this package stays stdlib-only and ANSI-focused. It is line oriented so the
// CLI can render a streamed reply as soon as each line arrives.
package markdown

import (
	"strings"

	"lightagent/internal/termcolor"
)

// ruleWidth is how wide the rendered horizontal rule is.
const ruleWidth = 44

// ANSI converts a markdown document into ANSI-styled text. Styling is applied
// through termcolor, so it degrades to plain text when color is disabled.
func ANSI(doc string) string {
	s := NewStream()
	return s.Write(doc) + s.Flush()
}

// Stream renders markdown incrementally. Callers feed chunks with Write and
// drain the tail with Flush; incomplete lines stay buffered, so a half-arrived
// marker is never interpreted.
type Stream struct {
	buf     string
	inFence bool
	fence   string
}

// NewStream returns an empty ANSI markdown stream.
func NewStream() *Stream { return &Stream{} }

// Write consumes a chunk and returns the rendered text of every line the chunk
// completed. A trailing partial line stays buffered.
func (s *Stream) Write(chunk string) string {
	if chunk == "" {
		return ""
	}
	s.buf += chunk
	idx := strings.LastIndexByte(s.buf, '\n')
	if idx < 0 {
		return ""
	}
	complete, rest := s.buf[:idx+1], s.buf[idx+1:]
	s.buf = rest
	return s.render(complete)
}

// Pending returns the partial line received so far, without rendering it. It
// never contains a newline (complete lines travel through Write), so a caller
// can show it as a streaming preview while the rest of the line is still
// arriving.
func (s *Stream) Pending() string {
	return s.buf
}

// Flush renders whatever is still buffered, without adding a line break.
func (s *Stream) Flush() string {
	if s.buf == "" {
		return ""
	}
	out := s.render(s.buf)
	s.buf = ""
	return out
}

// Reset drops buffered text and code-fence state, starting a fresh message.
func (s *Stream) Reset() {
	s.buf = ""
	s.inFence = false
	s.fence = ""
}

// render converts text whose lines are known to be complete.
func (s *Stream) render(text string) string {
	var out strings.Builder
	for len(text) > 0 {
		line := text
		hasNewline := false
		if idx := strings.IndexByte(text, '\n'); idx >= 0 {
			line, text = text[:idx], text[idx+1:]
			hasNewline = true
		} else {
			text = ""
		}
		rendered, keepBreak := s.line(strings.TrimSuffix(line, "\r"))
		out.WriteString(rendered)
		if hasNewline && keepBreak {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

// line renders one complete markdown line, tracking fenced code state. The
// second result reports whether the line's own break should be kept; code-fence
// marker lines are dropped entirely so they leave no blank line behind.
func (s *Stream) line(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)

	if s.inFence {
		if isFenceClose(trimmed, s.fence) {
			s.inFence = false
			s.fence = ""
			return "", false
		}
		return termcolor.Color(raw, termcolor.CyanCode), true
	}
	if marker, _, ok := fenceOpen(trimmed); ok {
		s.inFence = true
		s.fence = marker
		return "", false
	}
	if trimmed == "" {
		return "", true
	}
	if level, text, ok := heading(trimmed); ok {
		if level <= 2 {
			return inlineANSI(text, termcolor.BoldCode, termcolor.BlueCode), true
		}
		return inlineANSI(text, termcolor.BoldCode), true
	}
	if isRule(trimmed) {
		return termcolor.DimText(strings.Repeat("-", ruleWidth)), true
	}
	if text, ok := quoteText(trimmed); ok {
		return termcolor.DimText("> ") + inlineANSI(text), true
	}
	if prefix, text, _, ok := listItem(raw); ok {
		return prefix + inlineANSI(text), true
	}
	return inlineANSI(trimmed), true
}

// heading parses an ATX heading ("## Title") and returns its level and text.
func heading(line string) (int, string, bool) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	if level < len(line) && line[level] != ' ' {
		return 0, "", false
	}
	text := strings.TrimSpace(strings.Trim(line[level:], "#"))
	return level, text, text != ""
}

// isRule reports whether line is a thematic break ("---", "***", "___").
func isRule(line string) bool {
	if len(line) < 3 {
		return false
	}
	c := line[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	seen := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case c:
			seen++
		case ' ', '\t':
		default:
			return false
		}
	}
	return seen >= 3
}

// fenceOpen recognizes an opening code fence and returns its marker and info
// string ("```go" and "~~~ go" both yield ("```"/"~~~", "go")).
func fenceOpen(line string) (marker, info string, ok bool) {
	var c byte
	switch {
	case strings.HasPrefix(line, "```"):
		c = '`'
	case strings.HasPrefix(line, "~~~"):
		c = '~'
	default:
		return "", "", false
	}
	n := 0
	for n < len(line) && line[n] == c {
		n++
	}
	return strings.Repeat(string(c), n), strings.TrimSpace(line[n:]), true
}

// isFenceClose reports whether line closes a fence opened with marker.
func isFenceClose(line, marker string) bool {
	if marker == "" {
		return false
	}
	n := 0
	for n < len(line) && line[n] == marker[0] {
		n++
	}
	if n < len(marker) {
		return false
	}
	return strings.TrimSpace(line[n:]) == ""
}

// quoteText strips one blockquote marker from line.
func quoteText(line string) (string, bool) {
	if !strings.HasPrefix(line, ">") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, ">")), true
}

// listItem parses a list item and returns its normalized prefix (indentation
// plus marker), its text and whether the list is ordered.
func listItem(line string) (prefix, text string, ordered, ok bool) {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	head, rest := line[:i], line[i:]
	if len(rest) >= 2 && (rest[0] == '-' || rest[0] == '*' || rest[0] == '+') && rest[1] == ' ' {
		return head + "- ", strings.TrimSpace(rest[2:]), false, true
	}
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j > 0 && j+1 < len(rest) && (rest[j] == '.' || rest[j] == ')') && rest[j+1] == ' ' {
		return head + rest[:j+1] + " ", strings.TrimSpace(rest[j+2:]), true, true
	}
	return "", "", false, false
}

// spanKind classifies one inline markdown fragment.
type spanKind int

const (
	spanText spanKind = iota
	spanCode
	spanBold
	spanItalic
	spanStrike
	spanLink
)

// span is a parsed inline fragment.
type span struct {
	kind spanKind
	text string
	url  string
}

// inlineANSI renders inline markdown with ANSI styling. Extra codes (used for
// headings) are added to every fragment so the enclosing style survives.
func inlineANSI(s string, base ...string) string {
	var b strings.Builder
	for _, sp := range parseInline(s) {
		var codes []string
		switch sp.kind {
		case spanCode:
			codes = []string{termcolor.CyanCode}
		case spanBold:
			codes = []string{termcolor.BoldCode}
		case spanItalic:
			codes = []string{termcolor.ItalicCode}
		case spanStrike:
			codes = []string{termcolor.DimCode}
		case spanLink:
			codes = []string{termcolor.BlueCode}
		}
		b.WriteString(termcolor.Color(sp.text, append(codes, base...)...))
		if sp.kind == spanLink && sp.url != "" && sp.url != sp.text {
			b.WriteString(termcolor.Gray(" (" + sp.url + ")"))
		}
	}
	return b.String()
}

// parseInline splits s into styled fragments. Unmatched markers are kept as
// literal text, so partial input never loses characters.
func parseInline(s string) []span {
	var spans []span
	var plain strings.Builder
	flush := func() {
		if plain.Len() > 0 {
			spans = append(spans, span{kind: spanText, text: plain.String()})
			plain.Reset()
		}
	}

	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '`':
			if end := strings.IndexByte(s[i+1:], '`'); end >= 0 {
				flush()
				spans = append(spans, span{kind: spanCode, text: strings.TrimSpace(s[i+1 : i+1+end])})
				i += end + 2
				continue
			}
		case c == '[':
			if text, url, next, ok := linkAt(s, i); ok {
				flush()
				spans = append(spans, span{kind: spanLink, text: text, url: url})
				i = next
				continue
			}
		case c == '!' && i+1 < len(s) && s[i+1] == '[':
			if text, url, next, ok := linkAt(s, i+1); ok {
				flush()
				spans = append(spans, span{kind: spanLink, text: text, url: url})
				i = next
				continue
			}
		case c == '*':
			if end := boldEnd(s, i); end > 0 {
				flush()
				spans = append(spans, span{kind: spanBold, text: s[i+2 : i+2+end]})
				i += end + 4
				continue
			}
			if end := strings.IndexByte(s[i+1:], '*'); end > 0 {
				flush()
				spans = append(spans, span{kind: spanItalic, text: s[i+1 : i+1+end]})
				i += end + 2
				continue
			}
		case c == '_':
			if strings.HasPrefix(s[i:], "__") {
				if end := underscoreBoldEnd(s, i); end > 0 {
					flush()
					spans = append(spans, span{kind: spanBold, text: s[i+2 : i+2+end]})
					i += end + 4
					continue
				}
			}
			if end := underscoreItalicEnd(s, i); end > 0 {
				flush()
				spans = append(spans, span{kind: spanItalic, text: s[i+1 : i+1+end]})
				i += end + 2
				continue
			}
		case c == '~' && i+1 < len(s) && s[i+1] == '~':
			if end := strings.Index(s[i+2:], "~~"); end >= 0 {
				flush()
				spans = append(spans, span{kind: spanStrike, text: s[i+2 : i+2+end]})
				i += end + 4
				continue
			}
		}
		plain.WriteByte(s[i])
		i++
	}
	flush()
	return spans
}

// boldEnd returns the offset of the closing "**" within s[i+2:], or -1.
func boldEnd(s string, i int) int {
	if !strings.HasPrefix(s[i:], "**") {
		return -1
	}
	return strings.Index(s[i+2:], "**")
}

// underscoreBoldEnd mirrors boldEnd for "__", ignoring intraword underscores
// (read_file_lines stays literal, matching CommonMark).
func underscoreBoldEnd(s string, i int) int {
	if i > 0 && isWordByte(s[i-1]) {
		return -1
	}
	end := strings.Index(s[i+2:], "__")
	if end <= 0 {
		return -1
	}
	if close := i + 2 + end; close+2 < len(s) && isWordByte(s[close+2]) {
		return -1
	}
	return end
}

// underscoreItalicEnd returns the offset of a closing "_" within s[i+1:] that
// forms intraword-safe emphasis, or -1.
func underscoreItalicEnd(s string, i int) int {
	if i > 0 && isWordByte(s[i-1]) {
		return -1
	}
	end := strings.IndexByte(s[i+1:], '_')
	if end <= 0 {
		return -1
	}
	if close := i + 1 + end; close+1 < len(s) && isWordByte(s[close+1]) {
		return -1
	}
	return end
}

// isWordByte reports whether b can be part of an identifier.
func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// linkAt parses "[text](url)" starting at open (the index of '[').
func linkAt(s string, open int) (text, url string, next int, ok bool) {
	if open >= len(s) || s[open] != '[' {
		return "", "", 0, false
	}
	end := strings.IndexByte(s[open+1:], ']')
	if end < 0 {
		return "", "", 0, false
	}
	text = s[open+1 : open+1+end]
	paren := open + 1 + end + 1
	if paren >= len(s) || s[paren] != '(' {
		return "", "", 0, false
	}
	depth := 1
	j := paren + 1
	for j < len(s) && depth > 0 {
		switch s[j] {
		case '(':
			depth++
		case ')':
			depth--
		}
		j++
	}
	if depth != 0 {
		return "", "", 0, false
	}
	return text, strings.TrimSpace(s[paren+1 : j-1]), j, true
}
