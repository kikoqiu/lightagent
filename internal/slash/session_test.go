package slash

import (
	"strings"
	"testing"
	"time"

	"lightagent/internal/store"
	"lightagent/internal/textwidth"
)

// TestSessionListAlignsWideNames pins that a file name with East Asian wide
// runes does not push the timestamp column out of line: the name cell is padded
// by display columns, not by runes.
func TestSessionListAlignsWideNames(t *testing.T) {
	stamp := time.Date(2026, 10, 5, 14, 22, 10, 0, time.UTC)
	plain := stamp.Format("2006-01-02 15:04:05")
	out := SessionList([]store.SessionInfo{
		{Name: "session.json", ModTime: stamp},
		{Name: "会议笔记.json", ModTime: stamp},
		{Name: "アーカイブ.json", ModTime: stamp},
	})
	// "   1" + "  " + name (padded to sessionNameWidth) + "  " before the stamp.
	want := 3 + 2 + sessionNameWidth + 2
	for _, line := range strings.Split(out, "\n") {
		at := strings.Index(line, plain)
		if at < 0 {
			t.Fatalf("line %q has no timestamp", line)
		}
		if got := textwidth.Width(line[:at]); got != want {
			t.Fatalf("timestamp column for %q = %d, want %d", line, got, want)
		}
	}
}

// file name and the stamp, with the current file marked, so /load <n> addresses
// exactly what the table shows.
func TestSessionListNumbersNewestFirst(t *testing.T) {
	if got := SessionList(nil); got != "no saved sessions" {
		t.Fatalf("SessionList(nil) = %q", got)
	}
	stamp := time.Date(2026, 10, 5, 14, 22, 10, 0, time.UTC)
	out := SessionList([]store.SessionInfo{
		{Name: "session.json", ModTime: stamp, Current: true},
		{Name: "notes.json", ModTime: stamp.Add(-time.Hour)},
	})
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("SessionList = %q, want two lines", out)
	}
	for _, want := range []string{"session.json", "2026-10-05 14:22:10", "(current)"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("first row = %q, want %q", lines[0], want)
		}
	}
	if !strings.Contains(lines[1], "2") || !strings.Contains(lines[1], "notes.json") {
		t.Fatalf("second row = %q, want the older file at index 2", lines[1])
	}
}
