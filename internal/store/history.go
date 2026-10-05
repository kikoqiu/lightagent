// Package store persists lightagent's own state under the current working
// directory. The format here is a simple, self-contained JSON snapshot.
//
// A directory holds a set of named sessions; exactly one of them is the
// current session (session.json by default). The current session is written on
// demand (/save, /saveas or the exit confirmation) and read back when the user
// resumes it; /load and /saveas move the current pointer to another file.
// Declining the resume prompt archives the previous file instead.
//
// The state directory is created lazily on the first write, so a run that never
// saves (--no-save, a one-shot prompt, or a declined exit prompt) leaves no
// trace on disk.
//
// Layout (relative to the working directory):
//
//	.lightagent/sessions/session.json               the current conversation for this directory
//	.lightagent/sessions/<name>.json                a session saved with /saveas
//	.lightagent/sessions/session-<stamp>.json       archived conversations (see Archive)
//
// The session files live in their own subdirectory so a directory can hold
// several conversations while the lock, uploads and other state stay beside
// them in .lightagent/.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"lightagent/internal/llm"
)

// DirName is the state directory created inside the working directory.
const DirName = ".lightagent"

// SessionsDirName is the directory below the state directory that holds the
// session files, so the lock, uploads and other state stay beside it.
const SessionsDirName = "sessions"

// stateFile is the default (current) session file name inside SessionsDirName.
const stateFile = "session.json"

// State is the persisted conversation snapshot.
type State struct {
	Version   int           `json:"version"`
	UpdatedAt time.Time     `json:"updated_at"`
	Model     string        `json:"model,omitempty"`
	Summary   string        `json:"summary,omitempty"`
	Messages  []llm.Message `json:"messages"`
}

// Store manages the session files for a working directory (or an explicit
// path). The current session file is mutable: /saveas and /load point it at a
// different file, and Save then updates that one. The mutex guards the current
// file so the terminal and the web mirror can drive it concurrently.
type Store struct {
	// root is the state directory (.lightagent): the lock, uploads and other
	// state live here.
	root string
	// dir is the directory that holds the session files (root/sessions for the
	// default layout; for --session it is the session file's own directory).
	dir string

	mu   sync.Mutex
	file string // absolute path of the current session file
	// initial is the default session file (session.json, or the --session path):
	// Reset points the store back at it so a fresh conversation saves where a
	// run without /saveas or /load would.
	initial string
}

// New creates a store rooted at workdir/.lightagent, with the session files
// under workdir/.lightagent/sessions.
func New(workdir string) (*Store, error) {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return nil, fmt.Errorf("resolve working directory: %w", err)
	}
	root := filepath.Join(abs, DirName)
	dir := filepath.Join(root, SessionsDirName)
	file := filepath.Join(dir, stateFile)
	return &Store{root: root, dir: dir, file: file, initial: file}, nil
}

// NewFile creates a store for an explicit session file path. The file's
// directory becomes both the state directory and the sessions directory, so
// archives and listings live next to it. Nothing is created on disk here: the
// directory is made on the first write, so a session that is never saved leaves
// no directory behind.
func NewFile(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve session path: %w", err)
	}
	dir := filepath.Dir(abs)
	return &Store{root: dir, dir: dir, file: abs, initial: abs}, nil
}

// Root returns the state directory (.lightagent): the directory lock, the
// uploads and the other run state live here.
func (s *Store) Root() string { return s.root }

// Dir returns the directory that holds the session files.
func (s *Store) Dir() string { return s.dir }

// Path returns the current session file path.
func (s *Store) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file
}

// ResolvePath maps a session name to a file path. A name without a directory
// part lives in the sessions directory; one carrying a separator (or an
// absolute path) is used as given. A missing .json suffix is added.
func (s *Store) ResolvePath(name string) string {
	name = strings.TrimSpace(name)
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		name += ".json"
	}
	if filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) {
		return name
	}
	return filepath.Join(s.dir, name)
}

// UseFile makes path the current session file, so a later Save updates it
// (/saveas and /load both point the store at the file they touch).
func (s *Store) UseFile(path string) {
	s.mu.Lock()
	s.file = path
	s.mu.Unlock()
}

// Reset points the store back at its default session file (/new starts a fresh
// conversation that saves where a run without /saveas or /load would; /clear,
// which keeps the current file, does not call it).
func (s *Store) Reset() {
	s.mu.Lock()
	s.file = s.initial
	s.mu.Unlock()
}

// MigrateLegacy moves the session files an earlier layout kept directly in the
// state directory (.lightagent/session.json and its session-*.json archives)
// into the sessions subdirectory, so an existing conversation is still resumed
// after the layout change. It does nothing for --session (its files already live
// where the user pointed), when a session.json already sits in the sessions
// directory, or when there is nothing to move. The caller holds the directory
// lock, so the move cannot race a second instance.
func (s *Store) MigrateLegacy() error {
	if s.dir == s.root {
		return nil
	}
	if _, err := os.Stat(filepath.Join(s.dir, stateFile)); err == nil {
		return nil
	}
	entries, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var legacy []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if name == stateFile || strings.HasPrefix(name, "session-") {
			legacy = append(legacy, name)
		}
	}
	if len(legacy) == 0 {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	for _, name := range legacy {
		_ = os.Rename(filepath.Join(s.root, name), filepath.Join(s.dir, name))
	}
	return nil
}

// Save writes the session atomically. The media of the messages is stored as a
// reference to the file it was read from, never as its bytes (see
// llm.ReferenceMedia): a conversation holding pictures stays a readable JSON file
// instead of megabytes of base64. The caller's state is left untouched.
func (s *Store) Save(st State) error {
	s.mu.Lock()
	path := s.file
	s.mu.Unlock()
	return s.writeState(path, st)
}

// SaveAs writes st to the named session file and makes it the current session
// file, so a later Save updates it. It returns the path that was written.
func (s *Store) SaveAs(name string, st State) (string, error) {
	path := s.ResolvePath(name)
	if err := s.writeState(path, st); err != nil {
		return "", err
	}
	s.UseFile(path)
	return path, nil
}

// Exists reports whether the current session file is present.
func (s *Store) Exists() bool {
	_, err := os.Stat(s.Path())
	return err == nil
}

// Load reads the current session. It returns (nil, nil) when none exists yet.
func (s *Store) Load() (*State, error) {
	return s.LoadPath(s.Path())
}

// writeState versions, references the media of and atomically writes st to
// path.
func (s *Store) writeState(path string, st State) error {
	if st.Version == 0 {
		st.Version = 1
	}
	st.Messages = llm.ReferenceMedia(st.Messages)
	return s.writeJSON(path, st)
}

// LoadPath reads an arbitrary session file. It returns (nil, nil) when the file
// does not exist. The messages come back with the media parts still referring to
// their files (see llm.ReferenceMedia): reading them back is up to the caller that
// resumes the conversation (llm.ResolveMedia), so listing sessions stays cheap.
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

	base := strings.TrimSuffix(filepath.Base(path), ".json")
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

// List returns the session files in the sessions directory, newest first, with
// the current session flagged. Files that cannot be parsed are still listed
// (with Messages left at 0).
func (s *Store) List() ([]SessionInfo, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		// The sessions directory is created lazily, so a missing directory
		// simply means nothing has been saved yet.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	current := s.Path()
	infos := make([]SessionInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		info := SessionInfo{Path: path, Name: e.Name(), Current: path == current}
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

// Recent returns the newest n session files, newest first. n <= 0 means all of
// them. It backs /list and /api/sessions, whose 1-based indices also address
// /load.
func (s *Store) Recent(n int) ([]SessionInfo, error) {
	infos, err := s.List()
	if err != nil {
		return nil, err
	}
	if n > 0 && len(infos) > n {
		infos = infos[:n]
	}
	return infos, nil
}

// Latest returns the newest session file (the first one /list shows), or nil
// when the directory holds none. Startup resumes it, so a directory picks up
// where its last conversation left off whatever that file is named.
func (s *Store) Latest() (*SessionInfo, error) {
	infos, err := s.Recent(1)
	if err != nil {
		return nil, err
	}
	if len(infos) == 0 {
		return nil, nil
	}
	return &infos[0], nil
}

// Named reports whether the current session file is a named one: anything other
// than the default session.json. A nameless (default) session is the fresh
// conversation that has neither been loaded nor saved under a name, and it is
// the one whose default file a save may back up before overwriting.
func (s *Store) Named() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file != s.initial
}

// Default returns the default session file path: session.json (or the --session
// file). It is the name a nameless conversation is saved under, and the file
// whose previous content a nameless save may back up.
func (s *Store) Default() string { return s.initial }

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
