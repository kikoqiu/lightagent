//go:build windows

package tools

import (
	"sync"
	"syscall"

	"golang.org/x/text/encoding"
)

var procGetACP = syscall.NewLazyDLL("kernel32.dll").NewProc("GetACP")

var (
	ansiEncodingOnce sync.Once
	ansiEncodingVal  encoding.Encoding
)

// hostAnsiEncoding resolves the host ANSI code page (CP_ACP): the charset that
// Windows console programs use for stdio redirected to a pipe (GBK on a zh-CN
// host, Big5 on a zh-TW host, ...).
func hostAnsiEncoding() encoding.Encoding {
	ansiEncodingOnce.Do(func() {
		codePage, _, _ := procGetACP.Call()
		ansiEncodingVal = ansiEncodingFromCodePage(int(codePage))
	})
	return ansiEncodingVal
}

// hostAnsiCharsetLabel returns the WHATWG label of the host ANSI code page
// (CP_ACP), or "" when the code page is UTF-8 or not one of the known legacy
// charsets. The "auto" file reader uses it as a fallback guess for a file that
// is not valid UTF-8.
func hostAnsiCharsetLabel() string {
	codePage, _, _ := procGetACP.Call()
	return ansiCodePageCharsets[int(codePage)]
}
