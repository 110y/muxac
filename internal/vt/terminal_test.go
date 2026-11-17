package vt

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// testTerminal is a Terminal that records its replies.
type testTerminal struct {
	*Terminal
	replies []string
}

func newTestTerminal(cols, rows int) *testTerminal {
	tt := &testTerminal{}
	tt.Terminal = New(Options{Cols: cols, Rows: rows, Name: "muxac test", Reply: func(b []byte) {
		tt.replies = append(tt.replies, string(b))
	}})
	return tt
}

// feed feeds s and returns what is passed on to the attached terminals.
func (tt *testTerminal) feed(s string) string {
	return string(tt.Feed(nil, []byte(s)))
}

// rows returns the text of the screen's rows, trimmed.
func (tt *testTerminal) rows() []string {
	return strings.Split(strings.TrimSuffix(tt.Text(), "\n"), "\n")
}

func (tt *testTerminal) takeReplies() []string {
	r := tt.replies
	tt.replies = nil
	return r
}

func TestPrintAndText(t *testing.T) {
	t.Parallel()

	tt := newTestTerminal(10, 3)
	tt.feed("hello\r\nworld  \r\n")
	want := []string{"hello", "world", ""}
	if got := tt.rows(); !slices.Equal(got, want) {
		t.Errorf("rows = %q, want %q", got, want)
	}
	if tt.x != 0 || tt.y != 2 {
		t.Errorf("cursor = (%d, %d), want (0, 2)", tt.x, tt.y)
	}
}

func TestAutowrapAndPendingWrap(t *testing.T) {
	t.Parallel()

	tt := newTestTerminal(5, 3)
	tt.feed("abcde")
	if tt.x != 4 || !tt.pendingWrap {
		t.Fatalf("after filling a line: x = %d, pendingWrap = %v; want 4, true", tt.x, tt.pendingWrap)
	}
	tt.feed("f")
	if got := tt.rows(); got[0] != "abcde" || got[1] != "f" {
		t.Errorf("rows = %q, want the text wrapped", got)
	}
	if !tt.screen().lines[0].wrapped {
		t.Error("the first line is not marked as wrapped")
	}

	// A line feed in the pending state does not wrap again.
	tt = newTestTerminal(5, 3)
	tt.feed("abcde\r\nx")
	if got := tt.rows(); got[1] != "x" {
		t.Errorf("rows = %q, want x on the second line", got)
	}

	// Without autowrap, the last column is overwritten.
	tt = newTestTerminal(5, 3)
	tt.feed("\x1b[?7labcdefg")
	if got := tt.rows(); got[0] != "abcdg" || got[1] != "" {
		t.Errorf("rows = %q, want the last column overwritten", got)
	}
}

func TestWideCharactersAndClusters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		// x is the column of the cursor after the input.
		x    int
		text string
	}{
		{name: "CJK", input: "日本語", x: 6, text: "日本語"},
		{name: "emoji", input: "a😀b", x: 4, text: "a😀b"},
		{name: "combining accent", input: "éx", x: 2, text: "éx"},
		{name: "skin tone modifier", input: "👍🏽x", x: 3, text: "👍🏽x"},
		{name: "zero width joiner sequence", input: "👩‍💻x", x: 3, text: "👩‍💻x"},
		{name: "flag", input: "🇯🇵x", x: 3, text: "🇯🇵x"},
		{name: "VS16 widens a symbol", input: "⚠️x", x: 3, text: "⚠️x"},
		{name: "symbol without VS16 stays narrow", input: "⚠x", x: 2, text: "⚠x"},
		{name: "ambiguous width is narrow", input: "─●…x", x: 4, text: "─●…x"},
		{name: "Hangul jamo", input: "각x", x: 3, text: "각x"},
		{name: "lone combining mark", input: "́x", x: 1, text: "x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tt := newTestTerminal(20, 2)
			tt.feed(tc.input)
			if tt.x != tc.x {
				t.Errorf("cursor x = %d, want %d", tt.x, tc.x)
			}
			if got := tt.rows()[0]; got != tc.text {
				t.Errorf("text = %q, want %q", got, tc.text)
			}
		})
	}
}

func TestWideCharacterAtLastColumnWraps(t *testing.T) {
	t.Parallel()

	tt := newTestTerminal(5, 3)
	tt.feed("abcd日")
	if got := tt.rows(); got[0] != "abcd" || got[1] != "日" {
		t.Errorf("rows = %q, want the wide character on the next line", got)
	}
	if tt.x != 2 || tt.y != 1 {
		t.Errorf("cursor = (%d, %d), want (2, 1)", tt.x, tt.y)
	}
}

func TestOverwritingHalfOfAWideCharacter(t *testing.T) {
	t.Parallel()

	tt := newTestTerminal(10, 2)
	tt.feed("日本\x1b[1;2Hx")
	if got := tt.rows()[0]; got != " x本" {
		t.Errorf("text = %q, want the cut wide character blanked", got)
	}
}

func TestCursorMovementAndErase(t *testing.T) {
	t.Parallel()

	tt := newTestTerminal(10, 4)
	tt.feed("0123456789\r\nabcdefghij\r\nABCDEFGHIJ")
	tt.feed("\x1b[2;4H\x1b[K")
	tt.feed("\x1b[3;3H\x1b[1K")
	tt.feed("\x1b[1;5H\x1b[2X")
	want := []string{"0123  6789", "abc", "   DEFGHIJ", ""}
	if got := tt.rows(); !slices.Equal(got, want) {
		t.Errorf("rows = %q, want %q", got, want)
	}

	tt.feed("\x1b[2J")
	if got := strings.Join(tt.rows(), ""); got != "" {
		t.Errorf("rows after ED 2 = %q, want empty", got)
	}
	tt.feed("\x1b[10;20H")
	if tt.x != 9 || tt.y != 3 {
		t.Errorf("CUP out of bounds: cursor = (%d, %d), want (9, 3)", tt.x, tt.y)
	}
	tt.feed("\x1b[2A\x1b[3D")
	if tt.x != 6 || tt.y != 1 {
		t.Errorf("CUU/CUB: cursor = (%d, %d), want (6, 1)", tt.x, tt.y)
	}
}

func TestInsertAndDelete(t *testing.T) {
	t.Parallel()

	tt := newTestTerminal(8, 4)
	tt.feed("abcdef\r\n1\r\n2\r\n3")
	tt.feed("\x1b[1;3H\x1b[2@")
	tt.feed("\x1b[1;1H\x1b[P")
	if got := tt.rows()[0]; got != "b  cdef" {
		t.Errorf("after ICH and DCH: %q, want %q", got, "b  cdef")
	}
	tt.feed("\x1b[2;1H\x1b[L")
	if want := []string{"b  cdef", "", "1", "2"}; !slices.Equal(tt.rows(), want) {
		t.Errorf("after IL: %q, want %q", tt.rows(), want)
	}
	tt.feed("\x1b[M\x1b[M")
	if want := []string{"b  cdef", "2", "", ""}; !slices.Equal(tt.rows(), want) {
		t.Errorf("after DL: %q, want %q", tt.rows(), want)
	}

	// Insert mode shifts the line.
	tt.feed("\x1b[1;1H\x1b[4hXY\x1b[4l")
	if got := tt.rows()[0]; got != "XYb  cde" {
		t.Errorf("in insert mode: %q, want %q", got, "XYb  cde")
	}
}

func TestScrollingRegion(t *testing.T) {
	t.Parallel()

	tt := newTestTerminal(5, 5)
	tt.feed("1\r\n2\r\n3\r\n4\r\n5")
	tt.feed("\x1b[2;4r")
	if tt.x != 0 || tt.y != 0 {
		t.Errorf("DECSTBM did not home the cursor: (%d, %d)", tt.x, tt.y)
	}
	tt.feed("\x1b[4;1H\nX")
	if want := []string{"1", "3", "4", "X", "5"}; !slices.Equal(tt.rows(), want) {
		t.Errorf("after scrolling the region up: %q, want %q", tt.rows(), want)
	}
	tt.feed("\x1b[2;1H\x1bMY")
	if want := []string{"1", "Y", "3", "4", "5"}; !slices.Equal(tt.rows(), want) {
		t.Errorf("after scrolling the region down: %q, want %q", tt.rows(), want)
	}
	// Origin mode addresses the cursor relative to the region.
	tt.feed("\x1b[?6h\x1b[1;1HZ")
	if tt.rows()[1] != "Z" {
		t.Errorf("origin mode: %q, want Z on the region's first row", tt.rows())
	}
	tt.feed("\x1b[6n")
	if got := tt.takeReplies(); !slices.Equal(got, []string{"\x1b[1;2R"}) {
		t.Errorf("CPR in origin mode = %q, want relative to the region", got)
	}
}

func TestAlternateScreen(t *testing.T) {
	t.Parallel()

	tt := newTestTerminal(10, 3)
	tt.feed("main\x1b[2;3H")
	tt.feed("\x1b[?1049h")
	if !tt.alt || strings.Join(tt.rows(), "") != "" {
		t.Fatalf("1049h: alt = %v, rows = %q; want a blank alternate screen", tt.alt, tt.rows())
	}
	tt.feed("\x1b[Halt")
	tt.feed("\x1b[?1049l")
	if tt.alt || tt.rows()[0] != "main" {
		t.Errorf("1049l: alt = %v, rows = %q; want the main screen back", tt.alt, tt.rows())
	}
	if tt.x != 2 || tt.y != 1 {
		t.Errorf("1049l did not restore the cursor: (%d, %d)", tt.x, tt.y)
	}
	// 47 keeps the contents of the alternate screen.
	tt.feed("\x1b[?47h")
	if tt.rows()[0] != "alt" {
		t.Errorf("47h: rows = %q, want the alternate screen as it was", tt.rows())
	}
}

func TestCharsets(t *testing.T) {
	t.Parallel()

	tt := newTestTerminal(10, 2)
	tt.feed("\x1b(0lqk\x1b(Bq\x0e\x0f")
	if got := tt.rows()[0]; got != "┌─┐q" {
		t.Errorf("text = %q, want line drawing", got)
	}
	tt.feed("\x1b)0\x0ex\x0fx")
	if got := tt.rows()[0]; got != "┌─┐q│x" {
		t.Errorf("text = %q, want G1 invoked by SO", got)
	}
}

func TestRepeatAndTabs(t *testing.T) {
	t.Parallel()

	tt := newTestTerminal(20, 2)
	tt.feed("a\x1b[3b\tb")
	if got := tt.rows()[0]; got != "aaaa    b" {
		t.Errorf("text = %q, want %q", got, "aaaa    b")
	}
	tt.feed("\x1b[3g\r\x1b[5C\x1bH\r\tc")
	if got := tt.rows()[0]; got != "aaaa c  b" {
		t.Errorf("text = %q, want a custom tab stop used", got)
	}
}

func TestQueries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  []string
		// pass is what is passed on to the attached terminals.
		pass string
	}{
		{name: "DA1", input: "\x1b[c", want: []string{primaryDA}},
		{name: "DA1 with parameter 0", input: "\x1b[0c", want: []string{primaryDA}},
		{name: "DECID", input: "\x1bZ", want: []string{primaryDA}},
		{name: "DA2", input: "\x1b[>c", want: []string{secondaryDA}},
		{name: "DA3", input: "\x1b[=c", want: []string{tertiaryDA}},
		{name: "DSR status", input: "\x1b[5n", want: []string{"\x1b[0n"}},
		{name: "CPR", input: "ab\x1b[6n", want: []string{"\x1b[1;3R"}, pass: "ab"},
		{name: "DECXCPR", input: "\r\n\x1b[?6n", want: []string{"\x1b[?2;1R"}, pass: "\r\n"},
		{name: "XTVERSION", input: "\x1b[>q", want: []string{"\x1bP>|muxac test\x1b\\"}},
		{name: "DECRQM set private mode", input: "\x1b[?25$p", want: []string{"\x1b[?25;1$y"}},
		{name: "DECRQM reset private mode", input: "\x1b[?2004$p", want: []string{"\x1b[?2004;2$y"}},
		{name: "DECRQM unknown private mode", input: "\x1b[?2027$p", want: []string{"\x1b[?2027;0$y"}},
		{name: "DECRQM synchronized output", input: "\x1b[?2026h\x1b[?2026$p", want: []string{"\x1b[?2026;1$y"}, pass: "\x1b[?2026h"},
		{name: "DECRQM ANSI mode", input: "\x1b[4$p", want: []string{"\x1b[4;2$y"}},
		{name: "text area size", input: "\x1b[18t", want: []string{"\x1b[8;24;80t"}},
		{name: "pixel size unknown", input: "\x1b[14t", want: nil},
		{name: "DECRQSS SGR", input: "\x1b[1;31m\x1bP$qm\x1b\\", want: []string{"\x1bP1$r0;1;31m\x1b\\"}, pass: "\x1b[1;31m"},
		{name: "DECRQSS margins", input: "\x1b[2;10r\x1bP$qr\x1b\\", want: []string{"\x1bP1$r2;10r\x1b\\"}, pass: "\x1b[2;10r"},
		{name: "DECRQSS unknown", input: "\x1bP$qx\x1b\\", want: []string{"\x1bP0$r\x1b\\"}},
		{name: "XTGETTCAP", input: "\x1bP+q544e\x1b\\", want: []string{"\x1bP0+r\x1b\\"}},
		{name: "colors unknown", input: "\x1b]11;?\x07", want: nil},
		{name: "kitty keyboard query", input: "\x1b[?u", want: nil},
		{name: "clipboard read", input: "\x1b]52;c;?\x07", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tt := newTestTerminal(80, 24)
			out := tt.feed(tc.input)
			if got := tt.takeReplies(); !slices.Equal(got, tc.want) {
				t.Errorf("replies = %q, want %q", got, tc.want)
			}
			if out != tc.pass {
				t.Errorf("passed on %q, want %q", out, tc.pass)
			}
		})
	}
}

func TestColorQueries(t *testing.T) {
	t.Parallel()

	tt := newTestTerminal(80, 24)
	tt.SetColors("rgb:ffff/ffff/ffff", "rgb:1e1e/1e1e/2e2e")
	tt.feed("\x1b]10;?\x07\x1b]11;?\x1b\\\x1b]10;?;?\x07")
	want := []string{
		"\x1b]10;rgb:ffff/ffff/ffff\x07",
		"\x1b]11;rgb:1e1e/1e1e/2e2e\x1b\\",
		"\x1b]10;rgb:ffff/ffff/ffff\x07",
		"\x1b]11;rgb:1e1e/1e1e/2e2e\x07",
	}
	if got := tt.takeReplies(); !slices.Equal(got, want) {
		t.Errorf("replies = %q, want %q", got, want)
	}
	tt.feed("\x1b[?996n")
	if got := tt.takeReplies(); !slices.Equal(got, []string{"\x1b[?997;1n"}) {
		t.Errorf("color scheme report = %q, want dark", got)
	}
}

func TestPassthrough(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "text and controls", input: "a\tb\r\n\x07é日", want: "a\tb\r\n\x07é日"},
		{name: "SGR and cursor movement", input: "\x1b[1;38;2;1;2;3m\x1b[5;6H\x1b[K", want: "\x1b[1;38;2;1;2;3m\x1b[5;6H\x1b[K"},
		{name: "known modes", input: "\x1b[?1049h\x1b[?2004h\x1b[?25l", want: "\x1b[?1049h\x1b[?2004h\x1b[?25l"},
		{name: "unknown modes are dropped", input: "\x1b[?2027h\x1b[?69h\x1b[12h", want: ""},
		{name: "unknown modes are dropped from a list", input: "\x1b[?1000;2027;1006h", want: "\x1b[?1000;1006h"},
		{name: "kitty keyboard protocol is dropped", input: "\x1b[>1u\x1b[<u\x1b[=1;1u", want: ""},
		{name: "modifyOtherKeys is dropped", input: "\x1b[>4;2m", want: ""},
		{name: "title", input: "\x1b]0;title\x07\x1b]2;t2\x1b\\", want: "\x1b]0;title\x07\x1b]2;t2\x1b\\"},
		{name: "hyperlink", input: "\x1b]8;;https://example.com\x1b\\x\x1b]8;;\x1b\\", want: "\x1b]8;;https://example.com\x1b\\x\x1b]8;;\x1b\\"},
		{name: "clipboard write", input: "\x1b]52;c;aGk=\x07", want: "\x1b]52;c;aGk=\x07"},
		{name: "palette changes are dropped", input: "\x1b]4;1;rgb:00/00/00\x07\x1b]11;#000000\x07\x1b]104\x07", want: ""},
		{name: "images are dropped", input: "\x1bPq#0;2;0;0;0#0~~\x1b\\\x1b_Ga=q;AAAA\x1b\\x", want: "x"},
		{name: "tmux passthrough", input: "\x1bPtmux;\x1b\x1b]52;c;aGk=\x07\x1b\\", want: "\x1bPtmux;\x1b\x1b]52;c;aGk=\x07\x1b\\"},
		{name: "window manipulation is dropped", input: "\x1b[8;50;100t\x1b[22;0t", want: "\x1b[22;0t"},
		{name: "unknown sequences are dropped", input: "\x1b[5i\x1b#3x", want: "x"},
		{name: "unknown character set is taken for ASCII", input: "\x1b(<", want: "\x1b(B"},
		{name: "OSC ended by another escape sequence", input: "\x1b]2;t\x1b[1m", want: "\x1b]2;t\x1b[1m"},
		{name: "cancelled sequence", input: "\x1b[1\x18x", want: "x"},
		{name: "control inside a sequence", input: "\x1b[1\r;2H", want: "\r\x1b[1;2H"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tt := newTestTerminal(80, 24)
			if got := tt.feed(tc.input); got != tc.want {
				t.Errorf("passed on %q, want %q", got, tc.want)
			}
		})
	}
}

// corpus is output that exercises most of what the Terminal keeps track of.
var corpus = strings.Join([]string{
	"plain text\r\n",
	"\x1b[1;31mbold red\x1b[0m \x1b[4:3;58:2::1:2:3mcurly\x1b[24m \x1b[38;5;200;48;2;10;20;30mcolors\x1b[m\r\n",
	"日本語と絵文字👍🏽🇯🇵 é ⚠️\r\n",
	"\x1b[2;20r\x1b[?6h\x1b[3;5Horigin\x1b[?6l",
	"\x1b[10;1H\x1b[42m\x1b[K\x1b[0m\x1b[11;1H\x1b(0lqqk\x1b(B",
	"\x1b[12;70H0123456789ABCDEF",
	"\x1b7\x1b[1;1H\x1b[7msaved\x1b8",
	"\x1b[?1h\x1b=\x1b[?2004h\x1b[?1004h\x1b[?1002h\x1b[?1006h\x1b[5 q\x1b[4h\x1b[?12h",
	"\x1b]2;the title\x07",
	"\x1b[3g\x1b[1;4H\x1bH\x1b[1;30H\x1bH",
	"\x1b[20;1H" + strings.Repeat("wrapped line ", 10),
}, "")

func TestRedrawRestoresState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{name: "main screen", input: corpus},
		{name: "pending wrap", input: "\x1b[1;75Habcdef"},
		{name: "pending wrap after a wide character", input: "\x1b[1;79H日"},
		{name: "alternate screen 1049", input: corpus + "\x1b[?1049h\x1b[2;2Halt screen\x1b7\x1b[5;5Hx\x1b[32m"},
		{name: "alternate screen 47", input: corpus + "\x1b[?47h\x1b[Halt"},
		{name: "alternate screen 1047 after 1049", input: "x\x1b[?1049hy\x1b[?1049l\x1b[?1047hz"},
		{name: "hidden cursor and no autowrap", input: "\x1b[?25l\x1b[?7l\x1b[1;79Hxyz"},
		{name: "charsets", input: "\x1b)0\x1b*A\x0e"},
		{name: "insert mode", input: "abc\x1b[4h"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := newTestTerminal(80, 24)
			a.feed(tc.input)
			for _, fresh := range []bool{true, false} {
				b := newTestTerminal(80, 24)
				if fresh {
					// A terminal attaching shows the shell on its main screen.
					b.feed("\x1b[31mleftover\r\n\x1b[5;5Hshell prompt\r\n")
					b.Feed(nil, a.Attach(nil, b.y+1, b.x+1))
				} else {
					// A terminal attached already may show anything.
					b.feed("leftover\r\n\x1b[?1049h\x1b[31mon the alternate screen")
					b.Feed(nil, a.Redraw(nil))
				}
				assertSameState(t, a.Terminal, b.Terminal)

				// Output that follows has the same effect on both.
				const more = "\x1b[Hmore\r\n\x1b[2Kline\x1b[1;1Hx\x1b[?1049l\x1b[5;5Hafter"
				a2 := newTestTerminal(80, 24)
				a2.feed(tc.input + more)
				b.feed(more)
				assertSameState(t, a2.Terminal, b.Terminal)
			}
		})
	}
}

// assertSameState checks that b shows and does the same as a.
func assertSameState(t *testing.T, a, b *Terminal) {
	t.Helper()

	if diffs := stateDiff(a, b); len(diffs) > 0 {
		t.Fatal(strings.Join(diffs, "\n"))
	}
}

// stateDiff returns how b differs from a in what it shows and does, if it
// does: the first cell that differs, and the rest of its state.
func stateDiff(a, b *Terminal) []string {
	if a.alt != b.alt {
		return []string{fmt.Sprintf("alt = %v, want %v", b.alt, a.alt)}
	}
	var diffs []string
	for i := range a.screens {
		if i == 1 && !a.alt {
			// The alternate screen is drawn only while it is shown.
			continue
		}
		for y := range a.screens[i].lines {
			for x := range a.screens[i].lines[y].cells {
				ca, cb := visible(a.screens[i].lines[y].cells[x]), visible(b.screens[i].lines[y].cells[x])
				if ca != cb {
					return append(diffs, fmt.Sprintf("screen %d, cell (%d, %d) = %+v, want %+v", i, x, y, cb, ca))
				}
			}
		}
		// What restoring the cursor restores must match.
		ca, cb := a.screens[i].savedCursor(), b.screens[i].savedCursor()
		if ca.x != cb.x || ca.y != cb.y || ca.pen != cb.pen || ca.charsets != cb.charsets || ca.gl != cb.gl {
			diffs = append(diffs, fmt.Sprintf("screen %d: saved cursor = %+v, want %+v", i, cb, ca))
		}
	}
	if a.x != b.x || a.y != b.y || a.pendingWrap != b.pendingWrap {
		diffs = append(diffs, fmt.Sprintf("cursor = (%d, %d, pending %v), want (%d, %d, pending %v)", b.x, b.y, b.pendingWrap, a.x, a.y, a.pendingWrap))
	}
	if a.pen != b.pen {
		diffs = append(diffs, fmt.Sprintf("pen = %+v, want %+v", b.pen, a.pen))
	}
	if a.charsets != b.charsets || a.gl != b.gl {
		diffs = append(diffs, fmt.Sprintf("charsets = %v, GL = %d; want %v, %d", b.charsets, b.gl, a.charsets, a.gl))
	}
	if a.top != b.top || a.bottom != b.bottom {
		diffs = append(diffs, fmt.Sprintf("margins = %d-%d, want %d-%d", b.top, b.bottom, a.top, a.bottom))
	}
	ma, mb := a.modes, b.modes
	// Synchronized output is never left on for an attaching terminal.
	ma.syncOutput, mb.syncOutput = false, false
	if ma != mb {
		diffs = append(diffs, fmt.Sprintf("modes = %+v, want %+v", mb, ma))
	}
	if !slices.Equal(a.tabs, b.tabs) {
		diffs = append(diffs, "tab stops differ")
	}
	if a.title != b.title {
		diffs = append(diffs, fmt.Sprintf("title = %q, want %q", b.title, a.title))
	}
	return diffs
}

// visible returns what of c shows on the screen: blank cells only show their
// background.
func visible(c cell) cell {
	if c.isBlank() {
		return blank(c.style.bg)
	}
	return c
}

func TestLeaveResetsTheTerminal(t *testing.T) {
	t.Parallel()

	a := newTestTerminal(80, 24)
	a.feed(corpus + "\x1b[?1049h\x1b[?1003h\x1b[?25l")
	b := newTestTerminal(80, 24)
	b.Feed(nil, a.Attach(nil, 0, 0))
	b.Feed(nil, a.Leave(nil))

	if b.alt {
		t.Error("the alternate screen is still shown")
	}
	want := defaultModes()
	if b.modes != want {
		t.Errorf("modes = %+v, want the defaults %+v", b.modes, want)
	}
	if b.top != 0 || b.bottom != 23 || b.pen != (style{}) || b.charsets != [4]charset{} || b.gl != 0 {
		t.Errorf("margins, pen or charsets left set")
	}
	// The cursor is left below the last line in use, on the main screen.
	if b.x != 0 || b.y != 20 {
		t.Errorf("cursor = (%d, %d), want (0, 20)", b.x, b.y)
	}
}

func TestLeaveScrollsWhenTheScreenIsFull(t *testing.T) {
	t.Parallel()

	a := newTestTerminal(10, 3)
	a.feed("1\r\n2\r\n3")
	got := string(a.Leave(nil))
	if !strings.HasSuffix(got, "\x1b[3H\r\n\x1b[23;0t") {
		t.Errorf("Leave = %q, want the cursor moved to a new line at the bottom", got)
	}
}

func TestFeedSplitAnywhere(t *testing.T) {
	t.Parallel()

	input := corpus + "\x1b]11;?\x1b\\\x1b[6n\x1bPtmux;\x1b\x1b]2;x\x07\x1b\\\x1b[?1049h" + "日本"
	whole := newTestTerminal(80, 24)
	wantOut := whole.feed(input)

	split := newTestTerminal(80, 24)
	var out strings.Builder
	for i := range len(input) {
		out.WriteString(split.feed(input[i : i+1]))
	}
	if out.String() != wantOut {
		t.Errorf("passed on byte by byte:\n%q\nwant\n%q", out.String(), wantOut)
	}
	if !slices.Equal(split.replies, whole.replies) {
		t.Errorf("replies byte by byte = %q, want %q", split.replies, whole.replies)
	}
	assertSameState(t, whole.Terminal, split.Terminal)
}

func TestResize(t *testing.T) {
	t.Parallel()

	// The last line is rewrapped, the wide character that does not fit going
	// on to the next row, and the lines at the top go off the screen.
	tt := newTestTerminal(10, 5)
	tt.feed("1\r\n2\r\n3\r\n4\r\n5日本語abc")
	tt.Resize(6, 3)
	if want := []string{"4", "5日本", "語abc"}; !slices.Equal(tt.rows(), want) {
		t.Errorf("after shrinking: %q, want %q", tt.rows(), want)
	}
	if tt.y != 2 || tt.x != 5 {
		t.Errorf("cursor = (%d, %d), want (5, 2)", tt.x, tt.y)
	}

	// The line is joined again; the lines that went off the top do not come
	// back, with no history kept.
	tt.Resize(12, 4)
	if want := []string{"4", "5日本語abc", "", ""}; !slices.Equal(tt.rows(), want) {
		t.Errorf("after growing: %q, want %q", tt.rows(), want)
	}
	if tt.y != 1 || tt.x != 10 {
		t.Errorf("cursor = (%d, %d), want (10, 1)", tt.x, tt.y)
	}
	tt.feed("\x1b[18t")
	if got := tt.takeReplies(); !slices.Equal(got, []string{"\x1b[8;4;12t"}) {
		t.Errorf("size report = %q", got)
	}
	if tt.top != 0 || tt.bottom != 3 {
		t.Errorf("margins = %d-%d, want reset to the screen", tt.top, tt.bottom)
	}
}

func TestReset(t *testing.T) {
	t.Parallel()

	tt := newTestTerminal(10, 3)
	tt.feed(corpus)
	if out := tt.feed("\x1bc"); out != "\x1bc" {
		t.Errorf("RIS passed on as %q", out)
	}
	fresh := newTestTerminal(10, 3)
	assertSameState(t, fresh.Terminal, tt.Terminal)
}

func TestIsDark(t *testing.T) {
	t.Parallel()

	tests := []struct {
		spec     string
		dark, ok bool
	}{
		{spec: "rgb:0000/0000/0000", dark: true, ok: true},
		{spec: "rgb:ff/ff/ff", dark: false, ok: true},
		{spec: "rgb:1e1e/1e1e/2e2e", dark: true, ok: true},
		{spec: "#ffffff", ok: false},
		{spec: "rgb:ff/ff", ok: false},
	}
	for _, tc := range tests {
		dark, ok := isDark(tc.spec)
		if ok != tc.ok || (ok && dark != tc.dark) {
			t.Errorf("isDark(%q) = %v, %v; want %v, %v", tc.spec, dark, ok, tc.dark, tc.ok)
		}
	}
}

// historyTexts returns the text of the lines in the history of t, oldest
// first.
func historyTexts(t *Terminal) []string {
	var texts []string
	for i := range t.history.lines {
		line := t.history.lines[(t.history.start+i)%len(t.history.lines)]
		scratch := newTestTerminal(t.cols*2, 2)
		scratch.Feed(nil, line.b)
		texts = append(texts, scratch.rows()[0])
	}
	return texts
}

func TestHistory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		limit int
		input string
		want  []string
	}{
		{name: "lines scrolled off", limit: 10, input: "1\r\n2\r\n3\r\n4\r\n5", want: []string{"1", "2"}},
		{name: "limit", limit: 2, input: "1\r\n2\r\n3\r\n4\r\n5\r\n6", want: []string{"2", "3"}},
		{name: "no history", limit: 0, input: "1\r\n2\r\n3\r\n4", want: nil},
		{name: "styles are kept", limit: 10, input: "\x1b[31mred\x1b[0m\r\n2\r\n3\r\n4", want: []string{"red"}},
		{name: "scrolling up a region", limit: 10, input: "1\r\n2\r\n3\x1b[2;3r\x1b[3;1H\nx", want: []string{"2"}},
		{name: "scroll up", limit: 10, input: "1\r\n2\x1b[2S", want: []string{"1", "2"}},
		{name: "clearing the screen keeps it", limit: 10, input: "1\r\n2\x1b[2J", want: []string{"1", "2"}},
		{name: "erasing the history", limit: 10, input: "1\r\n2\r\n3\r\n4\x1b[3J", want: nil},
		{name: "the alternate screen keeps none", limit: 10, input: "\x1b[?1049h1\r\n2\r\n3\r\n4\x1b[2J", want: nil},
		{name: "deleting lines keeps none", limit: 10, input: "1\r\n2\x1b[H\x1b[2M", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tt := New(Options{Cols: 10, Rows: 3, HistoryLimit: tc.limit})
			tt.Feed(nil, []byte(tc.input))
			if got := historyTexts(tt); !slices.Equal(got, tc.want) {
				t.Errorf("history = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHistoryWhenTheScreenShrinks(t *testing.T) {
	t.Parallel()

	tt := New(Options{Cols: 10, Rows: 4, HistoryLimit: 10})
	tt.Feed(nil, []byte("1\r\n2\r\n3\r\n4"))
	tt.Resize(10, 2)
	if got, want := historyTexts(tt), []string{"1", "2"}; !slices.Equal(got, want) {
		t.Errorf("history = %q, want %q", got, want)
	}
}

func TestRedrawReplaysHistory(t *testing.T) {
	t.Parallel()

	a := New(Options{Cols: 20, Rows: 4, HistoryLimit: 100})
	for i := range 10 {
		a.Feed(nil, []byte("\x1b[1mline \x1b[0m"+strconv.Itoa(i)+"\r\n"))
	}
	// A terminal attaching shows the shell; the history goes after it into
	// its scrollback, and the screen is drawn as usual.
	b := New(Options{Cols: 20, Rows: 4, HistoryLimit: 100})
	b.Feed(nil, []byte("$ muxac attach"))
	b.Feed(nil, a.Attach(nil, 1, 15))
	want := append([]string{"$ muxac attach"}, historyTexts(a)...)
	if got := historyTexts(b); !slices.Equal(got, want) {
		t.Errorf("scrollback = %q, want %q", got, want)
	}
	assertSameState(t, a, b)
}

func TestCatchUp(t *testing.T) {
	t.Parallel()

	lines := func(from, to int) string {
		var s strings.Builder
		for i := from; i < to; i++ {
			s.WriteString("\x1b[1mline \x1b[0m")
			s.WriteString(strconv.Itoa(i))
			s.WriteString("\r\n")
		}
		return s.String()
	}
	tests := []struct {
		name string
		// before is the output that the terminal catching up was shown, and
		// after the output it missed.
		before, after string
	}{
		{name: "more lines than rows", before: lines(0, 6), after: lines(6, 16)},
		{name: "fewer lines than rows", before: lines(0, 6), after: lines(6, 8)},
		{name: "nothing missed", before: lines(0, 6)},
		{name: "history cleared", before: lines(0, 6), after: "\x1b[3J" + lines(6, 9)},
		{name: "alternate screen", before: lines(0, 6) + "\x1b[?1049h", after: "on the alternate screen"},
		{name: "left the alternate screen", before: lines(0, 2) + "\x1b[?1049h", after: "\x1b[?1049l" + lines(2, 9)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := New(Options{Cols: 20, Rows: 4, HistoryLimit: 100})
			b := New(Options{Cols: 20, Rows: 4, HistoryLimit: 100})
			a.Feed(nil, []byte(tc.before))
			b.Feed(nil, []byte(tc.before))
			mark := a.Mark()
			a.Feed(nil, []byte(tc.after))
			b.Feed(nil, a.CatchUp(nil, mark))
			// The scrollback of the terminal is what it would be had it been
			// shown what it missed.
			if got, want := historyTexts(b), historyTexts(a); !slices.Equal(got, want) {
				t.Errorf("scrollback = %q, want %q", got, want)
			}
			assertSameState(t, a, b)
		})
	}

	t.Run("lines wider than the terminal", func(t *testing.T) {
		t.Parallel()

		// The terminal stops showing the session when it gets wider, for
		// another one, and catches up once it is as narrow again.
		a := New(Options{Cols: 10, Rows: 4, HistoryLimit: 100})
		b := New(Options{Cols: 10, Rows: 4, HistoryLimit: 100})
		a.Feed(nil, []byte("start\r\n"))
		b.Feed(nil, []byte("start\r\n"))
		mark := a.Mark()
		a.Resize(20, 4)
		a.Feed(nil, []byte("L0-abcdefghijkl\r\nb\r\nc\r\nd\r\n"))
		a.Resize(10, 4)
		b.Feed(nil, a.CatchUp(nil, mark))
		// None of the lines is lost, although one takes two rows.
		want := []string{"start", "L0-abcdefg", "hijkl"}
		if got := historyTexts(b); !slices.Equal(got, want) {
			t.Errorf("scrollback = %q, want %q", got, want)
		}
		assertSameState(t, a, b)
	})
}

func TestAttachWrapsWideHistory(t *testing.T) {
	t.Parallel()

	// The history kept while the session was wider than the terminal that
	// attaches goes to its scrollback whole, its line taking two rows.
	a := New(Options{Cols: 20, Rows: 4, HistoryLimit: 100})
	a.Feed(nil, []byte("L0-abcdefghijkl\r\nb\r\nc\r\nd\r\n"))
	a.Resize(10, 4)
	b := New(Options{Cols: 10, Rows: 4, HistoryLimit: 100})
	b.Feed(nil, []byte("$ attach"))
	b.Feed(nil, a.Attach(nil, 1, 9))
	want := []string{"$ attach", "L0-abcdefg", "hijkl"}
	if got := historyTexts(b); !slices.Equal(got, want) {
		t.Errorf("scrollback = %q, want %q", got, want)
	}
	assertSameState(t, a, b)
}

func TestResizeRewrapsLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// between is fed while the screen is narrow.
		between string
		want    []string
	}{
		{name: "lines are joined again", want: []string{"abcdefghij", "日本語日本", "x", ""}},
		{name: "a narrow character drawn over the wide one that went on", between: "\x1b[3;1HZ", want: []string{"abcdefghij", "日本語日", "Z", "x"}},
		{name: "output in between", between: "yz\r\n0123456", want: []string{"abcdefghij", "日本語日本", "xyz", "0123456"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := New(Options{Cols: 10, Rows: 4, HistoryLimit: 100})
			a.Feed(nil, []byte("abcdefghij\r\n日本語日本\r\nx"))
			// Narrower, the lines are rewrapped, the wide characters that do
			// not fit going on to the next rows; the rows that no longer fit
			// go into the history.
			a.Resize(5, 4)
			if got, want := screenTexts(a), []string{"日本", "語日", "本", "x"}; !slices.Equal(got, want) {
				t.Fatalf("narrow: %q, want %q", got, want)
			}
			if got, want := historyTexts(a), []string{"abcde", "fghij"}; !slices.Equal(got, want) {
				t.Fatalf("history when narrow: %q, want %q", got, want)
			}
			a.Feed(nil, []byte(tc.between))
			// As wide as at first, the rows are joined again, along with the
			// ones taken back from the history.
			a.Resize(10, 4)
			if got := screenTexts(a); !slices.Equal(got, tc.want) {
				t.Errorf("wide again: %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("cursor", func(t *testing.T) {
		t.Parallel()

		a := New(Options{Cols: 10, Rows: 4, HistoryLimit: 100})
		a.Feed(nil, []byte("abcdefghij\r\n日本語日本\r\nx"))
		a.Resize(5, 4)
		if a.x != 1 || a.y != 3 {
			t.Errorf("narrow: cursor = (%d, %d), want (1, 3), after the x", a.x, a.y)
		}
		a.Resize(10, 4)
		if a.x != 1 || a.y != 2 {
			t.Errorf("wide again: cursor = (%d, %d), want (1, 2)", a.x, a.y)
		}
		if got := historyTexts(a); len(got) != 0 {
			t.Errorf("history = %q, want all taken back", got)
		}
	})
}

func TestResizeTakesLinesBack(t *testing.T) {
	t.Parallel()

	a := New(Options{Cols: 10, Rows: 5, HistoryLimit: 100})
	for i := range 8 {
		a.Feed(nil, []byte("line "+strconv.Itoa(i)+"\r\n"))
	}
	a.Feed(nil, []byte("$ "))
	rows, history := screenTexts(a), historyTexts(a)
	x, y := a.x, a.y

	// Shorter, the screen keeps the cursor, the lines going off its top into
	// the history; taller again, it takes them back.
	a.Resize(10, 2)
	if got, want := screenTexts(a), []string{"line 7", "$"}; !slices.Equal(got, want) {
		t.Errorf("shorter: %q, want %q", got, want)
	}
	a.Resize(10, 5)
	if got := screenTexts(a); !slices.Equal(got, rows) {
		t.Errorf("taller again: %q, want %q", got, rows)
	}
	if got := historyTexts(a); !slices.Equal(got, history) {
		t.Errorf("history: %q, want %q", got, history)
	}
	if a.x != x || a.y != y {
		t.Errorf("cursor = (%d, %d), want (%d, %d)", a.x, a.y, x, y)
	}
}

func TestCatchUpAcrossResizes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// resize resizes the session while the terminal catching up, which
		// is 20 columns by 5 rows, does not show it.
		resize func(a *Terminal)
		// cleared tells whether the scrollback of the terminal is replaced
		// with the history.
		cleared bool
	}{
		{name: "narrower and back", resize: func(a *Terminal) {
			a.Resize(8, 5)
			a.Resize(20, 5)
		}},
		{name: "shorter and back", resize: func(a *Terminal) {
			a.Resize(20, 2)
			a.Resize(20, 5)
		}},
		{name: "taller than the terminal and back", resize: func(a *Terminal) {
			a.Resize(20, 8)
			a.Resize(20, 5)
		}, cleared: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := New(Options{Cols: 20, Rows: 5, HistoryLimit: 100})
			b := New(Options{Cols: 20, Rows: 5, HistoryLimit: 100})
			var out strings.Builder
			for i := range 12 {
				out.WriteString("a rather long line ")
				out.WriteString(strconv.Itoa(i))
				out.WriteString("\r\n")
			}
			a.Feed(nil, []byte(out.String()))
			b.Feed(nil, []byte(out.String()))
			mark := a.Mark()
			tc.resize(a)
			catchUp := a.CatchUp(nil, mark)
			if got := strings.Contains(string(catchUp), "\x1b[3J"); got != tc.cleared {
				t.Errorf("the scrollback is cleared: %t, want %t", got, tc.cleared)
			}
			b.Feed(nil, catchUp)
			if got, want := historyTexts(b), historyTexts(a); !slices.Equal(got, want) {
				t.Errorf("scrollback = %q, want %q", got, want)
			}
			assertSameState(t, a, b)
		})
	}
}

// screenTexts returns the text of the lines of the screen t shows.
func screenTexts(t *Terminal) []string {
	scratch := &testTerminal{Terminal: t}
	return scratch.rows()
}

func TestCatchUpAfterOutputAtAnotherWidth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// cols is the width of the session while the lines are printed, as
		// another terminal is used, and n how long they are.
		cols, n int
	}{
		{name: "wider", cols: 16, n: 13},
		{name: "narrower", cols: 6, n: 9},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := New(Options{Cols: 10, Rows: 4, HistoryLimit: 100})
			b := New(Options{Cols: 10, Rows: 4, HistoryLimit: 100})
			a.Feed(nil, []byte("before\r\n"))
			b.Feed(nil, []byte("before\r\n"))
			mark := a.Mark()
			a.Resize(tc.cols, 4)
			var printed []string
			for i := range 6 {
				line := "L" + strconv.Itoa(i) + strings.Repeat("x", tc.n-2)
				printed = append(printed, line)
				a.Feed(nil, []byte(line+"\r\n"))
			}
			a.Resize(10, 4)
			b.Feed(nil, a.CatchUp(nil, mark))
			// Every line is there whole, once, its rows joined.
			want := append([]string{"before"}, printed...)
			want = append(want, "")
			if got := logicalLines(b); !slices.Equal(got, want) {
				t.Errorf("lines = %q, want %q", got, want)
			}
			assertSameState(t, a, b)
		})
	}
}

// logicalLines returns the lines of the history and of the screen of t, the
// rows that wrapped joined to the ones their lines go on on, without the
// blank rows at the end of the screen but one.
func logicalLines(t *Terminal) []string {
	var lines []string
	line := ""
	add := func(text string, wrapped bool) {
		line += text
		if !wrapped {
			lines = append(lines, line)
			line = ""
		}
	}
	// The text of a row that wrapped goes on to its end: its blanks are
	// part of the line, but for the one left by a wide character.
	for i := range t.history.lines {
		hl := t.history.lines[(t.history.start+i)%len(t.history.lines)]
		cols := hl.width
		if !hl.wrapped {
			cols++
		}
		scratch := New(Options{Cols: cols, Rows: 1})
		scratch.Feed(nil, hl.b)
		l := scratch.screens[0].lines[0]
		l.wrapped = hl.wrapped
		add(rowText(&l), hl.wrapped)
	}
	for y := range t.screens[0].lines {
		l := &t.screens[0].lines[y]
		add(rowText(l), l.wrapped)
	}
	if line != "" {
		lines = append(lines, line)
	}
	for len(lines) > 1 && lines[len(lines)-1] == "" && lines[len(lines)-2] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func TestReplayAtAnotherWidth(t *testing.T) {
	t.Parallel()

	wide := strings.Repeat("日", 7)
	tests := []struct {
		name string
		// cols is the width at which line is printed, which then goes into
		// the history.
		cols int
		line string
	}{
		// At 9 columns, the fifth character does not fit: the first row
		// ends with a blank that is no part of the line.
		{name: "wide characters", cols: 9, line: wide},
		// A line as wide as the terminal it is shown on keeps its last
		// character.
		{name: "as wide as the terminal", cols: 12, line: "abcdefghij"},
	}
	for _, tc := range tests {
		t.Run(tc.name+", attaching", func(t *testing.T) {
			t.Parallel()

			a := New(Options{Cols: tc.cols, Rows: 3, HistoryLimit: 100})
			a.Feed(nil, []byte(tc.line+"\r\na\r\nb\r\nc\r\n"))
			// The session takes the size of the terminal that attaches.
			a.Resize(10, 3)
			b := New(Options{Cols: 10, Rows: 3, HistoryLimit: 100})
			b.Feed(nil, a.Attach(nil, 1, 1))
			if got := logicalLines(b); !slices.Contains(got, tc.line) {
				t.Errorf("lines = %q, want %q among them", got, tc.line)
			}
			assertSameState(t, a, b)
		})
		t.Run(tc.name+", catching up", func(t *testing.T) {
			t.Parallel()

			a := New(Options{Cols: 10, Rows: 3, HistoryLimit: 100})
			b := New(Options{Cols: 10, Rows: 3, HistoryLimit: 100})
			mark := a.Mark()
			// Another terminal is used meanwhile.
			a.Resize(tc.cols, 3)
			a.Feed(nil, []byte(tc.line+"\r\na\r\nb\r\nc\r\n"))
			a.Resize(10, 3)
			b.Feed(nil, a.CatchUp(nil, mark))
			if got := logicalLines(b); !slices.Contains(got, tc.line) {
				t.Errorf("lines = %q, want %q among them", got, tc.line)
			}
			assertSameState(t, a, b)
		})
	}
}

// rowText returns the text of l: up to the end of its text if it wrapped,
// up to the blanks that end it otherwise.
func rowText(l *line) string {
	end, _ := trailingBlank(l)
	if l.wrapped {
		end = l.textEnd()
		if l.padded {
			end = min(end, len(l.cells))
		}
	}
	var s strings.Builder
	for x := 0; x < end; x++ {
		c := l.cells[x]
		switch {
		case c.width == 0:
		case c.r == 0:
			s.WriteByte(' ')
		default:
			s.WriteRune(c.r)
			s.WriteString(c.comb)
		}
	}
	return s.String()
}

func TestLineNoLongerGoingOn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		cols       int
		input      string
		wantLines  []string
		otherWidth int
	}{
		{
			// A progress line cleared from its start, which erases its
			// second row only.
			name:       "next row erased",
			cols:       20,
			input:      "first\r\n" + strings.Repeat("x", 25) + "\r\x1b[K\nline3\r\nline4",
			wantLines:  []string{"first", strings.Repeat("x", 20), "", "line3", "line4"},
			otherWidth: 30,
		},
		{
			// A narrow character drawn where the wide character that left
			// the last cell blank went.
			name:       "wide character drawn over",
			cols:       9,
			input:      "日日日日日\x1b[2;1HZ\r\nline3",
			wantLines:  []string{"日日日日", "Z", "line3"},
			otherWidth: 12,
		},
		{
			name:       "next row deleted",
			cols:       20,
			input:      "first\r\n" + strings.Repeat("x", 25) + "\r\nline3\r\nline4\x1b[3;1H\x1b[M",
			wantLines:  []string{"first", strings.Repeat("x", 20), "line3", "line4"},
			otherWidth: 30,
		},
		{
			name:       "row inserted",
			cols:       20,
			input:      "first\r\n" + strings.Repeat("x", 25) + "\r\nline3\r\nline4\x1b[3;1H\x1b[Lnew",
			wantLines:  []string{"first", strings.Repeat("x", 20), "new", "xxxxx", "line3", "line4"},
			otherWidth: 30,
		},
		{
			// The second row of a line scrolled off the bottom of a region.
			name:       "end scrolled off",
			cols:       20,
			input:      "a\r\nb\r\n" + strings.Repeat("x", 25) + "\r\nline5\x1b[1;4r\x1bM\x1b[r\x1b[6;1H",
			wantLines:  []string{"", "a", "b", strings.Repeat("x", 20), "line5"},
			otherWidth: 30,
		},
		{
			// The first row of the screen deleted, which the newest row of
			// the history went on on.
			name:       "first row deleted",
			cols:       20,
			input:      "first\r\n" + strings.Repeat("x", 25) + "\r\nline3\r\nline4\r\nline5\r\nline6\r\nline7\x1b[H\x1b[M",
			wantLines:  []string{"first", strings.Repeat("x", 20), "line3", "line4", "line5", "line6", "line7"},
			otherWidth: 30,
		},
		{
			// The blanks that end a row of the history are no longer part of
			// its line.
			name:       "first row deleted after blanks",
			cols:       20,
			input:      "abc" + strings.Repeat(" ", 17) + "def\r\nl2\r\nl3\r\nl4\r\nl5\r\nl6\x1b[H\x1b[M",
			wantLines:  []string{"abc", "l2", "l3", "l4", "l5", "l6"},
			otherWidth: 10,
		},
		{
			name:       "row inserted at the top",
			cols:       20,
			input:      "first\r\n" + strings.Repeat("x", 25) + "\r\nline3\r\nline4\r\nline5\r\nline6\r\nline7\x1b[H\x1b[Lnew",
			wantLines:  []string{"first", strings.Repeat("x", 20), "new", "xxxxx", "line3", "line4", "line5", "line6"},
			otherWidth: 30,
		},
	}
	for _, tc := range tests {
		newTerminal := func(cols int) *Terminal {
			return New(Options{Cols: cols, Rows: 6, HistoryLimit: 100})
		}
		t.Run(tc.name+", attaching", func(t *testing.T) {
			t.Parallel()

			a, b := newTerminal(tc.cols), newTerminal(tc.cols)
			a.Feed(nil, []byte(tc.input))
			b.Feed(nil, a.Attach(nil, 1, 1))
			assertSameState(t, a, b)
		})
		t.Run(tc.name+", catching up", func(t *testing.T) {
			t.Parallel()

			a, b := newTerminal(tc.cols), newTerminal(tc.cols)
			mark := a.Mark()
			a.Feed(nil, []byte(tc.input))
			b.Feed(nil, []byte("stale"))
			b.Feed(nil, a.CatchUp(nil, mark))
			assertSameState(t, a, b)
		})
		t.Run(tc.name+", drawn anew", func(t *testing.T) {
			t.Parallel()

			a, b := newTerminal(tc.cols), newTerminal(tc.cols)
			a.Feed(nil, []byte(tc.input))
			b.Feed(nil, []byte("stale\r\nlines\r\nhere"))
			b.Feed(nil, a.Redraw(nil))
			assertSameState(t, a, b)
		})
		t.Run(tc.name+", from the history", func(t *testing.T) {
			t.Parallel()

			// The lines go into the history, and a terminal of another
			// width attaches.
			a := newTerminal(tc.cols)
			a.Feed(nil, []byte(tc.input+strings.Repeat("\r\n", 6)))
			a.Resize(tc.otherWidth, 6)
			b := newTerminal(tc.otherWidth)
			b.Feed(nil, a.Attach(nil, 1, 1))
			got := logicalLines(b)
			if len(got) < len(tc.wantLines) || !slices.Equal(got[:len(tc.wantLines)], tc.wantLines) {
				t.Errorf("lines = %q, want them to start with %q", got, tc.wantLines)
			}
		})
		t.Run(tc.name+", resized", func(t *testing.T) {
			t.Parallel()

			a := newTerminal(tc.cols)
			a.Feed(nil, []byte(tc.input))
			rows := screenTexts(a)
			a.Resize(tc.otherWidth, 6)
			if got := logicalLines(a); len(got) < len(tc.wantLines) || !slices.Equal(got[:len(tc.wantLines)], tc.wantLines) {
				t.Errorf("lines at %d columns = %q, want them to start with %q", tc.otherWidth, got, tc.wantLines)
			}
			a.Resize(tc.cols, 6)
			if got := screenTexts(a); !slices.Equal(got, rows) {
				t.Errorf("rows = %q, want %q", got, rows)
			}
		})
	}
}

func TestBlankRowsGoingOnALine(t *testing.T) {
	t.Parallel()

	// A row that wrapped with nothing but blanks on it, at the end of the
	// line of the row above, which the terminal wraps to only once something
	// is drawn there.
	tests := []struct {
		name  string
		rows  int
		input string
	}{
		{name: "spaces", rows: 5, input: "X" + strings.Repeat(" ", 8) + "\r\nZ"},
		{name: "the next row erased", rows: 5, input: "abcd    x\x1b[2K\r\nZ"},
		{name: "a wide character that went on, erased", rows: 5, input: "abcdefgh\x1b[2K日\x1b[2K\r\nZ"},
		{name: "the first row, after the history", rows: 3, input: "abcd    x\x1b[2K\r\nZ"},
	}
	for _, tc := range tests {
		newTerminal := func() *Terminal {
			return New(Options{Cols: 4, Rows: tc.rows, HistoryLimit: 100})
		}
		t.Run(tc.name+", attaching", func(t *testing.T) {
			t.Parallel()

			a, b := newTerminal(), newTerminal()
			a.Feed(nil, []byte(tc.input))
			b.Feed(nil, a.Attach(nil, 1, 1))
			assertSameState(t, a, b)
		})
		t.Run(tc.name+", catching up", func(t *testing.T) {
			t.Parallel()

			a, b := newTerminal(), newTerminal()
			mark := a.Mark()
			a.Feed(nil, []byte(tc.input))
			b.Feed(nil, []byte("stale"))
			b.Feed(nil, a.CatchUp(nil, mark))
			assertSameState(t, a, b)
		})
		t.Run(tc.name+", drawn anew", func(t *testing.T) {
			t.Parallel()

			a, b := newTerminal(), newTerminal()
			a.Feed(nil, []byte(tc.input))
			b.Feed(nil, []byte("1111222233334444"))
			b.Feed(nil, a.Redraw(nil))
			assertSameState(t, a, b)
		})
	}
}

func TestReplayKeepsTheEndOfALineFromTheHistory(t *testing.T) {
	t.Parallel()

	const line = "0123456789abcdefghijklmn"
	tests := []struct {
		name string
		// output is printed at cols columns, and replayed at width.
		cols, width  int
		output, line string
	}{
		{
			// The first row of the line goes into the history at 10
			// columns, and the rest is on the screen at 12. A terminal of
			// 12 columns wraps the line from its start, so that it ends
			// right at the end of a row there.
			name: "a line going on on the screen", cols: 10, width: 12,
			output: line + "\r\n", line: line,
		},
		{
			name: "a line going on on the screen, on a background", cols: 10, width: 12,
			output: "\x1b[42m" + line + "\x1b[K\x1b[0m\r\n", line: line,
		},
		{
			// The row goes into the history at 12 columns, and ends right
			// at the end of a row of a terminal of 10.
			name: "a row of the history on a background", cols: 12, width: 10,
			output: "\x1b[42m0123456789\x1b[K\x1b[0m\r\na\r\nb\r\nc", line: "0123456789",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name+", attaching", func(t *testing.T) {
			t.Parallel()

			a := New(Options{Cols: tc.cols, Rows: 3, HistoryLimit: 100})
			a.Feed(nil, []byte(tc.output))
			a.Resize(tc.width, 3)
			b := New(Options{Cols: tc.width, Rows: 3, HistoryLimit: 100})
			b.Feed(nil, a.Attach(nil, 1, 1))
			if got := logicalLines(b); !slices.Contains(got, tc.line) {
				t.Errorf("lines = %q, want %q among them", got, tc.line)
			}
		})
		t.Run(tc.name+", catching up", func(t *testing.T) {
			t.Parallel()

			a := New(Options{Cols: tc.width, Rows: 3, HistoryLimit: 100})
			b := New(Options{Cols: tc.width, Rows: 3, HistoryLimit: 100})
			mark := a.Mark()
			a.Resize(tc.cols, 3)
			a.Feed(nil, []byte(tc.output))
			a.Resize(tc.width, 3)
			b.Feed(nil, a.CatchUp(nil, mark))
			if got := logicalLines(b); !slices.Contains(got, tc.line) {
				t.Errorf("lines = %q, want %q among them", got, tc.line)
			}
		})
	}
}

func TestReplayErasesWithTheBackgroundOfARow(t *testing.T) {
	t.Parallel()

	// A row of the history whose blanks have a background of their own, at
	// the width it went into the history at, and at a wider one.
	for _, width := range []int{12, 16} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			t.Parallel()

			a := New(Options{Cols: 12, Rows: 3, HistoryLimit: 100})
			a.Feed(nil, []byte("\x1b[42mbar\x1b[K\x1b[0m\r\na\r\nb\r\nc"))
			a.Resize(width, 3)
			b := New(Options{Cols: width, Rows: 4, HistoryLimit: 100})
			b.Feed(nil, a.Attach(nil, 1, 1))
			// The row is the first one of the terminal, which has a row more.
			want := blank(paletteColor(2))
			for x, c := range b.screens[0].lines[0].cells[3:] {
				if visible(c) != want {
					t.Fatalf("cell %d = %+v, want %+v", x+3, c, want)
				}
			}
		})
	}
}

func TestReplayOfRandomOutput(t *testing.T) {
	t.Parallel()

	// Pieces of what programs print, put together at random: text, wide
	// characters, moving the cursor, erasing, editing lines, scrolling,
	// colors and the alternate screen.
	pieces := [...]string{
		"a", "bcd", "efghijk", "日", "本語", " ", "   ", "\t",
		"\r", "\n", "\r\n", "\b",
		"\x1b[K", "\x1b[1K", "\x1b[2K", "\x1b[J", "\x1b[1J", "\x1b[2J", "\x1b[3J",
		"\x1b[A", "\x1b[B", "\x1b[C", "\x1b[D", "\x1b[H", "\x1b[2;3H", "\x1b[3;1H", "\x1b[1;5H",
		"\x1b[L", "\x1b[M", "\x1b[P", "\x1b[@", "\x1b[X", "\x1b[2X",
		"\x1bM", "\x1bD", "\x1bE", "\x1b[S", "\x1b[T",
		"\x1b[2;3r", "\x1b[r", "\x1b7", "\x1b8",
		"\x1b[31m", "\x1b[42m", "\x1b[0m",
		"\x1b[?1049h", "\x1b[?1049l",
	}
	sizes := [...]struct{ cols, rows int }{{4, 3}, {5, 4}, {7, 3}}
	// A xorshift generator, for the same output on every run.
	seed := uint64(1)
	random := func(n uint64) uint64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return seed % n
	}
	output := func() string {
		var sb strings.Builder
		for range 1 + random(30) {
			sb.WriteString(pieces[random(uint64(len(pieces)))])
		}
		return sb.String()
	}

	failures := 0
	for range 3000 {
		before, after := output(), output()
		size := sizes[random(uint64(len(sizes)))]
		newTerminal := func() *Terminal {
			return New(Options{Cols: size.cols, Rows: size.rows, HistoryLimit: 100})
		}
		for _, r := range [...]struct {
			name string
			// replay returns a terminal that is brought to the state of a,
			// which printed before and after.
			replay func(a *Terminal) *Terminal
		}{
			{name: "attaching", replay: func(a *Terminal) *Terminal {
				a.Feed(nil, []byte(before+after))
				b := newTerminal()
				b.Feed(nil, a.Attach(nil, 1, 1))
				return b
			}},
			{name: "catching up", replay: func(a *Terminal) *Terminal {
				mark := a.Mark()
				a.Feed(nil, []byte(before+after))
				b := newTerminal()
				b.Feed(nil, []byte("stale\r\nrows"))
				b.Feed(nil, a.CatchUp(nil, mark))
				return b
			}},
			{name: "catching up after output", replay: func(a *Terminal) *Terminal {
				a.Feed(nil, []byte(before))
				b := newTerminal()
				b.Feed(nil, a.Attach(nil, 1, 1))
				mark := a.Mark()
				a.Feed(nil, []byte(after))
				b.Feed(nil, a.CatchUp(nil, mark))
				return b
			}},
			{name: "drawn anew", replay: func(a *Terminal) *Terminal {
				a.Feed(nil, []byte(before+after))
				b := newTerminal()
				b.Feed(nil, []byte("stale\r\nrows"))
				b.Feed(nil, a.Redraw(nil))
				return b
			}},
		} {
			a := newTerminal()
			b := r.replay(a)
			if diffs := stateDiff(a, b); len(diffs) > 0 {
				t.Errorf("%s, %dx%d, after %q then %q:\n%s", r.name, size.cols, size.rows, before, after, strings.Join(diffs, "\n"))
				failures++
			}
		}
		if failures >= 10 {
			break
		}
	}
}

func TestResizeKeepsTheRowsOfALineEndingInBlanks(t *testing.T) {
	t.Parallel()

	// The line of X and spaces wraps to a row that then shows nothing but
	// blanks, and goes on to the next row only with its last space.
	const input = "X                \r\nAFTER"
	tests := []struct {
		name       string
		cols, rows int
	}{
		{name: "fewer rows", cols: 8, rows: 5},
		{name: "more rows", cols: 8, rows: 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tt := New(Options{Cols: 8, Rows: 6, HistoryLimit: 100})
			tt.Feed(nil, []byte(input))
			tt.Resize(tc.cols, tc.rows)
			want := []string{"X", "", "", "AFTER"}
			if got := screenTexts(tt); len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
				t.Errorf("rows = %q, want them to start with %q", got, want)
			}
			if tt.y != 3 {
				t.Errorf("cursor row = %d, want 3", tt.y)
			}
		})
	}
}
