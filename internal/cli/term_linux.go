//go:build linux

package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// escapeWaitMicros is how long ReadKey waits for the rest of a key sequence
// after its leading ESC byte: 50 ms, in microseconds, so the untyped constant
// fits syscall.Timeval.Usec on 32- and 64-bit targets alike. A key press
// reaches the terminal in a single write, so the rest of the sequence is
// normally already buffered and the wait only covers a burst the kernel
// happened to split. A lone Esc key press leaves the input empty, so it stays an
// Esc instead of turning the key typed after it into a meta chord.
const escapeWaitMicros = 50 * 1000

// The kitty keyboard protocol asks the terminal to disambiguate key presses
// (flag 1, "disambiguate escape codes"): the key is then reported as its code
// point plus the modifiers, so Ctrl+Enter arrives as "CSI 13;5u" instead of the
// CR that a plain Enter sends. A terminal without the protocol ignores both
// sequences; the push is undone by the pop on the way out.
const (
	kittyKeyboardPush = "\x1b[>1u"
	kittyKeyboardPop  = "\x1b[<u"
)

// linuxTerm reads raw keys from a Unix terminal by switching it out of
// canonical/echo mode with termios.
type linuxTerm struct {
	fd         int
	old        syscall.Termios
	reader     *bufio.Reader
	reportKeys bool
}

// newTermReader enables raw mode when stdin is a terminal. It returns ok=false
// for pipes/non-terminals so the caller can fall back to line scanning.
func newTermReader(in *os.File) (termReader, bool) {
	fd := int(in.Fd())
	var old syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&old))); errno != 0 {
		return nil, false
	}
	raw := old
	raw.Iflag &^= syscall.ICRNL | syscall.IXON | syscall.IXOFF | syscall.ISTRIP
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&raw))); errno != 0 {
		return nil, false
	}
	return &linuxTerm{fd: fd, old: old, reader: bufio.NewReader(in), reportKeys: enableKeyReporting()}, true
}

// enableKeyReporting asks the terminal for disambiguated key presses, so chords
// the legacy byte encoding cannot express (Ctrl+Enter above all) still reach the
// editor. It is best effort: only a terminal implementing the kitty keyboard
// protocol answers, the others ignore the sequence. LIGHTAGENT_SIMPLE_INPUT
// disables the attempt for terminals that get confused by it.
func enableKeyReporting() bool {
	if strings.TrimSpace(os.Getenv("LIGHTAGENT_SIMPLE_INPUT")) != "" {
		return false
	}
	// Only a real terminal can carry the request back to the keyboard.
	if !isInteractive(os.Stdout) {
		return false
	}
	fmt.Fprint(os.Stdout, kittyKeyboardPush)
	return true
}

// isInteractive reports whether in is a terminal.
func isInteractive(in *os.File) bool {
	fd := int(in.Fd())
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

// winsize mirrors struct winsize from <termios.h>.
type winsize struct {
	Row, Col, Xpixel, Ypixel uint16
}

// terminalWidth returns the terminal width in columns (0 when unknown).
func terminalWidth() int {
	fd := int(os.Stdout.Fd())
	var ws winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Col == 0 {
		return 0
	}
	return int(ws.Col)
}

// spinnerGlyphsSupported reports whether the terminal can draw the Unicode
// braille spinner; the platforms that fall back to line scanning are assumed to
// be able to.
func spinnerGlyphsSupported() bool { return true }

// Restore returns the terminal to its original settings.
func (t *linuxTerm) Restore() {
	if t.reportKeys {
		fmt.Fprint(os.Stdout, kittyKeyboardPop)
	}
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, uintptr(t.fd), uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&t.old)))
}

// ReadKey decodes one key press. Enter arrives as CR (a newline) and Ctrl+J as
// LF (a submit), which every terminal can send. Ctrl+Enter needs a terminal
// that answers the kitty keyboard protocol request (it then arrives as
// "CSI 13;5u"); Alt+Enter works everywhere, as the ESC prefix of a meta chord.
func (t *linuxTerm) ReadKey() (keyEvent, error) {
	r, _, err := t.reader.ReadRune()
	if err != nil {
		if err == io.EOF {
			return keyEvent{kind: keyEOF}, nil
		}
		return keyEvent{}, err
	}
	if r == 0x1b {
		if !t.escapePending() {
			// A lone Esc key press.
			return keyEvent{kind: keyIgnore}, nil
		}
		return classifyEscapeSequence(t.reader), nil
	}
	return classifyRune(r), nil
}

// escapePending reports whether more input follows the ESC byte that was just
// read, i.e. whether the ESC starts a key sequence rather than being the Esc key
// on its own.
func (t *linuxTerm) escapePending() bool {
	if t.reader.Buffered() > 0 {
		return true
	}
	var rfds syscall.FdSet
	// The word size differs between 32- and 64-bit targets.
	bits := uint(unsafe.Sizeof(rfds.Bits[0]) * 8)
	fd := uint(t.fd)
	rfds.Bits[fd/bits] |= 1 << (fd % bits)
	tv := syscall.Timeval{Usec: escapeWaitMicros}
	n, err := syscall.Select(t.fd+1, &rfds, nil, nil, &tv)
	return err == nil && n > 0
}
