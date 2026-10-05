package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
)

// writeRawFile writes raw bytes (an encoded fixture) to a temp file.
func writeRawFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// encodeWide encodes text through a wide codec (UTF-16/UTF-32) into file bytes.
func encodeWide(t *testing.T, text string, enc encoding.Encoding) []byte {
	t.Helper()
	data, err := enc.NewEncoder().Bytes([]byte(text))
	if err != nil {
		t.Fatalf("encode wide: %v", err)
	}
	return data
}

// utf16Bytes encodes text as UTF-16 with or without a byte order mark.
func utf16Bytes(t *testing.T, text string, order unicode.Endianness, withBOM bool) []byte {
	t.Helper()
	policy := unicode.IgnoreBOM
	if withBOM {
		policy = unicode.UseBOM
	}
	return encodeWide(t, text, unicode.UTF16(order, policy))
}

// readFileBody returns the content rows of a read_file result, i.e. everything
// after the blank line that separates them from the header/footer.
func readFileBody(t *testing.T, forLLM string) string {
	t.Helper()
	i := strings.Index(forLLM, "\n\n")
	if i < 0 {
		t.Fatalf("result has no header/content separator: %q", forLLM)
	}
	return forLLM[i+2:]
}

// TestReadFileAutoDetectsUTF16 is the regression test for the "decode first,
// then split lines" order: both byte orders, with and without a byte order mark,
// with LF and CRLF endings. Every line has to come back exactly as written. The
// old per-byte 0x0A split garbled every line after the first because a UTF-16
// newline is 0A 00 / 00 0A.
func TestReadFileAutoDetectsUTF16(t *testing.T) {
	lines := []string{"alpha one 第一", "beta two 第二", "gamma three 第三"}
	eols := map[string]string{"lf": "\n", "crlf": "\r\n"}
	orders := []struct {
		name string
		val  unicode.Endianness
		enc  string
	}{{"le", unicode.LittleEndian, "utf-16le"}, {"be", unicode.BigEndian, "utf-16be"}}

	for _, order := range orders {
		for _, bom := range []bool{true, false} {
			for eolName, eol := range eols {
				name := fmt.Sprintf("%s/bom=%t/%s", order.name, bom, eolName)
				t.Run(name, func(t *testing.T) {
					text := strings.Join(lines, eol)
					p := writeRawFile(t, t.TempDir(), "u16.txt", utf16Bytes(t, text, order.val, bom))
					res := NewReadFileLinesTool(FsConfig{}).Execute(context.Background(), map[string]any{"path": p})
					if res.IsError {
						t.Fatalf("read failed: %s", res.ForLLM)
					}
					if !strings.Contains(res.ForLLM, "encoding: "+order.enc) {
						t.Errorf("header does not name %s: %s", order.enc, res.ForLLM)
					}
					wantEOL := map[string]string{"lf": "LF", "crlf": "CRLF"}[eolName]
					if !strings.Contains(res.ForLLM, "eol: "+wantEOL) {
						t.Errorf("header does not report %s endings: %s", wantEOL, res.ForLLM)
					}
					if got := readFileBody(t, res.ForLLM); got != strings.Join(lines, "\n") {
						t.Errorf("content = %q, want %q", got, strings.Join(lines, "\n"))
					}
				})
			}
		}
	}
}

// TestReadFileUTF16SingleLine covers the shapes that used to "accidentally"
// work: one line with no terminator, and one line that ends in CRLF.
func TestReadFileUTF16SingleLine(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"no-terminator", "the only line"},
		{"crlf", "the only line\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeRawFile(t, t.TempDir(), "one.txt", utf16Bytes(t, tc.text, unicode.LittleEndian, true))
			res := NewReadFileLinesTool(FsConfig{}).Execute(context.Background(), map[string]any{"path": p})
			if res.IsError {
				t.Fatalf("read failed: %s", res.ForLLM)
			}
			if got := readFileBody(t, res.ForLLM); got != "the only line" {
				t.Errorf("content = %q, want %q", got, "the only line")
			}
		})
	}
}

// TestReadFileBOMStrippedEverywhere checks that a byte order mark never leaks
// into the content, for UTF-8, UTF-16 and UTF-32, with auto-detection and an
// explicit label.
func TestReadFileBOMStrippedEverywhere(t *testing.T) {
	fixtures := map[string][]byte{
		"utf8":    append([]byte{0xEF, 0xBB, 0xBF}, []byte("first\nsecond\n")...),
		"utf16le": utf16Bytes(t, "first\nsecond\n", unicode.LittleEndian, true),
		"utf16be": utf16Bytes(t, "first\nsecond\n", unicode.BigEndian, true),
		"utf32le": encodeWide(t, "first\nsecond\n", utf32.UTF32(utf32.LittleEndian, utf32.UseBOM)),
		"utf32be": encodeWide(t, "first\nsecond\n", utf32.UTF32(utf32.BigEndian, utf32.UseBOM)),
	}
	labels := map[string]string{"utf16le": "utf-16le", "utf16be": "utf-16be", "utf32le": "utf-32le", "utf32be": "utf-32be"}
	for name, data := range fixtures {
		p := writeRawFile(t, t.TempDir(), name+".txt", data)
		want := "first\nsecond"
		t.Run(name+"/auto", func(t *testing.T) {
			res := NewReadFileLinesTool(FsConfig{}).Execute(context.Background(), map[string]any{"path": p})
			if res.IsError {
				t.Fatalf("read failed: %s", res.ForLLM)
			}
			got := readFileBody(t, res.ForLLM)
			if got != want {
				t.Errorf("content = %q, want %q", got, want)
			}
			if strings.Contains(got, "\uFEFF") {
				t.Errorf("byte order mark leaked into the content: %q", got)
			}
		})
		if label, ok := labels[name]; ok {
			t.Run(name+"/explicit", func(t *testing.T) {
				res := NewReadFileLinesTool(FsConfig{}).Execute(context.Background(), map[string]any{"path": p, "encoding": label})
				if res.IsError {
					t.Fatalf("read failed: %s", res.ForLLM)
				}
				if got := readFileBody(t, res.ForLLM); got != want {
					t.Errorf("content = %q, want %q", got, want)
				}
			})
		}
	}
}

// TestReadFilePaginationUTF16 jets to line 100 of a UTF-16 file and checks that
// the reported line numbers and the rows line up with the original text.
func TestReadFilePaginationUTF16(t *testing.T) {
	var lines []string
	for i := 1; i <= 200; i++ {
		lines = append(lines, fmt.Sprintf("row %03d 第%d行", i, i))
	}
	text := strings.Join(lines, "\r\n")
	p := writeRawFile(t, t.TempDir(), "big.txt", utf16Bytes(t, text, unicode.BigEndian, true))

	res := NewReadFileLinesTool(FsConfig{}).Execute(context.Background(), map[string]any{
		"path": p, "start_line": 100, "max_lines": 3,
	})
	if res.IsError {
		t.Fatalf("read failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "lines 100-102") {
		t.Errorf("header range wrong: %s", res.ForLLM)
	}
	want := strings.Join(lines[99:102], "\n")
	if got := readFileBody(t, res.ForLLM); got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

// TestReadFileLegacyCharsetLineSplit keeps a multi-byte character right next to
// a line terminator: because the stream is decoded before it is split, the
// character stays whole regardless of the charset.
func TestReadFileLegacyCharsetLineSplit(t *testing.T) {
	cases := []struct{ label, text string }{
		{"gbk", "中文测试\n第二行中文\n尾行世界"},
		{"shift_jis", "こんにちは世界\n二行目です\n末尾"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			data, err := encodeTextToFileBytes(tc.text, tc.label)
			if err != nil {
				t.Fatalf("encode %s: %v", tc.label, err)
			}
			p := writeRawFile(t, t.TempDir(), "legacy.txt", data)
			res := NewReadFileLinesTool(FsConfig{}).Execute(context.Background(), map[string]any{
				"path": p, "encoding": tc.label,
			})
			if res.IsError {
				t.Fatalf("read failed: %s", res.ForLLM)
			}
			if got := readFileBody(t, res.ForLLM); got != tc.text {
				t.Errorf("content = %q, want %q", got, tc.text)
			}
		})
	}
}

// TestReadFileByteBudgetKeepsRunesWhole shrinks the byte budget into the middle
// of a multi-byte character and asserts the cut lands on a rune boundary.
func TestReadFileByteBudgetKeepsRunesWhole(t *testing.T) {
	p := writeFile(t, t.TempDir(), "runes.txt", "你好世界\nnext\n")
	tool := NewReadFileLinesTool(FsConfig{MaxReadFileSize: 5, MaxReadFileLines: 100})

	res := tool.Execute(context.Background(), map[string]any{"path": p})
	if res.IsError {
		t.Fatalf("read failed: %s", res.ForLLM)
	}
	body := readFileBody(t, res.ForLLM)
	if !utf8.ValidString(body) {
		t.Fatalf("the byte cut split a rune: %q", body)
	}
	if body != "你" {
		t.Errorf("content = %q, want %q", body, "你")
	}
	if !strings.Contains(res.ForLLM, "TRUNCATED") {
		t.Errorf("missing TRUNCATED marker: %s", res.ForLLM)
	}
}

// TestReadFileBinaryStillRejected checks that a real binary blob is still
// refused and that the hint points at encoding=utf-16.
func TestReadFileBinaryStillRejected(t *testing.T) {
	data := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x01, 0x00, 0xFF, 0xD8, 0xFF, 0xE0,
	}
	p := writeRawFile(t, t.TempDir(), "blob.bin", data)
	res := NewReadFileLinesTool(FsConfig{}).Execute(context.Background(), map[string]any{"path": p})
	if !res.IsError {
		t.Fatalf("binary content was not rejected: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "encoding=utf-16") {
		t.Errorf("hint does not mention encoding=utf-16: %s", res.ForLLM)
	}
}

// TestReadFileUTF16LabelAliases pins that every alias of utf-16 decodes the same
// way, so a stale "utf16" spelling no longer errors out.
func TestReadFileUTF16LabelAliases(t *testing.T) {
	text := "第一行\nsecond line"
	data := utf16Bytes(t, text, unicode.LittleEndian, true)
	p := writeRawFile(t, t.TempDir(), "alias.txt", data)
	tool := NewReadFileLinesTool(FsConfig{})

	for _, label := range []string{"utf-16", "utf16", "UTF-16", "utf_16", "unicode", "ucs-2", "utf-16le", "unicodefeff"} {
		res := tool.Execute(context.Background(), map[string]any{"path": p, "encoding": label})
		if res.IsError {
			t.Fatalf("encoding=%q failed: %s", label, res.ForLLM)
		}
		if got := readFileBody(t, res.ForLLM); got != text {
			t.Errorf("encoding=%q content = %q, want %q", label, got, text)
		}
	}
}

// TestReadFileUTF16GenericHonorsBOM checks that the generic utf-16 label follows
// a big-endian byte order mark instead of assuming little-endian.
func TestReadFileUTF16GenericHonorsBOM(t *testing.T) {
	text := "alpha\nbeta 第二"
	p := writeRawFile(t, t.TempDir(), "be.txt", utf16Bytes(t, text, unicode.BigEndian, true))
	res := NewReadFileLinesTool(FsConfig{}).Execute(context.Background(), map[string]any{"path": p, "encoding": "utf-16"})
	if res.IsError {
		t.Fatalf("read failed: %s", res.ForLLM)
	}
	if got := readFileBody(t, res.ForLLM); got != text {
		t.Errorf("content = %q, want %q", got, text)
	}
}

// TestReadFileUnsupportedEncodingMentionsWideForms checks the error names the
// wide Unicode forms, not only the legacy ones.
func TestReadFileUnsupportedEncodingMentionsWideForms(t *testing.T) {
	p := writeFile(t, t.TempDir(), "x.txt", "hi\n")
	res := NewReadFileLinesTool(FsConfig{}).Execute(context.Background(), map[string]any{"path": p, "encoding": "utf-99"})
	if !res.IsError {
		t.Fatal("an unknown encoding label should be rejected")
	}
	if !strings.Contains(res.ForLLM, "utf-16") || !strings.Contains(res.ForLLM, "utf-32") {
		t.Errorf("error does not list the wide forms: %s", res.ForLLM)
	}
}

// TestReadFileAutoDetectsUTF32 covers the UTF-32 codec, which htmlindex does not
// know: a byte order mark names the order, and a mark-less little-endian stream
// is caught by the three-NULs-per-unit heuristic.
func TestReadFileAutoDetectsUTF32(t *testing.T) {
	text := "first line\nsecond line\nthird"
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"le-bom", encodeWide(t, text, utf32.UTF32(utf32.LittleEndian, utf32.UseBOM)), "utf-32le"},
		{"be-bom", encodeWide(t, text, utf32.UTF32(utf32.BigEndian, utf32.UseBOM)), "utf-32be"},
		{"le-nobom", encodeWide(t, text, utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM)), "utf-32le"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeRawFile(t, t.TempDir(), tc.name+".txt", tc.data)
			res := NewReadFileLinesTool(FsConfig{}).Execute(context.Background(), map[string]any{"path": p})
			if res.IsError {
				t.Fatalf("read failed: %s", res.ForLLM)
			}
			if !strings.Contains(res.ForLLM, "encoding: "+tc.want) {
				t.Errorf("header = %s, want encoding %s", res.ForLLM, tc.want)
			}
			if got := readFileBody(t, res.ForLLM); got != text {
				t.Errorf("content = %q, want %q", got, text)
			}
		})
	}
}

// TestReadFileEmptyAndBOMOnly pins the degenerate shapes: nothing to read is an
// END OF FILE, and a lone byte order mark is not content either.
func TestReadFileEmptyAndBOMOnly(t *testing.T) {
	tool := NewReadFileLinesTool(FsConfig{})

	empty := writeFile(t, t.TempDir(), "empty.txt", "")
	res := tool.Execute(context.Background(), map[string]any{"path": empty})
	if res.IsError {
		t.Fatalf("empty file failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "END OF FILE") {
		t.Errorf("empty file: %s", res.ForLLM)
	}

	bomOnly := writeRawFile(t, t.TempDir(), "bom.txt", []byte{0xFF, 0xFE})
	res = tool.Execute(context.Background(), map[string]any{"path": bomOnly})
	if res.IsError {
		t.Fatalf("BOM-only file failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "END OF FILE") || strings.Contains(res.ForLLM, "\uFEFF") {
		t.Errorf("BOM-only file should be empty: %s", res.ForLLM)
	}
}

// TestNormalizeContentEncodingWide pins label normalization: separators and case
// do not matter, and the well-known aliases fold onto one codec.
func TestNormalizeContentEncodingWide(t *testing.T) {
	cases := map[string]string{
		"utf16":       contentEncodingUTF16,
		"UTF-16":      contentEncodingUTF16,
		"utf_16":      contentEncodingUTF16,
		" unicode ":   contentEncodingUTF16,
		"ucs-2":       contentEncodingUTF16,
		"ucs2":        contentEncodingUTF16,
		"utf-16le":    contentEncodingUTF16LE,
		"UTF16LE":     contentEncodingUTF16LE,
		"utf-16be":    contentEncodingUTF16BE,
		"unicodefffe": contentEncodingUTF16BE,
		"utf32":       contentEncodingUTF32,
		"utf-32le":    contentEncodingUTF32LE,
		"UTF-32BE":    contentEncodingUTF32BE,
		"utf8":        contentEncodingUTF8,
		"UTF-8":       contentEncodingUTF8,
		"auto":        contentEncodingAuto,
		"gbk":         "gbk",
		"Shift_JIS":   "shift_jis",
	}
	for raw, want := range cases {
		if got := normalizeContentEncoding(raw); got != want {
			t.Errorf("normalizeContentEncoding(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestDetectTextEncodingWide pins the "auto" order for wide streams: a byte
// order mark wins, then the mark-less parity heuristic, then the narrow forms.
func TestDetectTextEncodingWide(t *testing.T) {
	gbk, err := encodeTextToFileBytes("中文", "gbk")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		sample []byte
		host   string
		want   string
	}{
		{"utf16le-bom", utf16Bytes(t, "hi\nthere", unicode.LittleEndian, true), "", contentEncodingUTF16LE},
		{"utf16be-bom", utf16Bytes(t, "hi\nthere", unicode.BigEndian, true), "", contentEncodingUTF16BE},
		{"utf32le-bom", encodeWide(t, "hi\nthere", utf32.UTF32(utf32.LittleEndian, utf32.UseBOM)), "", contentEncodingUTF32LE},
		{"utf32be-bom", encodeWide(t, "hi\nthere", utf32.UTF32(utf32.BigEndian, utf32.UseBOM)), "", contentEncodingUTF32BE},
		{"utf16le-nobom", utf16Bytes(t, "hello world\nsecond line", unicode.LittleEndian, false), "gbk", contentEncodingUTF16LE},
		{"utf16be-nobom", utf16Bytes(t, "hello world\nsecond line", unicode.BigEndian, false), "gbk", contentEncodingUTF16BE},
		{"utf8", []byte("hello"), "gbk", contentEncodingUTF8},
		{"gbk", gbk, "gbk", "gbk"},
	}
	for _, tc := range cases {
		if got := detectTextEncoding(tc.sample, tc.host); got != tc.want {
			t.Errorf("%s: detectTextEncoding = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestReadFileUserReportedUTF16Cases walks the six files from the bug report
// through encoding="auto" (the acceptance criteria): LE/BE with and without a
// byte order mark, a single line with and without a terminator, and a PowerShell
// ">" redirect product (UTF-16LE with a byte order mark).
func TestReadFileUserReportedUTF16Cases(t *testing.T) {
	lines := []string{"第一行 first", "第二行 second", "第三行 third"}
	src := strings.Join(lines, "\n")

	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"A-utf16le-bom-multiline", utf16Bytes(t, src, unicode.LittleEndian, true), src},
		{"B-utf16le-nobom-multiline", utf16Bytes(t, src, unicode.LittleEndian, false), src},
		{"C-utf16be-bom-multiline", utf16Bytes(t, src, unicode.BigEndian, true), src},
		{"D-utf16le-single-nonl", utf16Bytes(t, "第一行 first", unicode.LittleEndian, true), "第一行 first"},
		{"E-utf16le-single-crlf", utf16Bytes(t, "第一行 first\r\n", unicode.LittleEndian, true), "第一行 first"},
		{"F-powershell-redirect-le-bom", utf16Bytes(t, src+"\r\n", unicode.LittleEndian, true), src},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeRawFile(t, t.TempDir(), "case.txt", tc.data)
			res := NewReadFileLinesTool(FsConfig{}).Execute(context.Background(), map[string]any{"path": p, "encoding": "auto"})
			if res.IsError {
				t.Fatalf("read failed: %s", res.ForLLM)
			}
			if got := readFileBody(t, res.ForLLM); got != tc.want {
				t.Errorf("content = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReadFileKeepBOM checks the optional keep_bom switch: by default the mark is
// dropped, and with keep_bom=true it survives in the first row.
func TestReadFileKeepBOM(t *testing.T) {
	data := append([]byte{0xEF, 0xBB, 0xBF}, []byte("first\nsecond\n")...)
	p := writeRawFile(t, t.TempDir(), "bom.txt", data)

	res := NewReadFileLinesTool(FsConfig{}).Execute(context.Background(), map[string]any{"path": p})
	if res.IsError {
		t.Fatalf("read failed: %s", res.ForLLM)
	}
	if body := readFileBody(t, res.ForLLM); strings.HasPrefix(body, "\uFEFF") {
		t.Errorf("default read should drop the mark, got %q", body)
	}

	res = NewReadFileLinesTool(FsConfig{}).Execute(context.Background(), map[string]any{"path": p, "keep_bom": true})
	if res.IsError {
		t.Fatalf("read failed: %s", res.ForLLM)
	}
	body := readFileBody(t, res.ForLLM)
	if !strings.HasPrefix(body, "\uFEFF") {
		t.Errorf("keep_bom=true should retain the mark, got %q", body)
	}
	if !strings.HasSuffix(body, "first\nsecond") {
		t.Errorf("content = %q, want it to end with %q", body, "first\nsecond")
	}
}

// TestSniffWideEncoding pins the BOM-less wide detection: a clear NUL parity picks
// the byte order, a split parity is broken by the script score, and a sample with
// no NUL (every legacy CJK charset, UTF-8, binary) is never treated as wide. A
// NUL-free pure-CJK file is genuinely ambiguous and is left to an explicit label.
func TestSniffWideEncoding(t *testing.T) {
	le := func(s string) []byte { return utf16Bytes(t, s, unicode.LittleEndian, false) }
	be := func(s string) []byte { return utf16Bytes(t, s, unicode.BigEndian, false) }
	legacy := func(label, s string) []byte {
		t.Helper()
		b, err := encodeTextToFileBytes(s, label)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// U+4E00 (一) has a zero low byte, so a little-endian stream of such
	// characters puts NULs on the even positions too; together with the newlines
	// (odd positions) the parity test cannot decide and the score must.
	splitParity := "一开一你好世界\n一二三\n一开一"

	cases := []struct {
		name   string
		sample []byte
		want   string // "" means "not detected as wide"
	}{
		{"ascii-le", le("hello world\nsecond line\nthird line"), contentEncodingUTF16LE},
		{"ascii-be", be("hello world\nsecond line\nthird line"), contentEncodingUTF16BE},
		{"ascii-single-le", le("hello world here"), contentEncodingUTF16LE},
		{"ascii-single-be", be("hello world here"), contentEncodingUTF16BE},
		{"cjk-nl-le", le("中文测试\n第二行内容\n第三行"), contentEncodingUTF16LE},
		{"cjk-nl-be", be("中文测试\n第二行内容\n第三行"), contentEncodingUTF16BE},
		{"cjk-split-le", le(splitParity), contentEncodingUTF16LE},
		{"cjk-split-be", be(splitParity), contentEncodingUTF16BE},
		{"gbk", legacy("gbk", "中文测试内容示例纯汉字没有空格这是一个很长的句子"), ""},
		{"shift_jis", legacy("shift_jis", "こんにちは世界これはとても長い日本語の文章です"), ""},
		{"big5", legacy("big5", "中文測試內容範例純漢字沒有空格這是一個很長的句子"), ""},
		{"utf8", []byte("hello\nworld\n"), ""},
		{"binary", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
			0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x01, 0x00, 0xFF, 0xD8, 0xFF, 0xE0}, ""},
		{"pure-cjk-no-nul", le("中文测试内容示例纯汉字没有空格也没有换行"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sniffWideEncoding(tc.sample)
			if tc.want == "" {
				if ok {
					t.Errorf("sniffWideEncoding = %q, want no wide match", got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Errorf("sniffWideEncoding = (%q, %v), want %q", got, ok, tc.want)
			}
		})
	}
}

// TestReadFileHeaderReportsBOM checks that the header says whether the file has a
// byte order mark — the one thing the encoding name alone cannot tell apart (a
// UTF-8 BOM and a BOM-less UTF-8 file both read as "utf-8").
func TestReadFileHeaderReportsBOM(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"utf8-bom", append([]byte{0xEF, 0xBB, 0xBF}, []byte("hi\n")...), "yes"},
		{"utf8-plain", []byte("hi\n"), "no"},
		{"utf16le-bom", utf16Bytes(t, "hi\n", unicode.LittleEndian, true), "yes"},
		{"utf16le-nobom", utf16Bytes(t, "hi\n", unicode.LittleEndian, false), "no"},
		{"utf32be-bom", encodeWide(t, "hi\n", utf32.UTF32(utf32.BigEndian, utf32.UseBOM)), "yes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeRawFile(t, t.TempDir(), "f.txt", tc.data)
			res := NewReadFileLinesTool(FsConfig{}).Execute(context.Background(), map[string]any{"path": p})
			if res.IsError {
				t.Fatalf("read failed: %s", res.ForLLM)
			}
			if !strings.Contains(res.ForLLM, "| bom: "+tc.want) {
				t.Errorf("header = %s, want | bom: %s", res.ForLLM, tc.want)
			}
		})
	}
}

// TestReadFileUTF8SigLabel checks that the explicit utf-8-sig label reads a
// BOM-prefixed UTF-8 file without leaking the mark (and equals a plain utf8 read
// of the same bytes).
func TestReadFileUTF8SigLabel(t *testing.T) {
	data := append([]byte{0xEF, 0xBB, 0xBF}, []byte("first\nsecond\n")...)
	p := writeRawFile(t, t.TempDir(), "sig.txt", data)

	res := NewReadFileLinesTool(FsConfig{}).Execute(context.Background(), map[string]any{"path": p, "encoding": "utf-8-sig"})
	if res.IsError {
		t.Fatalf("read failed: %s", res.ForLLM)
	}
	if got := readFileBody(t, res.ForLLM); got != "first\nsecond" {
		t.Errorf("content = %q, want %q", got, "first\nsecond")
	}
}

// TestWriteFileBOMBehavior pins the create/overwrite rule (utf-8-sig and generic
// utf-16/utf-32 write a mark, utf-8 and the explicit endian forms do not) and the
// append rule (append never adds a mark).
func TestWriteFileBOMBehavior(t *testing.T) {
	dir := t.TempDir()
	tool := NewWriteFileTool(FsConfig{MaxWriteLines: 100})
	ctx := context.Background()

	write := func(name, enc, content string) []byte {
		t.Helper()
		p := filepath.Join(dir, name)
		res := tool.Execute(ctx, map[string]any{"path": p, "content": content, "encoding": enc})
		if res.IsError {
			t.Fatalf("%s: %s", name, res.ForLLM)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	utf8BOM := []byte{0xEF, 0xBB, 0xBF}
	utf16LEBOM := []byte{0xFF, 0xFE}
	utf16BEBOM := []byte{0xFE, 0xFF}

	if got := write("a.txt", "utf8", "hi\n"); !bytes.Equal(got, []byte("hi\n")) {
		t.Errorf("utf8 = %q, want plain bytes", got)
	}
	if got := write("b.txt", "utf-8-sig", "hi\n"); !bytes.HasPrefix(got, utf8BOM) {
		t.Errorf("utf-8-sig = %q, want a UTF-8 BOM", got)
	}
	if got := write("c.txt", "utf-16", "hi\n"); !bytes.HasPrefix(got, utf16LEBOM) {
		t.Errorf("utf-16 = %q, want a little-endian BOM", got)
	}
	if got := write("d.txt", "utf-16le", "hi\n"); bytes.HasPrefix(got, utf16LEBOM) {
		t.Errorf("utf-16le = %q, want no BOM", got)
	}
	if got := write("e.txt", "utf-16be", "hi\n"); bytes.HasPrefix(got, utf16BEBOM) {
		t.Errorf("utf-16be = %q, want no BOM", got)
	}

	// create then append: exactly one mark, at the very start.
	p := filepath.Join(dir, "f.txt")
	if res := tool.Execute(ctx, map[string]any{"path": p, "content": "one\n", "encoding": "utf-16", "mode": "c"}); res.IsError {
		t.Fatalf("create: %s", res.ForLLM)
	}
	if res := tool.Execute(ctx, map[string]any{"path": p, "content": "two\n", "encoding": "utf-16", "mode": "a"}); res.IsError {
		t.Fatalf("append: %s", res.ForLLM)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, utf16LEBOM) {
		t.Errorf("create+append = %q, want a leading BOM", raw)
	}
	if n := bytes.Count(raw[len(utf16LEBOM):], utf16LEBOM); n != 0 {
		t.Errorf("append inserted %d extra mark(s)", n)
	}
	read := NewReadFileLinesTool(FsConfig{}).Execute(ctx, map[string]any{"path": p})
	if read.IsError {
		t.Fatalf("read back: %s", read.ForLLM)
	}
	if got := readFileBody(t, read.ForLLM); got != "one\ntwo" {
		t.Errorf("content = %q, want %q", got, "one\ntwo")
	}
}
