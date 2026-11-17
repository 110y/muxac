package mux

import (
	"errors"
	"io"
	"net"
	"sync"
)

// outbox queues the frames for an attached client, so that a slow client
// never holds up the session. Pushing never blocks.
type outbox struct {
	mu      sync.Mutex
	frames  []frame
	pending int
	closed  bool
	// wake is signaled whenever frames or closed changes.
	wake chan struct{}
}

func newOutbox() *outbox {
	return &outbox{wake: make(chan struct{}, 1)}
}

// push queues f. It reports false, queuing nothing, if too much output is
// pending already.
func (o *outbox) push(f frame) bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.closed {
		return true
	}
	if f.typ == msgOutput && o.pending+len(f.payload) > maxPendingOutput {
		return false
	}
	o.frames = append(o.frames, f)
	o.pending += len(f.payload)
	o.signal()
	return true
}

// pushUncapped queues f however much output is pending already, for a frame
// that brings the terminal up to date, which nothing pushed later would do.
func (o *outbox) pushUncapped(f frame) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.closed {
		return
	}
	o.frames = append(o.frames, f)
	o.pending += len(f.payload)
	o.signal()
}

// replace drops the pending frames and queues f instead.
func (o *outbox) replace(f frame) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.closed {
		return
	}
	o.frames = append(o.frames[:0], f)
	o.pending = len(f.payload)
	o.signal()
}

// close stops accepting frames. run returns once it has written the frames
// pushed before.
func (o *outbox) close() {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.closed = true
	o.signal()
}

func (o *outbox) signal() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// run writes the queued frames to conn until the outbox is closed and
// drained, or writing fails, and then closes conn. It returns the error that
// stopped it.
func (o *outbox) run(conn net.Conn) error {
	for range o.wake {
		for {
			o.mu.Lock()
			frames := o.frames
			o.frames = nil
			o.pending = 0
			closed := o.closed
			o.mu.Unlock()

			for _, f := range frames {
				if err := writeFrame(conn, f.typ, f.payload); err != nil {
					o.close()
					return errors.Join(err, conn.Close())
				}
			}
			if len(frames) > 0 {
				continue
			}
			if closed {
				return conn.Close()
			}
			break
		}
	}
	return conn.Close()
}

// inbox queues the input for the program: what the attached clients send and
// the replies to the program's queries. Pushing never blocks.
type inbox struct {
	mu      sync.Mutex
	chunks  [][]byte
	pending int
	wake    chan struct{}
}

func newInbox() *inbox {
	return &inbox{wake: make(chan struct{}, 1)}
}

// push queues b, unless too much input is pending already, as when the
// program does not read its input.
func (in *inbox) push(b []byte) {
	if len(b) == 0 {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()

	if in.pending+len(b) > maxPendingInput {
		return
	}
	in.chunks = append(in.chunks, append([]byte(nil), b...))
	in.pending += len(b)
	select {
	case in.wake <- struct{}{}:
	default:
	}
}

// run writes the queued input to w until writing fails, and returns the
// failure.
func (in *inbox) run(w io.Writer) error {
	for range in.wake {
		for {
			in.mu.Lock()
			chunks := in.chunks
			in.chunks = nil
			in.pending = 0
			in.mu.Unlock()

			if len(chunks) == 0 {
				break
			}
			for _, b := range chunks {
				if _, err := w.Write(b); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
