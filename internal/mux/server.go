package mux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/110y/muxac/internal/pty"
	"github.com/110y/muxac/internal/version"
	"github.com/110y/muxac/internal/vt"
)

const (
	// outputDrainTimeout bounds how long the server waits, once the command
	// has exited, for the output it left in the pseudo-terminal. A background
	// process that inherited the terminal can keep it open indefinitely.
	outputDrainTimeout = 500 * time.Millisecond
	// hangupTimeout is how long a killed session's command is given to exit
	// after its terminal hangs up, before it is killed with SIGKILL.
	hangupTimeout = 5 * time.Second
	// flushTimeout bounds how long the server waits for the last output to
	// reach the attached clients when the session ends.
	flushTimeout = 2 * time.Second
	// requestTimeout bounds how long a client may take to send its request,
	// and the server to answer it.
	requestTimeout = 5 * time.Second
	// maxPendingOutput is how much output may wait for a slow client. Beyond
	// it, the pending output is dropped and the client gets the whole screen
	// drawn again instead.
	maxPendingOutput = 8 << 20
	// maxPendingInput is how much input may wait for a program that does
	// not read it. Beyond it, input is dropped.
	maxPendingInput = 1 << 20
	// defaultCols and defaultRows are the size of a session created without
	// a terminal.
	defaultCols = 80
	defaultRows = 24
)

// config describes the session a server runs. The client that starts the
// server writes it to the server's standard input, as JSON.
type config struct {
	// Name is the name of the session.
	Name string `json:"name"`
	// Dir is the working directory of the command.
	Dir string `json:"dir"`
	// Env is the environment of the command.
	Env []string `json:"env"`
	// Command is the shell command the session runs.
	Command string `json:"command"`
	// Size is the size of the session's terminal.
	Size winsize `json:"size"`
	// Colors are the default foreground and background colors of the
	// terminal of the client that creates the session, if known.
	Colors [2]string `json:"colors"`
	// Creator tells that the client that creates the session attaches over
	// the connection it passed as creatorFd.
	Creator bool `json:"creator"`
	// HistoryLimit is how many lines that went off the top of the screen the
	// session keeps, to show them again to the terminals that attach.
	HistoryLimit int `json:"history_limit"`
}

// startResult is what the server reports to the client that started it once
// the session runs, or failed to start.
type startResult struct {
	Error string `json:"error,omitempty"`
}

// Files the client that starts a server passes to it, besides its standard
// input, which carries the config.
const (
	// readyFd is where the server reports a startResult, then closes it.
	readyFd = 3
	// creatorFd, if config.Creator is set, is a connection over which the
	// client that creates the session attaches before the command starts,
	// so that it sees all of the command's output even if the command exits
	// right away.
	creatorFd = 4
)

// watchTimeout bounds how long the server waits, once the session has
// ended, for ServeOptions.Watch to return.
const watchTimeout = 10 * time.Second

// ServeOptions configures Serve.
type ServeOptions struct {
	// OnError is called with the errors that occur once the session runs,
	// which do not end it, other than the ones telling that a client, the
	// command or the terminal went away. Calls are never concurrent and are
	// not made after Serve returns.
	OnError func(error)
	// Watch, if not nil, runs in the background as long as the session runs,
	// to keep track of it. ctx is done once the session ends, and the server
	// waits for Watch to return, up to watchTimeout, before it removes the
	// session's socket: until then, no other session of the same name can
	// start. If Watch panics, the panic is passed to OnError, and the session
	// runs on unwatched.
	Watch func(ctx context.Context, s Session)
}

// Session is what Watch knows of the session it keeps track of.
type Session interface {
	// Dir returns the working directory of the session's command.
	Dir() string
	// Getenv returns the value of the variable named key in the environment
	// of the session's command, "" if it is not set.
	Getenv(key string) string
	// Screen returns what the session's screen shows, as text: every line,
	// with the spaces at its end trimmed, followed by a newline.
	Screen() string
}

// Serve runs the server of a session: it starts the command on a
// pseudo-terminal of its own, listens on the session's socket for clients,
// and returns once the command has exited or the session has been killed. It
// is what `muxac server` runs, as started by Client. The session is killed
// when ctx is done.
func Serve(ctx context.Context, opts ServeOptions) error {
	// The files passed by the client must not leak into the session's
	// command: the client waits for the end of the ready pipe.
	syscall.CloseOnExec(readyFd)
	syscall.CloseOnExec(creatorFd)
	ready, err := pipe(readyFd)
	if err != nil {
		return fmt.Errorf("muxac server is only started by muxac itself: %w", err)
	}
	rep := &reporter{onError: opts.OnError}
	defer rep.close()

	s, err := start(os.Stdin, rep)
	if err != nil {
		return errors.Join(err, report(ready, err))
	}
	// The session runs on even if the client that started it went away.
	rep.report(report(ready, nil))
	s.run(ctx, opts.Watch)
	return nil
}

// pipe returns the file of the descriptor fd, which must be a pipe.
func pipe(fd int) (*os.File, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("descriptor %d: %w", fd, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFIFO {
		return nil, fmt.Errorf("descriptor %d is not a pipe", fd)
	}
	return os.NewFile(uintptr(fd), "pipe"), nil
}

// report sends the result of starting the session to the client.
func report(ready *os.File, startErr error) error {
	var res startResult
	if startErr != nil {
		res.Error = startErr.Error()
	}
	b, err := json.Marshal(res)
	if err != nil {
		return errors.Join(err, ready.Close())
	}
	_, err = ready.Write(b)
	return errors.Join(err, ready.Close())
}

// reporter passes errors to Serve's onError, one at a time, until it is
// closed.
type reporter struct {
	mu      sync.Mutex
	onError func(error)
	closed  bool
}

func (r *reporter) report(err error) {
	if err == nil || gone(err) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed || r.onError == nil {
		return
	}
	r.onError(err)
}

func (r *reporter) close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true
}

// gone reports whether err only tells that the other side of a connection
// or of the pseudo-terminal went away, or that the server closed it, which is
// how they normally end: reading the primary side of a pseudo-terminal fails
// with EIO once every process has closed the secondary side.
func gone(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, syscall.EIO) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET)
}

// server is the server of a session.
type server struct {
	cfg  config
	path string
	rep  *reporter
	// socket identifies the socket file the server created, so that it never
	// removes the socket of another server.
	socket os.FileInfo
	ln     *net.UnixListener
	ptmx   *os.File
	cmd    *exec.Cmd
	input  *inbox

	// exited is closed once the command has exited, and outputDone once
	// reading its output has ended.
	exited     chan struct{}
	outputDone chan struct{}
	// killed is closed when a client asks to kill the session.
	killed   chan struct{}
	killOnce sync.Once

	mu   sync.Mutex
	term *vt.Terminal
	size winsize
	// clients are the attached clients, the one used last, whose terminal
	// size the session follows, last.
	clients []*client
	// closing is set once the session ends: no client may attach anymore.
	closing bool
	// writers tracks the goroutines that write to the clients.
	writers sync.WaitGroup
	endOnce sync.Once

	// stopWatch ends the context of ServeOptions.Watch, and watched is
	// closed once Watch has returned; both are nil without Watch.
	stopWatch context.CancelFunc
	watched   chan struct{}
}

var _ Session = (*server)(nil)

func (s *server) Dir() string {
	return s.cfg.Dir
}

func (s *server) Getenv(key string) string {
	v := ""
	for _, kv := range s.cfg.Env {
		if value, ok := strings.CutPrefix(kv, key+"="); ok {
			// As with os/exec, the last value of a variable is the one set.
			v = value
		}
	}
	return v
}

func (s *server) Screen() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.term.Text()
}

// client is an attached client.
type client struct {
	conn net.Conn
	size winsize
	out  *outbox
	// stale is set while the terminal of the client does not have the size
	// of the session: the output passed on would not show the session right
	// there, as long lines would wrap and the program's moves of the cursor
	// would miss what it draws over. The terminal gets none, and is caught up
	// once it has the size of the session again, from the history as far as
	// mark, which it went when the terminal stopped getting the output.
	stale bool
	mark  vt.Mark
}

// fits reports whether the terminal of c has size, as far as what it shows
// goes.
func (c *client) fits(size winsize) bool {
	return c.size.Cols == size.Cols && c.size.Rows == size.Rows
}

// start reads the config from r, listens on the session's socket and starts
// the command.
func start(r io.Reader, rep *reporter) (*server, error) {
	var cfg config
	if err := json.NewDecoder(r).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("read the session config: %w", err)
	}
	if cfg.Size.Cols <= 0 || cfg.Size.Rows <= 0 {
		cfg.Size.Cols, cfg.Size.Rows = defaultCols, defaultRows
	}

	dir, err := socketDir(true)
	if err != nil {
		return nil, err
	}
	s := &server{
		cfg:        cfg,
		path:       socketPath(dir, cfg.Name),
		rep:        rep,
		input:      newInbox(),
		exited:     make(chan struct{}),
		outputDone: make(chan struct{}),
		killed:     make(chan struct{}),
		size:       cfg.Size,
	}
	s.term = vt.New(vt.Options{
		Cols:         cfg.Size.Cols,
		Rows:         cfg.Size.Rows,
		Reply:        s.input.push,
		Name:         "muxac " + version.Version,
		HistoryLimit: cfg.HistoryLimit,
	})
	s.term.SetColors(cfg.Colors[0], cfg.Colors[1])
	s.term.SetPixelSize(cfg.Size.XPixel, cfg.Size.YPixel)

	if err := s.listen(dir); err != nil {
		return nil, err
	}
	creator, err := s.attachCreator()
	if err == nil {
		err = s.startCommand()
	}
	if err != nil {
		err = errors.Join(err, s.removeSocket())
		if creator != nil {
			err = errors.Join(err, creator.conn.Close())
		}
		return nil, err
	}
	if creator != nil {
		go s.serveClient(creator)
	}
	return s, nil
}

// listen creates the session's socket, unless another server already serves
// a session of the same name.
func (s *server) listen(dir string) (err error) {
	unlock, err := lockDir(dir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()

	ok, err := alive(s.path)
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("session %s already exists", s.cfg.Name)
	}
	// A socket nobody listens on is left by a server that did not end
	// normally.
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.path, Net: "unix"})
	if err != nil {
		return err
	}
	ln.SetUnlinkOnClose(false)
	fi, err := os.Stat(s.path)
	if err != nil {
		return errors.Join(err, ln.Close())
	}
	s.ln, s.socket = ln, fi
	return nil
}

// removeSocket stops listening and removes the session's socket, if it is
// still the server's own, so that clients no longer find the session.
func (s *server) removeSocket() error {
	unlock, err := lockDir(filepath.Dir(s.path))
	if err != nil {
		return errors.Join(err, s.ln.Close())
	}
	fi, err := os.Stat(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Removed already.
		err = nil
	case err != nil:
	case os.SameFile(fi, s.socket):
		err = os.Remove(s.path)
	}
	return errors.Join(err, unlock(), s.ln.Close())
}

// attachCreator attaches the client that creates the session, if it passed
// a connection to attach over.
func (s *server) attachCreator() (*client, error) {
	if !s.cfg.Creator {
		return nil, nil
	}
	f := os.NewFile(creatorFd, "creator")
	conn, err := net.FileConn(f)
	if err := errors.Join(err, f.Close()); err != nil {
		return nil, err
	}
	req, err := readRequest(conn)
	if err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.register(conn, req), nil
}

// startCommand starts the command on a new pseudo-terminal through the
// shell, as tmux runs the command of a pane: $SHELL -c command.
func (s *server) startCommand() error {
	ptmx, tty, err := pty.Open()
	if err != nil {
		return err
	}
	// Reads block from now on rather than going through the runtime poller,
	// which not every system's pseudo-terminals get along with.
	ptmx.Fd()
	if err := pty.SetSize(ptmx, pty.Size(s.size)); err != nil {
		return errors.Join(err, tty.Close(), ptmx.Close())
	}

	sh := shell(s.cfg.Env)
	cmd := &exec.Cmd{
		Path:        sh,
		Args:        []string{sh, "-c", s.cfg.Command},
		Env:         s.cfg.Env,
		Dir:         s.cfg.Dir,
		Stdin:       tty,
		Stdout:      tty,
		Stderr:      tty,
		SysProcAttr: &syscall.SysProcAttr{Setsid: true, Setctty: true},
	}
	startErr := cmd.Start()
	// The command has a copy of the secondary side of its own, if it started.
	if err := errors.Join(startErr, tty.Close()); err != nil {
		return errors.Join(err, ptmx.Close())
	}
	s.ptmx, s.cmd = ptmx, cmd
	return nil
}

// shell returns the shell that runs the command: $SHELL if it names an
// executable file by an absolute path, /bin/sh otherwise.
func shell(env []string) string {
	sh := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "SHELL="); ok {
			sh = v
		}
	}
	if filepath.IsAbs(sh) {
		if fi, err := os.Stat(sh); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return sh
		}
	}
	return "/bin/sh"
}

// run serves the session until it ends, running watch, if not nil, as long
// as it does.
func (s *server) run(ctx context.Context, watch func(context.Context, Session)) {
	if watch != nil {
		// The watch outlives ctx: it ends with the session, which ctx being
		// done ends in turn.
		watchCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
		s.stopWatch, s.watched = stop, make(chan struct{})
		go func() {
			defer close(s.watched)
			defer func() {
				// Watching the session going wrong must not end the session.
				if r := recover(); r != nil {
					s.rep.report(fmt.Errorf("watching the session panicked: %v\n%s", r, debug.Stack()))
				}
			}()
			watch(watchCtx, s)
		}()
	}
	go func() {
		defer close(s.exited)
		// The session ends however its command exits, as in tmux.
		var exitErr *exec.ExitError
		if err := s.cmd.Wait(); err != nil && !errors.As(err, &exitErr) {
			s.rep.report(err)
		}
	}()
	go func() {
		defer close(s.outputDone)
		s.rep.report(s.readOutput())
	}()
	go func() {
		s.rep.report(s.input.run(s.ptmx))
	}()
	go func() {
		s.rep.report(s.accept())
	}()

	select {
	case <-s.exited:
		select {
		case <-s.outputDone:
		case <-time.After(outputDrainTimeout):
		}
		s.end()
		return
	case <-s.killed:
	case <-ctx.Done():
		s.end()
	}
	s.hangUp()
}

// readOutput reads the command's output, keeps track of the screen and
// passes the output on to the attached clients, until reading fails, which
// it does once every process has closed the secondary side. It returns the
// failure.
func (s *server) readOutput() error {
	buf := make([]byte, 64*1024)
	var out []byte
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.mu.Lock()
			mark := s.term.Mark()
			out = s.term.Feed(out[:0], buf[:n])
			if len(out) > 0 {
				s.broadcast(frame{typ: msgOutput, payload: slices.Clone(out)}, mark)
			}
			s.mu.Unlock()
		}
		if err != nil {
			return fmt.Errorf("read the output of the command: %w", err)
		}
	}
}

// broadcast queues f, output from before which the history went as far as
// mark, for every attached client whose terminal has the size of the
// session. A client that has too much output pending gets the screen drawn
// anew instead. s.mu must be held.
func (s *server) broadcast(f frame, mark vt.Mark) {
	for _, c := range s.clients {
		switch {
		case c.stale:
		case !c.fits(s.size):
			c.stale, c.mark = true, mark
		case !c.out.push(f):
			c.out.replace(frame{typ: msgOutput, payload: s.term.Redraw(nil)})
		}
	}
}

// accept serves the clients that connect until the listener is closed, and
// returns the error that stopped it.
func (s *server) accept() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(conn)
	}
}

// handle serves a connection.
func (s *server) handle(conn net.Conn) {
	req, err := readRequest(conn)
	if err != nil {
		s.rep.report(errors.Join(fmt.Errorf("read a request: %w", err), conn.Close()))
		return
	}
	if req.Version != protocolVersion {
		s.respond(conn, response{Error: fmt.Sprintf("the session speaks protocol version %d, not %d", protocolVersion, req.Version)})
		return
	}

	switch req.Op {
	case opInfo:
		s.respond(conn, response{Name: s.cfg.Name})
	case opCapture:
		s.mu.Lock()
		text := s.term.Text()
		s.mu.Unlock()
		s.respond(conn, response{Text: text})
	case opKill:
		s.end()
		s.respond(conn, response{Name: s.cfg.Name})
		s.killOnce.Do(func() { close(s.killed) })
	case opAttach:
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			s.rep.report(errors.Join(
				conn.SetWriteDeadline(time.Now().Add(requestTimeout)),
				writeFrame(conn, msgExit, []byte(exitExited)),
				conn.Close(),
			))
			return
		}
		c := s.register(conn, req)
		s.mu.Unlock()
		s.serveClient(c)
	default:
		s.respond(conn, response{Error: fmt.Sprintf("unknown request %q", req.Op)})
	}
}

// respond answers a request and closes the connection.
func (s *server) respond(conn net.Conn, res response) {
	s.rep.report(errors.Join(
		conn.SetWriteDeadline(time.Now().Add(requestTimeout)),
		writeJSON(conn, msgResponse, res),
		conn.Close(),
	))
}

// readRequest reads the request a client starts a connection with.
func readRequest(conn net.Conn) (request, error) {
	var req request
	if err := conn.SetReadDeadline(time.Now().Add(requestTimeout)); err != nil {
		return req, err
	}
	typ, payload, err := readFrame(conn)
	if err != nil {
		return req, err
	}
	if typ != msgRequest {
		return req, fmt.Errorf("unexpected message %d", typ)
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return req, err
	}
	return req, conn.SetReadDeadline(time.Time{})
}

// register attaches the client on conn to the session: it gets the screen
// drawn, and from then on the session's output. s.mu must be held.
func (s *server) register(conn net.Conn, req request) *client {
	c := &client{conn: conn, size: req.Size, out: newOutbox()}
	s.writers.Go(func() {
		s.rep.report(c.out.run(conn))
	})
	if req.Colors != ([2]string{}) {
		s.term.SetColors(req.Colors[0], req.Colors[1])
	}
	s.clients = append(s.clients, c)
	s.resize(c)
	// However long the history is, the terminal gets it: nothing else would
	// draw the session there.
	c.out.pushUncapped(frame{typ: msgOutput, payload: s.term.Attach(nil, req.Cursor[0], req.Cursor[1])})
	return c
}

// serveClient reads what the attached client c sends until it detaches or
// goes away.
func (s *server) serveClient(c *client) {
	for {
		typ, payload, err := readFrame(c.conn)
		if err != nil {
			s.rep.report(fmt.Errorf("read from a client: %w", err))
			s.detach(c, "")
			return
		}
		switch typ {
		case msgInput:
			// A terminal that only tells that it lost the focus is not being
			// used. The session takes the size of the one that is before the
			// command reads what was typed there.
			if !focusOut(payload) {
				s.mu.Lock()
				s.use(c)
				s.mu.Unlock()
			}
			s.input.push(payload)
		case msgResize:
			size, err := decodeSize(payload)
			if err != nil {
				s.rep.report(fmt.Errorf("resize: %w", err))
				continue
			}
			s.mu.Lock()
			if slices.Contains(s.clients, c) {
				c.size = size
				s.use(c)
			}
			s.mu.Unlock()
		case msgDetach:
			s.detach(c, exitDetached)
			return
		case msgRequest, msgOutput, msgExit, msgResponse:
			s.rep.report(fmt.Errorf("unexpected message %d from a client", typ))
		default:
			s.rep.report(fmt.Errorf("unknown message %d from a client", typ))
		}
	}
}

// detach detaches c, telling it why if reason is not empty.
func (s *server) detach(c *client, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := slices.Index(s.clients, c)
	if i < 0 {
		return
	}
	s.clients = slices.Delete(s.clients, i, i+1)
	if reason != "" {
		c.out.push(frame{typ: msgOutput, payload: s.term.Leave(nil)})
		c.out.push(frame{typ: msgExit, payload: []byte(reason)})
	}
	c.out.close()
	s.resize(nil)
}

// use makes c the client used last, which the session follows the size of,
// as tmux's window-size latest does: a client is used when it attaches, its
// terminal is resized, or input comes from it, such as the keys typed there
// or the report that it got the focus. The output passed on to a terminal
// shows the session right only at the size of the session, so hidden
// terminals attached to it, such as the ones of previews, must not keep it at
// theirs. s.mu must be held.
func (s *server) use(c *client) {
	i := slices.Index(s.clients, c)
	if i < 0 {
		return
	}
	if i != len(s.clients)-1 {
		s.clients = append(slices.Delete(s.clients, i, i+1), c)
	}
	s.resize(nil)
}

// focusOutReport is what a terminal with focus reporting (DECSET 1004) sends
// when it loses the focus.
const focusOutReport = "\x1b[O"

// focusOut reports whether input only tells that the terminal lost the
// focus, as many times as it does.
func focusOut(input []byte) bool {
	if len(input) == 0 {
		return false
	}
	for len(input) > 0 {
		rest, ok := bytes.CutPrefix(input, []byte(focusOutReport))
		if !ok {
			return false
		}
		input = rest
	}
	return true
}

// resize gives the session the size of the terminal of the client used last.
// Then, the clients other than skip whose terminals have the size of the
// session, which they had not, are caught up, and the others stop getting its
// output. A client whose own terminal got the new size is not drawn anew:
// the terminal already showed what the session showed, its own way, such as
// rewrapping the lines as Neovim's does, and keeping that in its scrollback.
// s.mu must be held.
func (s *server) resize(skip *client) {
	if len(s.clients) == 0 {
		return
	}
	mark := s.term.Mark()
	size := s.clients[len(s.clients)-1].size
	if size.Cols > 0 && size.Rows > 0 && size != s.size {
		if err := pty.SetSize(s.ptmx, pty.Size(size)); err != nil {
			s.rep.report(fmt.Errorf("resize the pseudo-terminal: %w", err))
			return
		}
		s.size = size
		s.term.Resize(size.Cols, size.Rows)
		s.term.SetPixelSize(size.XPixel, size.YPixel)
	}
	for _, c := range s.clients {
		switch {
		case c == skip:
		case !c.fits(s.size):
			if !c.stale {
				c.stale, c.mark = true, mark
			}
		case c.stale:
			c.stale = false
			c.out.pushUncapped(frame{typ: msgOutput, payload: s.term.CatchUp(nil, c.mark)})
		}
	}
}

// end ends the session for the attached clients, stops watching it and
// removes its socket: once end returns, clients no longer find the session.
// It may be called any number of times, also concurrently: every call
// returns once the session has ended.
func (s *server) end() {
	s.endOnce.Do(s.endSession)
}

func (s *server) endSession() {
	s.mu.Lock()
	s.closing = true
	clients := s.clients
	s.clients = nil
	leave := frame{typ: msgOutput, payload: s.term.Leave(nil)}
	for _, c := range clients {
		c.out.push(leave)
		c.out.push(frame{typ: msgExit, payload: []byte(exitExited)})
		c.out.close()
	}
	s.mu.Unlock()

	if s.stopWatch != nil {
		s.stopWatch()
		select {
		case <-s.watched:
		case <-time.After(watchTimeout):
		}
	}
	s.rep.report(s.removeSocket())

	flushed := make(chan struct{})
	go func() {
		s.writers.Wait()
		close(flushed)
	}()
	select {
	case <-flushed:
	case <-time.After(flushTimeout):
	}
}

// hangUp hangs up the command's terminal, as tmux does when it kills a
// session, and waits for the command to exit, killing it if it does not in
// time.
func (s *server) hangUp() {
	pgid := s.cmd.Process.Pid
	s.signal(pgid, syscall.SIGHUP)
	s.rep.report(s.ptmx.Close())
	select {
	case <-s.exited:
		return
	case <-time.After(hangupTimeout):
	}
	s.signal(pgid, syscall.SIGKILL)
	select {
	case <-s.exited:
	case <-time.After(time.Second):
	}
}

// signal sends sig to the process group pgid, which may be gone already.
func (s *server) signal(pgid int, sig syscall.Signal) {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		s.rep.report(fmt.Errorf("send %s to the command: %w", sig, err))
	}
}
