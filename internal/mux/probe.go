package mux

import (
	"bytes"
	"fmt"
	"os"
	"time"
)

// probeQueries asks the terminal for its default foreground and background
// colors (OSC 10 and 11) and for the position of its cursor (CPR), and then
// for its primary device attributes (DA1), which every terminal answers: the
// answer to DA1 tells that no other answer is coming anymore.
const probeQueries = "\x1b]10;?\x1b\\\x1b]11;?\x1b\\\x1b[6n\x1b[c"

// probeResult is what a terminal answered to probeQueries.
type probeResult struct {
	// colors are the default colors, in the form the terminal reported them
	// (such as "rgb:ffff/ffff/ffff"), or "" for one it did not report.
	colors [2]string
	// cursor is the row and the column of the cursor, 1-based, or zeros if
	// the terminal did not report it.
	cursor [2]int
}

// probeTerminal asks the terminal tty out for its default colors, so that the
// session can answer the same questions from its program as tmux does, and
// for the position of its cursor, which tells where what it shows ends. It
// also returns the input read meanwhile that was not an answer, which belongs
// to the session.
func probeTerminal(tty *os.File, input *inputReader) (probeResult, []byte, error) {
	var res probeResult
	if _, err := tty.WriteString(probeQueries); err != nil {
		return res, nil, fmt.Errorf("write to the terminal: %w", err)
	}
	timeout := time.After(probeTimeout)
	var buf []byte
	for {
		select {
		case b, ok := <-input.chunks:
			if !ok {
				// The terminal is gone; the relay tells so.
				rest, _ := parseProbeReplies(buf, &res)
				return res, rest, nil
			}
			buf = append(buf, b...)
			if rest, answered := parseProbeReplies(buf, &res); answered {
				return res, rest, nil
			}
		case <-timeout:
			// The terminal did not answer DA1: what it answered so far is all
			// there is to know.
			rest, _ := parseProbeReplies(buf, &res)
			return res, rest, nil
		}
	}
}

// parseProbeReplies takes the answers to probeQueries out of the input in,
// recording them in res, and reports whether the answer to DA1 was among
// them. It returns the rest of the input.
func parseProbeReplies(in []byte, res *probeResult) ([]byte, bool) {
	var rest []byte
	done := false
	for len(in) > 0 {
		i := bytes.IndexByte(in, 0x1b)
		if i < 0 {
			rest = append(rest, in...)
			break
		}
		rest = append(rest, in[:i]...)
		in = in[i:]

		if n, color, value := oscColorReply(in); n > 0 {
			res.colors[color] = value
			in = in[n:]
			continue
		}
		if n, row, col := cursorReply(in); n > 0 {
			res.cursor = [2]int{row, col}
			in = in[n:]
			continue
		}
		if n := da1Reply(in); n > 0 {
			done = true
			in = in[n:]
			continue
		}
		rest = append(rest, in[0])
		in = in[1:]
	}
	return rest, done
}

// oscColorReply parses an answer to OSC 10 or 11 at the start of b, such as
// "ESC ] 11 ; rgb:0000/0000/0000 ESC \", and returns its length, which color
// it reports (0 for the foreground, 1 for the background) and the color. It
// returns a length of 0 if b does not start with such an answer.
func oscColorReply(b []byte) (int, int, string) {
	var color int
	switch {
	case bytes.HasPrefix(b, []byte("\x1b]10;")):
		color = 0
	case bytes.HasPrefix(b, []byte("\x1b]11;")):
		color = 1
	default:
		return 0, 0, ""
	}
	const prefix = len("\x1b]10;")
	for i := prefix; i < len(b); i++ {
		switch {
		case b[i] == 0x07:
			return i + 1, color, string(b[prefix:i])
		case b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\':
			return i + 2, color, string(b[prefix:i])
		case b[i] < 0x20:
			return 0, 0, ""
		}
	}
	return 0, 0, ""
}

// cursorReply parses an answer to CPR at the start of b, such as
// "ESC [ 12 ; 1 R", and returns its length and the position it reports. It
// returns a length of 0 if b does not start with such an answer.
func cursorReply(b []byte) (int, int, int) {
	if !bytes.HasPrefix(b, []byte("\x1b[")) {
		return 0, 0, 0
	}
	end := bytes.IndexByte(b, 'R')
	if end < 0 {
		return 0, 0, 0
	}
	row, col, ok := bytes.Cut(b[2:end], []byte(";"))
	if !ok {
		return 0, 0, 0
	}
	r, rok := positive(row)
	c, cok := positive(col)
	if !rok || !cok {
		// Not a cursor position: something else that ends with "R".
		return 0, 0, 0
	}
	return end + 1, r, c
}

// positive returns the positive decimal number b holds, if it holds one.
func positive(b []byte) (int, bool) {
	if len(b) == 0 || len(b) > 5 {
		return 0, false
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, n > 0
}

// da1Reply returns the length of the answer to DA1 at the start of b, such as
// "ESC [ ? 62 ; 22 c", or 0 if b does not start with one.
func da1Reply(b []byte) int {
	if !bytes.HasPrefix(b, []byte("\x1b[?")) {
		return 0
	}
	for i := 3; i < len(b); i++ {
		switch c := b[i]; {
		case c == 'c':
			return i + 1
		case c >= '0' && c <= '9', c == ';':
		default:
			return 0
		}
	}
	return 0
}
