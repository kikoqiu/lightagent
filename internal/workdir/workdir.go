// Package workdir is the shared "current directory" service behind the /pwd,
// /cd and /ls slash commands. Both front-ends go through it, so a /cd typed in
// the terminal and one clicked in the mirror move the same directory: it wraps
// the process working directory instead of keeping a copy that could drift.
package workdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxListing bounds how many entries /ls prints: a directory with a very large
// number of children is summarised instead of flooding the transcript.
const maxListing = 500

// Get returns the process working directory.
func Get() (string, error) { return os.Getwd() }

// Expand resolves a leading ~ (the user's home directory) in a path; every
// other path is returned unchanged. A ~ that cannot name a home directory, and
// one that is not at the front, is left alone.
func Expand(path string) string {
	path = strings.TrimSpace(path)
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// Set changes the process working directory to dir (a leading ~ is expanded)
// and returns the absolute directory the process now runs in. An empty path or
// one that cannot be entered is an error; the working directory is then left
// alone.
func Set(dir string) (string, error) {
	target := Expand(dir)
	if target == "" {
		return "", errors.New("no directory given")
	}
	if err := os.Chdir(target); err != nil {
		return "", err
	}
	return os.Getwd()
}

// entry is one child of a listing: its name and whether a trailing slash marks
// it as a directory.
type entry struct {
	name string
	dir  bool
}

// Listing renders dir the way /ls reports it: the absolute directory on its own
// line, then one entry per line with a directory marked by a trailing slash.
// Hidden entries (a leading dot) are skipped the way a bare ls skips them, and
// a listing longer than maxListing is cut short with a count of the rest. An
// empty dir argument lists the process working directory; a leading ~ is
// expanded.
func Listing(dir string) (string, error) {
	target := Expand(dir)
	if target == "" {
		target = "."
	}
	items, err := os.ReadDir(target)
	if err != nil {
		return "", err
	}
	// The header names the directory unambiguously: the relative "." the caller
	// may have listed still reads as the absolute path it resolved to.
	abs, err := filepath.Abs(target)
	if err != nil {
		abs = target
	}
	entries := make([]entry, 0, len(items))
	for _, item := range items {
		if strings.HasPrefix(item.Name(), ".") {
			continue
		}
		entries = append(entries, entry{name: item.Name(), dir: isDir(target, item)})
	}
	var b strings.Builder
	b.WriteString(abs + ":\n")
	if len(entries) == 0 {
		b.WriteString("  (empty)\n")
	}
	for i, e := range entries {
		if i == maxListing {
			fmt.Fprintf(&b, "  … and %d more\n", len(entries)-i)
			break
		}
		name := e.name
		if e.dir {
			name += "/"
		}
		b.WriteString("  " + name + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// isDir reports whether item is a directory; a symbolic link is resolved once,
// because a link to a directory is used like one.
func isDir(parent string, item os.DirEntry) bool {
	if item.IsDir() {
		return true
	}
	if item.Type()&os.ModeSymlink == 0 {
		return false
	}
	info, err := os.Stat(filepath.Join(parent, item.Name()))
	return err == nil && info.IsDir()
}
