package tools

import (
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

// BM25 ranking engine for the MCP tool-discovery search tool. Only the standard
// library is used.
//
// Matching is deliberately asymmetric: only the query is tokenized (whitespace
// split), and every query term is then looked for as a plain substring of a
// document's raw text (name + description). Documents are never tokenized, so
// "set" or "color" finds office_wps_word_set_text_color and a Chinese keyword
// finds the description containing it, without both sides having to agree on a
// segmentation.
//
// An occurrence is classified as whole-word (both ends sit on a word boundary)
// or in-word (glued to ASCII alphanumerics, e.g. "word" inside "password"). Both
// match — the search stays permissive — but an in-word occurrence carries only a
// fraction of the term frequency weight, so whole-word hits rank higher.

const (
	// defaultBM25K1 is the term-frequency saturation factor.
	defaultBM25K1 = 1.2
	// defaultBM25B is the document-length normalization factor.
	defaultBM25B = 0.75
	// defaultBM25LooseMatchWeight is the term-frequency weight of an in-word
	// (non word-boundary) occurrence relative to a whole-word one.
	defaultBM25LooseMatchWeight = 0.25
)

// bm25Engine is a BM25 search engine over a generic corpus. T is the document
// type; the caller supplies a textFunc that extracts the searchable text from
// each document. The lowercased text snapshot and the document-length
// normalization are computed once at construction time.
type bm25Engine[T any] struct {
	corpus []T
	k1     float64
	b      float64

	// texts holds every document's lowercased searchable text; queries are
	// matched against it by substring containment.
	texts []string
	// docLenNorm[i] is k1 * (1 - b + b * runeLen(texts[i]) / avgRuneLen).
	docLenNorm []float32
}

// newBM25Engine builds an engine for the given corpus.
func newBM25Engine[T any](corpus []T, textFunc func(T) string) *bm25Engine[T] {
	engine := &bm25Engine[T]{
		corpus: corpus,
		k1:     defaultBM25K1,
		b:      defaultBM25B,
	}
	if len(corpus) == 0 {
		return engine
	}

	engine.texts = make([]string, len(corpus))
	runeLens := make([]int, len(corpus))
	totalLen := 0
	for i, doc := range corpus {
		text := strings.ToLower(textFunc(doc))
		engine.texts[i] = text
		runeLens[i] = utf8.RuneCountInString(text)
		totalLen += runeLens[i]
	}

	avgLen := float64(totalLen) / float64(len(corpus))
	if avgLen == 0 {
		avgLen = 1
	}
	engine.docLenNorm = make([]float32, len(corpus))
	for i, runeLen := range runeLens {
		engine.docLenNorm[i] = float32(engine.k1 * (1 - engine.b + engine.b*float64(runeLen)/avgLen))
	}
	return engine
}

// bm25Result is a single ranked result from a search call.
type bm25Result[T any] struct {
	Document T
	Score    float32
}

// search tokenizes query and ranks the corpus against it. Every query term is
// matched as a plain substring of each document's text; whole-word occurrences
// weigh more than in-word ones. It returns an empty slice (not nil) when there
// are no matches.
func (e *bm25Engine[T]) search(query string, topK int) []bm25Result[T] {
	if topK <= 0 {
		return []bm25Result[T]{}
	}

	queryTerms := bm25Dedupe(bm25Tokenize(query))
	if len(queryTerms) == 0 || len(e.texts) == 0 {
		return []bm25Result[T]{}
	}

	scores := make(map[int32]float32, 16)
	for _, term := range queryTerms {
		matches := e.matchTerm(term)
		if len(matches) == 0 {
			continue
		}
		// idf = ln((N - df + 0.5) / (df + 0.5) + 1) over the containing docs.
		df := float64(len(matches))
		n := float64(len(e.texts))
		idf := float32(math.Log((n-df+0.5)/(df+0.5) + 1))

		for _, m := range matches {
			denom := m.tf + e.docLenNorm[m.docID]
			if denom == 0 {
				continue
			}
			scores[m.docID] += idf * (m.tf * (float32(e.k1) + 1)) / denom
		}
	}
	if len(scores) == 0 {
		return []bm25Result[T]{}
	}

	heap := make([]bm25ScoredDoc, 0, topK)
	for docID, sc := range scores {
		switch {
		case len(heap) < topK:
			heap = append(heap, bm25ScoredDoc{docID: docID, score: sc})
			if len(heap) == topK {
				bm25MinHeapify(heap)
			}
		case sc > heap[0].score:
			heap[0] = bm25ScoredDoc{docID: docID, score: sc}
			bm25SiftDown(heap, 0)
		}
	}

	sort.Slice(heap, func(i, j int) bool { return heap[i].score > heap[j].score })

	out := make([]bm25Result[T], len(heap))
	for i, h := range heap {
		out[i] = bm25Result[T]{
			Document: e.corpus[h.docID],
			Score:    h.score,
		}
	}
	return out
}

// bm25DocMatch is one document's weighted term frequency for a query term.
type bm25DocMatch struct {
	docID int32
	tf    float32
}

// matchTerm returns every document whose text contains term, with the term's
// weighted frequency. Whole-word occurrences keep full weight; in-word
// occurrences are scaled by defaultBM25LooseMatchWeight.
func (e *bm25Engine[T]) matchTerm(term string) []bm25DocMatch {
	matches := make([]bm25DocMatch, 0, 8)
	for i, text := range e.texts {
		exact, loose := countBM25Matches(text, term)
		if exact == 0 && loose == 0 {
			continue
		}
		matches = append(matches, bm25DocMatch{
			docID: int32(i),
			tf:    float32(exact) + defaultBM25LooseMatchWeight*float32(loose),
		})
	}
	return matches
}

// countBM25Matches counts the non-overlapping occurrences of term in the
// lowercased text, split into whole-word hits (both ends on a word boundary) and
// in-word hits (glued to ASCII alphanumerics).
func countBM25Matches(text, term string) (exact, loose int) {
	if term == "" || len(term) > len(text) {
		return 0, 0
	}
	for pos := 0; pos+len(term) <= len(text); {
		i := strings.Index(text[pos:], term)
		if i < 0 {
			break
		}
		start := pos + i
		end := start + len(term)
		if bm25EdgeIsWordBreak(text, start) && bm25EdgeIsWordBreak(text, end) {
			exact++
		} else {
			loose++
		}
		pos = end
	}
	return exact, loose
}

// bm25EdgeIsWordBreak reports whether byte offset i of text sits on a word
// boundary: the neighbouring byte is not an ASCII alphanumeric (space,
// punctuation, '_' / '-', or a CJK byte), or the ASCII alphanumerics change
// class there (camelCase hump, letter/digit transition). Byte comparison is safe
// for multi-byte text: UTF-8 is self-synchronizing, so a valid term can only
// match at a rune boundary, and CJK bytes are never ASCII alphanumerics.
func bm25EdgeIsWordBreak(text string, i int) bool {
	if i <= 0 || i >= len(text) {
		return true
	}
	if !bm25IsASCIIAlnum(text[i-1]) || !bm25IsASCIIAlnum(text[i]) {
		return true
	}
	return bm25StartsWordAt(text, i)
}

// bm25StartsWordAt reports whether the ASCII alphanumerics around offset i are
// split into two words by case or digit transitions:
//
//	foo|Bar      lower -> upper
//	HTTP|Server  upper run followed by a lower
//	abc|123      123|abc  letter/digit transition
func bm25StartsWordAt(text string, i int) bool {
	prev, cur := text[i-1], text[i]
	switch {
	case cur >= 'A' && cur <= 'Z':
		if bm25IsLower(prev) || bm25IsDigit(prev) {
			return true
		}
		// Keep an acronym run together: split HTTP|Server, not HTTP|S|erver.
		return i+1 < len(text) && bm25IsLower(text[i+1])
	case cur >= 'a' && cur <= 'z':
		// digit -> lower; upper -> lower belongs to the acronym run above.
		return bm25IsDigit(prev)
	case cur >= '0' && cur <= '9':
		// letter -> digit; a digit run stays one number.
		return bm25IsLower(prev) || (prev >= 'A' && prev <= 'Z')
	}
	return false
}

// bm25IsASCIIAlnum reports whether c is an ASCII letter or digit.
func bm25IsASCIIAlnum(c byte) bool {
	return bm25IsLower(c) || bm25IsDigit(c) || (c >= 'A' && c <= 'Z')
}

// bm25EdgeCutset is the punctuation stripped from both ends of a query term.
const bm25EdgeCutset = ".,;:!?\"'()/\\-_"

// bm25Tokenize splits a query into lowercase terms, stripping edge punctuation.
// Only the query is tokenized: documents are matched as raw text by substring
// containment, so no document-side segmentation is needed, and a term finds its
// occurrence inside an identifier, a camelCase name, or a CJK clause alike.
func bm25Tokenize(s string) []string {
	raw := strings.Fields(s)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		t = strings.Trim(t, bm25EdgeCutset)
		if t == "" {
			continue
		}
		out = append(out, strings.ToLower(t))
	}
	return out
}

// bm25IsLower reports whether c is an ASCII lowercase letter.
func bm25IsLower(c byte) bool { return c >= 'a' && c <= 'z' }

// bm25IsDigit reports whether c is an ASCII digit.
func bm25IsDigit(c byte) bool { return c >= '0' && c <= '9' }

// bm25Dedupe returns a new slice with duplicate tokens removed, preserving
// first-occurrence order.
func bm25Dedupe(tokens []string) []string {
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	return out
}

type bm25ScoredDoc struct {
	docID int32
	score float32
}

// bm25MinHeapify builds a min-heap in-place using Floyd's algorithm: O(k).
func bm25MinHeapify(h []bm25ScoredDoc) {
	for i := len(h)/2 - 1; i >= 0; i-- {
		bm25SiftDown(h, i)
	}
}

// bm25SiftDown restores the min-heap property starting at node i: O(log k).
func bm25SiftDown(h []bm25ScoredDoc, i int) {
	n := len(h)
	for {
		smallest := i
		l, r := 2*i+1, 2*i+2
		if l < n && h[l].score < h[smallest].score {
			smallest = l
		}
		if r < n && h[r].score < h[smallest].score {
			smallest = r
		}
		if smallest == i {
			break
		}
		h[i], h[smallest] = h[smallest], h[i]
		i = smallest
	}
}
