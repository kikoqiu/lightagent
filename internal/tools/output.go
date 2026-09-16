package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// ansiEscapePattern matches cosmetic ANSI CSI sequences (colors, cursor moves).
var ansiEscapePattern = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]")

// stripANSI removes color/formatting escapes, preserving visible text.
func stripANSI(s string) string {
	return ansiEscapePattern.ReplaceAllString(s, "")
}

// normalizeTerminalOutput sanitizes raw terminal bytes: CRLF→LF, strip ANSI,
// then replay carriage-return/backspace rewrites so progress bars collapse.
func normalizeTerminalOutput(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = stripANSI(raw)
	return emulateLineTerminal(raw)
}

// emulateLineTerminal applies a minimal line-terminal model.
func emulateLineTerminal(input string) string {
	rows := make([]string, 0, 8)
	var row []byte
	for i := 0; i < len(input); i++ {
		switch c := input[i]; c {
		case '\n':
			rows = append(rows, string(row))
			row = row[:0]
		case '\r':
			row = row[:0]
		case '\b':
			if len(row) > 0 {
				row = row[:len(row)-1]
			}
		default:
			row = append(row, c)
		}
	}
	if len(row) > 0 || len(rows) == 0 || !strings.HasSuffix(input, "\n") {
		rows = append(rows, string(row))
	}
	return strings.Join(rows, "\n")
}

// splitFoldLines splits output into rows, dropping the trailing empty element.
func splitFoldLines(s string) []string {
	lines := strings.Split(s, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		return lines[:len(lines)-1]
	}
	return lines
}

// foldMarkerFmt renders the marker replacing the folded middle of oversized output.
const foldMarkerFmt = "\n... [Folded: %d lines / %d bytes omitted. Output exceeded limit. Use grep/tail or pagination to view] ...\n"

// foldOutput keeps the head 25% and tail 75% of the line/char budget and marks
// the omitted middle. It returns the folded text and whether folding happened.
func foldOutput(clean string, maxLines, maxChars int) (string, bool) {
	if maxLines <= 0 {
		maxLines = 200
	}
	if maxChars <= 0 {
		maxChars = 30000
	}

	lines := splitFoldLines(clean)
	if len(lines) == 0 {
		if clean == "" {
			return clean, false
		}
		lines = []string{clean}
	}

	totalLines := len(lines)
	joined := strings.Join(lines, "\n")
	totalBytes := len(joined)

	linesOver := totalLines > maxLines
	bytesOver := totalBytes > maxChars
	if !linesOver && !bytesOver {
		return clean, false
	}

	if !linesOver {
		headChars := maxInt(1, maxChars/4)
		tailChars := maxInt(1, maxChars-headChars)
		omittedBytes := maxInt(0, totalBytes-headChars-tailChars)
		marker := fmt.Sprintf(foldMarkerFmt, 0, omittedBytes)
		tailStart := maxInt(0, totalBytes-tailChars)
		folded := joined[:headChars] + marker + joined[tailStart:]
		if len(folded) > maxChars {
			folded = folded[:maxChars]
		}
		return folded, true
	}

	headLineBudget := maxInt(1, maxLines/4)
	tailLineBudget := maxInt(1, maxLines-headLineBudget)
	headCharsBudget := maxInt(1, maxChars/4)
	tailCharsBudget := maxChars - headCharsBudget

	headRows, headSize := 0, 0
	for headRows < totalLines && headRows < headLineBudget {
		next := len(lines[headRows])
		if headRows > 0 {
			next++
		}
		if headRows > 0 && headSize+next > headCharsBudget {
			break
		}
		headSize += next
		headRows++
	}

	tailRows, tailSize := 0, 0
	for tailRows < totalLines-headRows && tailRows < tailLineBudget {
		idx := totalLines - 1 - tailRows
		next := len(lines[idx])
		if tailRows > 0 {
			next++
		}
		if tailRows > 0 && tailSize+next > tailCharsBudget {
			break
		}
		tailSize += next
		tailRows++
	}

	omittedLines := maxInt(0, totalLines-headRows-tailRows)
	omittedBytes := maxInt(0, totalBytes-headSize-tailSize)

	headText := strings.Join(lines[:headRows], "\n")
	tailText := strings.Join(lines[totalLines-tailRows:], "\n")
	marker := fmt.Sprintf(foldMarkerFmt, omittedLines, omittedBytes)

	folded := headText + marker + tailText
	if len(folded) > maxChars {
		keepHead := minInt(len(headText), maxChars/4)
		keepTail := minInt(len(tailText), maxChars-len(marker)-keepHead)
		if keepTail < 0 {
			keepTail = 0
		}
		folded = headText[:keepHead] + marker + tailText[len(tailText)-keepTail:]
	}
	return folded, true
}

// countLinesAndBytes returns the visible row count and byte length.
func countLinesAndBytes(clean string) (lines, bytes int) {
	if clean == "" {
		return 0, 0
	}
	return len(splitFoldLines(clean)), len(clean)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
