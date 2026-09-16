package tools

import (
	"bytes"
	"io"
	"testing"

	"golang.org/x/text/encoding"
)

// decodeChunks feeds raw to an output writer in chunk-sized writes and returns
// the converted text.
func decodeChunks(t *testing.T, charset encoding.Encoding, raw []byte, chunk int) string {
	t.Helper()
	var out bytes.Buffer
	sink := func(p []byte) { _, _ = out.Write(p) }
	w := newConsoleOutputWriter(sink, charset)
	for start := 0; start < len(raw); start += chunk {
		end := start + chunk
		if end > len(raw) {
			end = len(raw)
		}
		if _, err := w.Write(raw[start:end]); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return out.String()
}

func gbkTestEncoding(t *testing.T) encoding.Encoding {
	t.Helper()
	enc, err := lookupCharsetEncoding("gbk")
	if err != nil {
		t.Fatalf("resolve gbk: %v", err)
	}
	return enc
}

// TestConsoleOutputDecodesHostCodePage covers output produced in the host code
// page (GBK on a zh-CN host), delivered as one chunk and split across writes.
func TestConsoleOutputDecodesHostCodePage(t *testing.T) {
	const want = "中文输出 ok 你好\n"
	raw, err := encodeTextToFileBytes(want, "gbk")
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []int{1, 2, 3, 5, len(raw)} {
		if got := decodeChunks(t, gbkTestEncoding(t), raw, chunk); got != want {
			t.Errorf("chunk=%d: converted %q, want %q", chunk, got, want)
		}
	}
}

// TestConsoleOutputPassThroughWithoutCharset covers hosts whose code page needs
// no conversion (UTF-8, or non-Windows): bytes must pass through unchanged.
func TestConsoleOutputPassThroughWithoutCharset(t *testing.T) {
	raw := []byte("ok 中文\n")
	for _, chunk := range []int{1, 3, len(raw)} {
		if got := decodeChunks(t, nil, raw, chunk); got != string(raw) {
			t.Errorf("chunk=%d: converted %q, want %q", chunk, got, string(raw))
		}
	}
}

// TestAnsiEncodingFromCodePage maps Windows code pages to x/text charsets.
func TestAnsiEncodingFromCodePage(t *testing.T) {
	gbk := ansiEncodingFromCodePage(936)
	if gbk == nil {
		t.Fatal("cp936 should resolve to a charset")
	}
	encoded, err := gbk.NewEncoder().Bytes([]byte("中"))
	if err != nil {
		t.Fatalf("encode with cp936: %v", err)
	}
	if !bytes.Equal(encoded, []byte{0xD6, 0xD0}) {
		t.Fatalf("cp936 encoding of 中 = % x, want d6 d0", encoded)
	}
	if enc := ansiEncodingFromCodePage(65001); enc != nil {
		t.Fatalf("cp65001 (UTF-8) should pass through, got %v", enc)
	}
	if enc := ansiEncodingFromCodePage(0); enc != nil {
		t.Fatalf("unknown code page should pass through, got %v", enc)
	}
}

// nopWriteCloser adapts a buffer to io.WriteCloser for stdin tests.
type nopWriteCloser struct{ w io.Writer }

func (n nopWriteCloser) Write(p []byte) (int, error) { return n.w.Write(p) }
func (n nopWriteCloser) Close() error                { return nil }

// TestConsoleStdinEncodesHostCodePage verifies that internal UTF-8 text is
// encoded into the host code page for the child process.
func TestConsoleStdinEncodesHostCodePage(t *testing.T) {
	var buf bytes.Buffer
	w := &consoleStdinWriter{w: nopWriteCloser{&buf}, enc: gbkTestEncoding(t)}
	if _, err := w.Write([]byte("你好\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	want, err := encodeTextToFileBytes("你好\n", "gbk")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("stdin bytes = % x, want % x", buf.Bytes(), want)
	}

	// ASCII input and control keys stay byte-for-byte identical.
	buf.Reset()
	if _, err := w.Write([]byte("yes\n\x03")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := buf.String(); got != "yes\n\x03" {
		t.Fatalf("stdin bytes = %q, want %q", got, "yes\n\x03")
	}
}
