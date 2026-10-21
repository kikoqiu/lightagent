//go:build !unix && !windows

package store

import (
	"os"
	"time"
)

// accessTime is unknown on this host: its stat data carries no last-access
// time, so no file ever looks old enough to delete. Reporting false keeps
// PruneTemp on the safe side instead of guessing from the write time.
func accessTime(os.FileInfo) (time.Time, bool) { return time.Time{}, false }
