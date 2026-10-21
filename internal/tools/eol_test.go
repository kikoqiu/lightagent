package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// eolRewriteField is the fragment the result's first line carries whenever an
// eol request really rewrote a payload's endings.
const eolRewriteField = " | eol: content written with "

// TestReadFileDescriptionNamesCompleteEndings pins what the model is told about
// read_file rows, and that both writers advertise the eol argument with the
// follow-the-file default.
func TestReadFileDescriptionNamesCompleteEndings(t *testing.T) {
	desc := NewReadFileLinesTool(FsConfig{}).Description()
	if !strings.Contains(desc, "complete line ending") {
		t.Errorf("read_file does not say rows carry their complete line endings: %s", desc)
	}
	for _, tool := range []Tool{NewWriteFileTool(FsConfig{}), NewEditFileTool()} {
		props, ok := tool.Parameters()["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no properties", tool.Name())
		}
		eol, ok := props["eol"].(map[string]any)
		if !ok {
			t.Fatalf("%s does not advertise an eol argument", tool.Name())
		}
		if eol["default"] != eolAuto {
			t.Errorf("%s eol default = %v, want %q", tool.Name(), eol["default"], eolAuto)
		}
		if !strings.Contains(tool.Description(), "eol") {
			t.Errorf("%s does not mention eol in its description", tool.Name())
		}
	}
}

// TestFileToolTextNamesEndingsByLabel pins how the three file tools talk about
// line endings: always by label (LF, CRLF, CR), never by a backslash escape. A
// tool call is not always JSON — an XML call carries the value between tags
// literally — so a description that spells an ending as an escape teaches a
// model to send two literal characters instead of a break.
func TestFileToolTextNamesEndingsByLabel(t *testing.T) {
	for _, tool := range []Tool{NewReadFileLinesTool(FsConfig{}), NewWriteFileTool(FsConfig{}), NewEditFileTool()} {
		text := tool.Description()
		props, _ := tool.Parameters()["properties"].(map[string]any)
		for name, raw := range props {
			prop, _ := raw.(map[string]any)
			desc, _ := prop["description"].(string)
			text += " | " + name + ": " + desc
		}
		if strings.Contains(text, "\\n") || strings.Contains(text, "\\r") {
			t.Errorf("%s spells a line ending as a backslash escape: %s", tool.Name(), text)
		}
		for _, want := range []string{"LF", "CRLF"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not name %s: %s", tool.Name(), want, text)
			}
		}
		if strings.Contains(text, "backslash") || strings.Contains(text, "JSON escaping") {
			t.Errorf("%s talks about argument escaping instead of line endings: %s", tool.Name(), text)
		}
	}
}

// TestParseEOLArg pins the spellings the eol argument accepts: following the file
// ('auto', the default), leaving the payload alone ('keep'), the three ending
// labels, and an ending's own bytes. Anything else is rejected with a message
// that names the accepted values.
func TestParseEOLArg(t *testing.T) {
	accepted := []struct {
		raw  string
		want string
	}{
		{"", eolAuto},
		{"auto", eolAuto},
		{" Auto ", eolAuto},
		{"keep", eolKeep},
		{"KEEP", eolKeep},
		{"lf", "\n"},
		{"crlf", "\r\n"},
		{"cr", "\r"},
		{"\n", "\n"},
		{"\r\n", "\r\n"},
		{"\r", "\r"},
	}
	for _, tc := range accepted {
		got, err := parseEOLArg(tc.raw)
		if err != nil {
			t.Errorf("parseEOLArg(%q) failed: %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseEOLArg(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	_, err := parseEOLArg("windows")
	if err == nil {
		t.Fatal("an unknown eol value was accepted")
	}
	for _, want := range []string{"'auto'", "'keep'", "'lf'", "'crlf'", "'cr'"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not offer %s: %v", want, err)
		}
	}
}

// TestWriteFileEOLExplicit pins the explicit values: content's own row endings are
// rewritten as asked, a payload with nothing to rewrite is left alone, and only a
// real rewrite is reported on the result's first line.
func TestWriteFileEOLExplicit(t *testing.T) {
	cases := []struct {
		name, eol, content, want, spelling string
		reported                           bool
	}{
		{name: "crlf", eol: "crlf", content: "a\nb\n", want: "a\r\nb\r\n", spelling: "CRLF", reported: true},
		{name: "lf", eol: "lf", content: "a\r\nb\r\n", want: "a\nb\n", spelling: "LF", reported: true},
		{name: "cr", eol: "cr", content: "a\nb\n", want: "a\rb\r", spelling: "CR", reported: true},
		{name: "single-line", eol: "crlf", content: "one line", want: "one line"},
		{name: "already-that-ending", eol: "crlf", content: "a\r\n", want: "a\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "w.txt")
			res := NewWriteFileTool(FsConfig{MaxWriteLines: 100}).Execute(context.Background(), map[string]any{
				"path": p, "content": tc.content, "eol": tc.eol,
			})
			if res.IsError {
				t.Fatalf("write failed: %s", res.ForLLM)
			}
			if got := readFile(t, p); got != tc.want {
				t.Errorf("file = %q, want %q", got, tc.want)
			}
			if got := strings.Contains(res.ForLLM, eolRewriteField+tc.spelling+", as asked"); got != tc.reported {
				t.Errorf("eol reported = %v, want %v: %s", got, tc.reported, res.ForLLM)
			}
		})
	}
}

// TestWriteFileEOLKeepKeepsThePayload pins eol='keep': content is written exactly
// as given, whatever the file's own endings are, and nothing is reported.
func TestWriteFileEOLKeepKeepsThePayload(t *testing.T) {
	p := writeFile(t, t.TempDir(), "k.txt", "a\r\n")
	res := NewWriteFileTool(FsConfig{MaxWriteLines: 100}).Execute(context.Background(), map[string]any{
		"path": p, "content": "b\n", "mode": "a", "eol": "keep",
	})
	if res.IsError {
		t.Fatalf("append failed: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "a\r\nb\n" {
		t.Errorf("file = %q, want the payload verbatim", got)
	}
	if strings.Contains(res.ForLLM, eolRewriteField) {
		t.Errorf("eol=keep reported a rewrite: %s", res.ForLLM)
	}
}

// TestWriteFileEOLAutoFollowsTheFile pins the default: content is written with the
// ending the file's own first row break uses — a CRLF file stays CRLF, a mixed
// file follows its first break, and a new file (nothing to follow) gets the
// payload as given. Only a real rewrite is reported.
func TestWriteFileEOLAutoFollowsTheFile(t *testing.T) {
	dir := t.TempDir()
	tool := NewWriteFileTool(FsConfig{MaxWriteLines: 100})

	// Append: the file's CRLF is followed, and the result says so.
	p := writeFile(t, dir, "crlf.txt", "a\r\nb\r\n")
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "content": "c\nd\n", "mode": "a",
	})
	if res.IsError {
		t.Fatalf("append failed: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "a\r\nb\r\nc\r\nd\r\n" {
		t.Errorf("appended file = %q, want CRLF throughout", got)
	}
	if !strings.Contains(res.ForLLM, eolRewriteField+"CRLF, following the file") {
		t.Errorf("the rewrite was not reported: %s", res.ForLLM)
	}

	// Overwrite follows the file too: its ending is read before the write.
	res = tool.Execute(context.Background(), map[string]any{
		"path": p, "content": "x\ny\n",
	})
	if res.IsError {
		t.Fatalf("overwrite failed: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "x\r\ny\r\n" {
		t.Errorf("overwritten file = %q, want CRLF", got)
	}

	// A new file has nothing to follow: the payload is written as given, silently.
	fresh := filepath.Join(dir, "fresh.txt")
	res = tool.Execute(context.Background(), map[string]any{
		"path": fresh, "content": "x\ny\n",
	})
	if res.IsError {
		t.Fatalf("create failed: %s", res.ForLLM)
	}
	if got := readFile(t, fresh); got != "x\ny\n" {
		t.Errorf("new file = %q, want the payload as given", got)
	}
	if strings.Contains(res.ForLLM, eolRewriteField) {
		t.Errorf("nothing was rewritten, but it was reported: %s", res.ForLLM)
	}

	// A mixed file still names one convention: its first break (LF here).
	mixed := writeFile(t, dir, "mixed.txt", "a\nb\r\n")
	res = tool.Execute(context.Background(), map[string]any{
		"path": mixed, "content": "x\r\ny\r\n", "mode": "a",
	})
	if res.IsError {
		t.Fatalf("append failed: %s", res.ForLLM)
	}
	if got := readFile(t, mixed); got != "a\nb\r\nx\ny\n" {
		t.Errorf("mixed file = %q, want the payload written with its first break", got)
	}
	if !strings.Contains(res.ForLLM, eolRewriteField+"LF, following the file") {
		t.Errorf("the first-break rewrite was not reported: %s", res.ForLLM)
	}
}

// TestWriteFileEOLDefaultIsAutoForBinaryPayloads pins that the default is a no-op
// for a byte payload (only an explicit ending conflicts with it), so a hex/base64
// write keeps working without the argument.
func TestWriteFileEOLDefaultIsAutoForBinaryPayloads(t *testing.T) {
	p := filepath.Join(t.TempDir(), "b.dat")
	tool := NewWriteFileTool(FsConfig{})
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "content": "0d0a", "encoding": "hex",
	})
	if res.IsError {
		t.Fatalf("a binary write failed without an eol request: %s", res.ForLLM)
	}
	if got, err := os.ReadFile(p); err != nil || string(got) != "\r\n" {
		t.Fatalf("bytes = %q (%v), want CRLF", got, err)
	}

	res = tool.Execute(context.Background(), map[string]any{
		"path": p, "content": "0d0a", "encoding": "hex", "eol": "crlf",
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "eol is only available for text content") {
		t.Fatalf("an explicit ending on a hex payload was not rejected: %+v", res)
	}
}

// TestEditFileEOLAutoKeepsTheFileUniform pins the default on a CRLF file: content
// written with LF is rewritten with the file's own ending, so the CRLF file stays
// CRLF, no mixing is reported, and the first line names the ending.
func TestEditFileEOLAutoKeepsTheFileUniform(t *testing.T) {
	p := writeFile(t, t.TempDir(), "auto.txt", "one\r\ntwo\r\n")
	res := NewEditFileTool().Execute(context.Background(), map[string]any{
		"path": p, "find": "two", "content": "mid\nnext\n", "mode": "insert",
	})
	if res.IsError {
		t.Fatalf("edit failed: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "one\r\nmid\r\nnext\r\ntwo\r\n" {
		t.Errorf("file = %q, want CRLF throughout", got)
	}
	if strings.Contains(res.ForLLM, "mix endings") {
		t.Errorf("content was rewritten but a mix was reported: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, eolRewriteField+"CRLF, following the file") {
		t.Errorf("the rewrite was not reported: %s", res.ForLLM)
	}
}

// TestEditFileEOLExplicitAndKeep pins the rest of the contract: eol='keep' writes
// content verbatim (the file may mix, and that is reported), while an explicit
// ending rewrites the payload and is named on the first line.
func TestEditFileEOLExplicitAndKeep(t *testing.T) {
	// keep: verbatim, with the mix reported and nothing said about eol.
	p := writeFile(t, t.TempDir(), "keep.txt", "one\r\ntwo\r\n")
	res := NewEditFileTool().Execute(context.Background(), map[string]any{
		"path": p, "find": "two", "content": "mid\n", "mode": "insert", "eol": "keep",
	})
	if res.IsError {
		t.Fatalf("edit failed: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "one\r\nmid\ntwo\r\n" {
		t.Errorf("file = %q, want the content verbatim", got)
	}
	if !strings.Contains(res.ForLLM, "mix endings") {
		t.Errorf("the mixed result was not reported: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, eolRewriteField) {
		t.Errorf("eol=keep reported a rewrite: %s", res.ForLLM)
	}

	// eol='crlf': the payload is rewritten instead, so the file stays uniform.
	p = writeFile(t, t.TempDir(), "asked.txt", "one\r\ntwo\r\n")
	res = NewEditFileTool().Execute(context.Background(), map[string]any{
		"path": p, "find": "two", "content": "mid\n", "mode": "insert", "eol": "crlf",
	})
	if res.IsError {
		t.Fatalf("edit failed: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "one\r\nmid\r\ntwo\r\n" {
		t.Errorf("file = %q, want CRLF throughout", got)
	}
	if !strings.Contains(res.ForLLM, eolRewriteField+"CRLF, as asked") {
		t.Errorf("the rewrite was not reported: %s", res.ForLLM)
	}
}

// TestEditFileEOLAutoFollowsTheFirstBreak pins 'auto' on a file that mixes
// endings: its first break names the convention. Content already written that way
// is left alone (and nothing is said); content written the other way is rewritten.
func TestEditFileEOLAutoFollowsTheFirstBreak(t *testing.T) {
	// First break LF, content LF: nothing to rewrite, nothing reported.
	p := writeFile(t, t.TempDir(), "mix.txt", "a\nb\r\n")
	res := NewEditFileTool().Execute(context.Background(), map[string]any{
		"path": p, "find": "b\r\n", "content": "B\n",
	})
	if res.IsError {
		t.Fatalf("edit failed: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "a\nB\n" {
		t.Errorf("file = %q, want the content as given", got)
	}
	if strings.Contains(res.ForLLM, eolRewriteField) {
		t.Errorf("nothing was rewritten, but it was reported: %s", res.ForLLM)
	}

	// First break CRLF, content LF: the content follows that break.
	p = writeFile(t, t.TempDir(), "mix2.txt", "a\r\nb\n")
	res = NewEditFileTool().Execute(context.Background(), map[string]any{
		"path": p, "find": "b\n", "content": "B\n",
	})
	if res.IsError {
		t.Fatalf("edit failed: %s", res.ForLLM)
	}
	if got := readFile(t, p); got != "a\r\nB\r\n" {
		t.Errorf("file = %q, want the content written with the first break", got)
	}
	if !strings.Contains(res.ForLLM, eolRewriteField+"CRLF, following the file") {
		t.Errorf("the rewrite was not reported: %s", res.ForLLM)
	}
}

// TestEditFileEOLDefaultIsAutoForBinaryPayloads pins that the default is a no-op
// for a byte edit, and that only an explicit ending conflicts with it.
func TestEditFileEOLDefaultIsAutoForBinaryPayloads(t *testing.T) {
	p := filepath.Join(t.TempDir(), "b.dat")
	if err := os.WriteFile(p, []byte{0x00, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewEditFileTool()
	res := tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "0001", "content": "ff", "encoding": "hex",
	})
	if res.IsError {
		t.Fatalf("a binary edit failed without an eol request: %s", res.ForLLM)
	}
	if got, err := os.ReadFile(p); err != nil || string(got) != string([]byte{0xff}) {
		t.Fatalf("bytes = %x (%v), want ff", got, err)
	}

	res = tool.Execute(context.Background(), map[string]any{
		"path": p, "find": "ff", "content": "ee", "encoding": "hex", "eol": "lf",
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "eol is only available for text edits") {
		t.Fatalf("an explicit ending on a hex payload was not rejected: %+v", res)
	}
}

// TestPlanWriteCallsKeepsEOL pins that a split write carries the eol request into
// every continuation, so all the chunks are written with the same ending.
func TestPlanWriteCallsKeepsEOL(t *testing.T) {
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = fmt.Sprintf("l%d", i+1)
	}
	tool := NewWriteFileTool(FsConfig{MaxWriteLines: 5})
	calls, split := tool.PlanWriteCalls(map[string]any{
		"path": "p.txt", "content": strings.Join(lines, "\n"), "eol": "keep",
	})
	if !split || len(calls) < 2 {
		t.Fatalf("the payload was not split: split=%v calls=%d", split, len(calls))
	}
	for i, call := range calls {
		if call["eol"] != "keep" {
			t.Errorf("call %d lost the eol request: %v", i+1, call["eol"])
		}
	}
}
