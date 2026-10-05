package slash

import (
	"fmt"
	"strings"

	"lightagent/internal/store"
	"lightagent/internal/textwidth"
)

// sessionNameWidth is the column width the file-name cell is padded to. The
// padding is in terminal columns, not runes (see textwidth), so a name with
// East Asian wide runes keeps the timestamps lined up instead of pushing them
// to the right.
const sessionNameWidth = 32

// SessionList renders the /list table as text for the terminal: a 1-based
// index, the file name and the last-modified stamp, one session per line,
// newest first. The index is what /load <n> accepts, so both front-ends number
// the sessions the same way (they read the same store listing). The current
// session is marked so a reader can tell what /save would update. The web mirror
// draws the same listing as a grid instead (see internal/web), because a browser
// font's CJK advance is not promised to match the space padding.
func SessionList(infos []store.SessionInfo) string {
	if len(infos) == 0 {
		return "no saved sessions"
	}
	var b strings.Builder
	for i, info := range infos {
		marker := ""
		if info.Current {
			marker = " (current)"
		}
		fmt.Fprintf(&b, "%3d  %s  %s%s\n",
			i+1, textwidth.PadRight(info.Name, sessionNameWidth), info.ModTime.Format("2006-01-02 15:04:05"), marker)
	}
	return strings.TrimRight(b.String(), "\n")
}
