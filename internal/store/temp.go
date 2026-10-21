package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// TempDirName is the scratch directory below the state directory. The built-in
// prompt tells the model to put temporary files there; the program creates it
// only when the model does.
const TempDirName = "temp"

// TempMaxAge is how long a file may sit in .lightagent/temp without being
// written or read before an exit deletes it. The built-in prompt points the
// model at .lightagent/archive/ for scratch files worth keeping.
const TempMaxAge = 180 * 24 * time.Hour

// PruneTemp deletes the files below workdir/.lightagent/temp whose last write
// and last access are both older than maxAge, and returns the removed paths.
// Requiring both times keeps a file that is still being read or changed, even
// when it was written long ago. Subdirectories are walked; symbolic links and
// other non-regular entries are left alone, and so is a file whose access time
// the host does not report (see accessTime). A missing temp directory is not an
// error.
//
// Removal is best effort: a file that cannot be deleted is skipped and its
// error is returned once the walk is over, so one locked file does not keep the
// rest of the folder around.
func PruneTemp(workdir string, now time.Time, maxAge time.Duration) ([]string, error) {
	if workdir == "" || maxAge <= 0 {
		return nil, nil
	}
	root := filepath.Join(workdir, DirName, TempDirName)
	cutoff := now.Add(-maxAge)
	var (
		removed  []string
		firstErr error
	)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A missing root or an unreadable subtree is skipped: there is
			// nothing to clean up behind it.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			if firstErr == nil {
				firstErr = infoErr
			}
			return nil
		}
		// A symlink or other special entry is never a temporary file the model
		// left behind, and following one could point outside the folder.
		if !info.Mode().IsRegular() {
			return nil
		}
		if !info.ModTime().Before(cutoff) {
			return nil
		}
		accessed, ok := accessTime(info)
		if !ok || !accessed.Before(cutoff) {
			return nil
		}
		if removeErr := os.Remove(path); removeErr != nil {
			if !errors.Is(removeErr, fs.ErrNotExist) && firstErr == nil {
				firstErr = removeErr
			}
			return nil
		}
		removed = append(removed, path)
		return nil
	})
	return removed, firstErr
}
