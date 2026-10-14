package tools

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// cwdRelativeMarker is what the output cleanup writes in place of the process
// working directory: the relative path to that directory, spelled with the
// host's own separator ("\" on Windows, "/" elsewhere) because that is the form
// the local tools of that host resolve.
func cwdRelativeMarker() string {
	return "." + string(os.PathSeparator)
}

// sanitizeScriptOutput runs the shared command-output governance pipeline (see
// sanitizeAndFold) with the working-directory cleanup that the run_script
// surface adds on top: every mention of dir, the absolute directory the script
// ran in ("" when it could not be resolved), is replaced by a relative marker
// and a header line naming the substitution opens the delivered output (see
// replaceWorkingDir). The replacement happens before the fold, so a folded
// output carries no fragment of an absolute path. The counters and the fold
// describe the cleaned output; the header is a note above it, so it is neither
// counted nor folded.
func sanitizeScriptOutput(raw string, maxLines, maxChars int, dir string) (shown string, truncated bool, totalLines, totalBytes int) {
	clean := normalizeTerminalOutput(raw)
	clean, replaced := replaceWorkingDir(clean, dir)
	shown, truncated = foldOutput(clean, maxLines, maxChars)
	totalLines, totalBytes = countLinesAndBytes(clean)
	if replaced {
		shown = workingDirCleanupHeader(dir) + "\n" + shown
	}
	return shown, truncated, totalLines, totalBytes
}

// workingDirCleanupHeader renders the line a command result carries above its
// output when the cleanup substituted the working directory: it names the
// directory that was hidden, in the absolute form the script wrote it, and the
// relative marker that took its place, so a reader knows why the paths below
// read as they do.
func workingDirCleanupHeader(dir string) string {
	return fmt.Sprintf("[output: cwd %s replaced by %s]", dirWithSeparator(dir), cwdRelativeMarker())
}

// dirWithSeparator spells a directory the way a path prefix uses it: the
// directory itself followed by the host's separator, unless it already ends
// with one.
func dirWithSeparator(dir string) string {
	if dir == "" || isPathSeparator(dir[len(dir)-1]) {
		return dir
	}
	return dir + string(os.PathSeparator)
}

// replaceWorkingDir writes the process working directory dir in a script's
// output as the relative path it stands for and reports whether it substituted
// anything.
//
// A script that prints an absolute path of its own working directory — the echo
// of `pwd`, a build tool's file list, a stack trace — names the caller's own
// directory under a name the caller's other tools do not take: what they
// resolve is the relative path, and the absolute form only leaks the layout of
// the machine. Every occurrence is therefore rewritten as the marker ("\" on
// Windows, "./" elsewhere).
//
// Only a whole path element is replaced, so a longer name that merely starts
// with the directory is left alone ("dir2\file" for dir "dir"). A match counts
// when the directory is followed by a path separator — the separator belongs to
// the match, so "dir\file" becomes ".\file" — by a byte that cannot continue a
// path name (a bare mention becomes ".\"), or by the end of the text. A match
// that does not start a path either — one preceded by a name byte or by a
// separator, as in "mnt/home/user" for dir "home/user" — is left alone. On
// Windows the comparison ignores case, because a program may report the path in
// whatever case it happens to hold it in.
func replaceWorkingDir(text, dir string) (string, bool) {
	if dir == "" || text == "" || isPathRoot(dir) {
		return text, false
	}
	foldCase := runtime.GOOS == "windows"
	marker := cwdRelativeMarker()

	// The scan only starts building the result once a match is found, so an
	// output that does not mention the directory is returned as it stands.
	start, last := 0, 0
	var sb strings.Builder
	replaced := false
	for start < len(text) {
		end, ok := workingDirMatchEnd(text, start, dir, foldCase)
		if !ok {
			start++
			continue
		}
		if !replaced {
			sb.Grow(len(text))
		}
		sb.WriteString(text[last:start])
		sb.WriteString(marker)
		last = end
		start = end
		replaced = true
	}
	if !replaced {
		return text, false
	}
	sb.WriteString(text[last:])
	return sb.String(), true
}

// workingDirMatchEnd reports whether dir is matched at text[i] as a whole path
// element and, when it is, the index to continue scanning from: a following
// separator belongs to the match.
func workingDirMatchEnd(text string, i int, dir string, foldCase bool) (int, bool) {
	if i > 0 && continuesPathElement(text[i-1]) {
		return 0, false
	}
	end := i + len(dir)
	if end > len(text) || !equalIgnoringCase(text[i:end], dir, foldCase) {
		return 0, false
	}
	if end == len(text) {
		// The directory stands at the very end of the text: a bare mention.
		return end, true
	}
	if isPathSeparator(text[end]) {
		// The leading element of a longer path: the separator, already part of
		// the marker, is consumed with the directory.
		return end + 1, true
	}
	if pathTokenEnders(text[end]) {
		return end, true
	}
	// A longer name that merely starts with the directory: leave it alone.
	return 0, false
}

// equalIgnoringCase compares a candidate with the directory, ignoring case on
// hosts whose paths are case-insensitive.
func equalIgnoringCase(candidate, dir string, foldCase bool) bool {
	if foldCase {
		return strings.EqualFold(candidate, dir)
	}
	return candidate == dir
}

// continuesPathElement reports whether b can be part of a path element, so an
// occurrence of the directory right after it continues a longer name instead of
// starting a path of its own.
func continuesPathElement(b byte) bool {
	return pathNameByte(b) || isPathSeparator(b)
}

// isPathRoot reports whether dir is a path root, a directory that names every
// absolute path of its volume ("/" or "C:\"). Replacing one would rewrite paths
// that have nothing to do with the call's directory, so the cleanup leaves the
// output alone.
func isPathRoot(dir string) bool {
	trimmed := trimTrailingSeparators(dir)
	if trimmed == "" {
		return true
	}
	return runtime.GOOS == "windows" && len(trimmed) == 2 && trimmed[1] == ':'
}

// trimTrailingSeparators drops the separators a path ends with, unless there is
// nothing else.
func trimTrailingSeparators(path string) string {
	end := len(path)
	for end > 0 && isPathSeparator(path[end-1]) {
		end--
	}
	return path[:end]
}

// isPathSeparator reports whether b separates path elements on this host. Both
// separators count on Windows, where a program may use either.
func isPathSeparator(b byte) bool {
	return b == '/' || b == byte(os.PathSeparator)
}

// pathNameByte reports whether b can appear inside a file or directory name
// rather than delimiting one.
func pathNameByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_', b == '-', b == '.', b == '~':
		return true
	}
	return false
}

// pathTokenEnders are the bytes that may follow a path in free text: the
// whitespace, quotes and punctuation that a log line, a sentence or a listing
// puts around a path. Any other byte may continue a longer name, so it keeps
// the directory from being replaced.
func pathTokenEnders(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '"', '\'', '`',
		'(', ')', '[', ']', '{', '}',
		'<', '>', '|', ';', ',', ':', '=', '?', '!':
		return true
	}
	return false
}
