package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
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

// eolScanBytes bounds read_file's ending scan: at most 1 MiB of decoded text, and
// it stops as soon as the file is known to mix endings, whichever comes first.
// The window is never cut by it — rows after the scan are still returned — and
// the header does not announce the cap: a file's convention is normally decided
// long before it. docs/tools.md owns the detail.
const eolScanBytes = 1 << 20

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
		"Rows come back exactly as the file holds them, each with its complete line ending (LF, CRLF "+
		"or CR) — only a final row the file leaves unterminated comes back without one — and without "+
		"line numbers; the header states the file line of the first row and reports eol for the "+
		"whole file: a file that mixes endings names its first break. "+
		"start_line is 1-indexed and inclusive, max_lines is a row count, so one call returns "+
		"[start_line, start_line + rows - 1]; continue with start_line = last returned file line + 1. "+
		"Line endings are byte-exact: LF and CRLF are matched and written exactly as the file holds "+
		"them, so a multi-line find copied from here stays byte for byte. With the default 'auto' "+
		"encoding the header also names the charset that was detected (UTF-8, UTF-16/UTF-32, or a "+
		"legacy code page). Per-call limit: up to %d lines / %d bytes.",
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
//
// The window limits what is returned, not what is examined: the decoded stream is
// read ahead so the header's ending report describes the file — the ending of its
// very first break, and whether the file mixes conventions — instead of one page.
// The scan is bounded (see eolScanBytes) and ends as soon as both the window and
// the report are settled, so reading a page near the top of a large file does not
// read the whole file to its end.
func (t *ReadFileLinesTool) readWindow(reader *bufio.Reader, displayName, encodingName string, startLine, limit, sizeBudget int64, stripBOM, hasBOM bool) (string, string, error) {
	lr := newLineReader(reader, int(sizeBudget), stripBOM)
	lineIndex := int64(0)

	// Rows come back verbatim, each with its own terminator: what the model reads
	// is what the file holds, so a find copied from here matches byte for byte.
	// The terminator counts against the byte budget with its row.
	var content strings.Builder
	var outputBytes, linesRead int64
	var byteTruncated, lineTruncated bool
	var endings fileEndings
	windowOpen := true
	scanOpen := true
	scannedBytes := int64(0)
	moreRows := false

	for {
		if !windowOpen && !scanOpen {
			// The output and the ending report are both settled. One more row
			// tells whether anything follows the window; the rest is not read.
			if _, _, _, err := lr.next(); err == nil {
				moreRows = true
			}
			break
		}
		line, term, truncated, err := lr.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", fmt.Errorf("failed to read file content: %v", err)
		}
		lineIndex++
		if scanOpen {
			endings.add(term)
			scannedBytes += int64(len(line) + len(term.seq()))
			if endings.mixed() || scannedBytes >= eolScanBytes {
				scanOpen = false
			}
		}

		// Rows before the window and rows after it are only counted: the
		// ending report needs the file's conventions, the output does not.
		if lineIndex < startLine || !windowOpen {
			continue
		}
		if limit > 0 && linesRead >= limit {
			windowOpen = false
			continue
		}
		remaining := sizeBudget - outputBytes
		if remaining <= 0 {
			byteTruncated = true
			windowOpen = false
			continue
		}
		sep := term.seq()
		if truncated {
			// The reader cut this row mid-way (its own maxBytes cap): the file
			// has no break at the cut, so none is returned and the rest of the
			// row was dropped with it.
			content.WriteString(line)
			outputBytes += int64(len(line))
			linesRead++
			byteTruncated = true
			lineTruncated = true
			windowOpen = false
			continue
		}
		if int64(len(line)+len(sep)) > remaining {
			// The row no longer fits together with its terminator. The line is
			// cut at the budget on a rune boundary, and the terminator is
			// dropped either way: the window ends where the budget ended, not
			// at a file break.
			if int64(len(line)) > remaining {
				line = cutToRuneBoundary(line[:remaining])
				lineTruncated = true
			}
			content.WriteString(line)
			outputBytes += int64(len(line))
			linesRead++
			byteTruncated = true
			windowOpen = false
			continue
		}
		content.WriteString(line)
		content.WriteString(sep)
		outputBytes += int64(len(line) + len(sep))
		linesRead++
	}

	if linesRead == 0 {
		return "", "", nil
	}

	endLine := startLine + linesRead - 1
	eolLabel, eolHint := endings.report()
	header := fmt.Sprintf("[file: %s | lines %d-%d | first row below = file line %d",
		displayName, startLine, endLine, startLine)
	if encodingName != "" {
		header += fmt.Sprintf(" | encoding: %s", encodingName)
	}
	header += fmt.Sprintf(" | bom: %s", yesNo(hasBOM))
	header += fmt.Sprintf(" | eol: %s", eolLabel)
	// The rows carry their endings (often invisible on screen), so the header
	// spells them out whenever they are not plain LF: what a multi-line find or
	// content has to reproduce.
	if eolHint != "" {
		header += " (" + eolHint + ")"
	}
	header += "]"

	var footer string
	switch {
	case lineTruncated:
		footer = fmt.Sprintf("%s\n[TRUNCATED - file line %d exceeded the %d-byte budget and was cut mid-line.]",
			header, endLine, sizeBudget)
	case byteTruncated:
		footer = fmt.Sprintf("%s\n[TRUNCATED - byte budget reached; call read_file start_line=%d max_lines=%d]",
			header, startLine+linesRead, limit)
	case moreRows || lineIndex > startLine+linesRead-1:
		// The window filled before the file ended: rows after it remain.
		footer = fmt.Sprintf("%s\n[PARTIAL - more content remains; call read_file start_line=%d max_lines=%d]",
			header, startLine+linesRead, limit)
	default:
		footer = header + "\n[END OF FILE - no further content.]"
	}
	return content.String(), footer, nil
}

// lineTerm is how a decoded line ended.
type lineTerm int

const (
	termNone lineTerm = iota // the final line had no terminator
	termLF
	termCRLF
	termCR
)

// seq is the terminator's own text: what read_file puts back after a row so the
// returned rows are the file's bytes. termNone is the end of a file that does
// not end with a break, so it adds nothing.
func (t lineTerm) seq() string {
	switch t {
	case termLF:
		return "\n"
	case termCRLF:
		return "\r\n"
	case termCR:
		return "\r"
	}
	return ""
}

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

// lineEndingLabel names the single row ending a text holds, "mixed" when several
// kinds occur, or "none" when no row ended. read_file's header is built from
// fileEndings instead, because it also remembers which ending came first.
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

// lineEndingLabelOf names the row endings of a whole decoded text payload. The
// reader's header comes from fileEndings, which also keeps the first break.
func lineEndingLabelOf(text string) string {
	lf, crlf, cr := lineEndingCounts(text)
	return lineEndingLabel(lf, crlf, cr)
}

// fileEndings counts the row terminators of the decoded prefix read_file scans.
// The header reports the ending of the file's very first break — the break an
// eol=auto write follows — and says so when the scanned rows mix conventions.
// Counting only the returned window would label a page by its own rows, which is
// not what a find has to reproduce.
type fileEndings struct {
	first string // label of the first break: "LF", "CRLF", "CR" or ""
	lf    int
	crlf  int
	cr    int
}

// add records the terminator a row ended with. termNone (a final row without a
// break) is not an ending and adds nothing.
func (e *fileEndings) add(term lineTerm) {
	switch term {
	case termLF:
		e.lf++
		if e.first == "" {
			e.first = "LF"
		}
	case termCRLF:
		e.crlf++
		if e.first == "" {
			e.first = "CRLF"
		}
	case termCR:
		e.cr++
		if e.first == "" {
			e.first = "CR"
		}
	}
}

// mixed reports whether the file holds more than one kind of row ending.
func (e *fileEndings) mixed() bool {
	kinds := 0
	for _, count := range []int{e.lf, e.crlf, e.cr} {
		if count > 0 {
			kinds++
		}
	}
	return kinds > 1
}

// kinds names the endings the file really holds, in a fixed order, for the mixed
// report.
func (e *fileEndings) kinds() string {
	var names []string
	if e.lf > 0 {
		names = append(names, "LF")
	}
	if e.crlf > 0 {
		names = append(names, "CRLF")
	}
	if e.cr > 0 {
		names = append(names, "CR")
	}
	switch len(names) {
	case 0:
		return "no row break"
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// report is the two pieces the header carries: the label, which is the ending
// of the file's very first break ("LF", "CRLF", "CR") or "none", and — only when
// the file mixes conventions — the note that names the mix and points at that
// first break. A uniform file needs no note: the label is the whole story.
func (e *fileEndings) report() (label, hint string) {
	if e.first == "" {
		return "none", ""
	}
	if !e.mixed() {
		return e.first, ""
	}
	return e.first, fmt.Sprintf("the file mixes %s; follow its first line break", e.kinds())
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
		"Mode: 'o' overwrite (default), 'a' append, 'c' create  only if the file does not exist. Line endings are byte-exact: "+
		"content written with LF or CRLF keeps exactly those breaks unless eol asks for a rewrite. By default (eol='auto') "+
		"they are written with the ending the file's own first row break uses; eol='keep' writes them exactly as given and "+
		"eol='lf'/'crlf'/'cr' picks one. The system never "+
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
				"description": fmt.Sprintf("Content to write. Any line beyond the limit [%d lines] is truncated and discarded. Line endings are written byte for byte (LF or CRLF as given, unless eol rewrites them).", t.fs.MaxWriteLines),
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
			"eol": map[string]any{
				"type":        "string",
				"default":     eolAuto,
				"description": "'auto' (default) writes content's line endings as the ending the file's own first row break uses (LF, CRLF or CR), so a file stays in the one convention it had; 'keep' writes them exactly as given; 'lf', 'crlf' or 'cr' rewrite them as that ending. A file that does not exist yet, is empty or holds no row break at all leaves nothing to follow, and content is written as given. A payload without a line break is written as given, and no trailing newline is ever added. The result names the ending on its first line whenever content's endings were really rewritten. Text content only: not available with encoding='hex'/'base64'.",
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
	eolRaw, _ := stringArg(args, "eol")
	eolReq, eolErr := parseEOLArg(eolRaw)
	if eolErr != nil {
		return Fail(eolErr.Error())
	}
	if contentEncodingIsBinary(encoding) && eolReq != eolKeep && eolReq != eolAuto {
		return Fail(fmt.Sprintf("eol is only available for text content; with encoding=%s the payload is written as the bytes it decodes to", encoding))
	}
	// A byte order mark belongs at the very start of a file: append writes the
	// same codec without one, so the mark the create/overwrite call laid down is
	// left untouched.
	writeEnc := encoding
	if appendMode {
		writeEnc = appendEncodingNoBOM(encoding)
	}

	var note, eolField string
	var data []byte
	if contentEncodingIsBinary(encoding) {
		data, err = decodeContentPayload(content, writeEnc)
		if err != nil {
			return Fail(fmt.Sprintf("failed to decode %s content: %v", encoding, err))
		}
	} else {
		kept, dropped, total, truncated := enforceWriteLineLimit(content, t.fs.MaxWriteLines)
		// The eol request is answered for the payload that is really written;
		// the line accounting above stays on the text as it was given.
		kept, eolField = respellPayload(kept, eolTarget(eolReq, detectFileEOL(path, encoding)), eolReq)
		switch {
		case truncated:
			// The cut restores the separator of the last kept line, so the tail
			// that resumes the write is the only concern left to report.
			note = truncationNote(dropped, t.fs.MaxWriteLines, total)
		case kept != "" && !hasRowEndingSuffix(kept):
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
		return OK(fmt.Sprintf("Appended to %s%s%s", path, eolField, note))

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
		return OK(fmt.Sprintf("File created: %s%s%s", path, eolField, note))

	default: // overwrite
		existed := true
		if _, err := os.Stat(path); os.IsNotExist(err) {
			existed = false
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return Fail(err.Error())
		}
		if existed {
			return OK(fmt.Sprintf("File overwritten: %s%s%s", path, eolField, note))
		}
		return OK(fmt.Sprintf("File created: %s (did not previously exist)%s%s", path, eolField, note))
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
		"(mode='a'), begin its content with a line break (CRLF for a CRLF file, LF otherwise).]"
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
		"how find is matched and where content lands. find and content are matched and written byte for " +
		"byte, line endings included: LF and CRLF are taken exactly as they are, so a multi-line find " +
		"has to carry the same line endings as the rows read_file returned. content's line endings " +
		"follow the file's own first row break by default (eol='auto'); eol='keep' writes them exactly " +
		"as given and eol='lf'/'crlf'/'cr' picks one. find is matched byte for byte first and is never " +
		"rewritten: a find the file only holds with other row endings is matched anyway, with the " +
		"file's own endings, and the result says so in a warning — send the endings the file has " +
		"(copy the rows read_file returned) to edit without one. Every edit reports what it did — the " +
		"file is never rewritten outside the match. A find that is not unique, or that matches " +
		"nothing, is reported as an error so the call can be retried with more context."
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
				"description": "Text to locate: an exact literal for mode='replace'/'insert' (must occur exactly once; a multi-line literal must carry the file's own line endings — LF or CRLF, byte for byte, as copied from the rows read_file returned. A literal the file only holds with other row endings is still matched, with the file's own endings, and the result carries a warning), an RE2 pattern for mode='regex', or the hex/base64 byte sequence when encoding is a binary representation.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Text written at the match: the replacement for mode='replace'/'regex', or the text inserted before the match for mode='insert'. Line endings are written byte for byte (LF or CRLF as given; eol='auto' follows the file's first break). In mode='regex' it may reference capture groups with $1, $2, ... ($$ = literal $).",
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
			"eol": map[string]any{
				"type":        "string",
				"default":     eolAuto,
				"description": "'auto' (default) writes content's line endings as the ending the file's own first row break uses (LF, CRLF or CR), so a file stays in the one convention it had; 'keep' writes them exactly as given; 'lf', 'crlf' or 'cr' rewrite them as that ending. Only content is rewritten: find is matched byte for byte and is never rewritten by this — a find the file only holds with other row endings is matched with the file's own ones and reported as a warning — a payload without a line break is written as given, and the file is never rewritten outside the match (convert a file's endings on purpose with mode='regex'). The result names the ending on its first line whenever content's endings were really rewritten. Text edits only: not available with encoding='hex'/'base64'.",
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
	eolRaw, _ := stringArg(args, "eol")
	eolReq, eolErr := parseEOLArg(eolRaw)
	if eolErr != nil {
		return Fail(eolErr.Error())
	}
	if contentEncodingIsBinary(encoding) && eolReq != eolKeep && eolReq != eolAuto {
		return Fail(fmt.Sprintf("eol is only available for text edits; with encoding=%s find and content are written as the bytes they decode to", encoding))
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
	note, warning, eolField := "", "", ""

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
		// The file is matched as it stands — read_file hands rows over verbatim,
		// so a find copied from it matches byte for byte — and written back
		// unchanged outside the match: the file's own endings are never
		// rewritten behind the model's back (the eol argument below rewrites the
		// payload it is given instead, and only when it is asked to). A find
		// spelled with other row endings is matched once more with the file's
		// own ones and the result says so (see applyTextEdit).
		before := lineEndingLabelOf(text)
		// The eol request is answered on the payload alone; find is matched as it
		// stands and is never rewritten by it (a find the file only holds with
		// other row endings is a relaxed match that reports itself — see
		// applyTextEdit and findEndingsWarning).
		content, eolField = respellPayload(content, eolTarget(eolReq, firstRowEnding(text)), eolReq)
		after, m, relaxed, eerr := applyTextEdit(text, find, content, mode)
		if eerr != nil {
			// Nothing was written, so the eol request has nothing to report.
			return Fail(eerr.Error())
		}
		matches = m
		warning = relaxed
		note = editLineEndingNote(after, before)
		newBytes, err = encodeTextToFileBytes(after, encoding)
		if err != nil {
			return Fail(err.Error())
		}
	}

	if err := os.WriteFile(path, newBytes, 0o644); err != nil {
		return Fail(err.Error())
	}

	return OK(editResultMessage(path, encoding, mode, find, matches, eolField) + warning + note)
}

// applyTextEdit runs the edit against the decoded file text. A literal find is
// matched byte for byte first — read_file hands rows over verbatim, so a find
// copied from it lands as written. A find the file only holds with other row
// endings is matched a second time with the file's own ones (see
// relaxedLiteralEdit): the edit still happens, and the result carries a warning
// naming the difference, because the bytes that were matched are not spelled the
// way find spelled them. regex and the byte-level encodings are matched strictly
// literally and are never relaxed.
func applyTextEdit(text, find, content, mode string) (result string, matches int, warning string, err error) {
	if mode == editModeRegex {
		out, n, rerr := regexReplaceText(text, find, content)
		return out, n, "", rerr
	}

	insert := mode == editModeInsert
	var out string
	var n int
	if insert {
		out, n, err = insertBeforeText(text, find, content)
	} else {
		out, n, err = literalReplaceText(text, find, content)
	}
	if err == nil {
		return out, n, "", nil
	}
	if !errors.Is(err, errFindNotFound) {
		// Not a miss (an ambiguous find, say): report it as it is.
		return "", 0, "", err
	}
	miss := err

	relaxed, count, rerr := relaxedLiteralEdit(text, find, content, insert)
	switch {
	case rerr != nil || count == 0:
		// The endings are not the whole difference either (or the find is not
		// there at all): the miss explains it better than a relaxed count.
		return "", 0, "", miss
	case count > 1:
		return "", 0, "", findEndingsAmbiguousError(count)
	}
	return relaxed, 1, findEndingsWarning(text, find), nil
}

// rowEndingTolerantEnding is what every row ending a literal find carries
// matches in the relaxed pattern: any of the endings a file may hold, so a find
// written in one convention can land on a file written in another.
const rowEndingTolerantEnding = `(?:\r\n|\r|\n)`

// rowEndingTolerantPattern turns a literal find into the pattern the relaxed
// match runs: every byte find spells is quoted, and each row ending it carries
// matches any row ending. A find without an ending is its own quoted literal.
func rowEndingTolerantPattern(find string) string {
	var b strings.Builder
	start := 0
	for i := 0; i < len(find); i++ {
		if find[i] != '\r' && find[i] != '\n' {
			continue
		}
		b.WriteString(regexp.QuoteMeta(find[start:i]))
		b.WriteString(rowEndingTolerantEnding)
		if find[i] == '\r' && i+1 < len(find) && find[i+1] == '\n' {
			i++
		}
		start = i + 1
	}
	b.WriteString(regexp.QuoteMeta(find[start:]))
	return b.String()
}

// relaxedLiteralEdit applies a literal replace or insert across the row endings:
// the find is matched on a pattern in which each ending it spells matches any
// ending, so it lands on the bytes the file really holds — those bytes are what
// gets edited, and every other row keeps its own ending. insert keeps the match
// and writes content in front of it. count is how many places the find matches
// that way (the caller requires exactly one); it is 0 when the find is not there
// at all, and more than one when ignoring the endings made it ambiguous. content
// is written verbatim: nothing in it is expanded, unlike mode='regex'.
func relaxedLiteralEdit(text, find, content string, insert bool) (string, int, error) {
	re, err := regexp.Compile(rowEndingTolerantPattern(find))
	if err != nil {
		return "", 0, err
	}
	locs := re.FindAllStringIndex(text, -1)
	switch {
	case len(locs) == 0:
		return "", 0, nil
	case len(locs) > 1:
		return "", len(locs), nil
	}
	start, end := locs[0][0], locs[0][1]
	if insert {
		return text[:start] + content + text[start:], 1, nil
	}
	return text[:start] + content + text[end:], 1, nil
}

// findEndingsAmbiguousError is the relaxed match's not-unique error: the find is
// not in the file byte for byte, and ignoring the row endings leaves more than
// one place it could land on, so nothing was written.
func findEndingsAmbiguousError(count int) error {
	return fmt.Errorf("%w: the file holds that text %d times with its own row endings; matching it while "+
		"ignoring the endings is ambiguous, so nothing was written. Add more context to make it unique, "+
		"or send the endings the file has (copy the rows read_file returned)", errFindNotFound, count)
}

// findEndingsWarning is the note an edit carries when its literal find was not in
// the file byte for byte and was matched with the file's own row endings instead.
// The edit did happen — that is what the relaxed match is for — but it happened
// on bytes spelled differently than find asked for, so the difference is named
// rather than absorbed silently, and the note says the edit stayed inside the
// matched range.
func findEndingsWarning(text, find string) string {
	return fmt.Sprintf(" | warning: `find` spells row endings the file does not hold (%s vs %s); "+
		"the match was made with the file's own endings, and only the matched range was touched",
		rowEndingPhrase(lineEndingLabelOf(find)), rowEndingPhrase(lineEndingLabelOf(text)))
}

// rowEndingPhrase spells a row-ending label out for a sentence: its name (LF,
// CRLF or CR), and a plain phrase for a text that mixes endings or has none.
// Endings are always named by label, never by an escape sequence, so the name
// cannot be mistaken for text that has to be typed.
func rowEndingPhrase(label string) string {
	switch label {
	case "LF", "CRLF", "CR":
		return label
	case "mixed":
		return "mixed endings"
	}
	return "no row ending at all"
}

// eolSpelling renders a row ending the way a result talks about it: its name
// alone (CRLF, LF or CR), never an escape sequence.
func eolSpelling(sep string) string { return lineEndingLabelOf(sep) }

// eolTarget resolves an eol request into the ending the payload has to be written
// with: "" means it is written exactly as given. fileEnding is the ending the
// target file's first row break uses (what 'auto' follows); it is "" when the
// file does not exist yet, is empty, or holds no row break at all.
func eolTarget(requested, fileEnding string) string {
	switch requested {
	case eolKeep:
		return ""
	case eolAuto:
		return fileEnding
	}
	return requested
}

// respellPayload answers an eol request on a payload that is about to be written:
// the payload written with sep, plus the field the result's first line carries
// about it. The field is empty — nothing is said — unless the payload's own
// endings are really rewritten: a payload already written with that ending, or
// one without a row break at all, is left exactly as it came in.
func respellPayload(payload, sep, requested string) (string, string) {
	if sep == "" {
		return payload, ""
	}
	rewritten := writeLineEndingsAs(payload, sep)
	if rewritten == payload {
		return payload, ""
	}
	source := "as asked"
	if requested == eolAuto {
		source = "following the file"
	}
	return rewritten, fmt.Sprintf(" | eol: content written with %s, %s", eolSpelling(sep), source)
}

// detectFileEOL reports the row ending the file at path opens with: the ending of
// its first row break, which is what an 'auto' request follows even when the rest
// of the file mixes endings. It is "" when the file does not exist, cannot be
// read or decoded, or holds no row break at all — there is then nothing to
// follow, and the payload is written as it was given.
func detectFileEOL(path, encoding string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text, err := decodeFileBytesToText(raw, encoding)
	if err != nil {
		return ""
	}
	return firstRowEnding(text)
}

// editLineEndingNote explains a result whose row endings are worth a word: an
// edit whose content mixed the file's endings, or one that changed the file's
// convention. It reports — it never rewrites. beforeLabel is the label of the
// file as it was found.
func editLineEndingNote(after, beforeLabel string) string {
	var b strings.Builder
	afterLabel := lineEndingLabelOf(after)
	switch {
	case beforeLabel != afterLabel && afterLabel == "mixed":
		b.WriteString(" | note: the file's rows now mix endings — the content's row endings differ from the file's")
		if isUniformEOL(beforeLabel) {
			fmt.Fprintf(&b, " (write them as %s to match)", beforeLabel)
		}
		b.WriteString("; use mode='regex' to change a file's endings on purpose")
	case isUniformEOL(beforeLabel) && isUniformEOL(afterLabel) && beforeLabel != afterLabel:
		fmt.Fprintf(&b, " | note: the file's row endings changed from %s to %s", beforeLabel, afterLabel)
	}
	return b.String()
}

// isUniformEOL reports whether a label names one row ending a payload can be
// written with ("LF", "CRLF" or "CR"); "mixed" and "none" name no convention.
func isUniformEOL(label string) bool {
	switch label {
	case "LF", "CRLF", "CR":
		return true
	}
	return false
}

// editResultMessage reports what the edit did, including how many occurrences
// were matched: regex mode may legitimately hit more than one. eolField is the
// clause an eol request added to the first line ("" when no ending was rewritten,
// so a default edit's line reads exactly as it always did).
func editResultMessage(path, encoding, mode, find string, matches int, eolField string) string {
	switch mode {
	case editModeInsert:
		return fmt.Sprintf("File edited: %s | mode=%s | encoding=%s%s | content inserted before the match (1 occurrence)",
			path, mode, encoding, eolField)
	case editModeRegex:
		return fmt.Sprintf("File edited: %s | mode=%s | encoding=%s%s | regex %q matched %d occurrence(s), all replaced",
			path, mode, encoding, eolField, find, matches)
	default:
		return fmt.Sprintf("File edited: %s | mode=%s | encoding=%s%s | replaced %d occurrence(s)",
			path, mode, encoding, eolField, matches)
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

// errFindNotFound is the miss a literal find reports. The caller tells it apart
// from an ambiguous hit to decide whether matching the find across the row
// endings is worth a try.
var errFindNotFound = errors.New("`find` not found in file")

// findNotFoundError is the error of a find that matched nothing in text: the
// plain message, one that calls out a find the file holds with other row endings
// (the fallback for a relaxed match that could not settle the spot — nothing is
// written for it, so the find has to be sent the way the file spells it), or one
// carrying the mismatch diagnostic when there is one to give. Both literal modes
// share it, so an insert that misses its anchor is explained the same way a
// replace is. The file's own row endings are named when they are not plain LF,
// and the diagnostic runs over a flat LF view, so its line numbers and excerpt
// read the same whatever the endings are.
func findNotFoundError(text, find string) error {
	hint := eolFindHint(text)
	if findDiffersOnlyByEndings(text, find) {
		return fmt.Errorf("%w: the file holds that text, but with different row endings. "+
			"%s`find` is matched byte for byte and is never rewritten, so nothing was written: "+
			"send the endings the file has (copy the rows read_file returned) and call again.",
			errFindNotFound, hint)
	}
	if diag := findMismatchDiagnostic(normalizeTextLinesToLF(text), normalizeTextLinesToLF(find)); diag != "" {
		return fmt.Errorf("%w. %s%s", errFindNotFound, hint, diag)
	}
	return fmt.Errorf("%w. %sMake sure it matches exactly", errFindNotFound, hint)
}

// findDiffersOnlyByEndings reports whether a find the file does not hold verbatim
// is in it once both sides are read with plain LF row endings — i.e. row endings
// are the whole difference. A find without a row ending of its own cannot differ
// that way: it would be in the file verbatim.
func findDiffersOnlyByEndings(text, find string) bool {
	if !strings.ContainsAny(find, "\r\n") {
		return false
	}
	return strings.Contains(normalizeTextLinesToLF(text), normalizeTextLinesToLF(find))
}

// eolFindHint names the row endings a multi-line find has to reproduce, for a
// file whose rows do not end with plain LF (the default says nothing).
func eolFindHint(text string) string {
	switch lineEndingLabelOf(text) {
	case "CRLF":
		return "The file's rows end with CRLF; a multi-line find has to carry the same endings byte for byte. "
	case "CR":
		return "The file's rows end with CR; a multi-line find has to carry the same ending byte for byte. "
	case "mixed":
		return "The file's rows mix endings; a multi-line find has to carry the same endings as the rows it copies. "
	}
	return ""
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
