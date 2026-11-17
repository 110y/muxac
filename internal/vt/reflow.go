package vt

// place is a place on the main screen that stays on the same character
// across a reflow: the cursor's or the saved cursor's.
type place struct {
	x, y        int
	pendingWrap bool
}

// reflowMain gives the main screen cols columns and rows rows, rewrapping its
// lines at the new width rather than cutting them, as tmux and the terminals
// that rewrap lines do: a row that wrapped is joined again with the rows its
// line goes on on, and the line is wrapped anew. The content stays at the
// bottom of the screen. The rows that no longer fit go first from the blanks
// at the bottom, then into the history from the top, as they would scroll
// off, while the room left at the top is filled with rows taken back from the
// history, joined to their lines again. The cursor, if the main screen is
// shown, and the saved cursor stay on the same characters. It returns whether
// the cursor then waits to wrap.
func (t *Terminal) reflowMain(cols, rows int) bool {
	s := &t.screens[0]
	var places []place
	cursor, saved := -1, -1
	if !t.alt {
		cursor = len(places)
		places = append(places, place{x: t.x, y: t.y, pendingWrap: t.pendingWrap})
	}
	if s.hasSaved {
		saved = len(places)
		places = append(places, place{x: s.saved.x, y: s.saved.y, pendingWrap: s.saved.pendingWrap})
	}

	src := s.lines
	taken := 0
	floor := t.history.pushed
	var out []line
	var at []place
	for {
		shifted := make([]place, len(places))
		for i, p := range places {
			shifted[i] = place{x: p.x, y: p.y + taken, pendingWrap: p.pendingWrap}
		}
		out, at = rewrap(src, shifted, cols)
		// Rows are taken back as long as there is room, and then as long as
		// the line of the first row starts in the history. Taking them back is
		// bounded, as a row may join a line without taking a row of the
		// screen.
		full := len(out) >= rows && (taken == 0 || !t.history.continuedBy(&src[0]))
		if full || taken >= 2*rows+len(s.lines) {
			break
		}
		l, ok := t.history.pop()
		if !ok {
			break
		}
		src = append([]line{l}, src...)
		taken++
		floor = min(floor, t.history.pushed)
	}
	if taken > 0 {
		t.history.rewound(floor)
	}

	// The blank rows at the bottom, below the places, go first.
	lowest := -1
	topmost := len(out)
	for _, p := range at {
		lowest = max(lowest, p.y)
		topmost = min(topmost, p.y)
	}
	for len(out) > rows && len(out)-1 > lowest && out[len(out)-1].isBlank() {
		out = out[:len(out)-1]
	}
	// Then the rows at the top go into the history, as far as the places
	// stay on the screen, and finally the rows at the bottom.
	if n := min(len(out)-rows, topmost); n > 0 {
		for y := range n {
			t.history.push(&out[y])
		}
		out = out[n:]
		for i := range at {
			at[i].y -= n
		}
	}
	if len(out) > rows {
		out = out[:rows]
	}
	for len(out) < rows {
		out = append(out, newLine(cols))
	}
	s.lines = out

	pendingWrap := false
	if cursor >= 0 {
		p := at[cursor]
		t.x, t.y = clamp(p.x, 0, cols-1), clamp(p.y, 0, rows-1)
		pendingWrap = p.pendingWrap
	}
	if saved >= 0 {
		p := at[saved]
		s.saved.x, s.saved.y = clamp(p.x, 0, cols-1), clamp(p.y, 0, rows-1)
		s.saved.pendingWrap = p.pendingWrap && s.saved.x == cols-1
	}
	return pendingWrap
}

// rewrap wraps the lines of src at cols columns, and returns the rows they
// take along with where the places at on src end up on them.
func rewrap(src []line, at []place, cols int) ([]line, []place) {
	var out []line
	res := make([]place, len(at))
	for y := 0; y < len(src); {
		end := y
		for end < len(src)-1 && src[end].continuedBy(&src[end+1]) {
			end++
		}
		cells, fill, offsets := joinRows(src[y:end+1], at, y)
		rows, places := wrapCells(cells, fill, cols, offsets)
		for i, p := range places {
			if offsets[i].column >= 0 {
				res[i] = place{x: p.x, y: len(out) + p.y, pendingWrap: p.pendingWrap}
			}
		}
		out = append(out, rows...)
		y = end + 1
	}
	return out, res
}

// offset is where a place is on a line, as a column of the line from its
// start, -1 if the place is on another line.
type offset struct {
	column      int
	pendingWrap bool
}

// joinRows joins the rows of a line, which wrapped but the last one, into
// the cells of the line, without the blanks that end it, and returns them
// along with the background of those blanks and where the places at, on the
// rows numbered from y0, are on the line.
func joinRows(rows []line, at []place, y0 int) ([]cell, color, []offset) {
	var cells []cell
	var fill color
	starts := make([]int, len(rows))
	for r := range rows {
		l := &rows[r]
		starts[r] = len(cells)
		if r < len(rows)-1 {
			// A wide character that did not fit at the end of a row went on
			// to the next one, leaving the last cell blank.
			cells = append(cells, l.cells[:l.textEnd()]...)
			continue
		}
		end, bg := trailingBlank(l)
		if r > 0 {
			// The line wrapped to this row, which takes at least its first
			// cell, blank or not, as the line does on the screen.
			end = max(end, 1)
		}
		cells = append(cells, l.cells[:end]...)
		fill = bg
	}
	offsets := make([]offset, len(at))
	for i, p := range at {
		r := p.y - y0
		if r < 0 || r >= len(rows) {
			offsets[i] = offset{column: -1}
			continue
		}
		column := starts[r] + p.x
		if p.pendingWrap {
			// The cursor waits at the last column: the next character goes
			// after it.
			column++
		}
		offsets[i] = offset{column: column, pendingWrap: p.pendingWrap}
	}
	return cells, fill, offsets
}

// wrapCells wraps the cells of a line at cols columns, filling the rest of
// its last row with the background fill, and returns its rows along with
// where the places at offsets end up on them. A line has as many rows as the
// places on it need.
func wrapCells(cells []cell, fill color, cols int, offsets []offset) ([]line, []place) {
	var rows []line
	places := make([]place, len(offsets))
	placed := make([]bool, len(offsets))
	row := make([]cell, 0, cols)
	newRow := func(wrapped bool, bg color) {
		// A row that wraps before its end does for a wide character.
		padded := wrapped && len(row) < cols
		for len(row) < cols {
			row = append(row, blank(bg))
		}
		rows = append(rows, line{cells: row, wrapped: wrapped, padded: padded})
		row = make([]cell, 0, cols)
	}

	i := 0
	for i < len(cells) {
		c := cells[i]
		w := 1
		switch c.width {
		case 2:
			w = 2
		case 0:
			// A right half without its left half is drawn as a blank.
			c = blank(c.style.bg)
		}
		if len(row)+w > cols && len(row) > 0 {
			newRow(true, 0)
		}
		for j, o := range offsets {
			if !placed[j] && o.column >= i && o.column < i+w {
				places[j] = place{x: min(len(row)+o.column-i, cols-1), y: len(rows)}
				placed[j] = true
			}
		}
		row = append(row, c)
		if w == 2 && len(row) < cols {
			row = append(row, cell{style: c.style})
		}
		i += w
	}

	// The places after the cells.
	end := len(rows)
	x := len(row)
	for j, o := range offsets {
		if placed[j] || o.column < 0 {
			continue
		}
		at := x + o.column - max(i, len(cells))
		if at > 0 && at%cols == 0 {
			// Right after the last column, the cursor waits to wrap there.
			places[j] = place{x: cols - 1, y: end + at/cols - 1, pendingWrap: true}
		} else {
			places[j] = place{x: at % cols, y: end + at/cols}
		}
		placed[j] = true
	}
	newRow(false, fill)
	last := len(rows) - 1
	for j := range places {
		if offsets[j].column >= 0 {
			last = max(last, places[j].y)
		}
	}
	for len(rows) <= last {
		rows = append(rows, newLine(cols))
	}
	return rows, places
}
