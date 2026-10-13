package workdir

import (
	"os"
	"runtime"
	"sort"
	"strings"
)

// Candidate is one name the '@' picker offers: a child of the directory the
// typed fragment points at. Dir marks a directory, so the web page appends its
// own separator when it inserts a name (the terminal completes the bare name and
// leaves the separator for the user to type).
type Candidate struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
}

// Complete resolves one level of a path fragment typed after '@' (the text up to
// the caret, the '@' itself excluded). It splits the fragment at its last path
// separator into a head — the directory part, kept exactly as the user spelled
// it so the caller can put it back in front of the picked name — and the base
// being completed, then returns the children of that directory whose names start
// with the base, directories first and each group sorted case-insensitively. A
// name that starts with a dot is offered only once the user has typed one, the
// way a shell completes hidden entries.
//
// An empty head is the process working directory and a relative one is resolved
// against it, while an absolute head (and a leading ~) is used as spelled, so
// "@src/ma" spans ./src and "@C:\build\go\li" spans C:\build\go. A directory
// that cannot be read simply has no candidates.
func Complete(fragment string) (head string, candidates []Candidate) {
	i := lastSeparator(fragment)
	head, base := fragment[:i+1], fragment[i+1:]

	spelled := head
	if spelled == "" {
		spelled = "."
	}
	dir := Expand(spelled)
	items, err := os.ReadDir(dir)
	if err != nil {
		return head, nil
	}

	// Windows resolves names without regard to case, so the match does too (a
	// Unix filesystem would then just offer a few extra candidates, never a
	// missing one).
	fold := func(s string) string {
		if runtime.GOOS == "windows" {
			return strings.ToLower(s)
		}
		return s
	}
	base = fold(base)
	hidden := strings.HasPrefix(base, ".")
	for _, item := range items {
		name := item.Name()
		if strings.HasPrefix(name, ".") && !hidden {
			continue
		}
		if !strings.HasPrefix(fold(name), base) {
			continue
		}
		candidates = append(candidates, Candidate{Name: name, Dir: isDir(dir, item)})
	}
	sort.Slice(candidates, func(a, b int) bool {
		if candidates[a].Dir != candidates[b].Dir {
			return candidates[a].Dir
		}
		return fold(candidates[a].Name) < fold(candidates[b].Name)
	})
	return head, candidates
}

// lastSeparator returns the index of the last path separator in s, or -1 when it
// carries none. It looks at bytes because a separator is always ASCII, so a
// multi-byte rune cannot be mistaken for one.
func lastSeparator(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if os.IsPathSeparator(s[i]) {
			return i
		}
	}
	return -1
}
