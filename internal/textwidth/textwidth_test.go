package textwidth

import "testing"

// TestWidthCountsWideRunesAsTwo pins the measurement: a CJK ideograph is two
// columns, ASCII is one, a tab is measured at its widest step and a control
// character draws nothing.
func TestWidthCountsWideRunesAsTwo(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"备注", 4},
		{"a备b", 4},
		{"ＡＢ", 4},   // fullwidth Latin
		{"　", 2},     // ideographic space
		{"。", 2},     // CJK full stop
		{"\t", 8},
		{"\x01", 0},
	}
	for _, tc := range cases {
		if got := Width(tc.s); got != tc.want {
			t.Errorf("Width(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

// TestPadRightPadsByColumns pins that padding is measured in columns: a CJK
// cell padded to a width reaches that width just like an ASCII one, and a
// longer string is returned unchanged.
func TestPadRightPadsByColumns(t *testing.T) {
	if got := PadRight("ab", 5); got != "ab   " {
		t.Errorf("PadRight(ab,5) = %q, want %q", got, "ab   ")
	}
	padded := PadRight("备注", 6)
	if got := Width(padded); got != 6 {
		t.Errorf("width of PadRight(备注,6) = %d, want 6 (%q)", got, padded)
	}
	if got := PadRight("abcdef", 3); got != "abcdef" {
		t.Errorf("a longer string must be returned unchanged: %q", got)
	}
}

// TestIsWide covers both ends of the wide ranges.
func TestIsWide(t *testing.T) {
	for _, r := range []rune{'中', 'あ', 'Ａ', '。', '가'} {
		if !IsWide(r) {
			t.Errorf("IsWide(%q) = false, want true", r)
		}
	}
	for _, r := range []rune{'a', '0', '~', '\t'} {
		if IsWide(r) {
			t.Errorf("IsWide(%q) = true, want false", r)
		}
	}
}
