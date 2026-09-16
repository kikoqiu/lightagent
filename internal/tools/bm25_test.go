package tools

import (
	"context"
	"strings"
	"testing"
)

// equalTokens reports whether two token slices hold the same strings in order.
func equalTokens(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBM25TokenizeIsWhitespaceOnly pins the query-side tokenizer: only the query
// is tokenized and it is split on whitespace alone (edge punctuation stripped,
// lowercased). Identifier pieces and CJK keywords are not extracted here —
// substring matching against the document text covers them.
func TestBM25TokenizeIsWhitespaceOnly(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "identifier stays one term", input: "office_wps_word_set_text_color", want: []string{"office_wps_word_set_text_color"}},
		{name: "natural language becomes words", input: "Set Text Color", want: []string{"set", "text", "color"}},
		{name: "edge punctuation is stripped", input: "(set_text_color),", want: []string{"set_text_color"}},
		{name: "camel case stays one term", input: "setTextColor", want: []string{"settextcolor"}},
		{name: "cjk clause stays one term", input: "设置选定文字的颜色", want: []string{"设置选定文字的颜色"}},
		{name: "cjk keywords split by the caller", input: "文字 颜色", want: []string{"文字", "颜色"}},
		{name: "punctuation only query has no terms", input: " , ", want: []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bm25Tokenize(tc.input)
			if !equalTokens(got, tc.want) {
				t.Fatalf("bm25Tokenize(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// substringCorpus is the document set shared by the substring tests: two
// identifier-named tools with Chinese descriptions, two English tools, and one
// tool whose description carries no hint at all.
func substringCorpus() []searchDoc {
	return []searchDoc{
		{Name: "mcp_office_wps_word_set_text_color", Description: "设置WPS文档中选定文字的颜色"},
		{Name: "mcp_office_wps_word_insert_image", Description: "在WPS文档中插入图片"},
		{Name: "mcp_office_excel_insert_row", Description: "Insert a row in an Excel sheet."},
		{Name: "mcp_account_password_change", Description: "Change the password of an account."},
		{Name: "mcp_office_excel_set_column_width", Description: `MCP tool provided by server "office_excel".`},
	}
}

// TestBM25SearchMatchesDocumentSubstrings pins the document side: documents are
// never tokenized, so a query term matches wherever it appears literally in the
// name or the description — inside identifiers, camelCase names, CJK text, and
// across mixed scripts.
func TestBM25SearchMatchesDocumentSubstrings(t *testing.T) {
	const (
		textColor   = "mcp_office_wps_word_set_text_color"
		insertImage = "mcp_office_wps_word_insert_image"
	)
	engine := newBM25Engine(substringCorpus(), func(d searchDoc) string { return d.Name + " " + d.Description })

	cases := []struct {
		name  string
		query string
		first string   // expected top result; empty when the order is not pinned
		has   []string // every name that must appear in the result set
	}{
		{name: "keywords inside an identifier", query: "set text color", first: textColor},
		{name: "bare exported name without the mcp prefix", query: "office_wps_word_set_text_color", first: textColor},
		{name: "identifier fragment", query: "text_color", first: textColor},
		{name: "cjk keyword in a chinese description", query: "图片", first: insertImage},
		{name: "cjk word inside a longer clause", query: "文字", first: textColor},
		{name: "single cjk rune", query: "色", first: textColor},
		{name: "latin keyword glued to cjk text", query: "wps", has: []string{textColor, insertImage}},
		{name: "identifier found through a description-less name", query: "column width", first: "mcp_office_excel_set_column_width"},
		{
			name:  "fragment inside another word still matches",
			query: "word",
			has:   []string{textColor, insertImage, "mcp_account_password_change"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ranked := engine.search(tc.query, 10)
			if len(ranked) == 0 {
				t.Fatalf("query %q returned no match", tc.query)
			}
			names := make([]string, len(ranked))
			for i, r := range ranked {
				names[i] = r.Document.Name
			}
			if tc.first != "" && names[0] != tc.first {
				t.Fatalf("query %q ranked %q first, want %q", tc.query, names[0], tc.first)
			}
			for _, want := range tc.has {
				if !strings.Contains(strings.Join(names, "\n"), want) {
					t.Fatalf("query %q result %q is missing %q", tc.query, names, want)
				}
			}
		})
	}
}

// TestBM25WholeWordRanksAboveInWord pins the weighting: an in-word occurrence
// still matches (the search stays permissive) but carries only
// defaultBM25LooseMatchWeight of a whole-word hit, so a document holding the term
// as a whole word ranks first.
func TestBM25WholeWordRanksAboveInWord(t *testing.T) {
	docs := []searchDoc{
		{Name: "mcp_password_change", Description: "Change a password."},
		{Name: "mcp_word_lookup", Description: "Look up a word."},
	}
	engine := newBM25Engine(docs, func(d searchDoc) string { return d.Name + " " + d.Description })

	ranked := engine.search("word", 5)
	if len(ranked) != 2 {
		t.Fatalf("whole-word and in-word documents must both match, got %+v", ranked)
	}
	if ranked[0].Document.Name != "mcp_word_lookup" {
		t.Fatalf("ranked %q first, want the whole-word document %q", ranked[0].Document.Name, "mcp_word_lookup")
	}

	for _, tc := range []struct {
		text         string
		term         string
		exact, loose int
	}{
		{text: "office_wps_word_set_text_color", term: "word", exact: 1},
		{text: "password_change", term: "word", loose: 1},
		{text: "insertRow", term: "insert", exact: 1},
		{text: "insertion", term: "insert", loose: 1},
		{text: "the word and word again", term: "word", exact: 2},
		{text: "设置选定文字的颜色", term: "文字", exact: 1},
	} {
		exact, loose := countBM25Matches(tc.text, tc.term)
		if exact != tc.exact || loose != tc.loose {
			t.Fatalf("countBM25Matches(%q, %q) = (%d, %d), want (%d, %d)",
				tc.text, tc.term, exact, loose, tc.exact, tc.loose)
		}
	}
}

// TestBM25SearchSubstringLimits pins what substring matching deliberately does
// not do: the document side is never normalized, so a query without the
// separators is not equivalent to the identifier.
func TestBM25SearchSubstringLimits(t *testing.T) {
	engine := newBM25Engine(substringCorpus(), func(d searchDoc) string { return d.Name + " " + d.Description })

	for _, query := range []string{"textcolor", "settextcolor", "zzzzzqqqq"} {
		if ranked := engine.search(query, 10); len(ranked) != 0 {
			t.Fatalf("query %q matched %+v, want no match", query, ranked)
		}
	}
}

// TestBM25DiscoveryFindsIdentifierByKeyword is the end-to-end guard: the
// discovery search finds an identifier-named locked function from a plain
// keyword query and activates nothing.
func TestBM25DiscoveryFindsIdentifierByKeyword(t *testing.T) {
	const identifier = "mcp_office_wps_word_set_text_color"

	reg := NewRegistry()
	reg.Register(stubTool{name: coreName, desc: "run a shell command"})
	reg.RegisterDeferred(stubTool{
		name: identifier,
		desc: `MCP tool provided by server "office_wps_word".`,
	})

	res := NewBM25SearchTool(reg, 20).Execute(context.Background(), map[string]any{"query": "set text color"})
	if res.IsError || !strings.Contains(res.ForLLM, identifier) {
		t.Fatalf("search result = %q (error=%v), want %q", res.ForLLM, res.IsError, identifier)
	}
	if !reg.IsDeferredLocked(identifier) {
		t.Fatal("discovery search must not activate the matched function")
	}
}
