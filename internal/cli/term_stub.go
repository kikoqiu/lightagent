//go:build !windows && !linux

package cli

import "os"

// newTermReader reports that raw multi-line editing is unavailable on this
// platform; the CLI falls back to line scanning (Enter sends).
func newTermReader(*os.File) (termReader, bool) { return nil, false }

// isInteractive falls back to the character-device check on this platform.
func isInteractive(in *os.File) bool {
	info, err := in.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// terminalWidth is unknown on this platform; callers fall back to COLUMNS.
func terminalWidth() int { return 0 }

// spinnerGlyphsSupported reports whether the terminal can draw the Unicode
// braille spinner; the platforms that fall back to line scanning are assumed to
// be able to.
func spinnerGlyphsSupported() bool { return true }
