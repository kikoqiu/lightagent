//go:build windows

package termcolor

import (
	"os"
	"syscall"
	"unsafe"
)

// enableVirtualTerminalProcessing is the console mode flag that makes a Windows
// console interpret ANSI escape sequences instead of printing them literally.
const enableVirtualTerminalProcessing = 0x0004

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

// supportsColor reports whether f is a Windows console that can interpret ANSI
// escape sequences. Legacy consoles (cmd.exe / Windows PowerShell in conhost)
// print the escapes as garbage, so coloring stays off unless virtual terminal
// processing is already enabled or can be turned on.
func supportsColor(f *os.File) bool {
	h := syscall.Handle(f.Fd())
	var mode uint32
	if err := getConsoleMode(h, &mode); err != nil {
		return false
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}
	return setConsoleMode(h, mode|enableVirtualTerminalProcessing) == nil
}

// getConsoleMode reads the console mode of h.
func getConsoleMode(h syscall.Handle, mode *uint32) error {
	r, _, err := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(mode)))
	if r == 0 {
		return err
	}
	return nil
}

// setConsoleMode writes the console mode of h.
func setConsoleMode(h syscall.Handle, mode uint32) error {
	r, _, err := procSetConsoleMode.Call(uintptr(h), uintptr(mode))
	if r == 0 {
		return err
	}
	return nil
}
