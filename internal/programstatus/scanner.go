package programstatus

import "bytes"

const (
	esc = 0x1b
	bel = 0x07
	can = 0x18
	sub = 0x1a
)

// oscPrefix is what follows ESC ] in every OSC 7501 sequence.
const oscPrefix = "7501;"

// Handler receives what a Scanner finds in a program's output.
type Handler interface {
	// Query is called when the program asks whether the terminal speaks the
	// protocol. terminator is the string terminator the query used; the reply
	// must use the same one. The reply has to reach the program before anything
	// the program wrote after the query is forwarded to the terminal: Claude Code
	// writes the query together with a primary device attributes request (DA1)
	// and only enables the protocol if the reply to the query arrives first.
	Query(terminator string)
	// Report is called with every valid status report, in output order.
	Report(r Report)
}

// Reply returns the reply to a support query that used terminator.
func Reply(terminator string) []byte {
	return []byte("\x1b]" + oscPrefix + "?" + terminator)
}

type scanState int

const (
	// stGround is outside any escape sequence.
	stGround scanState = iota
	// stEscape follows an ESC.
	stEscape
	// stPrefix is inside "ESC ]", matching oscPrefix.
	stPrefix
	// stPayload is inside the payload of an OSC 7501 sequence.
	stPayload
	// stPayloadEscape follows an ESC inside the payload, which is either the
	// first byte of the string terminator (ESC \) or the end of the sequence.
	stPayloadEscape
)

// Scanner extracts OSC 7501 sequences from a program's terminal output and
// passes everything else through unchanged. OSC 7501 sequences are consumed:
// the scanner answers queries on behalf of the terminal and hands reports to
// the Handler. A sequence may be split across any number of Scan calls.
//
// The zero value is ready to use.
type Scanner struct {
	state scanState
	// held are bytes withheld from the terminal while it is not yet known
	// whether they start an OSC 7501 sequence.
	held []byte
	// matched is how many bytes of oscPrefix have been seen.
	matched int
	payload []byte
	// overflow is set once a sequence exceeds the protocol's size limit; the
	// rest of it is discarded and it is dropped when it ends.
	overflow bool
}

// Scan processes the next chunk of program output, calling h for every OSC
// 7501 sequence it completes, and returns dst with the bytes that must be
// forwarded to the terminal appended.
//
// Callers must forward the returned bytes only after Scan returns, so that
// replies made by h.Query reach the program before any later output reaches
// the terminal.
func (s *Scanner) Scan(dst, p []byte, h Handler) []byte {
	for i := 0; i < len(p); i++ {
		b := p[i]
		switch s.state {
		case stGround:
			n := bytes.IndexByte(p[i:], esc)
			if n < 0 {
				return append(dst, p[i:]...)
			}
			dst = append(dst, p[i:i+n]...)
			i += n
			s.held = append(s.held[:0], esc)
			s.state = stEscape

		case stEscape:
			if b != ']' {
				dst = s.release(dst)
				i-- // not ours: process b again outside any sequence
				continue
			}
			s.held = append(s.held, b)
			s.matched = 0
			s.state = stPrefix

		case stPrefix:
			if b != oscPrefix[s.matched] {
				dst = s.release(dst)
				i--
				continue
			}
			s.held = append(s.held, b)
			s.matched++
			if s.matched == len(oscPrefix) {
				s.held = s.held[:0]
				s.payload = s.payload[:0]
				s.overflow = false
				s.state = stPayload
			}

		case stPayload:
			switch b {
			case bel:
				s.finish("\x07", h)
				s.state = stGround
			case esc:
				s.state = stPayloadEscape
			case can, sub:
				// CAN and SUB cancel the sequence. Like any other control in
				// ground state they are then passed on to the terminal.
				s.state = stGround
				i--
			default:
				if len(s.payload) >= maxSequenceBytes {
					s.overflow = true
					continue
				}
				s.payload = append(s.payload, b)
			}

		case stPayloadEscape:
			if b == '\\' {
				s.finish("\x1b\\", h)
				s.state = stGround
				continue
			}
			// ESC followed by anything but "\" ends the sequence without a
			// terminator and starts another escape sequence, so the incomplete
			// report is dropped and the ESC is processed again.
			s.held = append(s.held[:0], esc)
			s.state = stEscape
			i--
		}
	}
	return dst
}

// Flush returns dst with any bytes that are still withheld appended. It is
// called once the program's output has ended. An unterminated OSC 7501
// sequence is dropped.
func (s *Scanner) Flush(dst []byte) []byte {
	if s.state == stEscape || s.state == stPrefix {
		dst = append(dst, s.held...)
	}
	s.held = s.held[:0]
	s.payload = s.payload[:0]
	s.state = stGround
	return dst
}

// release forwards the withheld bytes, which turned out not to start an OSC
// 7501 sequence, and returns to ground state.
func (s *Scanner) release(dst []byte) []byte {
	dst = append(dst, s.held...)
	s.held = s.held[:0]
	s.state = stGround
	return dst
}

// finish handles a complete OSC 7501 sequence terminated by terminator.
func (s *Scanner) finish(terminator string, h Handler) {
	if s.overflow || len("\x1b]")+len(oscPrefix)+len(s.payload)+len(terminator) > maxSequenceBytes {
		return
	}
	if len(s.payload) > 0 && s.payload[0] == '?' {
		h.Query(terminator)
		return
	}
	if r, ok := parseReport(string(s.payload)); ok {
		h.Report(r)
	}
}
