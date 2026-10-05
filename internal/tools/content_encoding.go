package tools

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	xunicode "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
)

// Text-encoding identifiers. Any other label is treated as an IANA/WHATWG
// charset and resolved through htmlindex (gbk, big5, shift_jis, euc-jp, ...).
//
// The wide Unicode forms are handled explicitly: htmlindex knows no UTF-32, and
// a UTF-16/UTF-32 stream has to resolve its byte order from a byte order mark,
// which htmlindex's UTF-16 label does not do on its own.
const (
	contentEncodingUTF8    = "utf8"
	contentEncodingUTF8SIG = "utf-8-sig"
	contentEncodingUTF16   = "utf-16"
	contentEncodingUTF16LE = "utf-16le"
	contentEncodingUTF16BE = "utf-16be"
	contentEncodingUTF32   = "utf-32"
	contentEncodingUTF32LE = "utf-32le"
	contentEncodingUTF32BE = "utf-32be"
	contentEncodingHex     = "hex"
	contentEncodingBase64  = "base64"
	// contentEncodingAuto makes a reader sniff the charset of the bytes it is
	// given instead of trusting a fixed label.
	contentEncodingAuto = "auto"
)

// normalizeContentEncoding canonicalizes a user-provided encoding name. Labels
// are matched ignoring case and the separators '-'/'_'/' ', so utf-16, UTF16 and
// UTF_16 name the same codec, and the well-known aliases (unicode, ucs-2,
// unicodefeff, ...) fold onto the canonical identifier. An unrecognized label is
// returned lower-cased and is expected to resolve through htmlindex.
func normalizeContentEncoding(raw string) string {
	enc := strings.ToLower(strings.TrimSpace(raw))
	if enc == "" {
		return contentEncodingUTF8
	}
	switch compactEncodingLabel(enc) {
	case "auto":
		return contentEncodingAuto
	case "utf8", "unicode11utf8":
		return contentEncodingUTF8
	case "utf8sig", "utf8bom":
		return contentEncodingUTF8SIG
	case "utf16", "unicode", "ucs2", "iso10646ucs2", "csunicode":
		return contentEncodingUTF16
	case "utf16le", "unicodefeff":
		return contentEncodingUTF16LE
	case "utf16be", "unicodefffe":
		return contentEncodingUTF16BE
	case "utf32":
		return contentEncodingUTF32
	case "utf32le":
		return contentEncodingUTF32LE
	case "utf32be":
		return contentEncodingUTF32BE
	case "hex", "h":
		return contentEncodingHex
	case "base64", "b64":
		return contentEncodingBase64
	}
	return enc
}

// compactEncodingLabel drops the separators that make label spelling
// inconsistent (-, _ and spaces) so aliases compare equal.
func compactEncodingLabel(label string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '-', '_', ' ':
			return -1
		}
		return r
	}, label)
}

// Byte order marks. The UTF-32 forms are listed first because the UTF-32LE mark
// begins with the UTF-16LE mark.
var (
	utf8BOM    = []byte{0xEF, 0xBB, 0xBF}
	utf16LEBOM = []byte{0xFF, 0xFE}
	utf16BEBOM = []byte{0xFE, 0xFF}
	utf32LEBOM = []byte{0xFF, 0xFE, 0x00, 0x00}
	utf32BEBOM = []byte{0x00, 0x00, 0xFE, 0xFF}
)

// bomEncoding reports the encoding named by a leading byte order mark together
// with its length in bytes. A stream with no mark returns ("", 0).
func bomEncoding(sample []byte) (string, int) {
	switch {
	case bytes.HasPrefix(sample, utf32LEBOM):
		return contentEncodingUTF32LE, len(utf32LEBOM)
	case bytes.HasPrefix(sample, utf32BEBOM):
		return contentEncodingUTF32BE, len(utf32BEBOM)
	case bytes.HasPrefix(sample, utf16LEBOM):
		return contentEncodingUTF16LE, len(utf16LEBOM)
	case bytes.HasPrefix(sample, utf16BEBOM):
		return contentEncodingUTF16BE, len(utf16BEBOM)
	case bytes.HasPrefix(sample, utf8BOM):
		return contentEncodingUTF8, len(utf8BOM)
	}
	return "", 0
}

// detectTextEncoding resolves the charset of a byte sample for the "auto"
// reading mode. The order is:
//
//  1. a byte order mark, which names the exact codec (UTF-32 before UTF-16);
//  2. a byte order mark-less wide stream, which shows up as NUL bytes on a fixed
//     byte parity (UTF-16) or on three of every four bytes (UTF-32);
//  3. a sample that is already valid UTF-8;
//  4. the host ANSI code page (hostLabel, e.g. gbk on a zh-CN Windows host);
//     and UTF-8 as the last resort when nothing else matches.
//
// The result is an internal encoding identifier understood by
// lookupCharsetEncoding: contentEncodingUTF8, a wide identifier, or a charset
// label.
func detectTextEncoding(sample []byte, hostLabel string) string {
	if enc, _ := bomEncoding(sample); enc != "" {
		return enc
	}
	// A BOM-less wide stream has to be caught before the UTF-8/ANSI tests: its
	// NUL-filled bytes are not valid UTF-8, so those tests would otherwise hand
	// it to the host code page and mangle the text.
	if enc, ok := sniffWideEncoding(sample); ok {
		return enc
	}
	if sampleIsUTF8(sample) {
		return contentEncodingUTF8
	}
	if hostLabel != "" {
		if _, err := lookupCharsetEncoding(hostLabel); err == nil {
			return hostLabel
		}
	}
	return contentEncodingUTF8
}

// sniffWideEncoding guesses a byte order mark-less UTF-16 or UTF-32 stream. The
// order is: the UTF-32 four-byte pattern, then the UTF-16 byte parity of the NUL
// bytes, then, when the parity is split, a script score.
//
// A BOM-less wide stream always carries NUL bytes as soon as it holds any ASCII,
// space, tab or line break — that is, in practically every text file — so a
// sample without a single NUL is never treated as wide. Legacy CJK charsets
// never emit 0x00, and their bytes can decode to a perfectly plausible wide text
// (a GBK file read as UTF-16BE looks like Hangul), so guessing there would only
// misread real files. A NUL-free pure-CJK UTF-16 file is genuinely ambiguous and
// has to be named explicitly with encoding=utf-16.
func sniffWideEncoding(sample []byte) (string, bool) {
	if len(sample) < 4 {
		return "", false
	}
	if enc, ok := sniffWideUTF32(sample); ok {
		return enc, true
	}
	if bytes.IndexByte(sample, 0) < 0 {
		return "", false
	}
	evenNul, oddNul := nulParity(sample)
	nul := evenNul + oddNul
	// The high byte of a little-endian unit sits on the odd positions, so a
	// little-endian ASCIIish stream puts its NULs there and a big-endian one on
	// the even positions. A clear majority picks the byte order; it still has to
	// decode to text rather than to a longer run of NULs.
	switch {
	case oddNul*5 >= nul*4 && wideSampleIsText(sample, xunicode.LittleEndian):
		return contentEncodingUTF16LE, true
	case evenNul*5 >= nul*4 && wideSampleIsText(sample, xunicode.BigEndian):
		return contentEncodingUTF16BE, true
	}
	// The NULs are split across both parities, which CJK-heavy text produces
	// through characters such as U+4E00 (its low byte is 0x00): score both byte
	// orders and keep the one that reads as text.
	return sniffWideByScore(sample)
}

// nulParity counts the NUL bytes on even and odd positions.
func nulParity(sample []byte) (even, odd int) {
	for i, b := range sample {
		if b != 0 {
			continue
		}
		if i%2 == 0 {
			even++
		} else {
			odd++
		}
	}
	return even, odd
}

// wideSampleIsText decodes the sample in one byte order and reports whether the
// result reads as text (no NUL rune, few control or replacement runes).
func wideSampleIsText(sample []byte, order xunicode.Endianness) bool {
	text, err := decodeWideSample(sample, order)
	if err != nil {
		return false
	}
	return !decodedLooksBinary(text)
}

// decodeWideSample decodes a sample as BOM-less UTF-16 in the given byte order.
// An odd trailing byte cannot form a code unit and is dropped first.
func decodeWideSample(sample []byte, order xunicode.Endianness) (string, error) {
	out, err := xunicode.UTF16(order, xunicode.IgnoreBOM).NewDecoder().Bytes(sample[:len(sample)/2*2])
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// sniffWideByScore breaks a parity tie: it decodes the sample both ways and keeps
// the order that reads more like text. Whitespace is the first signal (a wrong
// byte order turns 0A 00 into U+0A00, so it destroys line breaks), then a script
// score decides. It returns ("", false) when neither order is text or the two are
// indistinguishable.
func sniffWideByScore(sample []byte) (string, bool) {
	le, okLE := scoreWideText(sample, xunicode.LittleEndian)
	be, okBE := scoreWideText(sample, xunicode.BigEndian)
	switch {
	case okLE && (!okBE || le.better(be)):
		return contentEncodingUTF16LE, true
	case okBE && (!okLE || be.better(le)):
		return contentEncodingUTF16BE, true
	}
	return "", false
}

// wideTextStats describes how text-like a decoded wide sample looks.
type wideTextStats struct {
	score int // higher is more text-like
	space int // whitespace runes: the most trustworthy signal
}

// better reports whether these stats describe clearly more text-like content
// than other: more whitespace wins, then a higher script score.
func (s wideTextStats) better(other wideTextStats) bool {
	if s.space != other.space {
		return s.space > other.space
	}
	return s.score > other.score
}

// scoreWideText decodes the sample in one byte order and scores the runes. The
// bool is false when the result is binary, which makes the order unusable.
//
// Note that a wrong byte order usually still decodes to valid code points (a CJK
// pair swaps into another script), so validity alone says nothing; the score
// weighs which scripts actually appear, and the whitespace count is what catches
// the tell-tale loss of line breaks.
func scoreWideText(sample []byte, order xunicode.Endianness) (wideTextStats, bool) {
	text, err := decodeWideSample(sample, order)
	if err != nil {
		return wideTextStats{}, false
	}
	var st wideTextStats
	for _, r := range text {
		switch {
		case r == 0:
			return wideTextStats{}, false
		case r == '\t' || r == '\n' || r == '\r' || r == ' ':
			st.space++
			st.score += 2
		case r == '\uFEFF':
		case r == '\uFFFD':
			st.score -= 8
		case r < 0x20 || (r >= 0x7F && r < 0xA0):
			st.score -= 6
		case r < 0x7F:
			st.score += 4
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r):
			st.score += 6
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			st.score += 4
		case unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.IsSpace(r):
			st.score += 3
		default:
			if !unicode.IsGraphic(r) {
				st.score -= 6
			}
		}
	}
	if decodedLooksBinary(text) {
		return wideTextStats{}, false
	}
	return st, true
}

// sniffWideUTF32 looks for the UTF-32 signature: for BMP text three of every
// four bytes are NUL and each four-byte group points the same way. Little-endian
// keeps the two high bytes zero, big-endian the two low bytes.
func sniffWideUTF32(sample []byte) (string, bool) {
	groups := len(sample) / 4
	if groups < 4 {
		return "", false
	}
	var little, big int
	for i := 0; i+3 < len(sample); i += 4 {
		if sample[i+2] == 0 && sample[i+3] == 0 && (sample[i] != 0 || sample[i+1] != 0) {
			little++
		}
		if sample[i] == 0 && sample[i+1] == 0 && (sample[i+2] != 0 || sample[i+3] != 0) {
			big++
		}
	}
	switch {
	case little*4 >= groups*3 && little > big:
		return contentEncodingUTF32LE, true
	case big*4 >= groups*3 && big > little:
		return contentEncodingUTF32BE, true
	}
	return "", false
}

// sampleIsUTF8 reports whether a byte sample is valid UTF-8. The sample is the
// head of a file, so a multi-byte rune may be cut at the end; an incomplete
// trailing sequence is dropped before the check instead of making the whole
// sample look non-UTF-8.
func sampleIsUTF8(sample []byte) bool {
	if utf8.Valid(sample) {
		return true
	}
	for i := len(sample) - 1; i >= 0 && i > len(sample)-utf8.UTFMax; i-- {
		if utf8.RuneStart(sample[i]) {
			return utf8.Valid(sample[:i])
		}
	}
	return false
}

// isWideContentEncoding reports whether enc is one of the wide Unicode codecs
// whose raw bytes are full of NULs by design.
func isWideContentEncoding(enc string) bool {
	switch enc {
	case contentEncodingUTF16, contentEncodingUTF16LE, contentEncodingUTF16BE,
		contentEncodingUTF32, contentEncodingUTF32LE, contentEncodingUTF32BE:
		return true
	}
	return false
}

// sampleLooksBinaryForEncoding reports whether a byte sample is binary for the
// resolved encoding. A wide codec is judged through its decoder (a raw NUL scan
// would reject every UTF-16/UTF-32 file), a narrow codec by a NUL scan as
// before.
func sampleLooksBinaryForEncoding(enc string, sample []byte) bool {
	if !isWideContentEncoding(enc) {
		return bytes.IndexByte(sample, 0) >= 0
	}
	codec, err := lookupCharsetEncoding(enc)
	if err != nil || codec == nil {
		return bytes.IndexByte(sample, 0) >= 0
	}
	decoded, err := codec.NewDecoder().Bytes(sample)
	if err != nil {
		return true
	}
	return decodedLooksBinary(string(decoded))
}

// decodedLooksBinary reports whether decoded text is really binary: a NUL rune is
// decisive, and a stream that decodes mostly to control or replacement runes is
// not text.
func decodedLooksBinary(text string) bool {
	if text == "" {
		return false
	}
	var bad, total int
	for _, r := range text {
		total++
		switch r {
		case 0:
			return true
		case '\n', '\r', '\t', '\uFEFF':
		default:
			if r < 0x20 || r == '\uFFFD' {
				bad++
			}
		}
	}
	return bad*10 > total
}

// contentEncodingDisplayName is the name an auto-detected encoding is reported
// under in a read header.
func contentEncodingDisplayName(enc string) string {
	if enc == contentEncodingUTF8 {
		return "utf-8"
	}
	return enc
}

// contentEncodingIsBinary reports whether the encoding is a binary payload
// representation (hex or base64).
func contentEncodingIsBinary(enc string) bool {
	return enc == contentEncodingHex || enc == contentEncodingBase64
}

func unsupportedContentEncoding(enc string) error {
	return fmt.Errorf("unsupported encoding %q: use utf8, utf-8-sig, hex, base64, a Unicode form (utf-16, utf-16le, utf-16be, utf-32, utf-32le, utf-32be), or a charset label such as gbk, big5, shift_jis, euc-jp, euc-kr, windows-1252", enc)
}

// appendEncodingNoBOM maps a BOM-writing label to the equivalent codec that does
// not write a byte order mark. Appending must never insert a mark in the middle
// of a file: a mark belongs at the very start, which a create/overwrite write
// already laid down (utf-8-sig and generic utf-16/utf-32 write one; utf-8 and the
// explicit little-/big-endian forms do not).
func appendEncodingNoBOM(enc string) string {
	switch enc {
	case contentEncodingUTF8SIG:
		return contentEncodingUTF8
	case contentEncodingUTF16:
		return contentEncodingUTF16LE
	case contentEncodingUTF32:
		return contentEncodingUTF32LE
	}
	return enc
}

// lookupCharsetEncoding resolves a charset label to its x/text encoding.
func lookupCharsetEncoding(enc string) (encoding.Encoding, error) {
	switch enc {
	case contentEncodingUTF8:
		return nil, nil
	case contentEncodingUTF8SIG:
		// UTF-8 with a byte order mark: the encoder writes the mark, the decoder
		// strips it.
		return xunicode.UTF8BOM, nil
	case contentEncodingUTF16:
		// Generic UTF-16: a byte order mark decides the byte order, and
		// little-endian (the Windows "Unicode" default) is used when there is
		// none. UseBOM leaves the mark out of the decoded text.
		return xunicode.UTF16(xunicode.LittleEndian, xunicode.UseBOM), nil
	case contentEncodingUTF16LE:
		return xunicode.UTF16(xunicode.LittleEndian, xunicode.IgnoreBOM), nil
	case contentEncodingUTF16BE:
		return xunicode.UTF16(xunicode.BigEndian, xunicode.IgnoreBOM), nil
	case contentEncodingUTF32:
		return utf32.UTF32(utf32.LittleEndian, utf32.UseBOM), nil
	case contentEncodingUTF32LE:
		return utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM), nil
	case contentEncodingUTF32BE:
		return utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM), nil
	}
	charset, err := htmlindex.Get(enc)
	if err != nil || charset == nil {
		return nil, unsupportedContentEncoding(enc)
	}
	return charset, nil
}

// decodeFileBytesToText decodes raw file bytes into an internal UTF-8 string.
// utf8 passes the bytes through unchanged; hex/base64 are not text encodings.
func decodeFileBytesToText(data []byte, enc string) (string, error) {
	switch enc {
	case contentEncodingUTF8:
		return strings.TrimPrefix(string(data), "\uFEFF"), nil
	case contentEncodingHex, contentEncodingBase64:
		return "", fmt.Errorf("encoding %q is a binary representation, not a text encoding", enc)
	}
	charset, err := lookupCharsetEncoding(enc)
	if err != nil {
		return "", err
	}
	decoded, err := charset.NewDecoder().Bytes(data)
	if err != nil {
		return "", fmt.Errorf("failed to decode content as %q: %w", enc, err)
	}
	// A byte order mark is not content: drop it when the codec did not (the
	// explicit little-/big-endian codecs treat it as a normal character).
	return strings.TrimPrefix(string(decoded), "\uFEFF"), nil
}

// encodeTextToFileBytes encodes an internal UTF-8 string into file bytes using
// the requested text encoding (utf8 passes the string through unchanged).
func encodeTextToFileBytes(text string, enc string) ([]byte, error) {
	switch enc {
	case contentEncodingUTF8:
		return []byte(text), nil
	case contentEncodingHex, contentEncodingBase64:
		return nil, fmt.Errorf("encoding %q is a binary representation, not a text encoding", enc)
	}
	charset, err := lookupCharsetEncoding(enc)
	if err != nil {
		return nil, err
	}
	encoded, err := charset.NewEncoder().Bytes([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("failed to encode content as %q: %w", enc, err)
	}
	return encoded, nil
}

// decodeContentPayload converts a write/edit content argument into raw bytes.
// utf8 content is passed through; hex/base64 payloads are decoded; other labels
// encode the UTF-8 text into that charset.
func decodeContentPayload(payload, enc string) ([]byte, error) {
	switch enc {
	case contentEncodingUTF8:
		return []byte(payload), nil
	case contentEncodingHex:
		return decodeHexData(payload)
	case contentEncodingBase64:
		return decodeBase64Data(payload)
	}
	return encodeTextToFileBytes(payload, enc)
}

// formatReadBytes renders read file bytes according to the requested encoding.
func formatReadBytes(data []byte, enc string) (string, error) {
	switch enc {
	case contentEncodingUTF8:
		return string(data), nil
	case contentEncodingHex:
		return hex.EncodeToString(data), nil
	case contentEncodingBase64:
		return base64.StdEncoding.EncodeToString(data), nil
	}
	return decodeFileBytesToText(data, enc)
}

// decodeHexData decodes a hex payload, tolerating whitespace and either case.
func decodeHexData(s string) ([]byte, error) {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, s)
	if cleaned == "" {
		return nil, fmt.Errorf("empty hex payload")
	}
	return hex.DecodeString(cleaned)
}

// decodeBase64Data decodes standard base64, tolerating whitespace and line
// breaks the model may have introduced.
func decodeBase64Data(s string) ([]byte, error) {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, s)
	if cleaned == "" {
		return nil, fmt.Errorf("empty base64 payload")
	}
	return base64.StdEncoding.DecodeString(cleaned)
}

// CRLF helpers shared by the text-oriented file tools.

// hasCRLFLineEndings reports whether text contains CRLF line endings.
func hasCRLFLineEndings(text string) bool {
	return strings.Contains(text, "\r\n")
}

// normalizeTextToLF converts every CRLF line ending to LF.
func normalizeTextToLF(text string) string {
	return strings.ReplaceAll(text, "\r\n", "\n")
}

// restoreTextToCRLF converts every LF line ending back to CRLF.
func restoreTextToCRLF(text string) string {
	return strings.ReplaceAll(text, "\n", "\r\n")
}
