package tools

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/transform"
)

// readSampleSize is how many leading bytes are sniffed to pick an encoding. It
// has to be big enough for the byte order mark-less wide heuristic to see a
// stable NUL pattern.
const readSampleSize = 4096

// FsConfig carries the file-tool limits.
type FsConfig struct {
	MaxReadFileSize  int
	MaxReadFileLines int
	MaxWriteLines    int
}

// ReadFileLinesTool reads a text file line by line with pagination.
type ReadFileLinesTool struct {
	fs FsConfig
}

// NewReadFileLinesTool creates the line-oriented reader.
func NewReadFileLinesTool(fs FsConfig) *ReadFileLinesTool {
	if fs.MaxReadFileSize <= 0 {
		fs.MaxReadFileSize = 65536
	}
	if fs.MaxReadFileLines <= 0 {
		fs.MaxReadFileLines = 2000
	}
	return &ReadFileLinesTool{fs: fs}
}

// Name implements Tool.
func (t *ReadFileLinesTool) Name() string { return "read_file" }

// Description implements Tool.
func (t *ReadFileLinesTool) Description() string {
	return fmt.Sprintf("Read a text file line by line (source, markdown, logs, configs). "+
		"Rows are returned without line numbers; the header states the file line of the first "+
		"row. CRLF is normalized to LF. start_line is 1-indexed and inclusive, max_lines is a row "+
		"count, so one call returns [start_line, start_line + rows - 1]; continue with "+
		"start_line = last returned file line + 1. With the default 'auto' encoding the header "+
		"also names the charset that was detected (UTF-8, UTF-16/UTF-32, or a legacy code page). "+
		"Per-call limit: up to %d lines / %d bytes.",
		t.fs.MaxReadFileLines, t.fs.MaxReadFileSize)
}

// Parameters implements Tool.
func (t *ReadFileLinesTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "File to read.",
			},
			"start_line": map[string]any{
				"type":        "integer",
				"default":     1,
				"description": "1-indexed inclusive first line to read. Default: 1.",
			},
			"max_lines": map[string]any{
				"type":        "integer",
				"default":     t.fs.MaxReadFileLines,
				"description": fmt.Sprintf("Maximum rows to return. Default/limit: %d.", t.fs.MaxReadFileLines),
			},
			"encoding": map[string]any{
				"type":        "string",
				"default":     contentEncodingAuto,
				"description": "'auto' (default) sniffs the file's charset (a byte order mark, a byte order mark-less UTF-16/UTF-32 pattern, else valid UTF-8, else the host ANSI code page) and the header names what it found and whether the file has a byte order mark; 'utf8' forces UTF-8; 'utf-8-sig' is UTF-8 with a byte order mark; any other value is a charset label that decodes the bytes into UTF-8 text: a Unicode form (utf-16, utf-16le, utf-16be, utf-32, utf-32le, utf-32be; aliases like utf16/unicode/ucs-2 are accepted) or a legacy label (gbk, big5, shift_jis, euc-jp, euc-kr, windows-1252).",
			},
			"keep_bom": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "Keep a leading byte order mark (U+FEFF) in the first row. Default: false (the mark is dropped — it is not content). The header's bom field reports whether the file has one either way.",
			},
		},
		"required": []string{"path"},
	}
}

// Execute implements Tool.
func (t *ReadFileLinesTool) Execute(ctx context.Context, args map[string]any) *Result {
	path, ok := stringArg(args, "path")
	if !ok || trimSpace(path) == "" {
		return Fail("path is required")
	}
	startLine := intArg(args, "start_line", 1)
	if startLine < 1 {
		return Fail("start_line must be >= 1")
	}
	if hasArg(args, "offset") {
		return Fail("offset is not supported in line mode; use start_line")
	}
	if hasArg(args, "length") {
		return Fail("length is not supported in line mode; use max_lines")
	}
	if hasArg(args, "limit") {
		return Fail("limit is not supported in line mode; use max_lines")
	}
	encodingRaw, _ := stringArg(args, "encoding")
	if trimSpace(encodingRaw) == "" {
		encodingRaw = contentEncodingAuto
	}
	encoding := normalizeContentEncoding(encodingRaw)
	autoDetect := encoding == contentEncodingAuto
	if contentEncodingIsBinary(encoding) {
		return Fail(fmt.Sprintf("encoding %q is a binary representation and cannot be applied line by line; use a charset label such as gbk, big5 or shift_jis", encoding))
	}
	if encoding != contentEncodingUTF8 && !autoDetect {
		if _, encErr := lookupCharsetEncoding(encoding); encErr != nil {
			return Fail(encErr.Error())
		}
	}

	sizeBudget := int64(t.fs.MaxReadFileSize)
	lineBudget := int64(t.fs.MaxReadFileLines)
	limit := lineBudget
	if hasArg(args, "max_lines") {
		limit = int64(intArg(args, "max_lines", t.fs.MaxReadFileLines))
		if limit <= 0 {
			return Fail("max_lines, if provided, must be > 0")
		}
		if limit > lineBudget {
			limit = lineBudget
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return Fail(err.Error())
	}
	defer f.Close()

	info, statErr := f.Stat()
	if statErr == nil && info.IsDir() {
		return Fail(fmt.Sprintf("failed to open file: path is a directory: %s", path))
	}

	// The encoding has to be known before anything is decoded, so the sniffing
	// runs on the raw head of the file.
	sample := make([]byte, readSampleSize)
	n, readErr := f.Read(sample)
	if readErr != nil && readErr != io.EOF {
		return Fail(fmt.Sprintf("failed to read file: %v", readErr))
	}
	sample = sample[:n]

	encodingName := ""
	if autoDetect {
		encoding = detectTextEncoding(sample, hostAnsiCharsetLabel())
		encodingName = contentEncodingDisplayName(encoding)
	}
	// A byte order mark sits at the very start of the file, so the sniffed head
	// already knows whether the file carries one (a UTF-8 BOM and a BOM-less UTF-8
	// file otherwise both read as "utf-8").
	_, bomLen := bomEncoding(sample)
	hasBOM := bomLen > 0

	// Content is refused as binary only when it still looks non-textual after
	// the sniffing above: a wide (UTF-16/UTF-32) stream is judged through its
	// decoder, a narrow one by the raw NUL scan as before. A byte order mark or
	// a wide guess therefore no longer trips the old "looks binary" test.
	if autoDetect || encoding == contentEncodingUTF8 {
		if sampleLooksBinaryForEncoding(encoding, sample) {
			return Fail(binaryReadHint())
		}
	}

	// Decode the whole byte stream into UTF-8 first, then split lines in the
	// decoded text. Splitting the raw bytes on 0x0A would cut a UTF-16 code unit
	// in half (its newline is 0A 00 / 00 0A) and garble every line after the
	// first, and a byte budget could halve a multi-byte character. The terminator
	// is therefore always looked for in the decoded text — never in the file's
	// own bytes. Do not "optimize" this back into a per-byte split.
	reader, err := decodeStream(encoding, sample, f)
	if err != nil {
		return Fail(err.Error())
	}
	content, footer, err := t.readWindow(reader, filepath.Base(path), encodingName, int64(startLine), limit, sizeBudget, !boolArgOr(args, "keep_bom", false), hasBOM)
	if err != nil {
		return Fail(err.Error())
	}
	if content == "" {
		return OK(fmt.Sprintf("[END OF FILE - no content at or after start_line=%d]", startLine))
	}
	return OK(footer + "\n\n" + content)
}

// decodeStream wraps the raw file bytes in a streaming decoder that turns them
// into UTF-8, so the line reader above only ever sees text: the decoded stream
// is where line terminators are safe to look for. The consumed sample is
// replayed ahead of the open file so no byte is read twice.
func decodeStream(encoding string, sample []byte, f *os.File) (*bufio.Reader, error) {
	codec, err := lookupCharsetEncoding(encoding)
	if err != nil {
		return nil, err
	}
	var src io.Reader = io.MultiReader(bytes.NewReader(sample), f)
	if codec != nil {
		src = transform.NewReader(src, codec.NewDecoder())
	}
	return bufio.NewReaderSize(src, 64*1024), nil
}

// binaryReadHint is the actionable message for content that still looks binary
// after encoding detection.
func binaryReadHint() string {
	return "file appears to be binary; if it is a UTF-16/UTF-32 text file pass encoding=utf-16 (or utf-32), " +
		"otherwise pass a charset label (e.g. gbk) if it is a legacy-encoded text file"
}

// readWindow reads [startLine, startLine+limit-1] honoring the byte budget and
// returns the content plus a header/footer describing the window. Every line
// number here counts decoded lines.
func (t *ReadFileLinesTool) readWindow(reader *bufio.Reader, displayName, encodingName string, startLine, limit, sizeBudget int64, stripBOM, hasBOM bool) (string, string, error) {
	lr := newLineReader(reader, int(sizeBudget), stripBOM)
	lineIndex := int64(1)
	reachedEOF := false

	for lineIndex < startLine {
		_, _, _, err := lr.next()
		if err == io.EOF {
			reachedEOF = true
			break
		}
		if err != nil {
			return "", "", fmt.Errorf("failed to read file content: %v", err)
		}
		lineIndex++
	}
	if reachedEOF {
		return "", "", nil
	}

	var lines []string
	var outputBytes, linesRead int64
	var byteTruncated, lineTruncated bool
	var lf, crlf, cr int

	for (limit < 0 || linesRead < limit) && !reachedEOF {
		remaining := sizeBudget - outputBytes
		if remaining <= 0 {
			byteTruncated = true
			break
		}
		line, term, truncated, err := lr.next()
		if err == io.EOF {
			reachedEOF = true
			break
		}
		if err != nil {
			return "", "", fmt.Errorf("failed to read file content: %v", err)
		}
		switch term {
		case termLF:
			lf++
		case termCRLF:
			crlf++
		case termCR:
			cr++
		}
		// The line reader already capped the line at sizeBudget on a rune
		// boundary; a shorter cut happens when the budget has shrunk after
		// earlier lines, and is kept rune-safe too.
		if int64(len(line)) > remaining {
			line = cutToRuneBoundary(line[:remaining])
			truncated = true
		}
		lines = append(lines, line)
		outputBytes += int64(len(line))
		linesRead++
		lineIndex++
		if truncated {
			byteTruncated = true
			lineTruncated = true
			break
		}
	}

	if len(lines) == 0 {
		return "", "", nil
	}

	content := strings.Join(lines, "\n")
	endLine := startLine + linesRead - 1
	header := fmt.Sprintf("[file: %s | lines %d-%d | first row below = file line %d",
		displayName, startLine, endLine, startLine)
	if encodingName != "" {
		header += fmt.Sprintf(" | encoding: %s", encodingName)
	}
	header += fmt.Sprintf(" | bom: %s", yesNo(hasBOM))
	header += fmt.Sprintf(" | eol: %s]", lineEndingLabel(lf, crlf, cr))

	var footer string
	switch {
	case lineTruncated:
		footer = fmt.Sprintf("%s\n[TRUNCATED - file line %d exceeded the %d-byte budget and was cut mid-line.]",
			header, endLine, sizeBudget)
	case byteTruncated:
		footer = fmt.Sprintf("%s\n[TRUNCATED - byte budget reached; call read_file start_line=%d max_lines=%d]",
			header, startLine+linesRead, limit)
	case !reachedEOF && limit > 0 && linesRead >= limit:
		footer = fmt.Sprintf("%s\n[PARTIAL - more content remains; call read_file start_line=%d max_lines=%d]",
			header, startLine+linesRead, limit)
	default:
		footer = header + "\n[END OF FILE - no further content.]"
	}
	return content, footer, nil
}

// lineTerm is how a decoded line ended.
type lineTerm int

const (
	termNone lineTerm = iota // the final line had no terminator
	termLF
	termCRLF
	termCR
)

// lineReader splits a decoded UTF-8 stream into lines. Terminators are LF, CRLF
// and lone CR; the terminator is not part of the returned line. Because the
// source is already decoded text, a terminator can never fall inside a
// multi-byte character — which is the whole point of decoding before splitting.
//
// A single line longer than maxBytes is cut at a rune boundary and the rest of
// it is dropped, so a runaway line cannot pull the whole file into memory. When
// stripBOM is set, the first line also has a leading U+FEFF (a byte order mark a
// codec left in place) removed: a mark is not content.
type lineReader struct {
	r        *bufio.Reader
	maxBytes int
	stripBOM bool
	atStart  bool
}

func newLineReader(r *bufio.Reader, maxBytes int, stripBOM bool) *lineReader {
	return &lineReader{r: r, maxBytes: maxBytes, stripBOM: stripBOM, atStart: true}
}

// next returns the next line, the terminator it was ended by and whether it was
// cut because it exceeded maxBytes. io.EOF is returned after the last line (the
// last line itself is still delivered first).
func (lr *lineReader) next() (string, lineTerm, bool, error) {
	line, term, truncated, err := lr.readRaw()
	if err != nil {
		return "", termNone, false, err
	}
	if lr.atStart {
		lr.atStart = false
		if lr.stripBOM {
			line = strings.TrimPrefix(line, "\uFEFF")
		}
	}
	return line, term, truncated, nil
}

func (lr *lineReader) readRaw() (string, lineTerm, bool, error) {
	var buf []byte
	for {
		b, err := lr.r.ReadByte()
		if err != nil {
			if err == io.EOF {
				if len(buf) == 0 {
					return "", termNone, false, io.EOF
				}
				return string(buf), termNone, false, nil
			}
			return "", termNone, false, err
		}
		switch b {
		case '\n':
			return string(buf), termLF, false, nil
		case '\r':
			term := termCR
			if nb, perr := lr.r.Peek(1); perr == nil && len(nb) == 1 && nb[0] == '\n' {
				lr.r.ReadByte()
				term = termCRLF
			}
			return string(buf), term, false, nil
		}
		buf = append(buf, b)
		if lr.maxBytes > 0 && len(buf) >= lr.maxBytes {
			term, derr := lr.discardToTerminator()
			if derr != nil && derr != io.EOF {
				return "", termNone, false, derr
			}
			// Cut on a rune boundary: maxBytes is a byte count, but the tail of
			// the line must never be a half-encoded character.
			return cutToRuneBoundary(string(buf)), term, true, nil
		}
	}
}

// discardToTerminator consumes and drops bytes up to and including the next line
// terminator, so the tail of a truncated line does not leak into the next one.
func (lr *lineReader) discardToTerminator() (lineTerm, error) {
	for {
		b, err := lr.r.ReadByte()
		if err != nil {
			return termNone, err
		}
		switch b {
		case '\n':
			return termLF, nil
		case '\r':
			if nb, perr := lr.r.Peek(1); perr == nil && len(nb) == 1 && nb[0] == '\n' {
				lr.r.ReadByte()
				return termCRLF, nil
			}
			return termCR, nil
		}
	}
}

// cutToRuneBoundary drops a trailing partial UTF-8 sequence left by a byte cap.
func cutToRuneBoundary(s string) string {
	for len(s) > 0 {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// yesNo renders a boolean as the read header reports it.
func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// lineEndingLabel names the line ending of the returned window: the single kind
// that occurred, "mixed" when several did, or "none" when no line ended.
func lineEndingLabel(lf, crlf, cr int) string {
	kinds := 0
	for _, n := range []int{lf, crlf, cr} {
		if n > 0 {
			kinds++
		}
	}
	switch {
	case kinds == 0:
		return "none"
	case kinds > 1:
		return "mixed"
	case crlf > 0:
		return "CRLF"
	case cr > 0:
		return "CR"
	default:
		return "LF"
	}
}

// AutoSplitWriteTool is implemented by a write tool that can spread a payload
// larger than its own per-call limit over several consecutive calls. The agent
// loop consults it for every write_file call: instead of letting the call be cut
// at the limit, the engine keeps the first slice in the model's own call and
// replays the remaining slices as follow-up calls it makes itself.
type AutoSplitWriteTool interface {
	// PlanWriteCalls reports how one decoded call has to be carried out. split
	// is false when the call keeps its arguments (the payload fits in a single
	// call, or it cannot be cut line by line). Otherwise the returned maps are
	// the arguments of the calls to run in order: the first replaces the call
	// the model made, the rest continue the same write.
	PlanWriteCalls(args map[string]any) (calls []map[string]any, split bool)
}

// WriteFileTool writes file content with overwrite/append/create modes.
type WriteFileTool struct {
	fs FsConfig
}

// NewWriteFileTool creates the writer.
func NewWriteFileTool(fs FsConfig) *WriteFileTool {
	if fs.MaxWriteLines <= 0 {
		fs.MaxWriteLines = 1000
	}
	return &WriteFileTool{fs: fs}
}

// Name implements Tool.
func (t *WriteFileTool) Name() string { return "write_file" }

// Description implements Tool.
func (t *WriteFileTool) Description() string {
	return fmt.Sprintf("Write content to a file. WARNING: Text content is LIMITED to %d lines per call; "+
		"lines beyond the limit are TRUNCATED and DISCARDED. For long files, write one part at a "+
		"time and continue with mode='a'; the newline that ended the last written line is kept, so "+
		"the next call starts with the next line's text directly. You're suggested to write one function(or several small functions) at a time for writing code. "+
		"Mode: 'o' overwrite (default), 'a' append, 'c' create  only if the file does not exist. Line endings (LF and CRLF) are preserved exactly as provided; the system never "+
		"adds a trailing newline automatically.", t.fs.MaxWriteLines)
}

// Parameters implements Tool.
func (t *WriteFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path of the file to write.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": fmt.Sprintf("Content to write. Any line beyond the limit [%d lines] is truncated and discarded.", t.fs.MaxWriteLines),
			},
			"mode": map[string]any{
				"type":        "string",
				"default":     "o",
				"description": "One of 'o' (overwrite, default), 'a' (append), 'c' (create only). Mutually exclusive.",
			},
			"encoding": map[string]any{
				"type":        "string",
				"default":     "utf8",
				"description": "'utf8' (default) writes text without a byte order mark; 'utf-8-sig' writes UTF-8 with one; 'hex'/'base64' decode an encoded binary payload; a Unicode form (utf-16, utf-16le, utf-16be, utf-32, utf-32le, utf-32be) or a legacy label (gbk, big5, shift_jis, euc-jp, euc-kr, windows-1252) encodes the text into that charset. A generic utf-16/utf-32 writes a byte order mark, the explicit little-/big-endian forms do not. mode='a' never writes a byte order mark, so an existing file's mark is left untouched.",
			},
		},
		"required": []string{"path", "content"},
	}
}

// parseWriteMode validates the mode flag.
func parseWriteMode(raw string) (appendMode, createMode, overwriteMode bool, err error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	if mode == "" {
		return false, false, true, nil
	}
	for _, flag := range mode {
		switch flag {
		case 'a':
			appendMode = true
		case 'c':
			createMode = true
		case 'o':
			overwriteMode = true
		default:
			return false, false, false, fmt.Errorf("invalid mode flag %q: allowed flags are 'a', 'o' and 'c'", string(flag))
		}
	}
	selected := 0
	for _, on := range []bool{appendMode, createMode, overwriteMode} {
		if on {
			selected++
		}
	}
	if selected != 1 {
		return false, false, false, fmt.Errorf("mode %q must select exactly one of 'a' (append), 'o' (overwrite) or 'c' (create)", raw)
	}
	return appendMode, createMode, overwriteMode, nil
}

// Execute implements Tool.
func (t *WriteFileTool) Execute(ctx context.Context, args map[string]any) *Result {
	path, ok := stringArg(args, "path")
	if !ok || trimSpace(path) == "" {
		return Fail("path is required")
	}
	content, ok := stringArg(args, "content")
	if !ok {
		return Fail("content is required")
	}
	encodingRaw, _ := stringArg(args, "encoding")
	encoding := normalizeContentEncoding(encodingRaw)
	if !contentEncodingIsBinary(encoding) && encoding != contentEncodingUTF8 {
		if _, encErr := lookupCharsetEncoding(encoding); encErr != nil {
			return Fail(encErr.Error())
		}
	}
	modeRaw, _ := stringArg(args, "mode")
	appendMode, createMode, _, err := parseWriteMode(modeRaw)
	if err != nil {
		return Fail(err.Error())
	}
	// A byte order mark belongs at the very start of a file: append writes the
	// same codec without one, so the mark the create/overwrite call laid down is
	// left untouched.
	writeEnc := encoding
	if appendMode {
		writeEnc = appendEncodingNoBOM(encoding)
	}

	var note string
	var data []byte
	if contentEncodingIsBinary(encoding) {
		data, err = decodeContentPayload(content, writeEnc)
		if err != nil {
			return Fail(fmt.Sprintf("failed to decode %s content: %v", encoding, err))
		}
	} else {
		kept, dropped, total, truncated := enforceWriteLineLimit(content, t.fs.MaxWriteLines)
		switch {
		case truncated:
			// The cut restores the separator of the last kept line, so the tail
			// that resumes the write is the only concern left to report.
			note = truncationNote(dropped, t.fs.MaxWriteLines, total)
		case kept != "" && !strings.HasSuffix(kept, "\n"):
			note = noFinalNewlineNote()
		}
		data, err = decodeContentPayload(kept, writeEnc)
		if err != nil {
			return Fail(fmt.Sprintf("failed to encode %s content: %v", encoding, err))
		}
	}

	switch {
	case appendMode:
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return Fail(err.Error())
		}
		defer f.Close()
		if _, err := f.Write(data); err != nil {
			return Fail(err.Error())
		}
		return OK(fmt.Sprintf("Appended to %s%s", path, note))

	case createMode:
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			if os.IsExist(err) {
				return Fail(fmt.Sprintf("file already exists: %s. Use mode 'a' to append, or edit_file to modify it.", path))
			}
			return Fail(err.Error())
		}
		defer f.Close()
		if _, err := f.Write(data); err != nil {
			return Fail(err.Error())
		}
		return OK(fmt.Sprintf("File created: %s%s", path, note))

	default: // overwrite
		existed := true
		if _, err := os.Stat(path); os.IsNotExist(err) {
			existed = false
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return Fail(err.Error())
		}
		if existed {
			return OK(fmt.Sprintf("File overwritten: %s%s", path, note))
		}
		return OK(fmt.Sprintf("File created: %s (did not previously exist)%s", path, note))
	}
}

// PlanWriteCalls implements AutoSplitWriteTool: a text payload longer than the
// per-call line limit is cut on line boundaries, so the agent can write it with
// a chain of calls instead of truncating its tail.
//
// The chunks reconstruct the payload byte for byte when they are written in
// order, and each one stays inside the limit as the truncation path counts it.
// A chunk that ends a line keeps that newline — it is what lets the next call
// start with the next line's text — and the accounting counts that separator as
// a line, so every chunk but the last carries MaxWriteLines-1 payload lines.
// Only the first call keeps the requested mode: the continuations always append,
// because the file already exists by then.
func (t *WriteFileTool) PlanWriteCalls(args map[string]any) ([]map[string]any, bool) {
	content, ok := stringArg(args, "content")
	if !ok || content == "" {
		return nil, false
	}
	// An encoded payload (hex/base64) is written whole: the line limit does not
	// apply to it, so there is nothing to spread.
	encodingRaw, _ := stringArg(args, "encoding")
	if contentEncodingIsBinary(normalizeContentEncoding(encodingRaw)) {
		return nil, false
	}
	chunks := splitWriteContent(content, t.fs.MaxWriteLines)
	if len(chunks) < 2 {
		return nil, false
	}
	calls := make([]map[string]any, 0, len(chunks))
	first := cloneArgs(args)
	first["content"] = chunks[0]
	calls = append(calls, first)
	for _, chunk := range chunks[1:] {
		next := cloneArgs(args)
		next["content"] = chunk
		next["mode"] = "a"
		calls = append(calls, next)
	}
	return calls, true
}

// splitWriteContent cuts a text payload into the chunks of a consecutive write
// chain, in order. Every chunk stays within limit lines as WriteFileTool counts
// them, and joining the chunks reproduces the payload exactly. It returns nil
// when the payload already fits in a single call.
func splitWriteContent(content string, limit int) []string {
	// A budget of one line leaves no room for the separator that keeps the
	// chunks apart; the caller then keeps the truncation behaviour instead.
	if limit < 2 {
		return nil
	}
	lines := strings.Split(content, "\n")
	if len(lines) <= limit {
		return nil
	}
	step := limit - 1
	chunks := make([]string, 0, len(lines)/step+1)
	for start := 0; start < len(lines); start += step {
		end := start + step
		if end >= len(lines) {
			chunks = append(chunks, strings.Join(lines[start:], "\n"))
			break
		}
		chunks = append(chunks, strings.Join(lines[start:end], "\n")+"\n")
	}
	return chunks
}

// cloneArgs copies a decoded argument map so a planned call can be adjusted
// without touching the model's own arguments or its siblings.
func cloneArgs(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for key, value := range args {
		out[key] = value
	}
	return out
}

// truncationPreviewLines is how many non-blank dropped lines the truncation note
// shows: enough to locate the cut without dumping the whole tail.
const truncationPreviewLines = 2

// enforceWriteLineLimit caps content at limit lines and reports the dropped tail
// so the caller can point at where the cut happened. When the content is cut, the
// newline that terminated the last kept line is written back as well, so the
// continuation appended with mode='a' starts on its own line.
func enforceWriteLineLimit(content string, limit int) (kept string, dropped []string, total int, truncated bool) {
	lines := strings.Split(content, "\n")
	total = len(lines)
	if total <= limit {
		return content, nil, total, false
	}
	// total > limit proves a newline followed line `limit`, so that separator can
	// be restored verbatim: splitting on "\n" leaves a CRLF file's CR inside the
	// line text, and joining plus "\n" rebuilds the original "\r\n".
	return strings.Join(lines[:limit], "\n") + "\n", lines[limit:], total, true
}

// truncationNote renders the note appended to a write result when the content was
// cut at the line limit: how much was written, and the content from the cut
// onwards, verbatim (blank lines spelled out so they stay visible) until two
// non-blank lines have been shown or the content ends, closed by an ellipsis.
// The model can tell exactly where to resume with mode='a'.
func truncationNote(dropped []string, written, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n[truncated: %d of %d lines written; continue with mode='a']", written, total)
	b.WriteString("\nDo NOT write the file in fewer lines than the original, it's a VIOLTION of the basic RULE. ")
	b.WriteString("\nWrite the file in several calls with mode='a'. The newline that ended the last written line has already been written for you, so start the next call with the next line's text directly (no leading newline). Keep the trailing newline of your final line if the original has one: the system never inserts it for you. ")
	b.WriteString("\n\nThe exact truncated lines are displayed as follows (CRITICAL: Check THESE LINES directly, NEVER issue a duplicate read in the next step):")
	shown := 0
	for _, line := range dropped {
		b.WriteString("\n")
		if line == "" {
			continue
		}
		b.WriteString(line)
		shown++
		if shown >= truncationPreviewLines {
			break
		}
	}
	b.WriteString("\n...")
	return b.String()
}

// noFinalNewlineNote renders the short note appended to a write result when the
// text payload does not end with a newline: the file ends at the last character
// written, and a follow-up append has to open with the newline the system never
// adds on its own.
func noFinalNewlineNote() string {
	return "\n[no trailing newline: the system never adds one. If the next call appends " +
		"(mode='a'), start its content with one newline: \\n, or \\r\\n for a CRLF file.]"
}

// Edit modes accepted by the "mode" argument of EditFileTool.
const (
	// editModeReplace swaps the matched literal for content (default).
	editModeReplace = "replace"
	// editModeInsert writes content in front of the matched literal.
	editModeInsert = "insert"
	// editModeRegex treats find as an RE2 pattern and replaces every match.
	editModeRegex = "regex"
)

// EditFileTool edits a file by locating find and writing content at that spot:
// the match is replaced (default), content is inserted in front of it, or the
// pattern's every match is replaced.
type EditFileTool struct{}

// NewEditFileTool creates the editor.
func NewEditFileTool() *EditFileTool { return &EditFileTool{} }

// Name implements Tool.
func (t *EditFileTool) Name() string { return "edit_file" }

// Description implements Tool.
func (t *EditFileTool) Description() string {
	return "Edit a file by locating find and writing content at that spot; the `mode` parameter says " +
		"how find is matched and where content lands. A find that is not unique, or that matches " +
		"nothing, is reported as an error so the call can be retried with more context. JSON escaping " +
		"applies to the arguments: \\n is a newline and \\\\n is a literal backslash-n."
}

// Parameters implements Tool.
func (t *EditFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path of the file to edit.",
			},
			"find": map[string]any{
				"type":        "string",
				"description": "Text to locate: an exact literal for mode='replace'/'insert' (must occur exactly once), an RE2 pattern for mode='regex', or the hex/base64 byte sequence when encoding is a binary representation.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Text written at the match: the replacement for mode='replace'/'regex', or the text inserted before the match for mode='insert'. In mode='regex' it may reference capture groups with $1, $2, ... ($$ = literal $). JSON escaping applies (\\n = newline, \\\\n = literal backslash-n).",
			},
			"mode": map[string]any{
				"type":        "string",
				"default":     "replace",
				"description": "'replace' (default) swaps the unique literal find for content; 'insert' writes content in front of the unique literal find (the find is kept, so the result is content + find; insert is not insert-line, no newline is added automatically); 'regex' treats find as an RE2 pattern and replaces every match.",
			},
			"encoding": map[string]any{
				"type":        "string",
				"default":     "utf8",
				"description": "'utf8' (default) text; 'hex'/'base64' treat find/content as literal byte sequences (mode='regex' is rejected); other values are charset labels (gbk, big5, shift_jis, windows-1252) for non-UTF-8 files.",
			},
		},
		"required": []string{"path", "find", "content"},
	}
}

// parseEditMode validates the "mode" argument; an omitted mode means a literal
// replacement.
func parseEditMode(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", editModeReplace:
		return editModeReplace, nil
	case editModeInsert:
		return editModeInsert, nil
	case editModeRegex:
		return editModeRegex, nil
	default:
		return "", fmt.Errorf("invalid mode %q: use 'replace' (default), 'insert' or 'regex'", raw)
	}
}

// Execute implements Tool.
func (t *EditFileTool) Execute(ctx context.Context, args map[string]any) *Result {
	path, ok := stringArg(args, "path")
	if !ok || trimSpace(path) == "" {
		return Fail("path is required")
	}
	find, ok := stringArg(args, "find")
	if !ok {
		return Fail("find is required")
	}
	content, ok := stringArg(args, "content")
	if !ok {
		return Fail("content is required")
	}
	if find == "" {
		return Fail("find must not be empty")
	}
	encodingRaw, _ := stringArg(args, "encoding")
	encoding := normalizeContentEncoding(encodingRaw)
	modeRaw, _ := stringArg(args, "mode")
	mode, err := parseEditMode(modeRaw)
	if err != nil {
		return Fail(err.Error())
	}
	if mode == editModeRegex && contentEncodingIsBinary(encoding) {
		return Fail(fmt.Sprintf("mode='regex' is only supported with text encodings; binary edits with encoding=%s are literal byte-sequence edits", encoding))
	}
	if !contentEncodingIsBinary(encoding) && encoding != contentEncodingUTF8 {
		if _, encErr := lookupCharsetEncoding(encoding); encErr != nil {
			return Fail(encErr.Error())
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return Fail(err.Error())
	}

	var matches int
	var newBytes []byte

	if contentEncodingIsBinary(encoding) {
		newBytes, matches, err = payloadEditContent(raw, encoding, find, content, mode == editModeInsert)
		if err != nil {
			return Fail(err.Error())
		}
	} else {
		text, derr := decodeFileBytesToText(raw, encoding)
		if derr != nil {
			return Fail(derr.Error())
		}
		// CRLF files are matched with LF patterns and restored on write.
		crlf := hasCRLFLineEndings(text)
		if crlf {
			text = normalizeTextToLF(text)
		}
		var after string
		switch mode {
		case editModeRegex:
			after, matches, err = regexReplaceText(text, find, content)
		case editModeInsert:
			after, matches, err = insertBeforeText(text, find, content)
		default:
			after, matches, err = literalReplaceText(text, find, content)
		}
		if err != nil {
			return Fail(err.Error())
		}
		if crlf {
			after = restoreTextToCRLF(after)
		}
		newBytes, err = encodeTextToFileBytes(after, encoding)
		if err != nil {
			return Fail(err.Error())
		}
	}

	if err := os.WriteFile(path, newBytes, 0o644); err != nil {
		return Fail(err.Error())
	}

	return OK(editResultMessage(path, encoding, mode, find, matches))
}

// editResultMessage reports what the edit did, including how many occurrences
// were matched: regex mode may legitimately hit more than one.
func editResultMessage(path, encoding, mode, find string, matches int) string {
	switch mode {
	case editModeInsert:
		return fmt.Sprintf("File edited: %s | mode=%s | encoding=%s | content inserted before the match (1 occurrence)",
			path, mode, encoding)
	case editModeRegex:
		return fmt.Sprintf("File edited: %s | mode=%s | encoding=%s | regex %q matched %d occurrence(s), all replaced",
			path, mode, encoding, find, matches)
	default:
		return fmt.Sprintf("File edited: %s | mode=%s | encoding=%s | replaced %d occurrence(s)",
			path, mode, encoding, matches)
	}
}

// payloadEditContent decodes the find/content hex or base64 byte sequences and
// edits the unique occurrence of the find bytes: replace swaps them for the
// content bytes, insert writes the content bytes in front of them.
func payloadEditContent(fileContent []byte, encoding, findPayload, contentPayload string, insert bool) ([]byte, int, error) {
	findBytes, err := decodeContentPayload(findPayload, encoding)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid %s find: %w", encoding, err)
	}
	newBytes, err := decodeContentPayload(contentPayload, encoding)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid %s content: %w", encoding, err)
	}
	if len(findBytes) == 0 {
		return nil, 0, fmt.Errorf("find decodes to an empty byte sequence; nothing to edit")
	}
	count := bytes.Count(fileContent, findBytes)
	if count == 0 {
		return nil, 0, fmt.Errorf("find bytes not found in file")
	}
	if count > 1 {
		return nil, 0, fmt.Errorf("find bytes appear %d times. Please provide more context to make it unique", count)
	}
	if !insert {
		return bytes.Replace(fileContent, findBytes, newBytes, 1), 1, nil
	}
	at := bytes.Index(fileContent, findBytes)
	updated := make([]byte, 0, len(fileContent)+len(newBytes))
	updated = append(updated, fileContent[:at]...)
	updated = append(updated, newBytes...)
	updated = append(updated, fileContent[at:]...)
	return updated, 1, nil
}

// literalReplaceText replaces a unique literal occurrence of find.
func literalReplaceText(text, find, content string) (string, int, error) {
	count := strings.Count(text, find)
	switch {
	case count == 0:
		return "", 0, findNotFoundError(text, find)
	case count > 1:
		return "", 0, findNotUniqueError(count)
	}
	return strings.Replace(text, find, content, 1), 1, nil
}

// mismatchDiagnosticMaxBytes caps the file excerpt of a mismatch diagnostic. The
// excerpt is bounded by lines first (see findMismatchDiagnostic), which a file
// written as one enormous line - a minified bundle, for instance - would not
// bound at all.
const mismatchDiagnosticMaxBytes = 2 << 10

// findNotFoundError is the error of a find that matched nothing in text: the
// plain message, or one carrying the mismatch diagnostic when there is one to
// give. Both literal modes share it, so an insert that misses its anchor is
// explained the same way a replace is.
func findNotFoundError(text, find string) error {
	if diag := findMismatchDiagnostic(text, find); diag != "" {
		return fmt.Errorf("`find` not found in file. %s", diag)
	}
	return fmt.Errorf("`find` not found in file. Make sure it matches exactly")
}

// findNotUniqueError is the error of a find that matched more than once. Both
// literal modes share it, like the not-found error above: the condition is the
// same in either mode, and so is the way out.
func findNotUniqueError(count int) error {
	return fmt.Errorf("`find` appears %d times. Please provide more context to make it unique, or use mode='regex' to replace every match", count)
}

// findMismatchDiagnostic attempts to find the longest line-prefix of find that
// appears uniquely in text, and returns a diagnostic message showing the actual
// file content from the last matched line onwards. Returns "" if no diagnostic
// is possible (e.g. find is a single line, or no line-prefix matches uniquely);
// an empty candidate is skipped for the same reason (see the loop below).
func findMismatchDiagnostic(text, find string) string {
	findLines := strings.Split(find, "\n")
	if len(findLines) > 1 && findLines[len(findLines)-1] == "" {
		findLines = findLines[:len(findLines)-1]
	}
	if len(findLines) <= 1 {
		return ""
	}

	f := len(findLines)
	for m := f - 1; m >= 1; m-- {
		candidate := strings.Join(findLines[:m], "\n")
		// An empty candidate occurs at every byte offset, so it says nothing
		// about where the file diverged. Only m == 1 can be empty here, and
		// only when the first line of find is blank.
		if candidate == "" {
			continue
		}
		if strings.Count(text, candidate) != 1 {
			continue
		}
		pos := strings.Index(text, candidate)
		s := strings.Count(text[:pos], "\n") + 1

		// Byte offset of the last matched line within the candidate.
		lastLineOffset := 0
		for i := 0; i < m-1; i++ {
			lastLineOffset += len(findLines[i]) + 1
		}
		lastLinePos := pos + lastLineOffset

		// Show at most f-m+3 lines of the actual file content from the last
		// matched line onwards (the mismatch region plus 2 extra lines as
		// slack for possible empty lines in the file).
		maxLines := f - m + 3
		remaining := text[lastLinePos:]
		if parts := strings.SplitN(remaining, "\n", maxLines+1); len(parts) > maxLines {
			remaining = strings.Join(parts[:maxLines], "\n")
		}
		remaining = cutExcerpt(remaining)

		return fmt.Sprintf(
			"`find` has %d total line(s), only the first %d line(s) matched. "+
				"This matched portion is unique in the file (starts at line %d). "+
				"The exact content from line %d (last matched line) onwards is:\n%s",
			f, m, s, s+m-1, remaining)
	}
	return ""
}

// cutExcerpt bounds the file excerpt of a mismatch diagnostic by bytes: the line
// limit alone still lets one minified line put a whole file into the error
// message, which then travels to the provider. The cut never splits a UTF-8 rune
// and says that it happened.
func cutExcerpt(excerpt string) string {
	if len(excerpt) <= mismatchDiagnosticMaxBytes {
		return excerpt
	}
	cut := excerpt[:mismatchDiagnosticMaxBytes]
	// A cut can land inside a rune: the last rune has to decode, or the byte it
	// is missing is dropped with it (DecodeLastRuneInString reports the
	// truncated tail as RuneError with a size of 1).
	for len(cut) > 0 {
		if r, size := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut + "\n... (rest of the excerpt omitted)"
}

// insertBeforeText inserts content immediately before the unique occurrence of
// find; the match itself is left untouched.
func insertBeforeText(text, find, content string) (string, int, error) {
	at := strings.Index(text, find)
	if at < 0 {
		return "", 0, findNotFoundError(text, find)
	}
	if count := strings.Count(text, find); count > 1 {
		return "", 0, findNotUniqueError(count)
	}
	return text[:at] + content + text[at:], 1, nil
}

// regexReplaceText replaces every RE2 match, however many there are; content
// may use $1, $2 groups.
func regexReplaceText(text, pattern, content string) (string, int, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", 0, fmt.Errorf("invalid regex pattern: %w", err)
	}
	matches := re.FindAllString(text, -1)
	if len(matches) == 0 {
		return "", 0, fmt.Errorf("regex pattern %q does not match any content in the file", pattern)
	}
	return re.ReplaceAllString(text, content), len(matches), nil
}
