package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// MaxAgentIncludeDepth caps how deeply @include directives may nest. It also
// bounds the amount of work a pathological prompt can trigger.
const MaxAgentIncludeDepth = 16

// agentIncludePattern matches a line that consists solely of an @include
// directive: optional indentation, @include("path") and optional trailing
// whitespace. Only whole lines are expanded so prose that merely mentions the
// syntax stays untouched.
var agentIncludePattern = regexp.MustCompile(`^[ \t]*@include\([ \t]*"([^"]+)"[ \t]*\)[ \t\r]*$`)

// expandAgentIncludes replaces every @include("path") line in text with the
// content the directive points at. A relative path is resolved against the
// directory of the file holding the directive (sourcePath); an absolute path is
// used as is. A directory contributes all of its files, joined with a blank
// line, and every included file is expanded the same way.
//
// depth counts the nesting level (0 for the top-level prompt) and stack holds
// the canonical paths of the files already being expanded, which detects
// include cycles.
func expandAgentIncludes(text, sourcePath string, depth int, stack []string) (string, error) {
	if depth > MaxAgentIncludeDepth {
		return "", fmt.Errorf("include %s: nested more than %d levels deep", sourcePath, MaxAgentIncludeDepth)
	}

	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		match := agentIncludePattern.FindStringSubmatch(line)
		if match == nil {
			out = append(out, line)
			continue
		}
		target := strings.TrimSpace(match[1])
		if target == "" {
			out = append(out, line)
			continue
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(sourcePath), target)
		}
		snippet, err := includeTarget(target, sourcePath, depth, stack)
		if err != nil {
			return "", err
		}
		// An empty target (or a directory holding no readable text) drops the
		// directive line instead of leaving a blank line behind.
		if snippet != "" {
			out = append(out, snippet)
		}
	}
	return strings.Join(out, "\n"), nil
}

// includeTarget returns the expanded content of target: the file itself, or
// every file under it when target is a directory.
func includeTarget(target, sourcePath string, depth int, stack []string) (string, error) {
	files, err := includeFiles(target)
	if err != nil {
		return "", fmt.Errorf("include \"%s\" in %s: %w", target, sourcePath, err)
	}

	parts := make([]string, 0, len(files))
	for _, file := range files {
		key := canonicalPath(file)
		if containsPath(stack, key) {
			chain := append(append([]string{}, stack...), key)
			return "", fmt.Errorf("include cycle: %s", strings.Join(chain, " -> "))
		}
		data, readErr := os.ReadFile(file)
		if readErr != nil {
			return "", fmt.Errorf("include \"%s\" in %s: %w", file, sourcePath, readErr)
		}
		next := append(append([]string{}, stack...), key)
		expanded, expErr := expandAgentIncludes(string(data), file, depth+1, next)
		if expErr != nil {
			return "", expErr
		}
		if snippet := strings.TrimSpace(expanded); snippet != "" {
			parts = append(parts, snippet)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// includeFiles resolves an include target to the list of files it contributes.
// A directory yields every regular file under it in lexical order.
func includeFiles(target string) ([]string, error) {
	info, err := os.Stat(target)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{target}, nil
	}

	var files []string
	walkErr := filepath.WalkDir(target, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return files, nil
}

// canonicalPath normalizes a path for cycle detection. Windows paths compare
// case-insensitively, so they are folded to lower case.
func canonicalPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if runtime.GOOS == "windows" {
		return strings.ToLower(abs)
	}
	return abs
}

// containsPath reports whether path is already present in stack.
func containsPath(stack []string, path string) bool {
	for _, seen := range stack {
		if seen == path {
			return true
		}
	}
	return false
}
