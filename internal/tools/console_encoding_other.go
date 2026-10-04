//go:build !windows

package tools

import "golang.org/x/text/encoding"

// hostAnsiEncoding returns nil on Unix-like hosts: child stdio is UTF-8, so
// bytes pass through unchanged.
func hostAnsiEncoding() encoding.Encoding { return nil }

// hostAnsiCharsetLabel returns "" on Unix-like hosts: there is no ANSI code
// page, so the "auto" file reader falls back to UTF-8.
func hostAnsiCharsetLabel() string { return "" }
