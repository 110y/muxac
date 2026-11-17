package vt

import "slices"

// history keeps the rows that went off the top of the main screen, as tmux
// keeps them in its history, so that they can be shown again: a terminal that
// attaches gets them in its own scrollback, and the screen takes them back
// as it has room for them.
//
// A row is kept as the bytes that draw it, which take much less memory than
// its cells.
type history struct {
	// lines is a ring of at most limit rows, oldest at start.
	lines []historyLine
	start int
	limit int
	// pushed counts the rows kept, along with the ones dropped for the
	// limit, and clears the times the history was cleared, which tell what
	// came after a Mark.
	pushed, clears int
	// rewinds counts the times the screen took rows back, and floors holds
	// what pushed went down to the last times, which tell whether the screen
	// took back rows from before a Mark.
	rewinds int
	floors  [16]int
}

// historyLine is a row of the history.
type historyLine struct {
	// b draws the row, but for the blanks that end it if it did not wrap.
	b []byte
	// width is the number of columns the row takes: up to the blanks that
	// end it, or all of them if it wrapped, but for the one left by a wide
	// character, which is not kept.
	width int
	// fill is the background of the blanks that end the row if it did not
	// wrap.
	fill color
	// wrapped is set when the row wrapped: the line goes on on the next row,
	// which a terminal that gets them both wraps at its own width, as long
	// as the next row goes on with it (see continues).
	wrapped bool
	// padded is set when the row wrapped before its last column, which a wide
	// character did not fit in, and startsWide when the row starts with a
	// wide character.
	padded, startsWide bool
}

// hasText reports whether the row shows something, as a row erased does not.
func (hl historyLine) hasText() bool {
	return hl.width > 0 || hl.wrapped
}

// push keeps l, dropping the oldest row if the history is full.
func (h *history) push(l *line) {
	if h.limit <= 0 {
		return
	}
	hl := newHistoryLine(l)
	h.pushed++
	if len(h.lines) < h.limit {
		h.lines = append(h.lines, hl)
		return
	}
	h.lines[h.start] = hl
	h.start = (h.start + 1) % len(h.lines)
}

// newHistoryLine returns l as a row of the history.
func newHistoryLine(l *line) historyLine {
	// The blanks that end a row that wrapped are part of the line, but for
	// the one left by a wide character.
	end, bg := l.textEnd(), color(0)
	if !l.wrapped {
		end, bg = trailingBlank(l)
	}
	// The row is drawn without erasing the rest of the line, which would
	// erase its last character on a terminal it fills: terminals draw it
	// where nothing is. Blanks with a background of their own are erased
	// with it where that is known not to happen (see appendLines).
	b, pen := appendText(nil, l, style{}, end)
	if pen != (style{}) {
		b = append(b, "\x1b[0m"...)
	}
	return historyLine{b: b, width: end, fill: bg, wrapped: l.wrapped, padded: l.wrapped && l.padded, startsWide: l.startsWide()}
}

// line draws hl again, at its own width, as a line as wide as the row if it
// wrapped, and wider by a column otherwise, which holds the background of
// the blanks that ended the row, or the blank left by a wide character.
func (hl historyLine) line() line {
	cols := hl.width
	if !hl.wrapped || hl.padded {
		cols++
	}
	scratch := New(Options{Cols: cols, Rows: 1})
	scratch.Feed(nil, hl.b)
	l := scratch.screens[0].lines[0]
	l.clear(hl.width, cols, hl.fill)
	l.wrapped, l.padded = hl.wrapped, hl.padded
	return l
}

// pop takes the newest row out of the history, as a line (see
// historyLine.line).
func (h *history) pop() (line, bool) {
	if len(h.lines) == 0 {
		return line{}, false
	}
	if h.start != 0 {
		h.lines = slices.Concat(h.lines[h.start:], h.lines[:h.start])
		h.start = 0
	}
	hl := h.lines[len(h.lines)-1]
	h.lines = h.lines[:len(h.lines)-1]
	h.pushed--
	return hl.line(), true
}

// continuedBy reports whether the line of the newest row goes on on next, the
// first line of the screen.
func (h *history) continuedBy(next *line) bool {
	if len(h.lines) == 0 {
		return false
	}
	l := h.lines[(h.start+len(h.lines)-1)%len(h.lines)]
	return l.wrapped && continues(l.padded, !next.isBlank(), next.startsWide())
}

// unwrap marks the newest row as no longer going on on the first line of the
// screen, which the line it went on on moved away from. The row then ends
// with its last character, as one that did not wrap.
func (h *history) unwrap() {
	if len(h.lines) == 0 {
		return
	}
	hl := &h.lines[(h.start+len(h.lines)-1)%len(h.lines)]
	if !hl.wrapped {
		return
	}
	l := hl.line()
	l.unwrap()
	*hl = newHistoryLine(&l)
}

// rewound records that the screen took rows back, pushed going down to
// floor.
func (h *history) rewound(floor int) {
	h.floors[h.rewinds%len(h.floors)] = floor
	h.rewinds++
}

func (h *history) clear() {
	h.lines = nil
	h.start = 0
	h.clears++
}

// Mark tells how far the history of a Terminal went at some point, for
// CatchUp to tell what came after it.
type Mark struct {
	pushed, clears, rewinds int
}

// Mark returns how far the history goes now.
func (t *Terminal) Mark() Mark {
	return Mark{pushed: t.history.pushed, clears: t.history.clears, rewinds: t.history.rewinds}
}

// kept reports whether the history still has every row it had at mark,
// with only rows that came after appended: it was neither cleared since, nor
// did the screen take back rows from before mark.
func (h *history) kept(mark Mark) bool {
	if mark.clears != h.clears || h.rewinds-mark.rewinds > len(h.floors) {
		return false
	}
	for r := mark.rewinds; r < h.rewinds; r++ {
		if h.floors[r%len(h.floors)] < mark.pushed {
			return false
		}
	}
	return true
}

// appendLines appends the rows kept since mark, oldest first, for a terminal
// of cols columns, each line followed by CR LF: a row whose line goes on on
// the next one is followed by it, which the terminal then wraps at its own
// width. It reports whether the line of the last row is left to go on, on
// the screen.
//
// The blanks that end a row with a background of their own are erased with
// it, as long as the row ends before the end of the row of the terminal,
// where erasing would erase its last character: if the rows of its line
// before it are as wide as the terminal, which wraps the line there as the
// screen did, and the row is narrower.
func (h *history) appendLines(b []byte, mark Mark, cols int) ([]byte, bool) {
	open := false
	// aligned tells whether the next row starts at the start of a row of the
	// terminal.
	aligned := true
	for i := h.since(mark); i < len(h.lines); i++ {
		l := h.lines[(h.start+i)%len(h.lines)]
		b = append(b, l.b...)
		if l.fill != 0 && aligned && l.width < cols {
			b = appendSGR(b, style{bg: l.fill})
			b = append(b, "\x1b[K\x1b[0m"...)
		}
		open = l.wrapped
		if i+1 < len(h.lines) {
			next := h.lines[(h.start+i+1)%len(h.lines)]
			open = open && continues(l.padded, next.hasText(), next.startsWide)
		}
		if !open {
			b = append(b, "\r\n"...)
			aligned = true
			continue
		}
		width := l.width
		if l.padded {
			width++
		}
		aligned = aligned && width == cols
	}
	return b, open
}

// since returns where the rows kept since mark start in lines, oldest first:
// the rows kept since the history was last cleared, as all of them are if
// that was after mark, number from pushed-len(lines).
func (h *history) since(mark Mark) int {
	if mark.clears != h.clears {
		return 0
	}
	return max(mark.pushed-(h.pushed-len(h.lines)), 0)
}

// lastLineFits reports whether the rows kept since mark of the line of the
// newest row are cols wide, the blank left by a wide character included, as
// a terminal of cols columns then wraps the line the same way.
func (h *history) lastLineFits(mark Mark, cols int) bool {
	first := h.since(mark)
	for i := len(h.lines) - 1; i >= first; i-- {
		hl := h.lines[(h.start+i)%len(h.lines)]
		width := hl.width
		if hl.padded {
			width++
		}
		if width != cols {
			return false
		}
		if i == first {
			break
		}
		// The line starts on this row unless the previous one goes on on it.
		prev := h.lines[(h.start+i-1)%len(h.lines)]
		if !prev.wrapped || !continues(prev.padded, hl.hasText(), hl.startsWide) {
			break
		}
	}
	return true
}

// saveLines keeps the lines [from, to) of the main screen in the history, if
// it is the screen shown.
func (t *Terminal) saveLines(from, to int) {
	if t.alt {
		return
	}
	lines := t.screens[0].lines
	for y := from; y < to; y++ {
		t.history.push(&lines[y])
	}
}
