package tools

import (
	"context"
	"fmt"
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
			ranked := engine.search(tc.query, 10, 0)
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

	ranked := engine.search("word", 5, 0)
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
		if ranked := engine.search(query, 10, 0); len(ranked) != 0 {
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

	res := NewBM25SearchTool(reg, 20, 0.5).Execute(context.Background(), map[string]any{"query": "set text color"})
	if res.IsError || !strings.Contains(res.ForLLM, identifier) {
		t.Fatalf("search result = %q (error=%v), want %q", res.ForLLM, res.IsError, identifier)
	}
	if !reg.IsDeferredLocked(identifier) {
		t.Fatal("discovery search must not activate the matched function")
	}
}

// rankedHas reports whether a ranked result set contains the named document.
func rankedHas(ranked []bm25Result[searchDoc], name string) bool {
	for _, r := range ranked {
		if r.Document.Name == name {
			return true
		}
	}
	return false
}

// TestBM25MinMatchRateFiltersDocuments pins the keyword match-rate gate: a
// document must contain at least minMatchRate of the query's keywords (a keyword
// repeated in the query counts once per occurrence) to be reported, and covering
// exactly the threshold is enough.
func TestBM25MinMatchRateFiltersDocuments(t *testing.T) {
	docs := []searchDoc{
		{Name: "mcp_set_color", Description: "set the color of a shape"},
		{Name: "mcp_set_only", Description: "set"},
		{Name: "mcp_color_only", Description: "color"},
		{Name: "mcp_unrelated", Description: "list servers"},
	}
	engine := newBM25Engine(docs, func(d searchDoc) string { return d.Name + " " + d.Description })

	// "set set color" has three keyword occurrences: the repeated "set" counts
	// twice, so the set-only document matches two thirds and the color-only one
	// only one third.
	ranked := engine.search("set set color", 10, 0.5)
	if len(ranked) != 2 || ranked[0].Document.Name != "mcp_set_color" || !rankedHas(ranked, "mcp_set_only") {
		t.Fatalf("ranked = %+v, want the complete and the two-thirds match", ranked)
	}
	if rankedHas(ranked, "mcp_color_only") || rankedHas(ranked, "mcp_unrelated") {
		t.Fatalf("ranked = %+v, want the one-third and the unrelated document dropped", ranked)
	}
	if ranked[0].MatchRate != 1 {
		t.Fatalf("top match rate = %v, want 1", ranked[0].MatchRate)
	}

	// Raising the gate to 1 keeps only the complete match; 0 disables it (a
	// document with no keyword at all never ranks, though).
	if strict := engine.search("set set color", 10, 1); len(strict) != 1 || strict[0].Document.Name != "mcp_set_color" {
		t.Fatalf("ranked at minMatchRate 1 = %+v, want only the complete match", strict)
	}
	if all := engine.search("set set color", 10, 0); len(all) != 3 {
		t.Fatalf("ranked at minMatchRate 0 = %+v, want every matching document", all)
	}

	// Exactly half of a two-keyword query sits on the threshold, not below it.
	half := engine.search("set color", 10, 0.5)
	if len(half) != 3 || half[0].Document.Name != "mcp_set_color" {
		t.Fatalf("ranked for a two-keyword query = %+v, want all three matches", half)
	}
}

// TestBM25MatchRateOutranksScore pins the two-level ranking: the keyword match
// rate decides first, so a document covering every query keyword stays ahead of
// one with a higher BM25 score that covers only part of the query.
func TestBM25MatchRateOutranksScore(t *testing.T) {
	docs := []searchDoc{
		// Covers both keywords, but the long text keeps its score low.
		{Name: "alpha_beta_long", Description: strings.Repeat("beta ", 200)},
		// Covers only "alpha" in a short, saturated text: a higher score.
		{Name: "alpha_short", Description: "alpha here"},
	}
	// "beta" is common, so it barely moves any score.
	for i := 0; i < 10; i++ {
		docs = append(docs, searchDoc{Name: fmt.Sprintf("filler_%02d", i), Description: "beta filler here"})
	}
	engine := newBM25Engine(docs, func(d searchDoc) string { return d.Name + " " + d.Description })

	ranked := engine.search("alpha beta", 10, 0.5)
	if len(ranked) < 2 || ranked[0].Document.Name != "alpha_beta_long" || ranked[0].MatchRate != 1 {
		t.Fatalf("ranked = %+v, want the complete match first", ranked)
	}
	if ranked[1].Document.Name != "alpha_short" {
		t.Fatalf("ranked = %+v, want the half match second", ranked)
	}
	if ranked[0].Score >= ranked[1].Score {
		t.Fatalf("the fixture must give the half match the higher score: %+v", ranked)
	}
}

// TestBM25DuplicateKeywordsAddWeight pins the duplicate policy: a keyword
// repeated in the query is one keyword per occurrence, so it raises both the
// keyword total of the match rate and the term weight of the BM25 score.
func TestBM25DuplicateKeywordsAddWeight(t *testing.T) {
	docs := []searchDoc{
		{Name: "mcp_set_color", Description: "set the color"},
		{Name: "mcp_set_size", Description: "set the size"},
	}
	engine := newBM25Engine(docs, func(d searchDoc) string { return d.Name + " " + d.Description })

	once := engine.search("set color", 10, 0)
	twice := engine.search("set set color", 10, 0)
	if len(once) != 2 || len(twice) != 2 {
		t.Fatalf("both queries must match both documents: %+v / %+v", once, twice)
	}
	if once[0].Document.Name != "mcp_set_color" || twice[0].Document.Name != "mcp_set_color" {
		t.Fatalf("the complete match must rank first: %+v / %+v", once, twice)
	}
	if once[1].MatchRate != 0.5 || twice[1].MatchRate != 2.0/3.0 {
		t.Fatalf("partial match rates = %v and %v, want 0.5 and 2/3", once[1].MatchRate, twice[1].MatchRate)
	}
	if twice[0].Score <= once[0].Score {
		t.Fatalf("repeating a keyword must add weight: score %v vs %v", twice[0].Score, once[0].Score)
	}
}

// TestBM25DiscoveryDropsWeakMatches verifies the tool-level match-rate gate: a
// locked function sharing only a fraction of the query keywords is not reported,
// and lowering minMatchRate brings it back.
func TestBM25DiscoveryDropsWeakMatches(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterDeferred(stubTool{
		name: "mcp_github_create_issue",
		desc: "Create a GitHub issue in a repository",
	})

	args := map[string]any{"query": "github upload avatar widget"} // 1 of 4 keywords
	res := NewBM25SearchTool(reg, 5, 0.5).Execute(context.Background(), args)
	if res.IsError || !strings.Contains(res.ForLLM, "No locked functions found") {
		t.Fatalf("search at 50%% = %q (error=%v), want no result", res.ForLLM, res.IsError)
	}
	res = NewBM25SearchTool(reg, 5, 0.25).Execute(context.Background(), args)
	if res.IsError || !strings.Contains(res.ForLLM, "mcp_github_create_issue") {
		t.Fatalf("search at 25%% = %q (error=%v), want the match", res.ForLLM, res.IsError)
	}
}
