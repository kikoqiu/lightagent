// Package termcolor provides minimal ANSI color helpers for the CLI.
//
// Colors are enabled only when stdout is a terminal that can actually interpret
// ANSI escape sequences. A non-TTY stdout, a set NO_COLOR, TERM=dumb or a
// Windows console without virtual terminal processing all keep coloring off, so
// redirected output stays clean and unsupported consoles never print raw escape
// bytes.
package termcolor

import (
	"os"
	"strings"
	"sync/atomic"
)

// ANSI escape codes used across the CLI.
const (
	ResetCode    = "\x1b[0m"
	BoldCode     = "\x1b[1m"
	DimCode      = "\x1b[2m"
	ItalicCode   = "\x1b[3m"
	RedCode      = "\x1b[31m"
	GreenCode    = "\x1b[32m"
	YellowCode   = "\x1b[33m"
	BlueCode     = "\x1b[34m"
	MagentaCode  = "\x1b[35m"
	CyanCode     = "\x1b[36m"
	GrayCode     = "\x1b[90m"
	DarkGrayCode = "\x1b[38;5;239m"
)

// enabled controls whether escape codes are emitted at all.
var enabled atomic.Bool

func init() {
	enabled.Store(shouldEnable(os.Stdout))
}

// shouldEnable reports whether ANSI colors should be emitted for f.
func shouldEnable(f *os.File) bool {
	if !colorAllowed() {
		return false
	}
	return isTerminal(f) && supportsColor(f)
}

// colorAllowed reports whether the environment asks for colored output. The
// NO_COLOR convention disables color when the variable is present and non-empty.
func colorAllowed() bool {
	if strings.TrimSpace(os.Getenv("NO_COLOR")) != "" {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb")
}

// isTerminal reports whether f is attached to a character device (a terminal).
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// SetEnabled forces color output on or off (useful for tests).
func SetEnabled(on bool) { enabled.Store(on) }

// Enabled reports whether color output is currently active.
func Enabled() bool { return enabled.Load() }

// Color wraps s with the given ANSI codes, honoring the enabled flag.
func Color(s string, codes ...string) string {
	if !enabled.Load() || len(codes) == 0 || s == "" {
		return s
	}
	out := ""
	for _, c := range codes {
		out += c
	}
	return out + s + ResetCode
}

// Red renders s in red.
func Red(s string) string { return Color(s, RedCode) }

// Green renders s in green.
func Green(s string) string { return Color(s, GreenCode) }

// Yellow renders s in yellow.
func Yellow(s string) string { return Color(s, YellowCode) }

// Blue renders s in blue.
func Blue(s string) string { return Color(s, BlueCode) }

// Magenta renders s in magenta.
func Magenta(s string) string { return Color(s, MagentaCode) }

// Cyan renders s in cyan.
func Cyan(s string) string { return Color(s, CyanCode) }

// Gray renders s in gray.
func Gray(s string) string { return Color(s, GrayCode) }

// DarkGray renders s in dark gray.
func DarkGray(s string) string { return Color(s, DarkGrayCode) }

// BoldText renders s in bold.
func BoldText(s string) string { return Color(s, BoldCode) }

// DimText renders s dimmed.
func DimText(s string) string { return Color(s, DimCode) }
