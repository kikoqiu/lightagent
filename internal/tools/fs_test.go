package tools

import (
	"context"
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
