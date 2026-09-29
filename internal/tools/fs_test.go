package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestReadFileLinesPagination(t *testing.T) {
	p := writeFile(t, t.TempDir(), "a.txt", "l1\nl2\nl3\nl4\nl5\n")
	tool := NewReadFileLinesTool(FsConfig{MaxReadFileSize: 65536, MaxReadFileLines: 100})

	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "start_line": 2, "max_lines": 2,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "lines 2-3") {
		t.Fatalf("header missing range: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "start_line=4") {
		t.Fatalf("PARTIAL marker missing: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "l2\nl3") {
		t.Fatalf("content missing: %s", res.ForLLM)
	}
}

func TestReadFileLinesEOF(t *testing.T) {
	p := writeFile(t, t.TempDir(), "b.txt", "x\ny\n")
	tool := NewReadFileLinesTool(FsConfig{})

	res := tool.Execute(context.Background(), map[string]any{"path": p})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "[END OF FILE - no further content.]") {
		t.Fatalf("EOF marker missing: %s", res.ForLLM)
	}
}

func TestWriteFileModes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "w.txt")
	tool := NewWriteFileTool(FsConfig{MaxWriteLines: 100})
	ctx := context.Background()

	res := tool.Execute(ctx, map[string]any{"path": p, "content": "hello", "mode": "c"})
	if res.IsError {
		t.Fatalf("create failed: %s", res.ForLLM)
	}

	// create-only must refuse an existing file.
	res = tool.Execute(ctx, map[string]any{"path": p, "content": "x", "mode": "c"})
	if !res.IsError {
		t.Fatal("create mode should fail when the file exists")
	}

	res = tool.Execute(ctx, map[string]any{"path": p, "content": " world", "mode": "a"})
	if res.IsError {
		t.Fatalf("append failed: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "hello world" {
		t.Fatalf("after append = %q", got)
	}

	res = tool.Execute(ctx, map[string]any{"path": p, "content": "new"})
	if res.IsError {
		t.Fatalf("overwrite failed: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "new" {
		t.Fatalf("after overwrite = %q", got)
	}
}

// truncationGuidance is the fixed advice the truncation note carries between its
// first line (how much was written) and the dropped lines. The wording is pinned
// verbatim, trailing spaces included.
const truncationGuidance = "\nDo NOT write the file in fewer lines than the original, it's a VIOLTION of the basic RULE. " +
	"\nWrite the file in several calls with mode='a'. The newline that ended the last written line has already been written for you, so start the next call with the next line's text directly (no leading newline). Keep the trailing newline of your final line if the original has one: the system never inserts it for you. " +
	"\n\nThe exact truncated lines are displayed as follows (CRITICAL: Check THESE LINES directly, NEVER issue a duplicate read in the next step):"

// noFinalNewlineMarker is a stable fragment of the notice a successful write
// result carries when the text payload does not end with a newline.
const noFinalNewlineMarker = "no trailing newline"

func TestWriteFileLineLimit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lim.txt")
	tool := NewWriteFileTool(FsConfig{MaxWriteLines: 2})

	res := tool.Execute(context.Background(), map[string]any{"path": p, "content": "a\nb\nc\nd"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "truncated") {
		t.Fatalf("expected truncation note: %s", res.ForLLM)
	}
	// The newline that terminated the last written line is kept, so appending the
	// remainder resumes on the next line without an extra one being inserted.
	if got := readFile(t, p); got != "a\nb\n" {
		t.Fatalf("content = %q, want the first two lines incl. the trailing newline", got)
	}
	want := "[truncated: 2 of 4 lines written; continue with mode='a']" +
		truncationGuidance + "\nc\nd\n..."
	if !strings.HasSuffix(res.ForLLM, want) {
		t.Fatalf("truncation note =\n%s\nwant it to end with\n%s", res.ForLLM, want)
	}
}

// TestWriteFileLineLimitKeepsCRLF pins the same rule for CRLF input: the CR of
// the cut line's terminator stays part of the written text, so the restored LF
// rebuilds the original "\r\n" instead of collapsing it to "\n".
func TestWriteFileLineLimitKeepsCRLF(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lim_crlf.txt")
	tool := NewWriteFileTool(FsConfig{MaxWriteLines: 2})

	res := tool.Execute(context.Background(), map[string]any{"path": p, "content": "a\r\nb\r\nc\r\nd"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "a\r\nb\r\n" {
		t.Fatalf("content = %q, want %q", got, "a\r\nb\r\n")
	}
}

// TestWriteFileLineLimitAppendRoundTrip checks that the cut tail appended with
// mode='a' rebuilds the original text exactly, i.e. the model must not add a
// leading newline of its own.
func TestWriteFileLineLimitAppendRoundTrip(t *testing.T) {
	original := "l1\nl2\nl3\nl4"
	dir := t.TempDir()
	p := filepath.Join(dir, "round.txt")
	tool := NewWriteFileTool(FsConfig{MaxWriteLines: 2})

	if res := tool.Execute(context.Background(), map[string]any{"path": p, "content": original}); res.IsError {
		t.Fatalf("first write failed: %s", res.ForLLM)
	}
	if res := tool.Execute(context.Background(), map[string]any{"path": p, "content": "l3\nl4", "mode": "a"}); res.IsError {
		t.Fatalf("append failed: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != original {
		t.Fatalf("content = %q, want %q", got, original)
	}
}

// TestWriteFileNoFinalNewlineNote pins the notice a successful write result
// carries when the text payload does not end with a newline: the write itself
// still reports success, the file keeps the payload byte for byte, and the model
// is told the missing final newline stays missing, plus that a follow-up append
// has to open with that newline itself. Content that already ends with a newline
// (LF or CRLF), an empty payload, a binary payload and a truncated write carry no
// such notice.
func TestWriteFileNoFinalNewlineNote(t *testing.T) {
	dir := t.TempDir()
	tool := NewWriteFileTool(FsConfig{MaxWriteLines: 100})
	ctx := context.Background()
	p := filepath.Join(dir, "no_eol.txt")

	res := tool.Execute(ctx, map[string]any{"path": p, "content": "a\nb"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.HasPrefix(res.ForLLM, "File created: ") {
		t.Fatalf("result = %q, want the success line first", res.ForLLM)
	}
	if !strings.HasSuffix(res.ForLLM, noFinalNewlineNote()) {
		t.Fatalf("result = %q, want it to end with the no-trailing-newline notice", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "If the next call appends") {
		t.Fatalf("result = %q, want the notice to explain the leading newline of an append", res.ForLLM)
	}
	if got := readFile(t, p); got != "a\nb" {
		t.Fatalf("content = %q, want %q", got, "a\nb")
	}

	// Appending a tail without a newline reports it too: the file ends there.
	res = tool.Execute(ctx, map[string]any{"path": p, "content": "\ntail", "mode": "a"})
	if res.IsError {
		t.Fatalf("append failed: %s", res.ForLLM)
	}
	if !strings.HasSuffix(res.ForLLM, noFinalNewlineNote()) {
		t.Fatalf("append result = %q, want the no-trailing-newline notice", res.ForLLM)
	}

	// Content ending with a newline needs no notice, LF and CRLF alike.
	for _, content := range []string{"a\nb\n", "a\r\nb\r\n"} {
		res = tool.Execute(ctx, map[string]any{"path": p, "content": content})
		if res.IsError {
			t.Fatalf("write %q failed: %s", content, res.ForLLM)
		}
		if strings.Contains(res.ForLLM, noFinalNewlineMarker) {
			t.Fatalf("result for %q = %q, want no notice", content, res.ForLLM)
		}
	}

	// An empty payload has no last line to report.
	res = tool.Execute(ctx, map[string]any{"path": p, "content": ""})
	if res.IsError {
		t.Fatalf("empty write failed: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, noFinalNewlineMarker) {
		t.Fatalf("empty write result = %q, want no notice", res.ForLLM)
	}

	// A truncated write restores the newline of the last kept line, so it carries
	// the truncation note only.
	limited := NewWriteFileTool(FsConfig{MaxWriteLines: 2})
	res = limited.Execute(ctx, map[string]any{"path": p, "content": "a\nb\nc"})
	if res.IsError {
		t.Fatalf("truncated write failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "[truncated: 2 of 3 lines written; continue with mode='a']") {
		t.Fatalf("truncated result = %q, want the truncation note", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, noFinalNewlineMarker) {
		t.Fatalf("truncated result = %q, want the truncation note only", res.ForLLM)
	}

	// Binary payloads are written whole and carry no line semantics.
	res = tool.Execute(ctx, map[string]any{"path": filepath.Join(dir, "bin.dat"), "content": "0a0b", "encoding": "hex"})
	if res.IsError {
		t.Fatalf("binary write failed: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, noFinalNewlineMarker) {
		t.Fatalf("binary result = %q, want no notice", res.ForLLM)
	}
}

// TestTruncationNote pins the note appended when a write is cut at the line
// limit: how many lines were written, the fixed guidance, then the dropped lines
// from the cut onwards, verbatim - a line that is empty on its own comes out as
// an empty line, while any line carrying characters (whitespace only included) is
// printed as it is and counts towards the preview - until two such lines were
// shown or the content ended, closed by an ellipsis.
func TestTruncationNote(t *testing.T) {
	cases := []struct {
		name    string
		dropped []string
		want    string
	}{
		{
			name:    "stops after two non-empty lines",
			dropped: []string{"", " testline1", "", "           testline2", "", "testline3"},
			want: "\n[truncated: 2 of 8 lines written; continue with mode='a']" +
				truncationGuidance + "\n\n testline1\n\n           testline2\n...",
		},
		{
			name:    "the content ends first",
			dropped: []string{"", "only one"},
			want: "\n[truncated: 2 of 4 lines written; continue with mode='a']" +
				truncationGuidance + "\n\nonly one\n...",
		},
		{
			name:    "whitespace-only lines print verbatim and count",
			dropped: []string{" \t ", "x", "y"},
			want: "\n[truncated: 2 of 5 lines written; continue with mode='a']" +
				truncationGuidance + "\n \t \nx\n...",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncationNote(tc.dropped, 2, 2+len(tc.dropped))
			if got != tc.want {
				t.Fatalf("truncationNote =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestEditFileLiteral(t *testing.T) {
	p := writeFile(t, t.TempDir(), "e.txt", "foo bar baz")
	tool := NewEditFileTool()

	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "bar", "content": "BAR",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "foo BAR baz" {
		t.Fatalf("content = %q", got)
	}
	if !strings.Contains(res.ForLLM, "mode=replace") {
		t.Fatalf("result does not report the mode: %s", res.ForLLM)
	}

	res = tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "missing", "content": "x",
	})
	if !res.IsError {
		t.Fatal("expected an error when find is absent")
	}
}

// TestEditFileLiteralMismatchDiagnostic verifies that when find is not found
// but a unique line-prefix exists, the error includes a diagnostic showing the
// actual file content from the last matched line onwards.
func TestEditFileLiteralMismatchDiagnostic(t *testing.T) {
	// File content:
	//   line1
	//   line2
	//   line3
	//   line4
	//   line5
	fileContent := "line1\nline2\nline3\nline4\nline5"
	p := writeFile(t, t.TempDir(), "diag.txt", fileContent)
	tool := NewEditFileTool()

	// find spans 3 lines but the 3rd line is wrong: "line3" should be "line3X"
	// The first 2 lines "line1\nline2" match uniquely, so diagnostic should fire.
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "line1\nline2\nline3X", "content": "replaced",
	})
	if !res.IsError {
		t.Fatalf("expected error, got success: %s", res.ForLLM)
	}
	// Should contain the diagnostic message.
	if !strings.Contains(res.ForLLM, "only the first 2 line(s) matched") {
		t.Fatalf("expected diagnostic about 2 matched lines, got: %s", res.ForLLM)
	}
	// The matched portion starts at line 1 (s) while the excerpt starts at the
	// last matched line (s+m-1 = 2), which is where the file stops agreeing.
	if !strings.Contains(res.ForLLM, "from line 2 (last matched line)") {
		t.Fatalf("expected the excerpt to start at line 2 (s+m-1), got: %s", res.ForLLM)
	}
	// Should show at most f-m+3 = 4 lines from line 2 (last matched line) onwards.
	if !strings.Contains(res.ForLLM, "line2\nline3") {
		t.Fatalf("expected actual file content from line 2 onwards, got: %s", res.ForLLM)
	}
	// File should not be modified.
	if got := readFile(t, p); got != fileContent {
		t.Fatalf("file was modified: %q", got)
	}
}

// TestEditFileLiteralMismatchDiagnosticNoMatch verifies that when no line-prefix
// of find matches uniquely, the original error message is returned.
func TestEditFileLiteralMismatchDiagnosticNoMatch(t *testing.T) {
	p := writeFile(t, t.TempDir(), "nomatch.txt", "aaa\nbbb\nccc")
	tool := NewEditFileTool()

	// find has 2 lines but neither line appears in the file.
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "xxx\nyyy", "content": "z",
	})
	if !res.IsError {
		t.Fatalf("expected error, got success: %s", res.ForLLM)
	}
	// Should be the original generic error, not the diagnostic.
	if !strings.Contains(res.ForLLM, "`find` not found in file. Make sure it matches exactly") {
		t.Fatalf("expected generic not-found error, got: %s", res.ForLLM)
	}
}

// TestEditFileLiteralMismatchDiagnosticSingleLine verifies that a single-line
// find that doesn't match does NOT trigger the diagnostic (needs >= 2 lines).
func TestEditFileLiteralMismatchDiagnosticSingleLine(t *testing.T) {
	p := writeFile(t, t.TempDir(), "single.txt", "hello\nworld")
	tool := NewEditFileTool()

	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "goodbye", "content": "x",
	})
	if !res.IsError {
		t.Fatalf("expected error, got success: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "`find` not found in file. Make sure it matches exactly") {
		t.Fatalf("expected generic not-found error, got: %s", res.ForLLM)
	}
}

// TestEditFileLiteralMismatchDiagnosticNonUniquePrefix verifies that when the
// longest matching prefix is not unique in the file, the diagnostic is not shown.
func TestEditFileLiteralMismatchDiagnosticNonUniquePrefix(t *testing.T) {
	// "dup" appears twice, so the prefix "dup\nnext" is not unique.
	p := writeFile(t, t.TempDir(), "dup.txt", "dup\nnext\nother\ndup\nnext\nother")
	tool := NewEditFileTool()

	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "dup\nnext\nWRONG", "content": "x",
	})
	if !res.IsError {
		t.Fatalf("expected error, got success: %s", res.ForLLM)
	}
	// The prefix "dup\nnext" appears twice, so no diagnostic should fire.
	if strings.Contains(res.ForLLM, "only the first") {
		t.Fatalf("should not show diagnostic for non-unique prefix: %s", res.ForLLM)
	}
}

// TestEditFileLiteralMismatchDiagnosticPartialLines verifies that the
// diagnostic works when the first or last line of find is not a complete
// line in the file (partial-line matching).
func TestEditFileLiteralMismatchDiagnosticPartialLines(t *testing.T) {
	// File:
	//   line1: "hello world foo"
	//   line2: "bar baz qux"
	//   line3: "test end"
	//
	// find = "world foo\nbar baz WRONG" (f=2)
	//   - First line "world foo" is a partial match of line1 (not a full line)
	//   - m=1: "world foo" appears once → s=1
	//   - Last matched line = 1, show at most f-m+3 = 4 lines from line 1
	fileContent := "hello world foo\nbar baz qux\ntest end"
	p := writeFile(t, t.TempDir(), "partial.txt", fileContent)
	tool := NewEditFileTool()

	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "world foo\nbar baz WRONG", "content": "x",
	})
	if !res.IsError {
		t.Fatalf("expected error, got success: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "only the first 1 line(s) matched") {
		t.Fatalf("expected 1 matched line, got: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "starts at line 1") {
		t.Fatalf("expected match at line 1, got: %s", res.ForLLM)
	}
	// Should show content from line 1 (partial line "world foo") onwards,
	// at most f-m+3 = 4 lines (file only has 3 lines from line 1, so all shown).
	if !strings.Contains(res.ForLLM, "world foo\nbar baz qux\ntest end") {
		t.Fatalf("expected partial-line content, got: %s", res.ForLLM)
	}
}

// TestEditFileLiteralMismatchDiagnosticCRLF verifies that the diagnostic
// works correctly when the file uses CRLF line endings. The text is
// normalized to LF before matching, so the diagnostic should report
// correct line numbers and content.
func TestEditFileLiteralMismatchDiagnosticCRLF(t *testing.T) {
	// File with CRLF:
	//   line1: "hello"
	//   line2: "world"
	//   line3: "foo"
	fileContent := "hello\r\nworld\r\nfoo\r\n"
	p := writeFile(t, t.TempDir(), "crlf_diag.txt", fileContent)
	tool := NewEditFileTool()

	// find uses LF (normalized internally), f=2
	// m=1: "world" appears once → s=2
	// Last matched line = 2, show at most f-m+3 = 4 lines from line 2
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "world\nfoo WRONG", "content": "x",
	})
	if !res.IsError {
		t.Fatalf("expected error, got success: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "only the first 1 line(s) matched") {
		t.Fatalf("expected 1 matched line, got: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "starts at line 2") {
		t.Fatalf("expected match at line 2, got: %s", res.ForLLM)
	}
	// Should show content from line 2 onwards (LF-normalized), at most 4 lines
	// (the file has only 2 lines left from line 2, so both are shown).
	if !strings.Contains(res.ForLLM, "world\nfoo") {
		t.Fatalf("expected CRLF-normalized content, got: %s", res.ForLLM)
	}
}

// TestEditFileInsertMismatchDiagnostic verifies that mode='insert' reports the
// same mismatch diagnostic as mode='replace': the diagnostic describes find
// against the file, which does not depend on what the mode would have written.
func TestEditFileInsertMismatchDiagnostic(t *testing.T) {
	fileContent := "line1\nline2\nline3\n"
	p := writeFile(t, t.TempDir(), "insert_diag.txt", fileContent)
	tool := NewEditFileTool()

	// f=3, the first 2 lines match uniquely, so the 3rd is where the file differs.
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "line1\nline2\nMISMATCH", "content": "inserted", "mode": "insert",
	})
	if !res.IsError {
		t.Fatalf("expected error, got success: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "only the first 2 line(s) matched") {
		t.Fatalf("expected the mismatch diagnostic for insert mode, got: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "starts at line 1") {
		t.Fatalf("expected match at line 1, got: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "line2\nline3") {
		t.Fatalf("expected actual file content from line 2 onwards, got: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != fileContent {
		t.Fatalf("file was modified: %q", got)
	}
}

// TestEditFileLiteralMismatchDiagnosticEmptyPrefixLine verifies the one candidate
// prefix the diagnostic skips: the empty one, which only m == 1 can produce - a
// find whose first line is blank. A longer find still has non-empty candidates
// (m >= 2), so a leading blank line does not stop the diagnostic.
func TestEditFileLiteralMismatchDiagnosticEmptyPrefixLine(t *testing.T) {
	tool := NewEditFileTool()
	cases := []struct {
		name     string
		content  string
		find     string
		wantDiag bool
	}{
		{"nonempty file, two-line find", "alpha\nbeta\n", "\ngamma", false},
		{"empty file, two-line find", "", "\ngamma", false},
		{"three-line find", "x\ngamma\ny\n", "\ngamma\nZZZ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeFile(t, t.TempDir(), "blank_first.txt", tc.content)
			res := tool.Execute(context.Background(), map[string]any{
				"path": p, "find": tc.find, "content": "x",
			})
			if !res.IsError {
				t.Fatalf("expected error, got success: %s", res.ForLLM)
			}
			if got := strings.Contains(res.ForLLM, "only the first"); got != tc.wantDiag {
				t.Fatalf("diagnostic = %v, want %v: %s", got, tc.wantDiag, res.ForLLM)
			}
			if tc.wantDiag {
				return
			}
			if !strings.Contains(res.ForLLM, "`find` not found in file. Make sure it matches exactly") {
				t.Fatalf("expected generic not-found error, got: %s", res.ForLLM)
			}
		})
	}
}

// TestEditFileLiteralMismatchDiagnosticCapsExcerpt verifies that the excerpt the
// diagnostic prints is bounded: one minified line (or a huge line) must not put
// the whole file into the error message, and a cut must not split a rune.
func TestEditFileLiteralMismatchDiagnosticCapsExcerpt(t *testing.T) {
	tool := NewEditFileTool()
	cases := []struct {
		name    string
		line    string
		wantCut bool
	}{
		{"huge single line", strings.Repeat("x", 4000), true},
		{"cut inside a multi-byte rune", strings.Repeat("中", 2000), true},
		{"short lines are never cut", "one\ntwo\nthree", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeFile(t, t.TempDir(), "wide.txt", "start\n"+tc.line+"\nend\n")
			res := tool.Execute(context.Background(), map[string]any{
				"path": p, "find": "start\nMISMATCH", "content": "x",
			})
			if !res.IsError {
				t.Fatalf("expected error, got success: %s", res.ForLLM)
			}
			if !strings.Contains(res.ForLLM, "starts at line 1") {
				t.Fatalf("expected the diagnostic, got: %s", res.ForLLM)
			}
			cut := strings.Contains(res.ForLLM, "rest of the excerpt omitted")
			if cut != tc.wantCut {
				t.Fatalf("excerpt cut = %v, want %v: %s", cut, tc.wantCut, res.ForLLM)
			}
			if !utf8.ValidString(res.ForLLM) {
				t.Fatalf("the excerpt must stay valid UTF-8, got: %q", res.ForLLM)
			}
			if len(res.ForLLM) > 2*mismatchDiagnosticMaxBytes {
				t.Fatalf("excerpt is not bounded: %d bytes", len(res.ForLLM))
			}
		})
	}
}

func TestEditFileLiteralRequiresUnique(t *testing.T) {
	p := writeFile(t, t.TempDir(), "u.txt", "x x")
	tool := NewEditFileTool()
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "x", "content": "y",
	})
	if !res.IsError {
		t.Fatal("expected an error when find is ambiguous")
	}
	// The error points at the mode that accepts several matches.
	if !strings.Contains(res.ForLLM, "mode='regex'") {
		t.Fatalf("error does not suggest regex mode: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "x x" {
		t.Fatalf("ambiguous edit modified the file: %q", got)
	}
}

func TestEditFileRegex(t *testing.T) {
	p := writeFile(t, t.TempDir(), "r.txt", "a1 b2 c3")
	tool := NewEditFileTool()
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": `([a-z])(\d)`, "content": "$2$1", "mode": "regex",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "1a 2b 3c" {
		t.Fatalf("content = %q", got)
	}
}

// TestEditFileRegexAllowsNonUnique pins the difference from literal mode: a
// pattern matching several times is not an error, every match is replaced and
// the result reports how many were matched.
func TestEditFileRegexAllowsNonUnique(t *testing.T) {
	p := writeFile(t, t.TempDir(), "r_many.txt", "a1 b2 c3")
	tool := NewEditFileTool()
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": `[a-z]`, "content": "X", "mode": "regex",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "X1 X2 X3" {
		t.Fatalf("content = %q", got)
	}
	if !strings.Contains(res.ForLLM, "matched 3 occurrence(s)") {
		t.Fatalf("result does not report the match count: %s", res.ForLLM)
	}
}

// TestEditFileInsertBeforeMatch pins insert mode: content lands in front of the
// match and the match itself stays untouched, so the same find can be used
// again.
func TestEditFileInsertBeforeMatch(t *testing.T) {
	p := writeFile(t, t.TempDir(), "ins.txt", "alpha beta gamma")
	tool := NewEditFileTool()

	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "beta", "content": "BEFORE ", "mode": "insert",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "alpha BEFORE beta gamma" {
		t.Fatalf("content = %q", got)
	}

	res = tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "beta", "content": "AGAIN ", "mode": "insert",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "alpha BEFORE AGAIN beta gamma" {
		t.Fatalf("content = %q", got)
	}
}

// TestEditFileInsertRequiresUnique keeps the literal uniqueness rule in insert
// mode: an ambiguous target is rejected and the file is left alone. The message
// is the shared one, so it points at mode='regex' exactly like replace does.
func TestEditFileInsertRequiresUnique(t *testing.T) {
	p := writeFile(t, t.TempDir(), "ins_u.txt", "x x")
	tool := NewEditFileTool()
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "x", "content": "y", "mode": "insert",
	})
	if !res.IsError {
		t.Fatal("expected an error when the insert target is ambiguous")
	}
	if !strings.Contains(res.ForLLM, "mode='regex'") {
		t.Fatalf("error does not suggest regex mode: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "x x" {
		t.Fatalf("ambiguous insert modified the file: %q", got)
	}
}

// TestEditFileInvalidMode pins the argument contract: an unknown mode value is
// rejected before the file is touched.
func TestEditFileInvalidMode(t *testing.T) {
	p := writeFile(t, t.TempDir(), "m.txt", "one")
	tool := NewEditFileTool()

	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "one", "content": "two", "mode": "replace-all",
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "invalid mode") {
		t.Fatalf("unknown mode was not rejected: %+v", res)
	}
	if got := readFile(t, p); got != "one" {
		t.Fatalf("an invalid call modified the file: %q", got)
	}
}

func TestGbkRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gbk.txt")

	wtool := NewWriteFileTool(FsConfig{MaxWriteLines: 100})
	res := wtool.Execute(context.Background(), map[string]any{
		"path": p, "content": "中文测试", "encoding": "gbk",
	})
	if res.IsError {
		t.Fatalf("write failed: %s", res.ForLLM)
	}
	if got := readFile(t, p); got == "中文测试" {
		t.Fatal("gbk write produced UTF-8 bytes; encoding was ignored")
	}

	rtool := NewReadFileLinesTool(FsConfig{MaxReadFileSize: 65536, MaxReadFileLines: 100})
	res = rtool.Execute(context.Background(), map[string]any{"path": p, "encoding": "gbk"})
	if res.IsError {
		t.Fatalf("read failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "中文测试") {
		t.Fatalf("decoded content missing: %s", res.ForLLM)
	}
}

func TestEditFileGbk(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "g.txt")
	data, err := encodeTextToFileBytes("你好世界", "gbk")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewEditFileTool()
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "世界", "content": "地球", "encoding": "gbk",
	})
	if res.IsError {
		t.Fatalf("edit failed: %s", res.ForLLM)
	}

	got, err := decodeFileBytesToText([]byte(readFile(t, p)), "gbk")
	if err != nil {
		t.Fatal(err)
	}
	if got != "你好地球" {
		t.Fatalf("decoded = %q, want 你好地球", got)
	}
}

func TestEditFileCRLFPreserved(t *testing.T) {
	p := writeFile(t, t.TempDir(), "crlf.txt", "one\r\ntwo\r\n")
	tool := NewEditFileTool()
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "two", "content": "TWO",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "one\r\nTWO\r\n" {
		t.Fatalf("CRLF not preserved: %q", got)
	}
}

// TestEditFileInsertCRLFPreserved keeps the line-ending contract in insert
// mode too: the restored CRLF file gets CRLF line endings inside the inserted
// text as well.
func TestEditFileInsertCRLFPreserved(t *testing.T) {
	p := writeFile(t, t.TempDir(), "crlf_ins.txt", "one\r\ntwo\r\n")
	tool := NewEditFileTool()
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "two", "content": "mid\n", "mode": "insert",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "one\r\nmid\r\ntwo\r\n" {
		t.Fatalf("CRLF not preserved: %q", got)
	}
}

// TestEditFileBinaryInsert covers the byte-level path: hex payloads are matched
// and content is inserted in front of the find bytes.
func TestEditFileBinaryInsert(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bin.dat")
	if err := os.WriteFile(p, []byte{0x00, 0x01, 0x02, 0x03}, 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewEditFileTool()
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "0102", "content": "ff", "mode": "insert", "encoding": "hex",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{0x00, 0xff, 0x01, 0x02, 0x03}; string(got) != string(want) {
		t.Fatalf("bytes = %x, want %x", got, want)
	}

	res = tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "0102", "content": "ff", "mode": "regex", "encoding": "hex",
	})
	if !res.IsError {
		t.Fatal("expected an error: regex mode is not available for binary payloads")
	}
}

// TestSplitWriteContentScale pins the chunking of a payload far beyond the
// limit: a 500-line write under the default 200-line limit becomes three calls
// that each stay inside the limit, and the concatenation is the payload again.
func TestSplitWriteContentScale(t *testing.T) {
	lines := make([]string, 500)
	for i := range lines {
		lines[i] = fmt.Sprintf("l%d", i+1)
	}
	payload := strings.Join(lines, "\n")

	chunks := splitWriteContent(payload, 200)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 for 500 lines under a 200-line limit", len(chunks))
	}
	if got := strings.Join(chunks, ""); got != payload {
		t.Fatalf("chunks do not rebuild the payload")
	}
	for i, chunk := range chunks {
		if _, _, _, truncated := enforceWriteLineLimit(chunk, 200); truncated {
			t.Fatalf("chunk %d exceeds the per-call limit", i+1)
		}
	}
}

func TestPlanWriteCallsNoSplit(t *testing.T) {
	tool := NewWriteFileTool(FsConfig{MaxWriteLines: 5})
	cases := []struct {
		name string
		args map[string]any
	}{
		{"fits in one call", map[string]any{"path": "a.txt", "content": "l1\nl2\nl3\nl4"}},
		{"exactly the limit", map[string]any{"path": "a.txt", "content": "l1\nl2\nl3\nl4\nl5"}},
		{"empty payload", map[string]any{"path": "a.txt", "content": ""}},
		{"no content", map[string]any{"path": "a.txt"}},
		{"encoded payload", map[string]any{"path": "a.txt", "encoding": "base64", "content": strings.Repeat("AA\n", 20)}},
	}
	for _, tc := range cases {
		if _, split := tool.PlanWriteCalls(tc.args); split {
			t.Errorf("%s: PlanWriteCalls split a call it should leave alone", tc.name)
		}
	}
}

// TestPlanWriteCallsChain writes a payload longer than the per-call limit through
// the planned calls: every call stays inside the limit on its own (the tool adds
// no truncation note), and the file ends up byte-identical to the payload.
func TestPlanWriteCallsChain(t *testing.T) {
	tool := NewWriteFileTool(FsConfig{MaxWriteLines: 5})
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		content string
		mode    string
	}{
		{"twelve lines", strings.Join([]string{"l1", "l2", "l3", "l4", "l5", "l6", "l7", "l8", "l9", "l10", "l11", "l12"}, "\n"), ""},
		{"trailing newline", "l1\nl2\nl3\nl4\nl5\nl6\nl7\n", ""},
		{"crlf", "l1\r\nl2\r\nl3\r\nl4\r\nl5\r\nl6\r\nl7\r\nl8", ""},
		{"append mode", "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10", "a"},
		{"create mode", "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10", "c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "chain.txt")
			args := map[string]any{"path": p, "content": tc.content}
			if tc.mode != "" {
				args["mode"] = tc.mode
			}

			calls, split := tool.PlanWriteCalls(args)
			if !split {
				t.Fatalf("PlanWriteCalls did not split a payload longer than the limit")
			}
			if len(calls) < 2 {
				t.Fatalf("planned calls = %d, want a chain", len(calls))
			}
			// The model's own arguments must stay untouched, and the planned
			// calls must not share one map.
			if args["content"] != tc.content {
				t.Fatalf("PlanWriteCalls mutated the caller's content")
			}
			if got := calls[0]["mode"]; got != args["mode"] {
				t.Fatalf("first call mode = %v, want the requested %v", got, args["mode"])
			}
			for i, call := range calls {
				if i > 0 && call["mode"] != "a" {
					t.Fatalf("call %d mode = %v, want append", i+1, call["mode"])
				}
				chunk, _ := call["content"].(string)
				if _, _, _, truncated := enforceWriteLineLimit(chunk, 5); truncated {
					t.Fatalf("call %d carries more lines than one call accepts", i+1)
				}
			}

			for i, call := range calls {
				res := tool.Execute(ctx, call)
				if res.IsError {
					t.Fatalf("call %d failed: %s", i+1, res.ForLLM)
				}
				if strings.Contains(res.ForLLM, "truncated") {
					t.Fatalf("call %d reported truncation: %s", i+1, res.ForLLM)
				}
			}
			if got := readFile(t, p); got != tc.content {
				t.Fatalf("content = %q, want the original payload %q", got, tc.content)
			}
		})
	}
}
