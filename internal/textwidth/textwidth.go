// Package textwidth measures text by the columns it occupies on a terminal, so
// a table padded with it lines up even when a cell holds East Asian wide runes
// (a CJK file name, say). A wide rune is one terminal column short of its byte
// or rune count, which is what makes a %-Ns pad in fmt misalign a CJK name.
package textwidth

import "strings"

// IsWide reports whether r renders as a double-width glyph: the East Asian
// wide and fullwidth ranges (CJK ideographs and punctuation, kana, Hangul,
// fullwidth forms and the supplementary CJK planes).
func IsWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115f, // Hangul Jamo
		r == 0x2329, r == 0x232a,
		r >= 0x2e80 && r <= 0x303e, // CJK radicals, Kangxi
		r >= 0x3041 && r <= 0x33ff, // kana, CJK symbols
		r >= 0x3400 && r <= 0x4dbf, // CJK extension A
		r >= 0x4e00 && r <= 0x9fff, // CJK unified ideographs
		r >= 0xa000 && r <= 0xa4cf, // Yi
		r >= 0xac00 && r <= 0xd7a3, // Hangul syllables
		r >= 0xf900 && r <= 0xfaff, // CJK compatibility
		r >= 0xfe30 && r <= 0xfe6f, // CJK compatibility forms
		r >= 0xff00 && r <= 0xff60, // fullwidth forms
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x20000 && r <= 0x3fffd:
		return true
	}
	return false
}

// RuneColumns returns the column width of a single rune. A tab advances to the
// next tab stop, so it is measured at the widest step (8); a control character
// draws nothing (0); a wide rune is 2; anything else is 1.
func RuneColumns(r rune) int {
	switch {
	case r == '\t':
		return 8
	case r < 0x20, r == 0x7f:
		return 0
	case IsWide(r):
		return 2
	}
	return 1
}

// Width returns the terminal columns s occupies.
func Width(s string) int {
	cols := 0
	for _, r := range s {
		cols += RuneColumns(r)
	}
	return cols
}

// PadRight pads s with spaces until it occupies at least width columns (wide
// runes count as two). An s already at or past width is returned unchanged, so
// a long name is never truncated.
func PadRight(s string, width int) string {
	if w := Width(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}
