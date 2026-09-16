//go:build windows

package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// Windows console input-mode flags.
const (
	enableProcessedInput            = 0x0001
	enableLineInput                 = 0x0002
	enableEchoInput                 = 0x0004
	enableWindowInput               = 0x0008
	enableMouseInput                = 0x0010
	enableVirtualTerminalProcessing = 0x0004
)

// win32InputModeMinBuild is the Windows build from which ConPTY translates
// Win32 input mode sequences into key records carrying their modifiers.
const win32InputModeMinBuild = 19041

// INPUT_RECORD event type and KEY_EVENT_RECORD control-key states.
const (
	keyEventType = 0x0001

	rightAltPressed  = 0x0001
	leftAltPressed   = 0x0002
	rightCtrlPressed = 0x0004
	leftCtrlPressed  = 0x0008
)

// Virtual key codes we care about.
const (
	vkBack   = 0x08
	vkTab    = 0x09
	vkReturn = 0x0d
	vkEscape = 0x1b
	vkPrior  = 0x21
	vkNext   = 0x22
	vkEnd    = 0x23
	vkHome   = 0x24
	vkLeft   = 0x25
	vkUp     = 0x26
	vkRight  = 0x27
	vkDown   = 0x28
	vkInsert = 0x2d
	vkDelete = 0x2e
)

var (
	termKernel32          = syscall.NewLazyDLL("kernel32.dll")
	termGetConsoleMode    = termKernel32.NewProc("GetConsoleMode")
	termSetConsoleMode    = termKernel32.NewProc("SetConsoleMode")
	termReadConsoleInputW = termKernel32.NewProc("ReadConsoleInputW")
	termScreenBufferInfo  = termKernel32.NewProc("GetConsoleScreenBufferInfo")

	termNtDLL         = syscall.NewLazyDLL("ntdll.dll")
	termRtlGetVersion = termNtDLL.NewProc("RtlGetVersion")
)

// shortCoord and shortRect mirror the COORD / SMALL_RECT members of
// CONSOLE_SCREEN_BUFFER_INFO.
type shortCoord struct{ X, Y int16 }
type shortRect struct{ Left, Top, Right, Bottom int16 }

// consoleScreenBufferInfo mirrors CONSOLE_SCREEN_BUFFER_INFO (only the leading
// members are needed for the visible-window width).
type consoleScreenBufferInfo struct {
	Size              shortCoord
	CursorPosition    shortCoord
	Attributes        uint16
	Window            shortRect
	MaximumWindowSize shortCoord
}

// terminalWidth returns the visible console width in columns (0 when unknown).
func terminalWidth() int {
	var info consoleScreenBufferInfo
	r, _, _ := termScreenBufferInfo.Call(uintptr(syscall.Stdout), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 0
	}
	width := int(info.Window.Right-info.Window.Left) + 1
	if width < 1 {
		return 0
	}
	return width
}

// spinnerGlyphsSupported reports whether the console can draw the Unicode
// braille spinner. The legacy console host (cmd.exe / Windows PowerShell before
// the default terminal app took over) draws characters its console font lacks —
// braille included — as a placeholder box, and it cannot fall back to another
// font. Windows Terminal, VS Code and other ConPTY-based hosts do their own font
// fallback, so they are fine.
//
// LIGHTAGENT_UNICODE_UI forces the braille spinner on (for example when a
// legacy console was assigned a font that does cover braille).
func spinnerGlyphsSupported() bool {
	if strings.TrimSpace(os.Getenv("LIGHTAGENT_UNICODE_UI")) != "" {
		return true
	}
	// Hosts that render with their own font stack, in alphabetical order.
	for _, name := range []string{"ConEmuANSI", "TERM_PROGRAM", "WEZTERM_EXECUTABLE", "WT_SESSION"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	return false
}

// osVersionInfoExW mirrors RTL_OSVERSIONINFOEXW (only the leading fields we
// need are declared; the rest is opaque padding).
type osVersionInfoExW struct {
	Size         uint32
	MajorVersion uint32
	MinorVersion uint32
	BuildNumber  uint32
	PlatformID   uint32
	CSDVersion   [128]uint16
}

// windowsBuild returns the OS build number (0 when it cannot be determined).
func windowsBuild() uint32 {
	var info osVersionInfoExW
	info.Size = uint32(unsafe.Sizeof(info))
	if r, _, _ := termRtlGetVersion.Call(uintptr(unsafe.Pointer(&info))); r != 0 {
		return 0
	}
	return info.BuildNumber
}

// keyEventRecord mirrors the KEY_EVENT_RECORD member of an INPUT_RECORD.
type keyEventRecord struct {
	KeyDown         int32
	RepeatCount     uint16
	VirtualKeyCode  uint16
	VirtualScanCode uint16
	UnicodeChar     uint16
	ControlKeyState uint32
}

// inputRecord mirrors INPUT_RECORD: a WORD event type, two bytes of alignment
// padding, then the union (KEY_EVENT_RECORD is the member we need).
type inputRecord struct {
	EventType uint16
	_         uint16
	Key       keyEventRecord
}

// windowsTerm reads raw key events from a Windows console via
// ReadConsoleInputW so modifier keys (notably Ctrl+Enter) can be told apart
// from the plain key.
type windowsTerm struct {
	h          syscall.Handle
	oldMode    uint32
	reportKeys bool
}

// newTermReader switches the console into raw input mode. It returns ok=false
// when stdin is not a console (piped input, tests) so the caller can fall back
// to line scanning.
func newTermReader(in *os.File) (termReader, bool) {
	h := syscall.Handle(in.Fd())
	var mode uint32
	if r, _, _ := termGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return nil, false
	}
	raw := mode &^ (enableLineInput | enableEchoInput | enableProcessedInput | enableWindowInput | enableMouseInput)
	if r, _, _ := termSetConsoleMode.Call(uintptr(h), uintptr(raw)); r == 0 {
		return nil, false
	}
	return &windowsTerm{h: h, oldMode: mode, reportKeys: enableKeyReporting()}, true
}

// enableKeyReporting asks the terminal for full key events (Win32 input mode,
// DECSET 9001) so chords such as Ctrl+Enter keep their modifier state through
// ConPTY. It is best effort: terminals that do not understand the sequence
// ignore it, and LIGHTAGENT_SIMPLE_INPUT disables the attempt entirely.
func enableKeyReporting() bool {
	if strings.TrimSpace(os.Getenv("LIGHTAGENT_SIMPLE_INPUT")) != "" {
		return false
	}
	if windowsBuild() < win32InputModeMinBuild {
		return false
	}
	// Only safe when stdout is a console that interprets VT sequences.
	h := syscall.Handle(os.Stdout.Fd())
	var mode uint32
	if r, _, _ := termGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false
	}
	if mode&enableVirtualTerminalProcessing == 0 {
		if r, _, _ := termSetConsoleMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing)); r == 0 {
			return false
		}
	}
	fmt.Fprint(os.Stdout, "\x1b[?9001h")
	return true
}

// disableKeyReporting turns Win32 input mode back off.
func disableKeyReporting() {
	fmt.Fprint(os.Stdout, "\x1b[?9001l")
}

// isInteractive reports whether in is attached to a Windows console.
func isInteractive(in *os.File) bool {
	h := syscall.Handle(in.Fd())
	var mode uint32
	r, _, _ := termGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode)))
	return r != 0
}

// Restore returns the console to its original input mode.
func (t *windowsTerm) Restore() {
	if t.reportKeys {
		disableKeyReporting()
	}
	_, _, _ = termSetConsoleMode.Call(uintptr(t.h), uintptr(t.oldMode))
}

// ReadKey decodes one key press. Enter inserts a newline while Ctrl+Enter
// submits: the console reports both as VK_RETURN and the difference is the
// control-key state of the input record. Ctrl+J (LF) also submits, as a
// fallback for terminals that cannot report the Ctrl+Enter chord.
func (t *windowsTerm) ReadKey() (keyEvent, error) {
	for {
		rec, err := t.readKeyRecord()
		if err != nil {
			return keyEvent{kind: keyEOF}, nil
		}
		ev, ok := classifyKeyRecord(rec)
		if !ok {
			continue
		}
		// A non-BMP character arrives as a surrogate pair spread over two
		// key records; join it before returning.
		if ev.kind == keyRune && utf16.IsSurrogate(ev.r) {
			next, nextErr := t.readKeyRecord()
			if nextErr != nil {
				continue
			}
			combined := utf16.DecodeRune(ev.r, rune(next.UnicodeChar))
			if combined == 0xfffd {
				continue
			}
			ev.r = combined
		}
		return ev, nil
	}
}

// classifyKeyRecord maps one key-down record to a key event. It reports false
// when the record carries nothing actionable (no character, unhandled chord).
//
// Character checks come first: depending on the terminal, Ctrl+J (LF) may
// arrive as an LF character with VK_J or synthesized as VK_RETURN, and Ctrl+Enter
// may or may not carry the control state through ConPTY.
func classifyKeyRecord(rec keyEventRecord) (keyEvent, bool) {
	ctrl := rec.ControlKeyState&(leftCtrlPressed|rightCtrlPressed) != 0
	alt := rec.ControlKeyState&(leftAltPressed|rightAltPressed) != 0
	ch := rune(rec.UnicodeChar)

	switch {
	case rec.VirtualKeyCode == vkReturn && ctrl:
		// Ctrl+Enter, reported with its modifier state.
		return keyEvent{kind: keySubmit}, true
	case ch == '\n':
		// LF: Ctrl+J, or how some terminals report Ctrl+Enter.
		return keyEvent{kind: keySubmit}, true
	case ch == '\r', rec.VirtualKeyCode == vkReturn:
		return keyEvent{kind: keyNewline}, true
	case rec.VirtualKeyCode == vkBack, ch == 0x7f:
		return keyEvent{kind: keyBackspace}, true
	case ctrl && rec.VirtualKeyCode == 'C', ctrl && ch == 0x03:
		return keyEvent{kind: keyInterrupt}, true
	case ctrl && rec.VirtualKeyCode == 'D', ctrl && rec.VirtualKeyCode == 'Z':
		return keyEvent{kind: keyEOF}, true
	case ctrl && rec.VirtualKeyCode == 'U':
		return keyEvent{kind: keyClearLine}, true
	case ctrl && rec.VirtualKeyCode == 'H':
		return keyEvent{kind: keyBackspace}, true
	case rec.VirtualKeyCode == vkEscape, rec.VirtualKeyCode == vkTab,
		rec.VirtualKeyCode == vkPrior, rec.VirtualKeyCode == vkNext,
		rec.VirtualKeyCode == vkHome, rec.VirtualKeyCode == vkEnd,
		rec.VirtualKeyCode == vkLeft, rec.VirtualKeyCode == vkUp,
		rec.VirtualKeyCode == vkRight, rec.VirtualKeyCode == vkDown,
		rec.VirtualKeyCode == vkInsert, rec.VirtualKeyCode == vkDelete:
		return keyEvent{kind: keyIgnore}, true
	case ctrl || alt:
		// Any other chord has no meaning here.
		return keyEvent{kind: keyIgnore}, true
	}

	if rec.UnicodeChar == 0 {
		return keyEvent{}, false
	}
	return classifyRune(ch), true
}

// readKeyRecord blocks until one key-down event is available, skipping key-up
// events and non-key events (resize, focus, ...).
func (t *windowsTerm) readKeyRecord() (keyEventRecord, error) {
	for {
		var rec inputRecord
		var read uint32
		r, _, err := termReadConsoleInputW.Call(
			uintptr(t.h),
			uintptr(unsafe.Pointer(&rec)),
			1,
			uintptr(unsafe.Pointer(&read)),
		)
		if r == 0 {
			return keyEventRecord{}, err
		}
		if read == 0 {
			return keyEventRecord{}, io.EOF
		}
		if rec.EventType != keyEventType || rec.Key.KeyDown == 0 {
			continue
		}
		return rec.Key, nil
	}
}
