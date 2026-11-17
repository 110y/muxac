package mux

import (
	"bytes"
	"fmt"
)

// Keys that control an attached client, as in tmux: the prefix key, Ctrl+b
// unless envPrefix names another, followed by d detaches. The prefix key
// pressed twice sends it to the session. Any other key after the prefix key
// is ignored.
const (
	defaultPrefix = 0x02 // Ctrl+b
	detachKey     = 'd'
)

// envPrefix names the environment variable that sets the prefix key, such as
// "C-o".
const envPrefix = "MUXAC_PREFIX"

// prefixKey returns the prefix key named s, a control key written as "C-x" or
// "^x", such as "C-o" for Ctrl+o. An empty s names the default, Ctrl+b.
func prefixKey(s string) (byte, error) {
	if s == "" {
		return defaultPrefix, nil
	}
	var c byte
	switch {
	case len(s) == 3 && (s[:2] == "C-" || s[:2] == "c-"):
		c = s[2]
	case len(s) == 2 && s[0] == '^':
		c = s[1]
	default:
		return 0, fmt.Errorf("%s=%q: want a control key, such as C-b", envPrefix, s)
	}
	if c >= 'a' && c <= 'z' {
		c -= 'a' - 'A'
	}
	// ESC would take the escape sequences of other keys for the prefix key.
	if c < 'A' || c > '_' || c == '[' {
		return 0, fmt.Errorf("%s=%q: want a control key, such as C-b", envPrefix, s)
	}
	return c & 0x1f, nil
}

// Bracketed paste markers: what is pasted is never taken for keys that
// control the client.
var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

// keyScanner picks the keys that control the client out of the terminal's
// input.
type keyScanner struct {
	// prefix is the prefix key, defaultPrefix if 0.
	prefix byte
	// prefixed is set after the prefix key.
	prefixed bool
	// skip is the state of skipping the key that follows the prefix key
	// when it is not a single byte.
	skip skipState
	// utf8Left counts the bytes left of a UTF-8 character being skipped.
	utf8Left int
	// pasting is set inside a bracketed paste.
	pasting bool
	// recent holds the last bytes of input, to find the paste markers.
	recent []byte
}

type skipState uint8

const (
	skipNone skipState = iota
	// skipEscape follows an ESC.
	skipEscape
	// skipCSI is inside a control sequence.
	skipCSI
	// skipSS3 expects the final byte of an SS3 sequence.
	skipSS3
)

// scan returns the input to pass on to the session, and whether the detach
// key was pressed, in which case the input after it is dropped.
func (k *keyScanner) scan(in []byte) ([]byte, bool) {
	out := make([]byte, 0, len(in))
	for _, b := range in {
		k.track(b)
		switch {
		case k.skip != skipNone:
			k.skipByte(b)
		case k.utf8Left > 0:
			if b&0xc0 == 0x80 {
				k.utf8Left--
			} else {
				k.utf8Left = 0
				out = k.key(out, b)
			}
		case k.prefixed:
			k.prefixed = false
			switch {
			case b == detachKey:
				return out, true
			case b == k.prefixKey():
				out = append(out, b)
			case b == 0x1b:
				k.skip = skipEscape
			case b >= 0xc0:
				k.utf8Left = utf8Length(b) - 1
			}
		default:
			out = k.key(out, b)
		}
	}
	return out, false
}

// key handles a byte outside of the keys that follow the prefix key.
func (k *keyScanner) key(out []byte, b byte) []byte {
	if b == k.prefixKey() && !k.pasting {
		k.prefixed = true
		return out
	}
	return append(out, b)
}

func (k *keyScanner) prefixKey() byte {
	if k.prefix == 0 {
		return defaultPrefix
	}
	return k.prefix
}

// skipByte skips a byte of the escape sequence of a key that follows the
// prefix key.
func (k *keyScanner) skipByte(b byte) {
	switch k.skip {
	case skipEscape:
		switch b {
		case '[':
			k.skip = skipCSI
		case 'O':
			k.skip = skipSS3
		default:
			k.skip = skipNone
		}
	case skipCSI:
		if b >= 0x40 && b <= 0x7e {
			k.skip = skipNone
		}
	case skipSS3, skipNone:
		k.skip = skipNone
	}
}

// track follows the bracketed paste markers in the input.
func (k *keyScanner) track(b byte) {
	k.recent = append(k.recent, b)
	if len(k.recent) > len(pasteStart) {
		k.recent = k.recent[1:]
	}
	switch {
	case bytes.Equal(k.recent, pasteStart):
		k.pasting = true
	case bytes.Equal(k.recent, pasteEnd):
		k.pasting = false
	}
}

// utf8Length returns the length of the UTF-8 sequence that starts with the
// byte b.
func utf8Length(b byte) int {
	switch {
	case b >= 0xf0:
		return 4
	case b >= 0xe0:
		return 3
	default:
		return 2
	}
}
