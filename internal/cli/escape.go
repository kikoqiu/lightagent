package cli

import (
	"bufio"
	"strconv"
	"strings"
)

// This file decodes the escape sequences a byte-stream terminal sends for a key
// press; term_linux.go is the caller. The console-record readers
// (term_windows.go) get the modifiers straight from the input record instead
// and do not need it.

// Disambiguating keyboard protocols encode a chord as the key's code point plus
// a modifier number: "CSI <code>[;<mods>]u" (kitty keyboard protocol, based on
// fixterms) and "CSI 27;<mods>;<code>~" (xterm modifyOtherKeys). The modifier
// number is one plus a bitmask:
const (
	modShift = 1 << iota
	modAlt
	modCtrl
)

// Key codes the decoder maps onto an editor key.
const (
	codeBackspace = 8
	codeTab       = 9
	codeEnter     = 13
	codeEscape    = 27
	codeDel       = 127
)

// classifyEscapeSequence decodes the rest of a key sequence whose leading ESC
// byte has already been read. The caller must know that more input is pending
// (see linuxTerm.escapePending), otherwise a lone Esc key press would swallow
// the key typed after it.
//
// A whole sequence is always consumed, so nothing of an unrecognised sequence
// leaks into the editor as literal text.
func classifyEscapeSequence(r *bufio.Reader) keyEvent {
	first, _, err := r.ReadRune()
	if err != nil {
		return keyEvent{kind: keyIgnore}
	}
	switch first {
	case '\r', '\n':
		// Alt+Enter: a terminal sends meta chords with an ESC prefix, which is
		// the one way to report the chord that needs no protocol at all.
		return keyEvent{kind: keySubmit}
	case '[':
		return classifyCSI(r)
	case 'O':
		// SS3: a function key without modifiers.
		_, _, _ = r.ReadRune()
		return keyEvent{kind: keyIgnore}
	}
	// "ESC <char>" is a meta chord (Alt+letter and friends), which the editor
	// has no binding for.
	return keyEvent{kind: keyIgnore}
}

// classifyCSI consumes one CSI sequence and maps the ones carrying a
// disambiguated key; cursor keys, private modes and the rest are swallowed.
func classifyCSI(r *bufio.Reader) keyEvent {
	var params strings.Builder
	for {
		b, err := r.ReadByte()
		if err != nil {
			return keyEvent{kind: keyIgnore}
		}
		switch {
		case b >= 0x30 && b <= 0x3f: // parameter bytes
			params.WriteByte(b)
		case b >= 0x20 && b <= 0x2f: // intermediate bytes
		case b >= 0x40 && b <= 0x7e: // final byte
			return classifyCSIFinal(params.String(), b)
		default:
			// Not a well-formed sequence: stop at the stray byte instead of
			// treating what follows as more parameters.
			return keyEvent{kind: keyIgnore}
		}
	}
}

// classifyCSIFinal maps a complete CSI sequence onto an editor key.
func classifyCSIFinal(params string, final byte) keyEvent {
	switch final {
	case 'u':
		code, mods, ok := parseKeyParams(params)
		if !ok {
			return keyEvent{kind: keyIgnore}
		}
		return classifyDisambiguatedKey(code, mods)
	case '~':
		// xterm modifyOtherKeys: "CSI 27;<mods>;<code>~".
		fields := strings.Split(params, ";")
		if len(fields) != 3 || fields[0] != "27" {
			return keyEvent{kind: keyIgnore}
		}
		mods, err := strconv.Atoi(fields[1])
		if err != nil {
			return keyEvent{kind: keyIgnore}
		}
		code, err := strconv.Atoi(fields[2])
		if err != nil {
			return keyEvent{kind: keyIgnore}
		}
		return classifyDisambiguatedKey(code, mods)
	}
	return keyEvent{kind: keyIgnore}
}

// parseKeyParams splits "13;5" (a CSI u sequence) into its key code and its
// modifier number; an omitted modifier means "no modifiers".
func parseKeyParams(params string) (code, mods int, ok bool) {
	fields := strings.SplitN(params, ";", 2)
	code, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, false
	}
	mods = 1
	if len(fields) == 2 {
		if mods, err = strconv.Atoi(fields[1]); err != nil {
			return 0, 0, false
		}
	}
	return code, mods, true
}

// classifyDisambiguatedKey maps a disambiguated key press onto an editor key.
// Only the chords the editor binds are recognised; everything else (the Esc key
// in its "CSI 27 u" form included) is ignored, exactly like an unhandled
// sequence in the legacy encoding.
func classifyDisambiguatedKey(code, mods int) keyEvent {
	m := mods - 1
	ctrl := m&modCtrl != 0
	alt := m&modAlt != 0
	switch {
	case code == codeEnter && (ctrl || alt):
		// Ctrl+Enter (or Alt+Enter) reported as a chord: this is the send key,
		// the whole point of asking the terminal for disambiguated keys.
		return keyEvent{kind: keySubmit}
	case code == codeEnter:
		// Plain or Shift+Enter keeps its legacy meaning: a line break.
		return keyEvent{kind: keyNewline}
	case code == codeBackspace || code == codeDel:
		return keyEvent{kind: keyBackspace}
	case code == codeTab, code == codeEscape:
		return keyEvent{kind: keyIgnore}
	case !ctrl:
		// Any other chord has no meaning here.
		return keyEvent{kind: keyIgnore}
	}
	// ctrl+<letter> keeps the meaning it has in the legacy encoding, so the
	// editor keys still work when the terminal reports them disambiguated.
	switch code {
	case 'c':
		return keyEvent{kind: keyInterrupt}
	case 'd', 'z':
		return keyEvent{kind: keyEOF}
	case 'h':
		return keyEvent{kind: keyBackspace}
	case 'j':
		return keyEvent{kind: keySubmit}
	case 'm':
		return keyEvent{kind: keyNewline}
	case 'u':
		return keyEvent{kind: keyClearLine}
	}
	return keyEvent{kind: keyIgnore}
}
