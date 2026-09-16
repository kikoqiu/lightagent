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
)

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
func (t *ReadFileLinesTool) Name() string { return "read_file_lines" }

// Description implements Tool.
func (t *ReadFileLinesTool) Description() string {
	return fmt.Sprintf("Read a text file line by line (source, markdown, logs, configs). "+
		"Rows are returned without line numbers; the header states the file line of the first "+
		"row. CRLF is normalized to LF. start_line is 1-indexed and inclusive, max_lines is a row "+
		"count, so one call returns [start_line, start_line + rows - 1]; continue with "+
		"start_line = last returned file line + 1. Per-call limit: up to %d lines / %d bytes. "+
		"encoding: 'utf8' (default) reads UTF-8 text; other charset labels (gbk, big5, "+
		"shift_jis, euc-jp, euc-kr, windows-1252) decode each line into UTF-8 text.",
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
				"default":     "utf8",
				"description": "'utf8' (default) or a charset label (gbk, big5, shift_jis, euc-jp, euc-kr, windows-1252).",
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
	encoding := normalizeContentEncoding(encodingRaw)
	if contentEncodingIsBinary(encoding) {
		return Fail(fmt.Sprintf("encoding %q is a binary representation and cannot be applied line by line; use a charset label such as gbk, big5 or shift_jis", encoding))
	}
	if encoding != contentEncodingUTF8 {
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

	sample := make([]byte, 512)
	n, readErr := f.Read(sample)
	if readErr != nil && readErr != io.EOF {
		return Fail(fmt.Sprintf("failed to read file: %v", readErr))
	}
	sample = sample[:n]
	if encoding == contentEncodingUTF8 && looksBinary(sample) {
		return Fail("file appears to be binary; pass an encoding label (e.g. gbk) if it is a legacy-encoded text file")
	}

	reader := bufio.NewReaderSize(io.MultiReader(bytes.NewReader(sample), f), 64*1024)
	content, footer, err := t.readWindow(reader, filepath.Base(path), encoding, int64(startLine), limit, sizeBudget)
	if err != nil {
		return Fail(err.Error())
	}
	if content == "" {
		return OK(fmt.Sprintf("[END OF FILE - no content at or after start_line=%d]", startLine))
	}
	return OK(footer + "\n\n" + content)
}

// readWindow reads [startLine, startLine+limit-1] honoring the byte budget and
// returns the content plus a header/footer describing the window.
func (t *ReadFileLinesTool) readWindow(reader *bufio.Reader, displayName, encoding string, startLine, limit, sizeBudget int64) (string, string, error) {
	lineIndex := int64(1)
	reachedEOF := false

	for lineIndex < startLine {
		_, rerr := readLine(reader)
		if rerr == io.EOF {
			reachedEOF = true
			break
		}
		if rerr != nil {
			return "", "", fmt.Errorf("failed to read file content: %v", rerr)
		}
		lineIndex++
	}
	if reachedEOF {
		return "", "", nil
	}

	var lines []string
	var outputBytes, linesRead int64
	var byteTruncated, lineTruncated bool

	for (limit < 0 || linesRead < limit) && !reachedEOF {
		remaining := sizeBudget - outputBytes
		if remaining <= 0 {
			byteTruncated = true
			break
		}
		raw, rerr := readLine(reader)
		if rerr != nil && rerr != io.EOF {
			return "", "", fmt.Errorf("failed to read file content: %v", rerr)
		}
		if rerr == io.EOF && raw == "" {
			reachedEOF = true
			break
		}
		display := strings.TrimSuffix(raw, "\r")
		if encoding != contentEncodingUTF8 {
			decoded, derr := decodeFileBytesToText([]byte(raw), encoding)
			if derr != nil {
				return "", "", derr
			}
			display = strings.TrimSuffix(decoded, "\r")
		}
		line := display
		if int64(len(line)) > remaining {
			lines = append(lines, line[:remaining])
			byteTruncated = true
			lineTruncated = true
			linesRead++
			break
		}
		lines = append(lines, line)
		outputBytes += int64(len(line))
		linesRead++
		lineIndex++
		if rerr == io.EOF {
			reachedEOF = true
			break
		}
	}

	if len(lines) == 0 {
		return "", "", nil
	}

	content := strings.Join(lines, "\n")
	endLine := startLine + linesRead - 1
	header := fmt.Sprintf("[file: %s | lines %d-%d | first row below = file line %d]",
		displayName, startLine, endLine, startLine)

	var footer string
	switch {
	case lineTruncated:
		footer = fmt.Sprintf("%s\n[TRUNCATED - file line %d exceeded the %d-byte budget and was cut mid-line.]",
			header, endLine, sizeBudget)
	case byteTruncated:
		footer = fmt.Sprintf("%s\n[TRUNCATED - byte budget reached; call read_file_lines start_line=%d max_lines=%d]",
			header, startLine+linesRead, limit)
	case !reachedEOF && limit > 0 && linesRead >= limit:
		footer = fmt.Sprintf("%s\n[PARTIAL - more content remains; call read_file_lines start_line=%d max_lines=%d]",
			header, startLine+linesRead, limit)
	default:
		footer = header + "\n[END OF FILE - no further content.]"
	}
	return content, footer, nil
}

// readLine reads one line, stripping the trailing newline.
func readLine(r *bufio.Reader) (string, error) {
	s, err := r.ReadString('\n')
	if err != nil {
		return s, err
	}
	return strings.TrimSuffix(s, "\n"), nil
}

// looksBinary reports whether a byte sample looks like binary content.
func looksBinary(sample []byte) bool {
	return bytes.IndexByte(sample, 0) >= 0
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
				"description": "'utf8' (default) writes text; 'hex'/'base64' decode an encoded binary payload; other values are charset labels (gbk, big5, shift_jis, windows-1252) that encode the text into that charset.",
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

	var note string
	var data []byte
	if contentEncodingIsBinary(encoding) {
		data, err = decodeContentPayload(content, encoding)
		if err != nil {
			return Fail(fmt.Sprintf("failed to decode %s content: %v", encoding, err))
		}
	} else {
		kept, dropped, total, truncated := enforceWriteLineLimit(content, t.fs.MaxWriteLines)
		if truncated {
			note = truncationNote(dropped, t.fs.MaxWriteLines, total)
		}
		data, err = decodeContentPayload(kept, encoding)
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

// EditFileTool edits a file by replacing old_text with new_text.
type EditFileTool struct{}

// NewEditFileTool creates the editor.
func NewEditFileTool() *EditFileTool { return &EditFileTool{} }

// Name implements Tool.
func (t *EditFileTool) Name() string { return "edit_file" }

// Description implements Tool.
func (t *EditFileTool) Description() string {
	return "Edit a file by replacing old_text with new_text. Literal (default): old_text must " +
		"appear exactly once. regex=true: old_text is an RE2 regex, every match is replaced, and " +
		"new_text may reference capture groups with $1, $2, ... ($$ = literal $). JSON escaping " +
		"applies: \\n = newline, \\\\n = literal backslash-n. Only utf8 is supported."
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
			"old_text": map[string]any{
				"type":        "string",
				"description": "Text to find: exact literal (must occur exactly once), or an RE2 regex when regex=true.",
			},
			"new_text": map[string]any{
				"type":        "string",
				"description": "Replacement text; with regex=true use $1, $2, ... for capture groups.",
			},
			"regex": map[string]any{
				"type":        "boolean",
				"default":     false,
				"description": "Treat old_text as an RE2 regex and replace every match.",
			},
			"encoding": map[string]any{
				"type":        "string",
				"default":     "utf8",
				"description": "'utf8' (default) text; 'hex'/'base64' binary payloads (regex disabled); other values are charset labels (gbk, big5, shift_jis, windows-1252) for non-UTF-8 files.",
			},
		},
		"required": []string{"path", "old_text", "new_text"},
	}
}

// Execute implements Tool.
func (t *EditFileTool) Execute(ctx context.Context, args map[string]any) *Result {
	path, ok := stringArg(args, "path")
	if !ok || trimSpace(path) == "" {
		return Fail("path is required")
	}
	oldText, ok := stringArg(args, "old_text")
	if !ok {
		return Fail("old_text is required")
	}
	newText, ok := stringArg(args, "new_text")
	if !ok {
		return Fail("new_text is required")
	}
	encodingRaw, _ := stringArg(args, "encoding")
	encoding := normalizeContentEncoding(encodingRaw)
	regexMode := boolArg(args, "regex")
	if regexMode && contentEncodingIsBinary(encoding) {
		return Fail(fmt.Sprintf("regex mode is only supported with text encodings; binary edits with encoding=%s are literal byte-sequence replacements", encoding))
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
		newBytes, matches, err = payloadReplaceContent(raw, encoding, oldText, newText)
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
		if regexMode {
			after, matches, err = regexReplaceText(text, oldText, newText)
		} else {
			after, matches, err = literalReplaceText(text, oldText, newText)
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

	msg := fmt.Sprintf("File edited: %s | encoding=%s | replaced %d occurrence(s)", path, encoding, matches)
	if regexMode {
		msg += fmt.Sprintf(" | regex=%q", oldText)
	}
	return OK(msg)
}

// payloadReplaceContent decodes the old/new hex or base64 byte sequences and
// replaces a unique occurrence of the old bytes in the file content.
func payloadReplaceContent(content []byte, encoding, oldPayload, newPayload string) ([]byte, int, error) {
	oldBytes, err := decodeContentPayload(oldPayload, encoding)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid %s old_text: %w", encoding, err)
	}
	newBytes, err := decodeContentPayload(newPayload, encoding)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid %s new_text: %w", encoding, err)
	}
	if len(oldBytes) == 0 {
		return nil, 0, fmt.Errorf("old_text decodes to an empty byte sequence; nothing to replace")
	}
	count := bytes.Count(content, oldBytes)
	if count == 0 {
		return nil, 0, fmt.Errorf("old_text bytes not found in file")
	}
	if count > 1 {
		return nil, 0, fmt.Errorf("old_text bytes appear %d times. Please provide more context to make it unique", count)
	}
	return bytes.Replace(content, oldBytes, newBytes, 1), 1, nil
}

// literalReplaceText replaces a unique literal occurrence of oldText.
func literalReplaceText(text, oldText, newText string) (string, int, error) {
	if !strings.Contains(text, oldText) {
		return "", 0, fmt.Errorf("old_text not found in file. Make sure it matches exactly")
	}
	count := strings.Count(text, oldText)
	if count > 1 {
		return "", 0, fmt.Errorf("old_text appears %d times. Please provide more context to make it unique", count)
	}
	return strings.Replace(text, oldText, newText, 1), 1, nil
}

// regexReplaceText replaces every RE2 match; newText may use $1, $2 groups.
func regexReplaceText(text, pattern, newText string) (string, int, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", 0, fmt.Errorf("invalid regex pattern: %w", err)
	}
	matches := re.FindAllString(text, -1)
	if len(matches) == 0 {
		return "", 0, fmt.Errorf("regex pattern %q does not match any content in the file", pattern)
	}
	return re.ReplaceAllString(text, newText), len(matches), nil
}
