// Package store persists lightagent's own state under the current working
// directory. The
// format here is a simple, self-contained JSON snapshot.
//
// A working directory always holds at most one current session. It is written
// on demand (/save or the exit confirmation) and read back when the user
// chooses to resume; declining the resume prompt archives it instead.
//
// The state directory is created lazily on the first write, so a run that never
// saves (--no-save, a one-shot prompt, or a declined exit prompt) leaves no
// trace on disk.
//
// Layout (relative to the working directory):
//
//	.lightagent/session.json               the current conversation for this directory
//	.lightagent/session-<stamp>.json       archived conversations (see Archive)
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lightagent/internal/llm"
)

// DirName is the state directory created inside the working directory.
const DirName = ".lightagent"

// stateFile is the single-session file name inside DirName.
const stateFile = "session.json"

// State is the persisted conversation snapshot.
type State struct {
	Version   int           `json:"version"`
	UpdatedAt time.Time     `json:"updated_at"`
	Model     string        `json:"model,omitempty"`
	Summary   string        `json:"summary,omitempty"`
	Messages  []llm.Message `json:"messages"`
}

// Store manages the session file for a working directory (or an explicit path).
type Store struct {
	dir  string
	file string
}

// New creates a store rooted at workdir/.lightagent.
func New(workdir string) (*Store, error) {
	return NewFile(filepath.Join(workdir, DirName, stateFile))
}

// NewFile creates a store for an explicit session file path. The file's
// directory becomes the state directory, so archives and listings live next to
// it. Nothing is created on disk here: the directory is made on the first
// write, so a session that is never saved leaves no directory behind.
func NewFile(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve session path: %w", err)
	}
	return &Store{dir: filepath.Dir(abs), file: filepath.Base(abs)}, nil
}

// Dir returns the state directory path.
func (s *Store) Dir() string { return s.dir }

// Path returns the session file path.
func (s *Store) Path() string { return filepath.Join(s.dir, s.file) }

// Save writes the session atomically.
func (s *Store) Save(st State) error {
	if st.Version == 0 {
		st.Version = 1
	}
	return s.writeJSON(s.Path(), st)
}

// Exists reports whether a saved session file is present.
func (s *Store) Exists() bool {
	_, err := os.Stat(s.Path())
	return err == nil
}

// Load reads the session. It returns (nil, nil) when none exists yet.
func (s *Store) Load() (*State, error) {
	return s.LoadPath(s.Path())
}

// LoadPath reads an arbitrary session file. It returns (nil, nil) when the file
// does not exist.
func (s *Store) LoadPath(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &st, nil
}

// Archive renames an existing session file to a timestamped backup so the
// working directory can start a fresh conversation without losing the previous
// one. It returns the backup path, or ("", nil) when there is nothing to
// archive.
func (s *Store) Archive(now time.Time) (string, error) {
	path := s.Path()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}

	base := strings.TrimSuffix(s.file, ".json")
	stamp := now.Format("20060102-150405")
	target := filepath.Join(s.dir, fmt.Sprintf("%s-%s.json", base, stamp))
	for i := 1; ; i++ {
		if _, err := os.Stat(target); os.IsNotExist(err) {
			break
		}
		target = filepath.Join(s.dir, fmt.Sprintf("%s-%s-%d.json", base, stamp, i))
	}
	if err := os.Rename(path, target); err != nil {
		return "", fmt.Errorf("archive session: %w", err)
	}
	return target, nil
}

// SessionInfo describes one session file on disk.
type SessionInfo struct {
	Path       string
	Name       string
	Current    bool
	ModTime    time.Time
	Size       int64
	Messages   int
	Model      string
	UpdatedAt  time.Time
	HasSummary bool
}

// List returns the session files in the state directory, newest first, with the
// current session flagged. Files that cannot be parsed are still listed (with
// Messages left at 0).
func (s *Store) List() ([]SessionInfo, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		// The state directory is created lazily, so a missing directory simply
		// means nothing has been saved yet.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	infos := make([]SessionInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		info := SessionInfo{Path: path, Name: e.Name(), Current: e.Name() == s.file}
		if fi, statErr := os.Stat(path); statErr == nil {
			info.ModTime = fi.ModTime()
			info.Size = fi.Size()
		}
		if st, loadErr := s.LoadPath(path); loadErr == nil && st != nil {
			info.Messages = len(st.Messages)
			info.Model = st.Model
			info.UpdatedAt = st.UpdatedAt
			info.HasSummary = strings.TrimSpace(st.Summary) != ""
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool {
		if !infos[i].ModTime.Equal(infos[j].ModTime) {
			return infos[i].ModTime.After(infos[j].ModTime)
		}
		// Archived names embed their timestamp, so a name tiebreak keeps the
		// order chronological even when mtimes collide.
		return infos[i].Name > infos[j].Name
	})
	return infos, nil
}

// PruneArchives removes archived sessions, keeping the newest keep of them
// (keep <= 0 removes every archive). The current session file is never touched.
// It returns the removed paths.
func (s *Store) PruneArchives(keep int) ([]string, error) {
	infos, err := s.List()
	if err != nil {
		return nil, err
	}
	archives := make([]SessionInfo, 0, len(infos))
	for _, info := range infos {
		if info.Current {
			continue
		}
		archives = append(archives, info)
	}
	// List is newest-first, so everything past keep is removed.
	if keep < 0 {
		keep = 0
	}
	var removed []string
	for i := len(archives) - 1; i >= keep; i-- {
		if err := os.Remove(archives[i].Path); err != nil && !os.IsNotExist(err) {
			return removed, err
		}
		removed = append(removed, archives[i].Path)
	}
	return removed, nil
}

// Remove deletes a session file (used by `sessions prune --file`). The current
// session may be removed explicitly.
func (s *Store) Remove(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// writeJSON atomically writes v as indented JSON.
func (s *Store) writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
