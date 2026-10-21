//go:build windows

package store

import (
	"os"
	"syscall"
	"time"
)

// accessTime reports when the file was last read. Windows keeps the time in the
// stat data the file info was built from.
func accessTime(info os.FileInfo) (time.Time, bool) {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(0, data.LastAccessTime.Nanoseconds()), true
}
