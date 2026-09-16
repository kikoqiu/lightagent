//go:build !windows

package tools

import "golang.org/x/text/encoding"

// hostAnsiEncoding returns nil on Unix-like hosts: child stdio is UTF-8, so
// bytes pass through unchanged.
func hostAnsiEncoding() encoding.Encoding { return nil }
