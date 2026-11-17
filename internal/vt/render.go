package vt

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// resetDrawing resets what affects how characters are drawn: the SGR
// attributes and the character sets.
const resetDrawing = "\x1b[0m\x1b(B\x1b)B\x1b*B\x1b+B\x0f"

// Redraw returns dst with the bytes appended that bring a terminal of the
// same size that shows the session already to the state of t: what both
// screens show, the cursor, and the modes the program set up.
func (t *Terminal) Redraw(dst []byte) []byte {
	b := append(dst, "\x1b[?2026h"...)
	b = appendModes(b, true, false, 1049)
	return t.appendState(b, true)
}

// Attach returns dst with the bytes appended that bring a terminal of the
// same size that shows something else, such as the shell muxac was started
// from, to the state of t, as Redraw does. What the terminal shows goes into
// its scrollback, followed by the history, and the terminal's title is saved
// for Leave to restore.
//
// row and col tell where the cursor of the terminal is, 1-based, so that the
// history goes right after what the terminal shows; 0 if unknown.
func (t *Terminal) Attach(dst []byte, row, col int) []byte {
	b := append(dst, "\x1b[?2026h\x1b[22;0t"...)
	b = append(b, resetDrawing...)
	b = append(b, "\x1b[r\x1b[?6l\x1b[4l\x1b[?7h"...)

	// The history goes, line by line, to the line after what the terminal
	// shows: the cursor's line if the cursor is at its start, which it is
	// once a shell has run muxac.
	start := t.rows - 1
	if row > 0 {
		start = min(row-1, t.rows-1)
	}
	b = appendCUP(b, start, 0)
	if row <= 0 || col > 1 {
		b = append(b, "\r\n"...)
	}
	// Where the lines go is blank.
	b = append(b, "\x1b[J"...)
	b, continued, aligned := t.appendHistory(b, Mark{clears: t.history.clears})
	b = t.appendLines(b, continued, aligned)
	return t.appendState(b, false)
}

// CatchUp returns dst with the bytes appended that bring a terminal of the
// same size, which stopped showing the session when its history went as far
// as mark, to the state of t, as Redraw does. The lines that went into the
// history since then go into the terminal's scrollback, as they would have if
// the terminal had gone on showing the session, after the scrollback is
// cleared if the history was.
func (t *Terminal) CatchUp(dst []byte, mark Mark) []byte {
	b := append(dst, "\x1b[?2026h"...)
	// The history goes to the scrollback of the main screen.
	b = appendModes(b, true, false, 1049)
	b = append(b, resetDrawing...)
	b = append(b, "\x1b[r\x1b[?6l\x1b[4l\x1b[?7h"...)
	if !t.history.kept(mark) {
		// The scrollback has lines that the history no longer has, or that
		// the screen took back: it is replaced with the history.
		b = append(b, "\x1b[3J"...)
		mark = Mark{clears: t.history.clears}
	}
	// The lines replace what the terminal shows, which they followed, from
	// its top. It is erased line by line first: some terminals keep what
	// the whole screen erased at once shows in their scrollback.
	for y := range t.rows {
		b = appendCUP(b, y, 0)
		b = append(b, "\x1b[K"...)
	}
	b = appendCUP(b, 0, 0)
	b, continued, aligned := t.appendHistory(b, mark)
	b = t.appendLines(b, continued, aligned)
	return t.appendState(b, false)
}

// appendLines draws the lines of the main screen from the cursor on, as the
// history goes on with them: a row that wrapped goes on on the next one,
// which the terminal, as wide as the screen, then wraps the same way, keeping
// track of the line it is part of. continued tells whether the first row goes
// on with the line of the last row of the history, and aligned whether the
// terminal wraps that line as the screen does (see appendRow). Once they are
// drawn, the screen of the terminal is the main screen, and what came before
// is in its scrollback.
func (t *Terminal) appendLines(b []byte, continued, aligned bool) []byte {
	lines := t.screens[0].lines
	for y := range lines {
		l := &lines[y]
		goesOn := y+1 < len(lines) && l.continuedBy(&lines[y+1])
		b = appendRow(b, l, continued, goesOn, aligned)
		if !goesOn {
			// The lines after are drawn from the start of a row.
			aligned = true
			if y < len(lines)-1 {
				b = append(b, "\r\n"...)
			}
		}
		continued = goesOn
	}
	return b
}

// appendHistory draws the rows of the history kept since mark from the
// cursor on, ending the line of the last one unless the first line of the
// screen goes on with it. It reports whether it does, and if so whether the
// rows of that line it drew are as wide as the screen, for the terminal,
// wrapping them, to wrap the line as the screen does.
func (t *Terminal) appendHistory(b []byte, mark Mark) ([]byte, bool, bool) {
	b, open := t.history.appendLines(b, mark, t.cols)
	if !open {
		return b, false, true
	}
	if !t.history.continuedBy(&t.screens[0].lines[0]) {
		return append(b, "\r\n"...), false, true
	}
	return b, true, t.history.lastLineFits(mark, t.cols)
}

// appendRow draws the row l from the cursor on, as far as rowExtent tells,
// erasing the rest of it, and leaves the attributes of the terminal as they
// were. The terminal may have filled the rest with the background of the
// character it wrapped to the row with, as it scrolled. Unless aligned, the
// line of the row goes on from rows of the history as wide as another
// terminal was, which the terminal wraps otherwise than the screen: the row
// may end right at the end of a row of the terminal there, where erasing
// would erase its last character, so that the rest is left as it is.
func appendRow(b []byte, l *line, continued, goesOn, aligned bool) []byte {
	end, bg := rowExtent(l, continued, goesOn)
	b, pen := appendText(b, l, style{}, end)
	if end < len(l.cells) && aligned {
		b, pen = appendErase(b, pen, bg)
	}
	if pen != (style{}) {
		b = append(b, "\x1b[0m"...)
	}
	return b
}

// rowExtent returns how far to draw the row l, and the background of the
// blanks after that: all of its text if its line goes on on the next row,
// and otherwise up to the blanks that end it. A row the line of the previous
// one goes on on, continued, which left the cursor waiting to wrap, is drawn
// at least to its first character, for the terminal to wrap to it, even if
// it is all blanks.
func rowExtent(l *line, continued, goesOn bool) (int, color) {
	if goesOn {
		return l.textEnd(), 0
	}
	end, bg := trailingBlank(l)
	if continued {
		end = max(end, 1)
	}
	return end, bg
}

// appendState draws both screens over the terminal, the main one unless
// drawMain is false, and sets the cursor and the modes.
func (t *Terminal) appendState(b []byte, drawMain bool) []byte {
	b = append(b, "\x1b[?25l\x1b[r\x1b[?6l\x1b[4l\x1b[?7h"...)
	b = append(b, resetDrawing...)

	main := &t.screens[0]
	if drawMain {
		b = t.appendScreen(b, main)
	}
	if t.alt {
		b = appendCursorState(b, main.savedCursor())
		if t.altMode == 1049 {
			// Switching saves the cursor as DECSC does, and clears the
			// alternate screen.
			b = appendModes(b, true, true, 1049)
			b = append(b, resetDrawing...)
		} else {
			b = append(b, "\x1b7"...)
			b = appendModes(b, true, true, t.altMode)
			b = append(b, resetDrawing...)
			b = append(b, "\x1b[H\x1b[2J"...)
		}
		b = t.appendScreen(b, &t.screens[1])
		b = appendCursorState(b, t.screens[1].savedCursor())
		b = append(b, "\x1b7"...)
		b = append(b, resetDrawing...)
	} else {
		b = appendCursorState(b, main.savedCursor())
		b = append(b, "\x1b7"...)
		b = append(b, resetDrawing...)
	}
	b = t.appendTabs(b)

	if t.top != 0 || t.bottom != t.rows-1 {
		b = append(b, "\x1b["...)
		b = strconv.AppendInt(b, int64(t.top+1), 10)
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(t.bottom+1), 10)
		b = append(b, 'r')
	}
	if t.modes.origin {
		b = append(b, "\x1b[?6h"...)
	}
	b = t.appendCursor(b)
	b = t.appendModes(b)
	b = appendSGR(b, t.pen)
	b = appendCharsets(b, t.charsets, t.gl)
	if t.title != "" {
		b = append(b, "\x1b]2;"...)
		b = append(b, t.title...)
		b = append(b, "\x1b\\"...)
	}
	if !t.modes.cursorHidden {
		b = append(b, "\x1b[?25h"...)
	}
	return append(b, "\x1b[?2026l"...)
}

// appendScreen draws the lines of s over the whole terminal.
func (t *Terminal) appendScreen(b []byte, s *screen) []byte {
	var pen style
	b = append(b, "\x1b[0m"...)
	// continued is set when the previous line wrapped, so that this one is
	// drawn as its continuation, which the terminal then wraps the same way
	// and keeps track of (see rowExtent).
	continued := false
	for y := range s.lines {
		l := &s.lines[y]
		if !continued {
			b = appendCUP(b, y, 0)
		}
		goesOn := y+1 < len(s.lines) && l.continuedBy(&s.lines[y+1])
		end, bg := rowExtent(l, continued, goesOn)
		b, pen = appendCells(b, l, pen, end, bg)
		continued = goesOn
	}
	return append(b, "\x1b[0m"...)
}

// appendCells draws the cells [0, end) of l as appendText does, erasing the
// rest of the line with the background bg. It returns the attributes it
// leaves the terminal with.
func appendCells(b []byte, l *line, pen style, end int, bg color) ([]byte, style) {
	b, pen = appendText(b, l, pen, end)
	if end == len(l.cells) {
		return b, pen
	}
	return appendErase(b, pen, bg)
}

// appendErase erases the rest of the line from the cursor on, which has the
// attributes pen, with the background bg, and returns the attributes it
// leaves the terminal with.
func appendErase(b []byte, pen style, bg color) ([]byte, style) {
	if want := (style{bg: bg}); pen != want {
		b = appendSGR(b, want)
		pen = want
	}
	return append(b, "\x1b[K"...), pen
}

// appendText draws the cells [0, end) of l from the cursor on, which has the
// attributes pen, and returns the attributes it leaves the terminal with.
func appendText(b []byte, l *line, pen style, end int) ([]byte, style) {
	for x := 0; x < end; {
		c := l.cells[x]
		if c.style != pen {
			b = appendSGR(b, c.style)
			pen = c.style
		}
		b = appendGlyph(b, c)
		if c.width == 2 {
			x += 2
		} else {
			x++
		}
	}
	return b, pen
}

// trailingBlank returns where the run of blank cells that ends l starts, and
// the background color they share, which erasing the rest of the line
// fills them with.
func trailingBlank(l *line) (int, color) {
	n := len(l.cells)
	if n == 0 || !l.cells[n-1].isBlank() {
		return n, 0
	}
	bg := l.cells[n-1].style.bg
	i := n
	for i > 0 && l.cells[i-1].isBlank() && l.cells[i-1].style.bg == bg {
		i--
	}
	return i, bg
}

// appendCursorState moves the cursor and sets the attributes and character
// sets as saved in c, for DECSC to save them again.
func appendCursorState(b []byte, c savedCursor) []byte {
	b = appendCUP(b, c.y, c.x)
	b = appendSGR(b, c.pen)
	return appendCharsets(b, c.charsets, c.gl)
}

// appendCharsets designates the character sets cs and invokes the set gl
// into GL.
func appendCharsets(b []byte, cs [4]charset, gl int) []byte {
	for i, c := range cs {
		b = append(b, 0x1b, "()*+"[i], c.final())
	}
	switch gl {
	case 1:
		return append(b, 0x0e)
	case 2:
		return append(b, 0x1b, 'n')
	case 3:
		return append(b, 0x1b, 'o')
	}
	return append(b, 0x0f)
}

// appendTabs sets the tab stops, unless they are the default ones.
func (t *Terminal) appendTabs(b []byte) []byte {
	if slices.Equal(t.tabs, defaultTabs(t.cols)) {
		return b
	}
	b = append(b, "\x1b[3g"...)
	for x, set := range t.tabs {
		if set {
			b = appendCUP(b, 0, x)
			b = append(b, "\x1bH"...)
		}
	}
	return b
}

// appendCursor moves the cursor where the program left it.
func (t *Terminal) appendCursor(b []byte) []byte {
	y := t.y
	if t.modes.origin {
		y -= t.top
	}
	if !t.pendingWrap {
		return appendCUP(b, y, t.x)
	}
	// Writing the last character of the line again leaves the cursor
	// pending a wrap, as the program left it.
	l := t.line(t.y)
	x := t.cols - 1
	if l.cells[x].width == 0 && x > 0 && l.cells[x-1].width == 2 {
		x--
	}
	c := l.cells[x]
	b = appendCUP(b, y, x)
	b = appendSGR(b, c.style)
	return appendGlyph(b, c)
}

// appendGlyph draws the character of c: a blank for a blank cell, or for the
// right half of a wide character without its left half.
func appendGlyph(b []byte, c cell) []byte {
	if c.r == 0 || c.width == 0 {
		return append(b, ' ')
	}
	b = utf8.AppendRune(b, c.r)
	return append(b, c.comb...)
}

// appendModes sets the modes the program changed from their defaults, other
// than the cursor visibility, the alternate screen and origin mode.
func (t *Terminal) appendModes(b []byte) []byte {
	m := &t.modes
	if m.insert {
		b = appendModes(b, false, true, 4)
	}
	if m.newline {
		b = appendModes(b, false, true, 20)
	}
	for _, pm := range [...]struct {
		mode int
		set  bool
	}{
		{1, m.appCursor},
		{5, m.reverseVideo},
		{12, m.cursorBlink},
		{1004, m.focusEvents},
		{1007, m.altScroll},
		{2004, m.bracketedPaste},
	} {
		if pm.set {
			b = appendModes(b, true, true, pm.mode)
		}
	}
	if !m.autowrap {
		b = appendModes(b, true, false, 7)
	}
	if m.mouse != 0 {
		b = appendModes(b, true, true, m.mouse)
	}
	for _, enc := range mouseEncodings(m) {
		b = appendModes(b, true, true, enc)
	}
	if m.appKeypad {
		b = append(b, "\x1b="...)
	}
	if m.cursorStyle != 0 {
		b = append(b, "\x1b["...)
		b = strconv.AppendInt(b, int64(m.cursorStyle), 10)
		b = append(b, " q"...)
	}
	return b
}

// mouseEncodings returns the extended mouse coordinate encodings set in m.
func mouseEncodings(m *modes) []int {
	var encs []int
	for _, enc := range mouseEncodingModes {
		if m.mouseEncodings&mouseEncodingBit(enc) != 0 {
			encs = append(encs, enc)
		}
	}
	return encs
}

// Leave returns dst with the bytes appended that undo, on an attached
// terminal about to be detached, what the program set up: they show the
// main screen again, reset the modes the program set and the title Redraw
// saved, and move the cursor to the line after the last one in use, for the
// shell to go on from there.
func (t *Terminal) Leave(dst []byte) []byte {
	b := append(dst, "\x1b[?2026l"...)
	main := &t.screens[0]
	cursorY := t.y
	if t.alt {
		b = appendModes(b, true, false, t.altMode)
		if t.altMode == 1049 && main.hasSaved {
			// Leaving restores the cursor saved when the program switched.
			cursorY = main.saved.y
		}
	}

	m := &t.modes
	if m.insert {
		b = appendModes(b, false, false, 4)
	}
	if m.newline {
		b = appendModes(b, false, false, 20)
	}
	for _, pm := range [...]struct {
		mode int
		set  bool
	}{
		{1, m.appCursor},
		{5, m.reverseVideo},
		{12, m.cursorBlink},
		{1004, m.focusEvents},
		{1007, m.altScroll},
		{2004, m.bracketedPaste},
	} {
		if pm.set {
			b = appendModes(b, true, false, pm.mode)
		}
	}
	if !m.autowrap {
		b = appendModes(b, true, true, 7)
	}
	if m.mouse != 0 {
		b = appendModes(b, true, false, m.mouse)
	}
	for _, enc := range mouseEncodings(m) {
		b = appendModes(b, true, false, enc)
	}
	if m.appKeypad {
		b = append(b, "\x1b>"...)
	}
	if m.cursorStyle != 0 {
		b = append(b, "\x1b[0 q"...)
	}
	b = append(b, "\x1b[r\x1b[?6l"...)
	b = append(b, resetDrawing...)
	b = append(b, "\x1b[?25h"...)

	row := max(lastUsedRow(main), cursorY) + 1
	if row < t.rows {
		b = appendCUP(b, row, 0)
	} else {
		b = appendCUP(b, t.rows-1, 0)
		b = append(b, "\r\n"...)
	}
	return append(b, "\x1b[23;0t"...)
}

// lastUsedRow returns the last row of s that is not blank, or -1.
func lastUsedRow(s *screen) int {
	for y := len(s.lines) - 1; y >= 0; y-- {
		if end, bg := trailingBlank(&s.lines[y]); end > 0 || bg != 0 {
			return y
		}
	}
	return -1
}

// Text returns what the screen shows as text: every line, with the spaces at
// its end trimmed, followed by a newline.
func (t *Terminal) Text() string {
	var sb strings.Builder
	var lb []byte
	for _, l := range t.screen().lines {
		lb = lb[:0]
		for _, c := range l.cells {
			switch {
			case c.width == 0:
			case c.r == 0:
				lb = append(lb, ' ')
			default:
				lb = utf8.AppendRune(lb, c.r)
				lb = append(lb, c.comb...)
			}
		}
		sb.Write(bytes.TrimRight(lb, " "))
		sb.WriteByte('\n')
	}
	return sb.String()
}

func appendCUP(b []byte, y, x int) []byte {
	b = append(b, "\x1b["...)
	b = strconv.AppendInt(b, int64(y+1), 10)
	if x > 0 {
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(x+1), 10)
	}
	return append(b, 'H')
}

// savedCursor returns what restoring the cursor on s restores. Without a
// cursor saved, that is the home position with the default attributes, so
// saving that instead on an attached terminal makes restoring do the same
// there, whatever it saved before.
func (s *screen) savedCursor() savedCursor {
	if !s.hasSaved {
		return savedCursor{}
	}
	return s.saved
}
