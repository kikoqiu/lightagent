package tools

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
)

// Text-encoding identifiers. Any other label is treated as an IANA/WHATWG
// charset and resolved through htmlindex (gbk, big5, shift_jis, euc-jp, ...).
const (
	contentEncodingUTF8   = "utf8"
	contentEncodingHex    = "hex"
	contentEncodingBase64 = "base64"
	// contentEncodingAuto makes a reader sniff the charset of the bytes it is
	// given instead of trusting a fixed label.
	contentEncodingAuto = "auto"
)

// normalizeContentEncoding canonicalizes a user-provided encoding name.
func normalizeContentEncoding(raw string) string {
	enc := strings.ToLower(strings.TrimSpace(raw))
	switch enc {
	case "", "utf8", "utf-8", "unicode-1-1-utf-8":
		return contentEncodingUTF8
	case "auto":
		return contentEncodingAuto
	case "hex", "h":
		return contentEncodingHex
	case "base64", "b64":
		return contentEncodingBase64
	}
	return enc
}

// utf8BOM is the UTF-8 byte order mark.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// detectTextEncoding resolves the charset of a byte sample for the "auto"
// reading mode. A UTF-8 byte order mark wins, then a sample that is already
// valid UTF-8, then the host ANSI code page (hostLabel, e.g. gbk on a zh-CN
// Windows host) — and UTF-8 is the last resort when the sample matches nothing
// else. The result is an internal encoding identifier: contentEncodingUTF8 or a
// charset label accepted by lookupCharsetEncoding.
func detectTextEncoding(sample []byte, hostLabel string) string {
	if bytes.HasPrefix(sample, utf8BOM) || sampleIsUTF8(sample) {
		return contentEncodingUTF8
	}
	if hostLabel != "" {
		if _, err := lookupCharsetEncoding(hostLabel); err == nil {
			return hostLabel
		}
	}
	return contentEncodingUTF8
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
	return fmt.Errorf("unsupported encoding %q: use utf8, hex, base64 or a charset label such as gbk, big5, shift_jis, euc-jp, euc-kr, windows-1252", enc)
}

// lookupCharsetEncoding resolves a charset label to its x/text encoding.
func lookupCharsetEncoding(enc string) (encoding.Encoding, error) {
	if enc == contentEncodingUTF8 {
		return nil, nil
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
		return string(data), nil
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
	return string(decoded), nil
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
