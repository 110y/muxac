// Package vt is a virtual terminal: it keeps track of the screen of a program
// running on a pseudo-terminal the way a terminal emulator would, so that
// muxac can draw that screen again on a terminal that attaches to the session
// later, tell what is shown on it, and answer the questions the program asks
// its terminal even while no terminal is attached.
//
// A Terminal does not draw anything itself. The program's output it is fed is
// passed on unchanged to the attached terminals, except for the sequences only
// a single, stable terminal can deal with: queries, which the Terminal answers
// itself, and settings it does not keep track of, such as the kitty keyboard
// protocol, which it drops so that the program never comes to depend on them.
// The attached terminals therefore always show what the Terminal holds.
package vt

import (
	"slices"
	"unicode/utf8"
)

// Limits on what the parser keeps of a sequence.
const (
	maxParams     = 32
	maxParamValue = 65535
	// maxStringBytes bounds an OSC or DCS string. Longer ones, such as large
	// images, are dropped.
	maxStringBytes = 1 << 20
)

type parserState uint8

const (
	stateGround parserState = iota
	stateEscape
	stateCSI
	stateCSIIgnore
	stateOSC
	stateDCS
	stateDCSData
	stateIgnoreString
)

// parser holds the escape sequence being read.
type parser struct {
	state parserState
	// seq holds the bytes of the sequence read so far, to be passed on if the
	// sequence turns out to be one to forward.
	seq []byte
	// stringEscape is set after an ESC inside a string (OSC, DCS, SOS, PM or
	// APC), which is either the start of the string terminator or the end of
	// the string.
	stringEscape bool
	// overflow is set once a string has outgrown maxStringBytes.
	overflow bool
	// prefix is the private marker of a CSI or DCS sequence: one of "<=>?",
	// or 0.
	prefix byte
	inter  []byte
	// started is set once a parameter character has been read.
	started bool
	// vals are the parameters, and sub[i] tells whether vals[i] is a
	// subparameter: it follows a colon rather than a semicolon.
	vals [maxParams]int
	sub  [maxParams]bool
	n    int
	// final is the final character of a DCS sequence's introducer.
	final byte
	data  []byte

	// utf8 decoding state
	utf8Need int
	utf8Rune rune
	utf8Min  rune
}

func (p *parser) begin(state parserState) {
	p.state = state
	p.prefix = 0
	p.inter = p.inter[:0]
	p.started = false
	p.n = 0
	p.final = 0
	p.data = p.data[:0]
	p.stringEscape = false
	p.overflow = false
}

// param adds a parameter character to the sequence.
func (p *parser) param(b byte) {
	if !p.started {
		p.started = true
		p.n = 1
		p.vals[0] = 0
		p.sub[0] = false
	}
	switch b {
	case ';', ':':
		if p.n < maxParams {
			p.vals[p.n] = 0
			p.sub[p.n] = b == ':'
			p.n++
		}
	default:
		if p.n <= maxParams {
			v := p.vals[p.n-1]*10 + int(b-'0')
			p.vals[p.n-1] = min(v, maxParamValue)
		}
	}
}

// arg returns the i-th parameter, not counting subparameters, or def if it is
// missing or zero.
func (p *parser) arg(i, def int) int {
	k := -1
	for j := 0; j < p.n; j++ {
		if p.sub[j] {
			continue
		}
		k++
		if k == i {
			if p.vals[j] == 0 {
				return def
			}
			return p.vals[j]
		}
	}
	return def
}

// args returns the parameters, not counting subparameters.
func (p *parser) args() []int {
	var out []int
	for j := 0; j < p.n; j++ {
		if !p.sub[j] {
			out = append(out, p.vals[j])
		}
	}
	return out
}

// hasSubparams reports whether any parameter has subparameters.
func (p *parser) hasSubparams() bool {
	for j := 0; j < p.n; j++ {
		if p.sub[j] {
			return true
		}
	}
	return false
}

// modes are the terminal modes the Terminal keeps track of.
type modes struct {
	insert         bool // IRM
	newline        bool // LNM
	appCursor      bool // DECCKM
	reverseVideo   bool // DECSCNM
	origin         bool // DECOM
	autowrap       bool // DECAWM
	cursorBlink    bool // xterm's ATT610
	cursorHidden   bool // reset DECTCEM
	appKeypad      bool // DECKPAM, DECNKM
	focusEvents    bool // 1004
	altScroll      bool // 1007
	bracketedPaste bool // 2004
	syncOutput     bool // 2026
	// mouse is the mouse tracking mode: 9, 1000, 1002, 1003, or 0 for none.
	mouse int
	// mouseEncodings are the extended mouse coordinate encodings that are set,
	// as xterm keeps them: a bit for each of mouseEncodingModes.
	mouseEncodings uint8
	// cursorStyle is the DECSCUSR parameter.
	cursorStyle int
}

func defaultModes() modes {
	return modes{autowrap: true}
}

// mouseEncodingModes are the modes that select extended mouse coordinate
// encodings, which xterm lets be set together.
var mouseEncodingModes = [...]int{1005, 1006, 1015, 1016}

// mouseEncodingBit returns the bit of modes.mouseEncodings for the mode m.
func mouseEncodingBit(m int) uint8 {
	for i, e := range mouseEncodingModes {
		if e == m {
			return 1 << i
		}
	}
	return 0
}

// screen is a screen buffer: the main or the alternate screen.
type screen struct {
	lines []line
	// saved is the cursor saved by DECSC while this screen was shown.
	saved    savedCursor
	hasSaved bool
}

// savedCursor is the state DECSC saves.
type savedCursor struct {
	x, y        int
	pendingWrap bool
	pen         style
	charsets    [4]charset
	gl          int
	origin      bool
}

// Terminal is a virtual terminal. It is not safe for concurrent use.
type Terminal struct {
	cols, rows int
	// screens are the main screen and the alternate screen.
	screens [2]screen
	// alt is set while the alternate screen is shown.
	alt bool
	// altMode is the DEC private mode that switched to the alternate screen:
	// 47, 1047 or 1049.
	altMode int

	x, y int
	// pendingWrap is set when a character was written to the last column:
	// the cursor stays there, and the next character goes to the next line.
	pendingWrap bool
	pen         style
	charsets    [4]charset
	// gl is the character set invoked into GL (0 to 3).
	gl int
	// top and bottom are the scrolling margins, 0-based and inclusive.
	top, bottom int
	tabs        []bool
	modes       modes
	title       string
	// last is the last character written, for REP.
	last rune

	// colors are the replies to OSC 10 and 11 queries, such as
	// "rgb:ffff/ffff/ffff", or "" when unknown.
	colors [2]string
	// pixels is the size of the terminal in pixels, if known.
	pixelWidth, pixelHeight int
	// history holds the lines that went off the top of the main screen.
	history history

	reply func([]byte)
	name  string

	parser parser
	out    []byte
}

// Options configures a Terminal.
type Options struct {
	// Cols and Rows are the size of the screen.
	Cols, Rows int
	// Reply is called with the replies to the program's queries, which must
	// be written to the program's input. It is called during Feed.
	Reply func([]byte)
	// Name is the name and version reported to XTVERSION queries.
	Name string
	// HistoryLimit is how many of the lines that went off the top of the
	// main screen are kept, for Redraw to show them again.
	HistoryLimit int
}

// New returns a Terminal with a blank screen.
func New(opts Options) *Terminal {
	t := &Terminal{
		cols:    max(opts.Cols, 1),
		rows:    max(opts.Rows, 1),
		reply:   opts.Reply,
		name:    opts.Name,
		history: history{limit: opts.HistoryLimit},
	}
	t.reset()
	return t
}

// reset brings the terminal to its initial state, as RIS does. The size and
// what is known about the attached terminal are kept.
func (t *Terminal) reset() {
	for i := range t.screens {
		s := &t.screens[i]
		s.lines = make([]line, t.rows)
		for y := range s.lines {
			s.lines[y] = newLine(t.cols)
		}
		s.hasSaved = false
		s.saved = savedCursor{}
	}
	t.alt = false
	t.altMode = 0
	t.x, t.y = 0, 0
	t.pendingWrap = false
	t.pen = style{}
	t.charsets = [4]charset{}
	t.gl = 0
	t.top, t.bottom = 0, t.rows-1
	t.tabs = defaultTabs(t.cols)
	t.modes = defaultModes()
	t.title = ""
	t.last = 0
}

func defaultTabs(cols int) []bool {
	tabs := make([]bool, cols)
	for x := 8; x < cols; x += 8 {
		tabs[x] = true
	}
	return tabs
}

// Size returns the size of the screen.
func (t *Terminal) Size() (cols, rows int) {
	return t.cols, t.rows
}

// SetColors sets the default foreground and background colors of the
// attached terminal, as it reports them to OSC 10 and 11 queries (for example
// "rgb:1e1e/1e1e/2e2e"), so that the program's queries can be answered. An
// empty string leaves the color unknown, and queries for it unanswered.
func (t *Terminal) SetColors(fg, bg string) {
	t.colors = [2]string{fg, bg}
}

// SetPixelSize sets the size of the attached terminal's text area in pixels,
// or 0 if unknown.
func (t *Terminal) SetPixelSize(width, height int) {
	t.pixelWidth, t.pixelHeight = width, height
}

func (t *Terminal) screen() *screen {
	if t.alt {
		return &t.screens[1]
	}
	return &t.screens[0]
}

func (t *Terminal) line(y int) *line {
	return &t.screen().lines[y]
}

func (t *Terminal) sendReply(s string) {
	if t.reply != nil {
		t.reply([]byte(s))
	}
}

// Feed processes the output p of the program and returns dst with the bytes
// to pass on to the attached terminals appended. A sequence may be split
// across any number of calls.
func (t *Terminal) Feed(dst, p []byte) []byte {
	t.out = dst
	for i := 0; i < len(p); i++ {
		b := p[i]
		switch t.parser.state {
		case stateGround:
			if b >= 0x20 && b < 0x7f && t.parser.utf8Need == 0 {
				// Fast path for runs of printable ASCII.
				j := i + 1
				for j < len(p) && p[j] >= 0x20 && p[j] < 0x7f {
					j++
				}
				t.out = append(t.out, p[i:j]...)
				for _, c := range p[i:j] {
					t.print(rune(c))
				}
				i = j - 1
				continue
			}
			t.ground(b)
		case stateEscape:
			t.escape(b)
		case stateCSI:
			t.csi(b)
		case stateCSIIgnore:
			t.csiIgnore(b)
		case stateOSC, stateDCSData, stateIgnoreString:
			if t.stringByte(b) {
				i-- // the string ended without a terminator: read b again
			}
		case stateDCS:
			t.dcs(b)
		}
	}
	dst = t.out
	t.out = nil
	return dst
}

func (t *Terminal) ground(b byte) {
	if b < 0x20 || b == 0x7f {
		t.flushUTF8()
		t.control(b)
		return
	}
	t.out = append(t.out, b)
	if b < 0x80 {
		t.flushUTF8()
		t.print(rune(b))
		return
	}
	t.decodeUTF8(b)
}

// flushUTF8 drops an incomplete UTF-8 sequence, showing it as U+FFFD.
func (t *Terminal) flushUTF8() {
	if t.parser.utf8Need > 0 {
		t.parser.utf8Need = 0
		t.print(utf8.RuneError)
	}
}

func (t *Terminal) decodeUTF8(b byte) {
	p := &t.parser
	if p.utf8Need > 0 {
		if b&0xc0 == 0x80 {
			p.utf8Rune = p.utf8Rune<<6 | rune(b&0x3f)
			p.utf8Need--
			if p.utf8Need == 0 {
				r := p.utf8Rune
				if r < p.utf8Min || r > utf8.MaxRune || (r >= 0xd800 && r <= 0xdfff) {
					r = utf8.RuneError
				}
				t.print(r)
			}
			return
		}
		t.flushUTF8()
	}
	switch {
	case b&0xe0 == 0xc0:
		p.utf8Need, p.utf8Rune, p.utf8Min = 1, rune(b&0x1f), 0x80
	case b&0xf0 == 0xe0:
		p.utf8Need, p.utf8Rune, p.utf8Min = 2, rune(b&0x0f), 0x800
	case b&0xf8 == 0xf0:
		p.utf8Need, p.utf8Rune, p.utf8Min = 3, rune(b&0x07), 0x10000
	default:
		t.print(utf8.RuneError)
	}
}

// control executes a C0 control character.
func (t *Terminal) control(b byte) {
	switch b {
	case 0x07: // BEL
		t.out = append(t.out, b)
	case 0x08: // BS
		t.out = append(t.out, b)
		t.backspace()
	case 0x09: // HT
		t.out = append(t.out, b)
		t.tab(1)
	case 0x0a, 0x0b, 0x0c: // LF, VT, FF
		t.out = append(t.out, b)
		t.linefeed()
		if t.modes.newline {
			t.carriageReturn()
		}
	case 0x0d: // CR
		t.out = append(t.out, b)
		t.carriageReturn()
	case 0x0e: // SO
		t.out = append(t.out, b)
		t.gl = 1
	case 0x0f: // SI
		t.out = append(t.out, b)
		t.gl = 0
	case 0x1b: // ESC
		t.parser.begin(stateEscape)
		t.parser.seq = append(t.parser.seq[:0], b)
	case 0x18, 0x1a: // CAN, SUB
		t.parser.state = stateGround
	}
}

func (t *Terminal) escape(b byte) {
	p := &t.parser
	if b < 0x20 {
		// A control inside a sequence is executed as it comes. ESC starts a
		// new sequence, CAN and SUB cancel this one; the others leave it going on.
		t.control(b)
		return
	}
	if b == 0x7f {
		return
	}
	p.seq = append(p.seq, b)
	switch {
	case b <= 0x2f:
		p.inter = append(p.inter, b)
	case len(p.inter) == 0 && b == '[':
		p.begin(stateCSI)
	case len(p.inter) == 0 && b == ']':
		p.begin(stateOSC)
	case len(p.inter) == 0 && b == 'P':
		p.begin(stateDCS)
	case len(p.inter) == 0 && (b == 'X' || b == '^' || b == '_'):
		p.begin(stateIgnoreString)
	default:
		p.state = stateGround
		t.escDispatch(b)
	}
}

func (t *Terminal) csi(b byte) {
	p := &t.parser
	if b < 0x20 {
		// A control inside a sequence is executed as it comes. ESC starts a
		// new sequence, CAN and SUB cancel this one; the others leave it going on.
		t.control(b)
		return
	}
	if b == 0x7f {
		return
	}
	p.seq = append(p.seq, b)
	switch {
	case b >= '0' && b <= ';':
		if len(p.inter) > 0 {
			p.state = stateCSIIgnore
			return
		}
		p.param(b)
	case b >= '<' && b <= '?':
		if p.started || p.prefix != 0 || len(p.inter) > 0 {
			p.state = stateCSIIgnore
			return
		}
		p.prefix = b
	case b <= 0x2f:
		p.inter = append(p.inter, b)
	default:
		p.state = stateGround
		if len(p.inter) > 2 {
			return
		}
		t.csiDispatch(b)
	}
}

func (t *Terminal) csiIgnore(b byte) {
	if b < 0x20 {
		// A control inside a sequence is executed as it comes. ESC starts a
		// new sequence, CAN and SUB cancel this one; the others leave it going on.
		t.control(b)
		return
	}
	if b >= 0x40 && b <= 0x7e {
		t.parser.state = stateGround
	}
}

func (t *Terminal) dcs(b byte) {
	p := &t.parser
	if b < 0x20 {
		switch b {
		case 0x1b, 0x18, 0x1a:
			t.control(b)
		}
		return
	}
	if b == 0x7f {
		return
	}
	p.seq = append(p.seq, b)
	switch {
	case b >= '0' && b <= ';':
		p.param(b)
	case b >= '<' && b <= '?':
		p.prefix = b
	case b <= 0x2f:
		p.inter = append(p.inter, b)
	default:
		p.final = b
		p.state = stateDCSData
	}
}

// stringByte handles a byte of an OSC, DCS, SOS, PM or APC string. It
// reports whether the string ended without a terminator, in which case b
// must be processed again.
func (t *Terminal) stringByte(b byte) bool {
	p := &t.parser
	dcs := p.state == stateDCSData
	if p.stringEscape {
		p.stringEscape = false
		switch {
		case b == '\\':
			t.appendString(0x1b, false)
			t.appendString(b, false)
			t.endString("\x1b\\")
		case dcs:
			// Inside a DCS string an ESC not followed by "\" is part of the
			// data, as in tmux passthrough, which doubles the ESCs of the
			// sequences it wraps.
			t.appendString(0x1b, true)
			t.appendString(b, b != 0x1b)
		default:
			// Elsewhere it ends the string and starts another escape
			// sequence.
			t.endString("")
			t.control(0x1b)
			return true
		}
		return false
	}
	switch {
	case b == 0x1b:
		p.stringEscape = true
	case b == 0x18 || b == 0x1a:
		p.state = stateGround
	case b == 0x07 && !dcs:
		t.appendString(b, false)
		t.endString("\x07")
	default:
		t.appendString(b, b >= 0x20 || dcs)
	}
	return false
}

func (t *Terminal) appendString(b byte, data bool) {
	p := &t.parser
	if p.state == stateIgnoreString || p.overflow {
		return
	}
	if len(p.seq) >= maxStringBytes {
		p.overflow = true
		return
	}
	p.seq = append(p.seq, b)
	if data {
		p.data = append(p.data, b)
	}
}

// endString dispatches the string that ended with terminator, which is "" if
// it ended without one.
func (t *Terminal) endString(terminator string) {
	p := &t.parser
	state := p.state
	p.state = stateGround
	if p.overflow {
		return
	}
	switch state {
	case stateOSC:
		t.oscDispatch(terminator)
	case stateDCSData:
		t.dcsDispatch()
	case stateIgnoreString:
		// SOS, PM and APC strings are dropped.
	case stateGround, stateEscape, stateCSI, stateCSIIgnore, stateDCS:
		// Not in a string.
	}
}

// forward passes the current sequence on to the attached terminals.
func (t *Terminal) forward() {
	t.out = append(t.out, t.parser.seq...)
}

// Resize changes the size of the screen. The lines of the main screen are
// rewrapped at the new width, as terminals that rewrap them do (see
// reflowMain). The lines of the alternate screen, which its program draws
// anew, are cut or extended on the right instead; when it gets shorter, its
// lines go away at the bottom, unless the cursor would then be off the
// screen: they then go away at the top, as far as needed to keep the cursor
// on the screen.
func (t *Terminal) Resize(cols, rows int) {
	cols, rows = max(cols, 1), max(rows, 1)
	if cols == t.cols && rows == t.rows {
		return
	}
	pendingWrap := t.reflowMain(cols, rows)

	s := &t.screens[1]
	// keep is the row that must stay on the screen: the cursor's if the
	// alternate screen is shown, the saved cursor's otherwise.
	keep := -1
	if t.alt {
		keep = t.y
	} else if s.hasSaved {
		keep = s.saved.y
	}
	if rows < len(s.lines) {
		shift := max(keep-rows+1, 0)
		s.lines = slices.Clone(s.lines[shift : shift+rows])
		if t.alt {
			t.y -= shift
		}
		if s.hasSaved {
			s.saved.y = max(s.saved.y-shift, 0)
		}
	}
	for len(s.lines) < rows {
		s.lines = append(s.lines, newLine(cols))
	}
	for y := range s.lines {
		s.lines[y].resize(cols)
	}
	s.saved.x = min(s.saved.x, cols-1)
	s.saved.y = min(s.saved.y, rows-1)

	tabs := defaultTabs(cols)
	copy(tabs, t.tabs)
	t.tabs = tabs
	t.cols, t.rows = cols, rows
	t.x = min(t.x, cols-1)
	t.y = min(t.y, rows-1)
	t.pendingWrap = pendingWrap && !t.alt && t.x == cols-1
	t.top, t.bottom = 0, rows-1
}
