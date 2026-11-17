package vt

import (
	"unicode"

	"github.com/clipperhouse/displaywidth"
)

// widthOptions measures characters the way the programs muxac runs do:
// Claude Code (Bun's stringWidth) and Codex (unicode-width) both take East
// Asian ambiguous characters as narrow. The zero value also keeps the
// measure independent of the locale.
var widthOptions = displaywidth.Options{}

// runeWidth returns the number of columns r takes on its own: 0, 1 or 2.
func runeWidth(r rune) int {
	switch {
	case r < 0x80:
		if r < 0x20 || r == 0x7f {
			return 0
		}
		return 1
	case unicode.In(r, unicode.Mn, unicode.Me):
		// Combining marks, including the ones newer than displaywidth's
		// tables.
		return 0
	case r >= 0x1160 && r <= 0x11ff, r >= 0xd7b0 && r <= 0xd7ff:
		// Hangul jamo vowels and final consonants, which join the leading
		// consonant before them.
		return 0
	}
	return widthOptions.Rune(r)
}

// clusterWidth returns the number of columns the grapheme cluster s takes.
func clusterWidth(s string) int {
	return widthOptions.String(s)
}

// extendsCluster reports whether r continues the grapheme cluster s, as a
// combining mark, a variation selector or the next part of an emoji sequence
// does.
func extendsCluster(s string, r rune) bool {
	g := widthOptions.StringGraphemes(s + string(r))
	n := 0
	for g.Next() {
		n++
		if n > 1 {
			return false
		}
	}
	return n == 1
}
