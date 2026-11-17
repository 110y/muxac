package mux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/110y/muxac/internal/pty"
	"github.com/110y/muxac/internal/signals"
)

// probeTimeout bounds how long attaching waits for the terminal to answer
// the questions about its colors.
const probeTimeout = 500 * time.Millisecond

// detachTimeout bounds how long detaching waits for the server to undo what
// the session set up on the terminal.
const detachTimeout = 2 * time.Second

// ErrLostServer tells that the server of the session went away while a
// terminal was attached. The client has told so on the terminal already.
var ErrLostServer = errors.New("lost server")

// AttachSession attaches the terminal muxac runs in to the session until it
// detaches or the session ends.
func (c *Client) AttachSession(ctx context.Context, sessionName string) error {
	return c.attachTerminal(ctx, sessionName, nil)
}

// NewSession creates a session that runs command in dir, with env added to
// muxac's environment, and attaches the terminal muxac runs in to it until
// it detaches or the session ends.
func (c *Client) NewSession(ctx context.Context, sessionName, dir string, env []string, command string) error {
	return c.attachTerminal(ctx, sessionName, func(ctx context.Context, conn *os.File, size winsize, colors [2]string) error {
		return c.startServer(ctx, config{
			Name:    sessionName,
			Dir:     dir,
			Env:     sessionEnv(sessionName, env),
			Command: command,
			Size:    size,
			Colors:  colors,
			Creator: true,
		}, conn)
	})
}

// createFunc creates a session whose creator attaches over conn.
type createFunc func(ctx context.Context, conn *os.File, size winsize, colors [2]string) error

// attachTerminal attaches the terminal to the session, creating the session
// first if create is not nil.
//
// As with tmux, the terminal is the one on the standard input: the session is
// shown on it as well. The standard output only gets the line telling how
// the attachment ended, so that a script can attach with the output
// captured.
func (c *Client) attachTerminal(ctx context.Context, sessionName string, create createFunc) (err error) {
	in, out := c.Stdin, c.Stdout
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	if !term.IsTerminal(int(in.Fd())) {
		return errors.New("open terminal failed: not a terminal")
	}
	if os.Getenv(envSocket) == socketPath(socketDirPath(), sessionName) {
		return fmt.Errorf("cannot attach session %s to itself", sessionName)
	}
	prefix, err := prefixKey(os.Getenv(envPrefix))
	if err != nil {
		return err
	}

	conn, creator, err := c.connect(ctx, sessionName, create != nil)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeIgnoringClosed(conn)) }()

	state, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return errors.Join(fmt.Errorf("put the terminal into raw mode: %w", err), closeFile(creator))
	}
	restored := false
	restore := func() error {
		if restored {
			return nil
		}
		restored = true
		if err := term.Restore(int(in.Fd()), state); err != nil {
			return fmt.Errorf("restore the terminal: %w", err)
		}
		return nil
	}
	defer func() { err = errors.Join(err, restore()) }()

	input := readInput(in)
	probed, pending, err := probeTerminal(in, input)
	if err != nil {
		return errors.Join(err, closeFile(creator))
	}
	size, err := terminalSize(in)
	if err != nil {
		return errors.Join(err, closeFile(creator))
	}

	req := request{Version: protocolVersion, Op: opAttach, Size: size, Colors: probed.colors, Cursor: probed.cursor}
	if err := writeJSON(conn, msgRequest, req); err != nil {
		return errors.Join(err, closeFile(creator))
	}
	if create != nil {
		err := create(ctx, creator, size, probed.colors)
		if err := errors.Join(err, closeFile(creator)); err != nil {
			return err
		}
	}

	reason, err := relay(ctx, conn, input, pending, in, keyScanner{prefix: prefix}, size)
	if err := restore(); err != nil {
		return err
	}
	var msg string
	switch {
	case errors.Is(err, ErrLostServer):
		msg = "[lost server]"
	case err != nil:
		return err
	case reason == exitDetached:
		msg = "[detached (from session " + sessionName + ")]"
	default:
		msg = "[exited]"
	}
	if _, werr := fmt.Fprintln(out, msg); werr != nil {
		return errors.Join(err, fmt.Errorf("write the end of the attachment: %w", werr))
	}
	return err
}

func closeFile(f *os.File) error {
	if f == nil {
		return nil
	}
	return f.Close()
}

// closeIgnoringClosed closes conn, which the relay may have closed already.
func closeIgnoringClosed(conn net.Conn) error {
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// connect returns the connection to attach over. To create a session, it is
// one end of a socket pair whose other end, returned as a file, is passed to
// the server; otherwise it connects to the session's socket.
func (c *Client) connect(ctx context.Context, sessionName string, create bool) (net.Conn, *os.File, error) {
	if !create {
		conn, err := c.dial(ctx, sessionName)
		if errors.Is(err, errNoSession) {
			return nil, nil, fmt.Errorf("can't find session: %s", sessionName)
		}
		return conn, nil, err
	}
	fds, err := socketpair()
	if err != nil {
		return nil, nil, fmt.Errorf("create a socket pair: %w", err)
	}
	local := os.NewFile(uintptr(fds[0]), "session")
	remote := os.NewFile(uintptr(fds[1]), "creator")
	conn, err := net.FileConn(local)
	if err := errors.Join(err, local.Close()); err != nil {
		return nil, nil, errors.Join(err, remote.Close())
	}
	return conn, remote, nil
}

// terminalSize returns the size of the terminal f, or the default size if it
// tells none.
func terminalSize(f *os.File) (winsize, error) {
	s, err := pty.GetSize(f)
	if err != nil {
		return winsize{}, fmt.Errorf("get the size of the terminal: %w", err)
	}
	if s.Cols <= 0 || s.Rows <= 0 {
		return winsize{Cols: defaultCols, Rows: defaultRows}, nil
	}
	return winsize(s), nil
}

// inputReader reads the terminal in the background, so that reading can
// time out while the terminal is probed.
type inputReader struct {
	chunks chan []byte
	// err is the error reading ended with, set before chunks is closed.
	err error
}

func readInput(f *os.File) *inputReader {
	r := &inputReader{chunks: make(chan []byte, 64)}
	go func() {
		defer close(r.chunks)
		for {
			buf := make([]byte, 4096)
			n, err := f.Read(buf)
			if n > 0 {
				r.chunks <- buf[:n]
			}
			if err != nil {
				r.err = err
				return
			}
		}
	}()
	return r
}

// relay relays between the terminal and the session until the attachment
// ends, and returns why it ended. attached is the size of the terminal the
// session was told of when attaching.
func relay(ctx context.Context, conn net.Conn, input *inputReader, pending []byte, tty *os.File, keys keyScanner, attached winsize) (string, error) {
	// failure is the error that ended the relay, other than the server going
	// away.
	var failMu sync.Mutex
	var failure error
	fail := func(err error) {
		failMu.Lock()
		if failure == nil {
			failure = err
		}
		failMu.Unlock()
		conn.Close()
	}
	failed := func() error {
		failMu.Lock()
		defer failMu.Unlock()
		return failure
	}

	var wmu sync.Mutex
	// send sends a message to the server, and reports whether it could. When
	// it cannot, the server is gone: the connection is closed, which ends
	// the relay.
	send := func(typ msgType, payload []byte) bool {
		wmu.Lock()
		defer wmu.Unlock()
		if err := writeFrame(conn, typ, payload); err != nil {
			conn.Close()
			return false
		}
		return true
	}

	done := make(chan struct{})
	// detaching is closed once the client asked to detach.
	detaching := make(chan struct{})
	var detachOnce sync.Once
	detach := func() {
		detachOnce.Do(func() {
			send(msgDetach, nil)
			close(detaching)
		})
	}

	go func() {
		forward := func(b []byte) bool {
			data, detached := keys.scan(b)
			if len(data) > 0 && !send(msgInput, data) {
				return false
			}
			if detached {
				detach()
				return false
			}
			return true
		}
		if len(pending) > 0 && !forward(pending) {
			return
		}
		for {
			select {
			case b, ok := <-input.chunks:
				if ok {
					if !forward(b) {
						return
					}
					continue
				}
				if input.err != nil && !errors.Is(input.err, io.EOF) && !errors.Is(input.err, syscall.EIO) {
					fail(fmt.Errorf("read the terminal: %w", input.err))
					return
				}
				// The terminal is gone: the session goes on without it.
				detach()
				return
			case <-done:
				return
			}
		}
	}()

	// The session follows the size of the terminal.
	var handlers sync.WaitGroup
	sigCtx, stopSignals := context.WithCancel(context.Background())
	signals.OnEach(sigCtx, &handlers, syscall.SIGWINCH, func() {
		size, err := terminalSize(tty)
		if err != nil {
			fail(err)
			return
		}
		send(msgResize, encodeSize(size))
	})
	// The terminal may have been resized since its size was read for
	// attaching, as when the window of an editor showing it is split right
	// after opening it, while the session was starting: that resize is told
	// now, since the session would be drawn for a terminal of another size.
	if now, err := terminalSize(tty); err != nil {
		fail(err)
	} else if now != attached {
		send(msgResize, encodeSize(now))
	}
	// Interrupted or terminated: detach, leaving the session running.
	stopDetach := context.AfterFunc(ctx, detach)
	defer func() {
		stopDetach()
		close(done)
		conn.Close()
		stopSignals()
		handlers.Wait()
	}()

	go func() {
		select {
		case <-detaching:
		case <-done:
			return
		}
		// The server answers a detach by undoing what the session set up on
		// the terminal and ending the attachment; it may be too busy to.
		select {
		case <-time.After(detachTimeout):
			conn.Close()
		case <-done:
		}
	}()

	for {
		typ, payload, err := readFrame(conn)
		if err != nil {
			if err := failed(); err != nil {
				return "", err
			}
			select {
			case <-detaching:
				// The server did not answer the detach in time.
				if _, err := tty.WriteString(genericLeave); err != nil {
					return "", fmt.Errorf("write to the terminal: %w", err)
				}
				return exitDetached, nil
			default:
			}
			// Nothing undoes what the session set up on the terminal anymore.
			if _, err := tty.WriteString(genericLeave); err != nil {
				return "", errors.Join(ErrLostServer, fmt.Errorf("write to the terminal: %w", err))
			}
			return "", ErrLostServer
		}
		switch typ {
		case msgOutput:
			if _, err := tty.Write(payload); err != nil {
				return "", fmt.Errorf("write to the terminal: %w", err)
			}
		case msgExit:
			return string(payload), nil
		case msgRequest, msgInput, msgResize, msgDetach, msgResponse:
			return "", fmt.Errorf("unexpected message %d from the session", typ)
		default:
			return "", fmt.Errorf("unknown message %d from the session", typ)
		}
	}
}

// genericLeave resets the modes a program commonly sets up, for when the
// server cannot tell which ones the session's program did, and goes to a
// new line. Resetting the margins moves the cursor home, so the cursor is
// saved and restored around it.
const genericLeave = "\x1b[?2026l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1004l\x1b[?2004l" +
	"\x1b[?1l\x1b>\x1b7\x1b[r\x1b8\x1b[0m\x1b[?25h\r\n"
