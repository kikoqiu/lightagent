//go:build !windows

package termcolor

import "os"

// supportsColor reports whether the terminal behind f understands ANSI escape
// sequences. On Unix-like systems a character device is assumed capable.
func supportsColor(_ *os.File) bool { return true }
