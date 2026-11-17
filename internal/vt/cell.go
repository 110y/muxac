package vt

// color is a foreground, background or underline color: the terminal's
// default color (the zero value), an entry of its palette, or an RGB color.
// Its top byte tells which, as a colorKind.
type color uint32

// colorKind tells which kind of color a color is.
type colorKind uint8

const (
	colorDefault colorKind = iota
	colorPalette
	colorRGB
)

func (c color) kind() colorKind {
	return colorKind(c >> 24)
}

func paletteColor(i int) color {
	return color(colorPalette)<<24 | color(i&0xff)
}

func rgbColor(r, g, b int) color {
	return color(colorRGB)<<24 | color(r&0xff)<<16 | color(g&0xff)<<8 | color(b&0xff)
}

// attrs are the character attributes other than colors and underlining.
type attrs uint16

const (
	attrBold attrs = 1 << iota
	attrDim
	attrItalic
	attrBlink
	attrInverse
	attrInvisible
	attrStrike
	attrOverline
)

// underline is the style of the line drawn under a character.
type underline uint8

const (
	underlineNone underline = iota
	underlineSingle
	underlineDouble
	underlineCurly
	underlineDotted
	underlineDashed
)

// style is how a character is drawn, as set by SGR.
type style struct {
	fg, bg, ul color
	attrs      attrs
	underline  underline
}

// cell is one position of a screen.
type cell struct {
	// r is the character shown in the cell, 0 for a blank cell.
	r rune
	// comb holds the characters drawn together with r in the same cell, such
	// as combining accents, variation selectors and the rest of an emoji
	// sequence.
	comb string
	// width is the number of columns the character takes: 1, or 2 for a wide
	// character. The right half of a wide character is the next cell, which
	// has width 0.
	width uint8
	style style
}

// blank returns an empty cell. Erasing fills the screen with blank cells of
// the current background color, as terminals do.
func blank(bg color) cell {
	return cell{width: 1, style: style{bg: bg}}
}

// isBlank reports whether c shows nothing but its background.
func (c cell) isBlank() bool {
	return (c.r == 0 || c.r == ' ') && c.comb == "" && c.width == 1 &&
		c.style.attrs&attrInverse == 0 && c.style.underline == underlineNone &&
		c.style.attrs&(attrStrike|attrOverline) == 0
}

// line is one row of a screen.
type line struct {
	cells []cell
	// wrapped is set when the text on the line continues on the next one
	// because it reached the right margin.
	wrapped bool
	// padded is set, along with wrapped, when the last cell of the line was
	// left blank because the wide character after it did not fit there and
	// went on to the next line: that cell is not part of the text.
	padded bool
}

// textEnd returns where the text of l ends if it wrapped: at its last cell,
// unless that cell is the blank left by a wide character.
func (l *line) textEnd() int {
	if l.padded {
		return len(l.cells) - 1
	}
	return len(l.cells)
}

// wrap marks l as continuing on the next line, padded or not.
func (l *line) wrap(padded bool) {
	l.wrapped, l.padded = true, padded
}

// unwrap marks l as no longer continuing on the next line, which the line it
// went on on moved away from.
func (l *line) unwrap() {
	l.wrapped, l.padded = false, false
}

// newLine returns a blank line of cols cells.
func newLine(cols int) line {
	l := line{cells: make([]cell, cols)}
	l.clear(0, cols, 0)
	return l
}

// clear blanks the cells in [from, to).
func (l *line) clear(from, to int, bg color) {
	b := blank(bg)
	for i := from; i < to; i++ {
		l.cells[i] = b
	}
}

// isBlank reports whether l shows nothing but its background.
func (l *line) isBlank() bool {
	end, _ := trailingBlank(l)
	return end == 0 && !l.wrapped
}

// startsWide reports whether the first character of l is a wide one.
func (l *line) startsWide() bool {
	return len(l.cells) > 0 && l.cells[0].width == 2
}

// continuedBy reports whether the text of l goes on on next, the line after
// it, as it did when l wrapped. It no longer does once next was erased, or
// drawn over with a narrow character where the wide one went that left the
// last cell of l blank.
func (l *line) continuedBy(next *line) bool {
	return l.wrapped && next != nil && continues(l.padded, !next.isBlank(), next.startsWide())
}

// continues reports whether a line that wrapped, padded or not, goes on on
// the next one, which has text or not and starts with a wide character or
// not.
func continues(padded, hasText, startsWide bool) bool {
	return hasText && (!padded || startsWide)
}

// charset is a character set that can be designated as G0 to G3.
type charset uint8

const (
	charsetASCII charset = iota
	// charsetDECSpecial is the DEC Special Character and Line Drawing Set.
	charsetDECSpecial
	// charsetUK is the United Kingdom set, in which "#" is "£".
	charsetUK
)

// decSpecial maps the characters 0x5f to 0x7e to the DEC Special Character and
// Line Drawing Set.
var decSpecial = [...]rune{
	' ', '◆', '▒', '␉', '␌', '␍', '␊', '°', '±', '␤', '␋', '┘', '┐', '┌', '└', '┼',
	'⎺', '⎻', '─', '⎼', '⎽', '├', '┤', '┴', '┬', '│', '≤', '≥', 'π', '≠', '£', '·',
}

// translate maps the ASCII character r to the character set cs.
func (cs charset) translate(r rune) rune {
	switch cs {
	case charsetDECSpecial:
		if r >= 0x5f && r <= 0x7e {
			return decSpecial[r-0x5f]
		}
	case charsetUK:
		if r == '#' {
			return '£'
		}
	case charsetASCII:
	}
	return r
}

// final returns the final character that designates cs.
func (cs charset) final() byte {
	switch cs {
	case charsetDECSpecial:
		return '0'
	case charsetUK:
		return 'A'
	case charsetASCII:
	}
	return 'B'
}

// resize cuts or extends l to cols cells.
func (l *line) resize(cols int) {
	n := len(l.cells)
	switch {
	case cols < n:
		l.cells = l.cells[:cols:cols]
		if last := &l.cells[cols-1]; last.width == 2 {
			*last = blank(last.style.bg)
		}
		l.wrapped = false
	case cols > n:
		l.cells = append(l.cells, make([]cell, cols-n)...)
		l.clear(n, cols, 0)
	}
}
