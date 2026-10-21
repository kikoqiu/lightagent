package agent

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lightagent/internal/textwidth"
)

// memoDirRel is the project-memory directory below the working directory, as
// the built-in prompt names it.
const memoDirRel = ".lightagent/memo"

// engineMetaHeader opens the engine meta info block a user message carries. It
// is English like every other engine message: the block is appended to the
// request on its own, so it must read the same way whatever language the user
// writes in.
const engineMetaHeader = "[engine meta info]"

// memoInfoHeader introduces the memo listing, below the working-directory line.
const memoInfoHeader = "The memo folder holds these files:"

// memoFile is one file of the memo folder.
type memoFile struct {
	// name is the path relative to the memo folder, with forward slashes.
	name    string
	modTime time.Time
	size    int64
}

// memoInfo renders the engine meta info block a user message carries: the
// directory the process runs in (the path only, while includeDir asks for it)
// and every file below workdir/.lightagent/memo, newest change first, with a
// friendly age and a human-readable size. It returns "" when there is nothing
// to report — no working directory at all, or a block that would neither name
// the directory nor list a memo file.
func memoInfo(workdir string, includeDir bool, now time.Time) string {
	if workdir == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(engineMetaHeader)
	if includeDir {
		b.WriteString("\nworking directory: " + workdir)
	}
	files := memoFiles(workdir)
	if len(files) == 0 {
		if !includeDir {
			return ""
		}
		return b.String()
	}
	nameWidth := textwidth.Width("file")
	ageWidth := textwidth.Width("updated")
	for _, f := range files {
		nameWidth = max(nameWidth, textwidth.Width(f.name))
		ageWidth = max(ageWidth, textwidth.Width(memoAge(now.Sub(f.modTime))))
	}
	b.WriteByte('\n')
	b.WriteString(memoInfoHeader)
	b.WriteByte('\n')
	fmt.Fprintf(&b, "%s  %s  %s\n",
		textwidth.PadRight("file", nameWidth),
		textwidth.PadRight("updated", ageWidth),
		"size")
	for _, f := range files {
		fmt.Fprintf(&b, "%s  %s  %s\n",
			textwidth.PadRight(f.name, nameWidth),
			textwidth.PadRight(memoAge(now.Sub(f.modTime)), ageWidth),
			memoSize(f.size))
	}
	return strings.TrimRight(b.String(), "\n")
}

// memoFiles lists the files below the memo folder, newest change first (ties
// broken by name, so the listing is stable). Subdirectories are walked rather
// than listed: a note kept in one shows its path relative to the memo folder. A
// missing or unreadable folder yields no file.
func memoFiles(workdir string) []memoFile {
	if workdir == "" {
		return nil
	}
	root := filepath.Join(workdir, filepath.FromSlash(memoDirRel))
	var files []memoFile
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Skip what cannot be read and keep walking the rest.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		files = append(files, memoFile{
			name:    filepath.ToSlash(rel),
			modTime: info.ModTime(),
			size:    info.Size(),
		})
		return nil
	})
	sort.Slice(files, func(i, j int) bool {
		if !files[i].modTime.Equal(files[j].modTime) {
			return files[i].modTime.After(files[j].modTime)
		}
		return files[i].name < files[j].name
	})
	return files
}

// memoAge renders how long before now a file changed, the way a person says it:
// just now, 5 minutes ago, 3 hours ago, 1 day ago, 2 months ago, 1 year ago. It
// stays English like the rest of the engine text. A timestamp in the future (a
// clock that moved) counts as just now.
func memoAge(age time.Duration) string {
	unit := func(n int, name string) string {
		if n == 1 {
			return "1 " + name + " ago"
		}
		return fmt.Sprintf("%d %ss ago", n, name)
	}
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return unit(int(age/time.Minute), "minute")
	case age < 24*time.Hour:
		return unit(int(age/time.Hour), "hour")
	case age < 30*24*time.Hour:
		return unit(int(age/(24*time.Hour)), "day")
	case age < 365*24*time.Hour:
		return unit(int(age/(30*24*time.Hour)), "month")
	}
	return unit(int(age/(365*24*time.Hour)), "year")
}

// memoSize renders a byte count in a compact unit (B, KB, MB, GB, TB).
func memoSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(n) / 1024
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
}
