//go:build unix && !darwin && !freebsd && !netbsd

package store

import (
	"os"
	"syscall"
	"time"
)

// accessTime reports when the file was last read. These hosts keep it in the
// stat data under Atim (AIX narrows its nanoseconds to int32).
func accessTime(info os.FileInfo) (time.Time, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(int64(st.Atim.Sec), int64(st.Atim.Nsec)), true
}
