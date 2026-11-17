package mux

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// protocolVersion is the version of the protocol spoken over the socket of a
// session. A session's server keeps running the muxac it was started with,
// so a newer muxac must still understand it.
const protocolVersion = 1

// msgType is the type of a message exchanged over the socket of a session.
// Every message is a frame: the type, the length of the payload as a 32-bit
// big-endian integer, and the payload.
type msgType uint8

const (
	// msgRequest is the first message of every connection, from the client:
	// a JSON request.
	msgRequest msgType = iota + 1
	// msgInput carries input for the program, from an attached client.
	msgInput
	// msgResize carries the new size of an attached client's terminal, as
	// encoded by encodeSize.
	msgResize
	// msgDetach tells that an attached client detaches.
	msgDetach
	// msgOutput carries output for an attached client's terminal.
	msgOutput
	// msgExit ends an attachment, from the server. Its payload tells why:
	// exitDetached or exitExited.
	msgExit
	// msgResponse answers a request other than attaching, from the server: a
	// JSON response.
	msgResponse
)

// maxFrame bounds the payload of a frame.
const maxFrame = 64 << 20

// Operations a request asks for.
const (
	opAttach  = "attach"
	opInfo    = "info"
	opCapture = "capture"
	opKill    = "kill"
)

// request is the first message a client sends on a connection.
type request struct {
	Version int    `json:"version"`
	Op      string `json:"op"`
	// Size and Colors describe the terminal of a client that attaches, and
	// Cursor tells where its cursor is: the row and the column, 1-based, or
	// zeros if unknown.
	Size   winsize   `json:"size"`
	Colors [2]string `json:"colors"`
	Cursor [2]int    `json:"cursor"`
}

// winsize is the size of a terminal.
type winsize struct {
	Cols   int `json:"cols"`
	Rows   int `json:"rows"`
	XPixel int `json:"xpixel"`
	YPixel int `json:"ypixel"`
}

// response answers a request other than attaching.
type response struct {
	Name  string `json:"name,omitempty"`
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
}

// Reasons an attachment ends.
const (
	exitDetached = "detached"
	exitExited   = "exited"
)

// frame is a message.
type frame struct {
	typ     msgType
	payload []byte
}

func writeFrame(w io.Writer, typ msgType, payload []byte) error {
	n := len(payload)
	if n < 0 || n > maxFrame {
		return fmt.Errorf("frame of %d bytes is too large", n)
	}
	b := make([]byte, 5, 5+n)
	b[0] = byte(typ)
	binary.BigEndian.PutUint32(b[1:], uint32(n))
	_, err := w.Write(append(b, payload...))
	return err
}

func writeJSON(w io.Writer, typ msgType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFrame(w, typ, b)
}

func readFrame(r io.Reader) (msgType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes is too large", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return msgType(hdr[0]), payload, nil
}

// sizeBytes is the length of an encoded size.
const sizeBytes = 16

// encodeSize encodes s as four 32-bit big-endian integers.
func encodeSize(s winsize) []byte {
	b := make([]byte, sizeBytes)
	for i, v := range [...]int{s.Cols, s.Rows, s.XPixel, s.YPixel} {
		binary.BigEndian.PutUint32(b[4*i:], uint32(min(max(v, 0), 0xffff)))
	}
	return b
}

// decodeSize decodes a size encoded by encodeSize.
func decodeSize(b []byte) (winsize, error) {
	if len(b) != sizeBytes {
		return winsize{}, fmt.Errorf("a size of %d bytes, not %d", len(b), sizeBytes)
	}
	v := func(i int) int { return int(binary.BigEndian.Uint32(b[4*i:])) }
	return winsize{Cols: v(0), Rows: v(1), XPixel: v(2), YPixel: v(3)}, nil
}
