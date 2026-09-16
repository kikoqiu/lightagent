package tools

import (
	"fmt"
	"io"
	"sync"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"
)

// consoleCodec carries the charset used to convert child process stdio: the host
// ANSI code page (CP_ACP; CP936 → GBK, CP950 → Big5, ...). The same code page is
// used in both directions — stdout/stderr bytes are decoded from it and text
// written to stdin is encoded into it — so the shell and everything it runs see
// one consistent encoding. A nil charset means the host code page is UTF-8 or the
// host is not Windows, so bytes pass through unchanged.
type consoleCodec struct {
	charset encoding.Encoding
}

var (
	consoleCodecOnce sync.Once
	consoleCodecVal  consoleCodec
)

// hostConsoleCodec returns the cached stdio codec for this host.
func hostConsoleCodec() consoleCodec {
	consoleCodecOnce.Do(func() {
		consoleCodecVal = consoleCodec{charset: hostAnsiEncoding()}
	})
	return consoleCodecVal
}

// wrapStdin encodes internal UTF-8 text into the host code page.
func (c consoleCodec) wrapStdin(w io.WriteCloser) io.WriteCloser {
	if c.charset == nil {
		return w
	}
	return &consoleStdinWriter{w: w, enc: c.charset}
}

// ansiCodePageCharsets maps Windows ANSI code pages to WHATWG charset labels.
var ansiCodePageCharsets = map[int]string{
	874:  "windows-874",
	932:  "shift_jis",
	936:  "gbk",
	949:  "euc-kr",
	950:  "big5",
	1250: "windows-1250",
	1251: "windows-1251",
	1252: "windows-1252",
	1253: "windows-1253",
	1254: "windows-1254",
	1255: "windows-1255",
	1256: "windows-1256",
	1257: "windows-1257",
	1258: "windows-1258",
}

// ansiEncodingFromCodePage resolves a Windows ANSI code page to its x/text
// charset. UTF-8 (65001), ASCII and unknown code pages return nil, meaning
// pass-through.
func ansiEncodingFromCodePage(codePage int) encoding.Encoding {
	name, ok := ansiCodePageCharsets[codePage]
	if !ok {
		return nil
	}
	charset, err := htmlindex.Get(name)
	if err != nil || charset == nil {
		return nil
	}
	return charset
}

// consoleOutputWriter converts child stdout/stderr bytes from the host code page
// into internal UTF-8 text and forwards them to sink. Bytes of a character that
// is split by the pipe are held back until the next write, so a chunk boundary
// never mangles a character.
type consoleOutputWriter struct {
	mu      sync.Mutex
	sink    func([]byte)
	decoder transform.Transformer
	pending []byte
}

// newConsoleOutputWriter builds a decoding writer. A nil charset forwards the
// bytes unchanged.
func newConsoleOutputWriter(sink func([]byte), charset encoding.Encoding) *consoleOutputWriter {
	w := &consoleOutputWriter{sink: sink}
	if charset != nil {
		w.decoder = charset.NewDecoder()
	}
	return w
}

// Write converts p and forwards the result to the sink.
func (w *consoleOutputWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.decoder == nil {
		w.sink(p)
		return len(p), nil
	}
	w.pending = append(w.pending, p...)
	if !w.convert(false) && len(p) > 0 {
		w.sink(nil) // an incomplete trailing character: still wake the reader
	}
	return len(p), nil
}

// Close converts the bytes that are left over at end of stream.
func (w *consoleOutputWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.decoder != nil {
		w.convert(true)
	}
	return nil
}

// convert runs the decoder over the buffered bytes and reports whether text was
// emitted. atEOF also converts a truncated trailing character (into the charset's
// replacement character) instead of holding it back.
func (w *consoleOutputWriter) convert(atEOF bool) bool {
	emitted := false
	for len(w.pending) > 0 {
		dst := make([]byte, len(w.pending)*4+16)
		nDst, nSrc, err := w.decoder.Transform(dst, w.pending, atEOF)
		if nDst > 0 {
			w.sink(dst[:nDst])
			emitted = true
		}
		if err != nil && err != transform.ErrShortDst && err != transform.ErrShortSrc {
			// The decoder rejected the bytes: forward the raw remainder so that
			// nothing is lost and stop decoding this stream.
			if nSrc < len(w.pending) {
				w.sink(w.pending[nSrc:])
				emitted = true
			}
			w.pending = nil
			w.decoder = nil
			return emitted
		}
		if nSrc == 0 {
			return emitted // incomplete trailing character: wait for the next write
		}
		w.pending = append([]byte(nil), w.pending[nSrc:]...)
		if err != transform.ErrShortDst {
			return emitted // fully converted, or the tail is kept for later
		}
	}
	return emitted
}

// consoleStdinWriter encodes internal UTF-8 text into the host code page.
type consoleStdinWriter struct {
	w   io.WriteCloser
	enc encoding.Encoding
}

func (w *consoleStdinWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	encoded, err := w.enc.NewEncoder().Bytes(p)
	if len(encoded) > 0 {
		if _, werr := w.w.Write(encoded); werr != nil {
			return 0, werr
		}
	}
	if err != nil {
		return 0, fmt.Errorf("cannot encode stdin with the host code page: %w", err)
	}
	return len(p), nil
}

func (w *consoleStdinWriter) Close() error { return w.w.Close() }
