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
