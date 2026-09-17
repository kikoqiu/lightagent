package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"path": p, "old_text": "bar", "new_text": "BAR",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "foo BAR baz" {
		t.Fatalf("content = %q", got)
	}

	res = tool.Execute(context.Background(), map[string]any{
		"path": p, "old_text": "missing", "new_text": "x",
	})
	if !res.IsError {
		t.Fatal("expected an error when old_text is absent")
	}
}

func TestEditFileLiteralRequiresUnique(t *testing.T) {
	p := writeFile(t, t.TempDir(), "u.txt", "x x")
	tool := NewEditFileTool()
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "old_text": "x", "new_text": "y",
	})
	if !res.IsError {
		t.Fatal("expected an error when old_text is ambiguous")
	}
}

func TestEditFileRegex(t *testing.T) {
	p := writeFile(t, t.TempDir(), "r.txt", "a1 b2 c3")
	tool := NewEditFileTool()
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "old_text": `([a-z])(\d)`, "new_text": "$2$1", "regex": true,
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "1a 2b 3c" {
		t.Fatalf("content = %q", got)
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
		"path": p, "old_text": "世界", "new_text": "地球", "encoding": "gbk",
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
		"path": p, "old_text": "two", "new_text": "TWO",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "one\r\nTWO\r\n" {
		t.Fatalf("CRLF not preserved: %q", got)
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
