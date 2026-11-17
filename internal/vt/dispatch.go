package vt

import (
	"fmt"
	"strconv"
	"strings"
)

// Replies to the device attributes queries.
const (
	// primaryDA identifies a VT220 with ANSI color (DA1).
	primaryDA = "\x1b[?62;22c"
	// secondaryDA identifies a VT220 with firmware version 10 (DA2).
	secondaryDA = "\x1b[>1;10;0c"
	// tertiaryDA reports a unit ID of zero (DA3).
	tertiaryDA = "\x1bP!|00000000\x1b\\"
)

// Mode states reported to DECRQM.
const (
	modeNotRecognized = 0
	modeSet           = 1
	modeReset         = 2
)

func modeState(set bool) int {
	if set {
		return modeSet
	}
	return modeReset
}

// escDispatch executes an escape sequence other than CSI and the strings.
func (t *Terminal) escDispatch(final byte) {
	p := &t.parser
	switch len(p.inter) {
	case 0:
		switch final {
		case '7': // DECSC
			t.saveCursor()
		case '8': // DECRC
			t.restoreCursor()
		case 'D': // IND
			t.linefeed()
		case 'E': // NEL
			t.carriageReturn()
			t.linefeed()
		case 'H': // HTS
			t.tabs[t.x] = true
		case 'M': // RI
			t.reverseIndex()
		case 'c': // RIS
			t.reset()
		case '=': // DECKPAM
			t.modes.appKeypad = true
		case '>': // DECKPNM
			t.modes.appKeypad = false
		case 'n': // LS2
			t.gl = 2
		case 'o': // LS3
			t.gl = 3
		case 'Z': // DECID
			t.sendReply(primaryDA)
			return
		default:
			return
		}
	case 1:
		switch i := p.inter[0]; {
		case i >= '(' && i <= '+': // designate G0 to G3
			cs, ok := designation(final)
			t.charsets[i-'('] = cs
			if !ok {
				// A set the Terminal does not know is taken for ASCII, on the
				// attached terminals as well.
				t.out = append(t.out, 0x1b, i, 'B')
				return
			}
		case i == '#' && final == '8': // DECALN
			t.alignmentTest()
		default:
			return
		}
	default:
		return
	}
	t.forward()
}

func designation(final byte) (charset, bool) {
	switch final {
	case 'B':
		return charsetASCII, true
	case '0':
		return charsetDECSpecial, true
	case 'A':
		return charsetUK, true
	}
	return charsetASCII, false
}

// csiDispatch executes a control sequence. Sequences the Terminal does not
// know are dropped.
func (t *Terminal) csiDispatch(final byte) {
	p := &t.parser
	inter := string(p.inter)
	switch p.prefix {
	case 0:
		switch inter {
		case "":
			t.csiPlain(final)
		case " ":
			if final == 'q' { // DECSCUSR
				t.modes.cursorStyle = p.arg(0, 0)
				t.forward()
			}
		case "!":
			if final == 'p' { // DECSTR
				t.softReset()
				t.forward()
			}
		case "$":
			if final == 'p' { // DECRQM
				t.reportMode(p.arg(0, 0), false)
			}
		}
	case '?':
		switch inter {
		case "":
			switch final {
			case 'h', 'l': // DECSET, DECRST
				t.setModes(true, final == 'h')
			case 'J': // DECSED, the same as ED without protected characters
				t.eraseInDisplay(p.arg(0, 0))
				t.forward()
			case 'K': // DECSEL
				t.eraseInLine(p.arg(0, 0))
				t.forward()
			case 'n': // DSR, DEC-specific
				t.privateStatusReport(p.arg(0, 0))
			}
		case "$":
			if final == 'p' { // DECRQM
				t.reportMode(p.arg(0, 0), true)
			}
		}
	case '>':
		if inter == "" && p.arg(0, 0) == 0 {
			switch final {
			case 'c': // DA2
				t.sendReply(secondaryDA)
			case 'q': // XTVERSION
				t.sendReply("\x1bP>|" + t.name + "\x1b\\")
			}
		}
	case '=':
		if inter == "" && final == 'c' && p.arg(0, 0) == 0 { // DA3
			t.sendReply(tertiaryDA)
		}
	}
}

// csiPlain executes a control sequence without private marker or
// intermediates.
func (t *Terminal) csiPlain(final byte) {
	p := &t.parser
	switch final {
	case '@': // ICH
		t.pendingWrap = false
		t.insertCells(t.x, p.arg(0, 1))
	case 'A': // CUU
		t.moveUp(p.arg(0, 1))
	case 'B', 'e': // CUD, VPR
		t.moveDown(p.arg(0, 1))
	case 'C', 'a': // CUF, HPR
		t.moveColumn(t.x + p.arg(0, 1))
	case 'D': // CUB
		t.moveColumn(t.x - p.arg(0, 1))
	case 'E': // CNL
		t.moveDown(p.arg(0, 1))
		t.carriageReturn()
	case 'F': // CPL
		t.moveUp(p.arg(0, 1))
		t.carriageReturn()
	case 'G', '`': // CHA, HPA
		t.moveColumn(p.arg(0, 1) - 1)
	case 'H', 'f': // CUP, HVP
		t.moveTo(p.arg(1, 1)-1, p.arg(0, 1)-1)
	case 'I': // CHT
		t.tab(p.arg(0, 1))
	case 'J': // ED
		t.eraseInDisplay(p.arg(0, 0))
	case 'K': // EL
		t.eraseInLine(p.arg(0, 0))
	case 'L': // IL
		t.insertLines(p.arg(0, 1))
	case 'M': // DL
		t.deleteLines(p.arg(0, 1))
	case 'P': // DCH
		t.deleteCells(p.arg(0, 1))
	case 'S': // SU
		n := min(p.arg(0, 1), t.bottom-t.top+1)
		t.saveLines(t.top, t.top+n)
		t.scrollUp(t.top, t.bottom, n)
	case 'T': // SD; with more parameters, mouse highlight tracking
		if len(p.args()) > 1 {
			return
		}
		t.scrollDown(t.top, t.bottom, p.arg(0, 1))
	case 'X': // ECH
		t.eraseCells(p.arg(0, 1))
	case 'Z': // CBT
		t.tab(-p.arg(0, 1))
	case 'b': // REP
		t.repeat(p.arg(0, 1))
	case 'c': // DA1
		if p.arg(0, 0) == 0 {
			t.sendReply(primaryDA)
		}
		return
	case 'd': // VPA
		t.moveTo(t.x, p.arg(0, 1)-1)
	case 'g': // TBC
		switch p.arg(0, 0) {
		case 0:
			t.tabs[t.x] = false
		case 3:
			clear(t.tabs)
		default:
			return
		}
	case 'h', 'l': // SM, RM
		t.setModes(false, final == 'h')
		return
	case 'm': // SGR
		t.selectGraphicRendition()
	case 'n': // DSR
		t.statusReport(p.arg(0, 0))
		return
	case 'r': // DECSTBM
		top := p.arg(0, 1) - 1
		bottom := min(p.arg(1, t.rows), t.rows) - 1
		if top >= bottom {
			return
		}
		t.top, t.bottom = top, bottom
		t.moveTo(0, 0)
	case 's': // SCOSC
		t.saveCursor()
	case 'u': // SCORC
		t.restoreCursor()
	case 't': // window manipulation
		t.windowOp()
		return
	default:
		return
	}
	t.forward()
}

// setModes executes SM, RM, DECSET or DECRST. The modes the Terminal does
// not keep track of are dropped from the sequence passed on.
func (t *Terminal) setModes(private, set bool) {
	p := &t.parser
	var kept []int
	all := !p.hasSubparams()
	for _, m := range p.args() {
		var ok bool
		if private {
			ok = t.setPrivateMode(m, set)
		} else {
			ok = t.setANSIMode(m, set)
		}
		if ok {
			kept = append(kept, m)
		} else {
			all = false
		}
	}
	if len(kept) == 0 {
		return
	}
	if all {
		t.forward()
		return
	}
	t.out = appendModes(t.out, private, set, kept...)
}

// appendModes appends the sequence that sets or resets modes.
func appendModes(b []byte, private, set bool, modes ...int) []byte {
	b = append(b, "\x1b["...)
	if private {
		b = append(b, '?')
	}
	for i, m := range modes {
		if i > 0 {
			b = append(b, ';')
		}
		b = strconv.AppendInt(b, int64(m), 10)
	}
	if set {
		return append(b, 'h')
	}
	return append(b, 'l')
}

func (t *Terminal) setANSIMode(m int, set bool) bool {
	switch m {
	case 4: // IRM
		t.modes.insert = set
	case 20: // LNM
		t.modes.newline = set
	default:
		return false
	}
	return true
}

func (t *Terminal) setPrivateMode(m int, set bool) bool {
	switch m {
	case 1:
		t.modes.appCursor = set
	case 5:
		t.modes.reverseVideo = set
	case 6:
		t.modes.origin = set
		t.moveTo(0, 0)
	case 7:
		t.modes.autowrap = set
		if !set {
			t.pendingWrap = false
		}
	case 9, 1000, 1002, 1003:
		// Resetting any tracking mode turns mouse tracking off, as in xterm.
		t.modes.mouse = 0
		if set {
			t.modes.mouse = m
		}
	case 12:
		t.modes.cursorBlink = set
	case 25:
		t.modes.cursorHidden = !set
	case 47, 1047, 1049:
		t.setAlternateScreen(m, set)
	case 66:
		t.modes.appKeypad = set
	case 1004:
		t.modes.focusEvents = set
	case 1005, 1006, 1015, 1016:
		if set {
			t.modes.mouseEncodings |= mouseEncodingBit(m)
		} else {
			t.modes.mouseEncodings &^= mouseEncodingBit(m)
		}
	case 1007:
		t.modes.altScroll = set
	case 1048:
		if set {
			t.saveCursor()
		} else {
			t.restoreCursor()
		}
	case 2004:
		t.modes.bracketedPaste = set
	case 2026:
		t.modes.syncOutput = set
	default:
		return false
	}
	return true
}

func (t *Terminal) setAlternateScreen(m int, set bool) {
	switch m {
	case 1049:
		if set {
			if t.alt {
				return
			}
			t.saveCursor()
			t.switchScreen(true)
			t.clearAlternate()
			t.altMode = m
			return
		}
		if t.alt {
			t.switchScreen(false)
		}
		t.restoreCursor()
	case 1047:
		if set {
			if !t.alt {
				t.switchScreen(true)
				t.altMode = m
			}
			return
		}
		if t.alt {
			t.clearAlternate()
			t.switchScreen(false)
		}
	default: // 47
		if set {
			if !t.alt {
				t.switchScreen(true)
				t.altMode = m
			}
			return
		}
		if t.alt {
			t.switchScreen(false)
		}
	}
}

// reportMode answers DECRQM.
func (t *Terminal) reportMode(m int, private bool) {
	state := modeNotRecognized
	prefix := ""
	if private {
		prefix = "?"
		state = t.privateModeState(m)
	} else {
		switch m {
		case 4:
			state = modeState(t.modes.insert)
		case 20:
			state = modeState(t.modes.newline)
		}
	}
	t.sendReply(fmt.Sprintf("\x1b[%s%d;%d$y", prefix, m, state))
}

func (t *Terminal) privateModeState(m int) int {
	switch m {
	case 1:
		return modeState(t.modes.appCursor)
	case 5:
		return modeState(t.modes.reverseVideo)
	case 6:
		return modeState(t.modes.origin)
	case 7:
		return modeState(t.modes.autowrap)
	case 9, 1000, 1002, 1003:
		return modeState(t.modes.mouse == m)
	case 12:
		return modeState(t.modes.cursorBlink)
	case 25:
		return modeState(!t.modes.cursorHidden)
	case 47, 1047, 1049:
		return modeState(t.alt && t.altMode == m)
	case 66:
		return modeState(t.modes.appKeypad)
	case 1004:
		return modeState(t.modes.focusEvents)
	case 1005, 1006, 1015, 1016:
		return modeState(t.modes.mouseEncodings&mouseEncodingBit(m) != 0)
	case 1007:
		return modeState(t.modes.altScroll)
	case 1048:
		return modeReset
	case 2004:
		return modeState(t.modes.bracketedPaste)
	case 2026:
		return modeState(t.modes.syncOutput)
	}
	return modeNotRecognized
}

// statusReport answers DSR.
func (t *Terminal) statusReport(n int) {
	switch n {
	case 5:
		t.sendReply("\x1b[0n")
	case 6:
		row, col := t.cursorReport()
		t.sendReply(fmt.Sprintf("\x1b[%d;%dR", row, col))
	}
}

// privateStatusReport answers the DEC-specific forms of DSR.
func (t *Terminal) privateStatusReport(n int) {
	switch n {
	case 6: // DECXCPR
		row, col := t.cursorReport()
		t.sendReply(fmt.Sprintf("\x1b[?%d;%dR", row, col))
	case 15: // printer status: no printer
		t.sendReply("\x1b[?13n")
	case 996: // color scheme, dark (1) or light (2)
		if dark, ok := isDark(t.colors[1]); ok {
			scheme := 2
			if dark {
				scheme = 1
			}
			t.sendReply(fmt.Sprintf("\x1b[?997;%dn", scheme))
		}
	}
}

// cursorReport returns the 1-based cursor position to report, relative to
// the scrolling region in origin mode.
func (t *Terminal) cursorReport() (row, col int) {
	y := t.y
	if t.modes.origin {
		y -= t.top
	}
	return y + 1, t.x + 1
}

// windowOp answers the window manipulation reports, and passes on the title
// stack operations. The other operations, which would resize or move the
// attached terminal's window, are dropped.
func (t *Terminal) windowOp() {
	args := t.parser.args()
	if len(args) == 0 {
		return
	}
	switch args[0] {
	case 14: // text area size in pixels
		if t.pixelWidth > 0 && t.pixelHeight > 0 {
			t.sendReply(fmt.Sprintf("\x1b[4;%d;%dt", t.pixelHeight, t.pixelWidth))
		}
	case 16: // cell size in pixels
		if t.pixelWidth > 0 && t.pixelHeight > 0 {
			t.sendReply(fmt.Sprintf("\x1b[6;%d;%dt", t.pixelHeight/t.rows, t.pixelWidth/t.cols))
		}
	case 18: // text area size in characters
		t.sendReply(fmt.Sprintf("\x1b[8;%d;%dt", t.rows, t.cols))
	case 19: // screen size in characters
		t.sendReply(fmt.Sprintf("\x1b[9;%d;%dt", t.rows, t.cols))
	case 22, 23: // push and pop the title
		t.forward()
	}
}

// oscDispatch executes an operating system command. Queries are answered,
// changes to the colors of the attached terminal are dropped, and the rest,
// such as titles, hyperlinks, notifications and clipboard writes, is passed
// on.
func (t *Terminal) oscDispatch(terminator string) {
	num, rest, _ := strings.Cut(string(t.parser.data), ";")
	switch num {
	case "0", "2":
		t.title = rest
		t.forward()
	case "10":
		t.colorQuery(10, rest, terminator)
	case "11":
		t.colorQuery(11, rest, terminator)
	case "12":
		t.colorQuery(12, rest, terminator)
	case "52":
		// Reading the clipboard is refused: it belongs to the user of the
		// attached terminal.
		if i := strings.LastIndexByte(rest, ';'); i >= 0 && rest[i+1:] == "?" {
			return
		}
		t.forward()
	case "4", "5", "13", "14", "15", "16", "17", "18", "19", "21", "104", "105", "106",
		"110", "111", "112", "113", "114", "115", "116", "117", "118", "119":
	default:
		t.forward()
	}
}

// colorQuery answers the queries of OSC 10 and 11 for the default colors,
// starting at the color number n. The sequence may query several colors in a
// row, as in "10;?;?".
func (t *Terminal) colorQuery(n int, rest, terminator string) {
	if terminator == "" {
		terminator = "\x1b\\"
	}
	for part := range strings.SplitSeq(rest, ";") {
		if part == "?" && (n == 10 || n == 11) {
			if v := t.colors[n-10]; v != "" {
				t.sendReply(fmt.Sprintf("\x1b]%d;%s%s", n, v, terminator))
			}
		}
		n++
	}
}

// dcsDispatch executes a device control string. Only the queries the
// Terminal answers and tmux passthrough, for a session whose terminal runs in
// tmux, are handled; everything else, such as images, is dropped.
func (t *Terminal) dcsDispatch() {
	p := &t.parser
	switch {
	case p.prefix == 0 && string(p.inter) == "$" && p.final == 'q': // DECRQSS
		t.requestStatusString(string(p.data))
	case p.prefix == 0 && string(p.inter) == "+" && p.final == 'q': // XTGETTCAP
		t.sendReply("\x1bP0+r\x1b\\")
	case p.prefix == 0 && len(p.inter) == 0 && p.final == 't' && strings.HasPrefix(string(p.data), "mux;"):
		t.forward()
	}
}

// requestStatusString answers DECRQSS.
func (t *Terminal) requestStatusString(req string) {
	var v string
	switch req {
	case "m":
		v = string(appendSGRParams(nil, t.pen)) + "m"
	case "r":
		v = fmt.Sprintf("%d;%dr", t.top+1, t.bottom+1)
	case " q":
		v = fmt.Sprintf("%d q", t.modes.cursorStyle)
	case "\"q":
		v = "0\"q"
	case "\"p":
		v = "62;1\"p"
	default:
		t.sendReply("\x1bP0$r\x1b\\")
		return
	}
	t.sendReply("\x1bP1$r" + v + "\x1b\\")
}

// isDark reports whether an X11 color specification such as
// "rgb:1e1e/1e1e/2e2e" is a dark color.
func isDark(spec string) (bool, bool) {
	rest, ok := strings.CutPrefix(spec, "rgb:")
	if !ok {
		return false, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return false, false
	}
	var rgb [3]float64
	for i, part := range parts {
		if len(part) == 0 || len(part) > 4 {
			return false, false
		}
		v, err := strconv.ParseUint(part, 16, 16)
		if err != nil {
			return false, false
		}
		rgb[i] = float64(v) / float64(uint64(1)<<(4*len(part))-1)
	}
	return 0.2126*rgb[0]+0.7152*rgb[1]+0.0722*rgb[2] < 0.5, true
}
