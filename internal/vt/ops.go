package vt

// maxCombining bounds the bytes kept in a cell besides its main character.
const maxCombining = 64

// print writes a character at the cursor, translated through the character
// set invoked into GL.
func (t *Terminal) print(r rune) {
	if r < 0x80 {
		r = t.charsets[t.gl].translate(r)
	}
	t.printRune(r)
}

func (t *Terminal) printRune(r rune) {
	if r >= 0x80 && t.extendPrevious(r) {
		return
	}
	w := runeWidth(r)
	if w == 0 {
		// A zero-width character that continues nothing shows nothing.
		return
	}
	if w == 2 && t.cols < 2 {
		w = 1
	}

	if t.pendingWrap {
		if t.modes.autowrap {
			t.line(t.y).wrap(false)
			t.x = 0
			t.linefeed()
		}
		t.pendingWrap = false
	}
	if w == 2 && t.x == t.cols-1 {
		if t.modes.autowrap {
			// A wide character does not fit in the last column: it goes to
			// the next line, leaving the column blank.
			l := t.line(t.y)
			t.setCell(l, t.x, blank(t.pen.bg))
			l.wrap(true)
			t.x = 0
			t.linefeed()
		} else {
			t.x = t.cols - 2
		}
	}

	if t.modes.insert {
		t.insertCells(t.x, w)
	}
	t.setCell(t.line(t.y), t.x, cell{r: r, width: uint8(w), style: t.pen})
	t.last = r
	t.advance(t.x, w)
}

// advance moves the cursor past a character of width w written at column x.
func (t *Terminal) advance(x, w int) {
	if x+w >= t.cols {
		t.x = t.cols - 1
		t.pendingWrap = t.modes.autowrap
		return
	}
	t.x = x + w
}

// previousCell returns the column of the character written last before the
// cursor, or -1 if there is none.
func (t *Terminal) previousCell() int {
	x := t.x
	if !t.pendingWrap {
		if x == 0 {
			return -1
		}
		x--
	}
	l := t.line(t.y)
	if l.cells[x].width == 0 && x > 0 && l.cells[x-1].width == 2 {
		x--
	}
	if l.cells[x].r == 0 || l.cells[x].width == 0 {
		return -1
	}
	return x
}

// extendPrevious adds r to the character before the cursor if r continues
// its grapheme cluster, as a combining mark, a variation selector or the
// next part of an emoji sequence does, the way the programs measure text.
// A cluster that gets wide, such as a symbol followed by VS16, then takes
// two columns. It reports whether r was added.
func (t *Terminal) extendPrevious(r rune) bool {
	x := t.previousCell()
	if x < 0 {
		return false
	}
	l := t.line(t.y)
	c := &l.cells[x]
	cluster := string(c.r) + c.comb
	if !extendsCluster(cluster, r) {
		return false
	}
	if len(c.comb) >= maxCombining {
		return true
	}
	c.comb += string(r)
	if c.width == 1 && x+1 < t.cols && clusterWidth(cluster+string(r)) == 2 {
		wide := *c
		wide.width = 2
		t.setCell(l, x, wide)
		t.advance(x, 2)
	}
	return true
}

// setCell writes c to column x of l, blanking the other half of any wide
// character it partly overwrites.
func (t *Terminal) setCell(l *line, x int, c cell) {
	cells := l.cells
	if cells[x].width == 0 && x > 0 {
		cells[x-1] = blank(cells[x-1].style.bg)
	}
	if cells[x].width == 2 && x+1 < len(cells) {
		cells[x+1] = blank(cells[x+1].style.bg)
	}
	cells[x] = c
	if c.width == 2 && x+1 < len(cells) {
		if cells[x+1].width == 2 && x+2 < len(cells) {
			cells[x+2] = blank(cells[x+2].style.bg)
		}
		cells[x+1] = cell{style: c.style}
	}
	// What the last cell holds now is no padding.
	if x+max(int(c.width), 1) >= len(cells) {
		l.padded = false
	}
}

// fixWide blanks the halves of wide characters cut at column x of l: a right
// half at x, or a left half just before it.
func fixWide(l *line, x int) {
	if x <= 0 || x >= len(l.cells) {
		return
	}
	if l.cells[x].width == 0 {
		l.cells[x-1] = blank(l.cells[x-1].style.bg)
		l.cells[x] = blank(l.cells[x].style.bg)
	}
}

func (t *Terminal) linefeed() {
	t.pendingWrap = false
	switch {
	case t.y == t.bottom:
		t.saveLines(t.top, t.top+1)
		t.scrollUp(t.top, t.bottom, 1)
	case t.y < t.rows-1:
		t.y++
	}
}

func (t *Terminal) reverseIndex() {
	t.pendingWrap = false
	switch {
	case t.y == t.top:
		t.scrollDown(t.top, t.bottom, 1)
	case t.y > 0:
		t.y--
	}
}

func (t *Terminal) carriageReturn() {
	t.x = 0
	t.pendingWrap = false
}

func (t *Terminal) backspace() {
	t.pendingWrap = false
	if t.x > 0 {
		t.x--
	}
}

// tab moves the cursor n tab stops forward, or backward if n is negative.
func (t *Terminal) tab(n int) {
	t.pendingWrap = false
	for ; n > 0 && t.x < t.cols-1; n-- {
		t.x++
		for t.x < t.cols-1 && !t.tabs[t.x] {
			t.x++
		}
	}
	for ; n < 0 && t.x > 0; n++ {
		t.x--
		for t.x > 0 && !t.tabs[t.x] {
			t.x--
		}
	}
}

// scrollUp scrolls the lines in [top, bottom] up by n, blanking the lines
// that appear at the bottom. The line above top, unless top is the first
// line, no longer goes on on the line at top, which went off.
func (t *Terminal) scrollUp(top, bottom, n int) {
	n = min(n, bottom-top+1)
	if n <= 0 {
		return
	}
	lines := t.screen().lines
	rotate(lines[top:bottom+1], n)
	for y := bottom - n + 1; y <= bottom; y++ {
		lines[y].clear(0, t.cols, t.pen.bg)
		lines[y].wrapped = false
	}
	if top > 0 {
		lines[top-1].unwrap()
	}
}

// scrollDown scrolls the lines in [top, bottom] down by n, blanking the lines
// that appear at the top. Neither the line above top, which may be the newest
// row of the history, nor the one at bottom goes on on the line after it any
// longer: the line each went on on moved down, or off.
func (t *Terminal) scrollDown(top, bottom, n int) {
	n = min(n, bottom-top+1)
	if n <= 0 {
		return
	}
	lines := t.screen().lines
	rotate(lines[top:bottom+1], bottom-top+1-n)
	for y := top; y < top+n; y++ {
		lines[y].clear(0, t.cols, t.pen.bg)
		lines[y].wrapped = false
	}
	t.unwrapAbove(top)
	lines[bottom].unwrap()
}

// unwrapAbove marks the line above line y of the screen shown as no longer
// going on on it: the newest row of the history if y is the first line of the
// main screen.
func (t *Terminal) unwrapAbove(y int) {
	switch {
	case y > 0:
		t.screen().lines[y-1].unwrap()
	case !t.alt:
		t.history.unwrap()
	}
}

// rotate moves the first n lines of ls to its end.
func rotate(ls []line, n int) {
	if n <= 0 || n >= len(ls) {
		return
	}
	reverse(ls[:n])
	reverse(ls[n:])
	reverse(ls)
}

func reverse(ls []line) {
	for i, j := 0, len(ls)-1; i < j; i, j = i+1, j-1 {
		ls[i], ls[j] = ls[j], ls[i]
	}
}

// moveTo moves the cursor to column x and row y, which are relative to the
// scrolling region in origin mode.
func (t *Terminal) moveTo(x, y int) {
	t.pendingWrap = false
	t.x = clamp(x, 0, t.cols-1)
	if t.modes.origin {
		t.y = clamp(y+t.top, t.top, t.bottom)
	} else {
		t.y = clamp(y, 0, t.rows-1)
	}
}

// moveUp moves the cursor up by n rows, stopping at the top margin if it
// starts below it.
func (t *Terminal) moveUp(n int) {
	t.pendingWrap = false
	limit := 0
	if t.y >= t.top {
		limit = t.top
	}
	t.y = max(t.y-n, limit)
}

// moveDown moves the cursor down by n rows, stopping at the bottom margin if
// it starts above it.
func (t *Terminal) moveDown(n int) {
	t.pendingWrap = false
	limit := t.rows - 1
	if t.y <= t.bottom {
		limit = t.bottom
	}
	t.y = min(t.y+n, limit)
}

func (t *Terminal) moveColumn(x int) {
	t.pendingWrap = false
	t.x = clamp(x, 0, t.cols-1)
}

// insertCells inserts n blank cells at column x of the cursor line, moving the
// rest of the line right.
func (t *Terminal) insertCells(x, n int) {
	l := t.line(t.y)
	n = min(n, t.cols-x)
	fixWide(l, x)
	copy(l.cells[x+n:], l.cells[x:t.cols-n])
	l.clear(x, x+n, t.pen.bg)
	if l.cells[t.cols-1].width == 2 {
		l.cells[t.cols-1] = blank(l.cells[t.cols-1].style.bg)
	}
	l.padded = false
}

// deleteCells deletes n cells at the cursor, moving the rest of the line left.
func (t *Terminal) deleteCells(n int) {
	t.pendingWrap = false
	l := t.line(t.y)
	x := t.x
	n = min(n, t.cols-x)
	fixWide(l, x)
	fixWide(l, x+n)
	copy(l.cells[x:], l.cells[x+n:])
	l.clear(t.cols-n, t.cols, t.pen.bg)
	l.wrapped, l.padded = false, false
}

// eraseCells blanks n cells at the cursor.
func (t *Terminal) eraseCells(n int) {
	t.pendingWrap = false
	l := t.line(t.y)
	end := min(t.x+n, t.cols)
	fixWide(l, t.x)
	fixWide(l, end)
	l.clear(t.x, end, t.pen.bg)
}

// eraseInLine erases part of the cursor line: from the cursor to its end (0),
// from its start to the cursor (1), or all of it (2).
func (t *Terminal) eraseInLine(mode int) {
	t.pendingWrap = false
	l := t.line(t.y)
	switch mode {
	case 0:
		fixWide(l, t.x)
		l.clear(t.x, t.cols, t.pen.bg)
		l.wrapped = false
	case 1:
		fixWide(l, t.x+1)
		l.clear(0, t.x+1, t.pen.bg)
	case 2:
		l.clear(0, t.cols, t.pen.bg)
		l.wrapped = false
	}
}

// eraseInDisplay erases part of the screen: from the cursor to its end (0),
// from its start to the cursor (1), or all of it (2), or the history (3).
// Erasing the main screen first keeps what it shows in the history, as tmux
// does.
func (t *Terminal) eraseInDisplay(mode int) {
	lines := t.screen().lines
	switch mode {
	case 3:
		t.history.clear()
	case 0:
		t.eraseInLine(0)
		for y := t.y + 1; y < t.rows; y++ {
			lines[y].clear(0, t.cols, t.pen.bg)
			lines[y].wrapped = false
		}
	case 1:
		t.eraseInLine(1)
		for y := range t.y {
			lines[y].clear(0, t.cols, t.pen.bg)
			lines[y].wrapped = false
		}
	case 2:
		t.pendingWrap = false
		t.saveLines(0, lastUsedRow(t.screen())+1)
		for y := range lines {
			lines[y].clear(0, t.cols, t.pen.bg)
			lines[y].wrapped = false
		}
	}
}

// insertLines inserts n blank lines at the cursor line, within the scrolling
// region.
func (t *Terminal) insertLines(n int) {
	if t.y < t.top || t.y > t.bottom {
		return
	}
	t.scrollDown(t.y, t.bottom, n)
	t.carriageReturn()
}

// deleteLines deletes n lines at the cursor line, within the scrolling
// region. Deleting the first line of the main screen, which no row of the
// history scrolled off for, the newest row of the history no longer goes on
// on it either.
func (t *Terminal) deleteLines(n int) {
	if t.y < t.top || t.y > t.bottom {
		return
	}
	t.scrollUp(t.y, t.bottom, n)
	if t.y == 0 {
		t.unwrapAbove(0)
	}
	t.carriageReturn()
}

// repeat writes the last character written n more times (REP).
func (t *Terminal) repeat(n int) {
	if t.last == 0 {
		return
	}
	for range min(n, t.cols*t.rows) {
		t.printRune(t.last)
	}
}

func (t *Terminal) saveCursor() {
	s := t.screen()
	s.hasSaved = true
	s.saved = savedCursor{
		x:           t.x,
		y:           t.y,
		pendingWrap: t.pendingWrap,
		pen:         t.pen,
		charsets:    t.charsets,
		gl:          t.gl,
		origin:      t.modes.origin,
	}
}

func (t *Terminal) restoreCursor() {
	s := t.screen()
	if !s.hasSaved {
		// Restoring a cursor that was never saved homes it with default
		// attributes, as xterm does.
		t.pen = style{}
		t.charsets = [4]charset{}
		t.gl = 0
		t.modes.origin = false
		t.moveTo(0, 0)
		return
	}
	c := s.saved
	t.pen = c.pen
	t.charsets = c.charsets
	t.gl = c.gl
	t.modes.origin = c.origin
	t.x = clamp(c.x, 0, t.cols-1)
	t.y = clamp(c.y, 0, t.rows-1)
	t.pendingWrap = c.pendingWrap && t.x == t.cols-1
}

// switchScreen shows the alternate screen if alt is set and the main screen
// otherwise.
func (t *Terminal) switchScreen(alt bool) {
	t.alt = alt
	t.pendingWrap = false
}

// clearAlternate blanks the alternate screen.
func (t *Terminal) clearAlternate() {
	for y := range t.screens[1].lines {
		t.screens[1].lines[y].clear(0, t.cols, t.pen.bg)
		t.screens[1].lines[y].wrapped = false
	}
}

// alignmentTest fills the screen with "E" (DECALN).
func (t *Terminal) alignmentTest() {
	t.top, t.bottom = 0, t.rows-1
	t.modes.origin = false
	for y := range t.screen().lines {
		l := &t.screen().lines[y]
		for x := range l.cells {
			l.cells[x] = cell{r: 'E', width: 1}
		}
		l.wrapped = false
	}
	t.moveTo(0, 0)
}

// softReset performs DECSTR.
func (t *Terminal) softReset() {
	t.modes.cursorHidden = false
	t.modes.insert = false
	t.modes.origin = false
	t.modes.autowrap = true
	t.modes.appKeypad = false
	t.modes.appCursor = false
	t.top, t.bottom = 0, t.rows-1
	t.charsets = [4]charset{}
	t.gl = 0
	t.pen = style{}
	t.screen().hasSaved = false
	t.pendingWrap = false
}

func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}
