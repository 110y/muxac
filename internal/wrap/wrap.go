// Package wrap runs a command on a pseudo-terminal of muxac's own, placed
// between the tmux pane and the command, so that muxac sees everything the
// command writes to its terminal.
//
// Agents that speak the Program Status Protocol (OSC 7501), such as Claude
// Code, report through escape sequences whether they are working, blocked on
// the user or idle — but only once their terminal has answered their support
// query, and tmux neither answers it nor keeps the reports. wrap answers the
// query on the terminal's behalf, consumes the reports and turns them into the
// muxac session status. Everything else passes through unchanged in both
// directions, so the command behaves as if it ran in the pane directly.
package wrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/110y/muxac/internal/programstatus"
	"github.com/110y/muxac/internal/status"
)

// outputDrainTimeout bounds how long Run waits, once the command has exited,
// for the output it left in the pseudo-terminal to be forwarded. A background
// process that inherited the terminal can keep it open indefinitely.
const outputDrainTimeout = 500 * time.Millisecond

// forwardedSignals are relayed to the command. SIGHUP arrives when the tmux
// pane is closed; with the terminal in raw mode, SIGINT and SIGQUIT only come
// from kill(1) and the like, as their keys reach the command as plain input.
var forwardedSignals = []os.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT}

// Options configures Run.
type Options struct {
	// Argv is the command to run and its arguments. Argv[0] is looked up in
	// PATH unless it contains a slash.
	Argv []string
	// Stdin and Stdout are the terminal Run relays to and from, normally the
	// tmux pane.
	Stdin  *os.File
	Stdout *os.File
	// OnStatus is called with the session status derived from the command's
	// program status reports: on its first report, and then whenever the
	// status changes. When the command exits, the records describing a running
	// process are cleared, which can report a last change. Calls are made from
	// a single goroutine, in order, and all of them before Run returns. A
	// command that never reports causes no calls.
	OnStatus func(status.Status)
	// OnError is called with errors that occur after the command has started
	// and do not stop it, such as a failure to resize the pseudo-terminal or to
	// forward a signal. The terminal, the pseudo-terminal or the command going
	// away is the normal end of relaying and is not reported. Calls are never
	// concurrent and are not made after Run returns.
	OnError func(error)
}

// Run runs the command on a new pseudo-terminal and relays between it and the
// terminal until the command exits. It returns the command's exit status, or
// 128+N if the command was killed by signal N, and an error only if the
// command could not be started.
func Run(opts Options) (int, error) {
	if len(opts.Argv) == 0 {
		return 0, errors.New("no command to run")
	}
	if opts.Stdin == nil || opts.Stdout == nil {
		return 0, errors.New("no terminal to relay to")
	}
	onStatus := opts.OnStatus
	if onStatus == nil {
		onStatus = func(status.Status) {}
	}

	rep := &reporter{onError: opts.OnError}
	defer rep.close()

	// The terminal is put into raw mode before the command starts: the
	// answers to the queries the command makes right away (such as DA1) come
	// back as input, and a terminal still in its default mode would echo them
	// to the screen.
	if inFd := int(opts.Stdin.Fd()); term.IsTerminal(inFd) {
		state, err := term.MakeRaw(inFd)
		if err != nil {
			return 0, fmt.Errorf("put the terminal into raw mode: %w", err)
		}
		defer func() {
			rep.report(term.Restore(inFd, state))
		}()
	}

	ptmx, tty, err := openPTY()
	if err != nil {
		return 0, err
	}
	defer func() {
		rep.report(ptmx.Close())
	}()
	ptmxFd := int(ptmx.Fd())

	// Signal handlers stop, and finish, before the pseudo-terminal is closed.
	sigCtx, stopSignals := context.WithCancel(context.Background())
	var handlers sync.WaitGroup
	defer func() {
		stopSignals()
		handlers.Wait()
	}()

	if sizeFd, ok := terminalFd(opts.Stdin, opts.Stdout); ok {
		// Resizes are followed from before the initial size is copied, so
		// that none in between is missed.
		onEachSignal(sigCtx, &handlers, syscall.SIGWINCH, func() {
			rep.report(copySize(ptmxFd, sizeFd))
		})
		rep.report(copySize(ptmxFd, sizeFd))
	}

	cmd, err := start(opts.Argv, tty)
	rep.report(tty.Close()) // the command has a copy of its own, if it started
	if err != nil {
		return 0, err
	}

	for _, sig := range forwardedSignals {
		onEachSignal(sigCtx, &handlers, sig, func() {
			rep.report(cmd.Process.Signal(sig))
		})
	}

	p := &proxy{ptmx: ptmx, rep: rep, queue: newStatusQueue()}

	recorded := make(chan struct{})
	go func() {
		defer close(recorded)
		p.queue.run(onStatus)
	}()

	go func() {
		rep.report(p.relayInput(opts.Stdin))
	}()

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		rep.report(p.relayOutput(opts.Stdout))
	}()

	waitErr := cmd.Wait()
	select {
	case <-drained:
	case <-time.After(outputDrainTimeout):
	}
	p.exited()
	<-recorded
	return exitStatus(waitErr), nil
}

// start starts argv on the secondary side tty of a pseudo-terminal, as the
// leader of a new session with tty as its controlling terminal.
func start(argv []string, tty *os.File) (*exec.Cmd, error) {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, err
	}
	cmd := &exec.Cmd{
		Path:        path,
		Args:        argv,
		Stdin:       tty,
		Stdout:      tty,
		Stderr:      tty,
		SysProcAttr: &syscall.SysProcAttr{Setsid: true, Setctty: true},
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// onEachSignal calls handle every time sig arrives, until ctx is done. sig is
// caught from the moment onEachSignal returns; the goroutine watching it is
// tracked by handlers.
func onEachSignal(ctx context.Context, handlers *sync.WaitGroup, sig os.Signal, handle func()) {
	arrived, stop := signal.NotifyContext(ctx, sig)
	handlers.Go(func() {
		for {
			<-arrived.Done()
			if ctx.Err() != nil {
				stop()
				return
			}
			// The next arrival is caught before this one is handled and before
			// its registration is dropped, so the signal is never left to its
			// default action in between.
			next, stopNext := signal.NotifyContext(ctx, sig)
			stop()
			handle()
			arrived, stop = next, stopNext
		}
	})
}

// terminalFd returns the descriptor of the first file that is a terminal.
func terminalFd(files ...*os.File) (int, bool) {
	for _, f := range files {
		if fd := int(f.Fd()); term.IsTerminal(fd) {
			return fd, true
		}
	}
	return 0, false
}

// copySize gives the pseudo-terminal ptmxFd the size of the terminal fd.
func copySize(ptmxFd, fd int) error {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return fmt.Errorf("get the terminal size: %w", err)
	}
	if err := unix.IoctlSetWinsize(ptmxFd, unix.TIOCSWINSZ, ws); err != nil {
		return fmt.Errorf("resize the pseudo-terminal: %w", err)
	}
	return nil
}

// exitStatus converts the result of exec.Cmd.Wait into an exit status.
func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 1
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return exitErr.ExitCode()
}

// endOfRelay reports whether err only tells that the terminal, the
// pseudo-terminal or the command has gone away, which is how relaying
// normally ends: reading the primary side of a pseudo-terminal fails with EIO
// once every process has closed the secondary side, and so do reads and writes
// on a terminal that was hung up.
func endOfRelay(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.EIO) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, os.ErrProcessDone)
}

// reporter passes errors other than the normal end of relaying to
// Options.OnError, one at a time, until it is closed.
type reporter struct {
	mu      sync.Mutex
	onError func(error)
	closed  bool
}

func (r *reporter) report(err error) {
	if err == nil || endOfRelay(err) {
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

// proxy relays between the terminal and the command's pseudo-terminal and
// keeps track of the command's program status records.
type proxy struct {
	ptmx *os.File
	rep  *reporter
	// inputMu serializes writes to the command's input, which come from both
	// the terminal and the replies to the command's queries.
	inputMu sync.Mutex

	// mu guards the fields below.
	mu      sync.Mutex
	records programstatus.Records
	// reported is set once the command has sent its first report.
	reported bool
	// last is the status most recently pushed to queue.
	last  status.Status
	queue *statusQueue
}

var _ programstatus.Handler = (*proxy)(nil)

// Query answers the command's support query. It is called while the output
// that followed the query is still held back, so the reply reaches the command
// before the terminal can answer any query made after it — in particular the
// DA1 request Claude Code sends right after it to detect terminals that ignore
// the query.
func (p *proxy) Query(terminator string) {
	p.rep.report(p.writeInput(programstatus.Reply(terminator)))
}

// Report applies a report from the command.
func (p *proxy) Report(r programstatus.Report) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.records.Apply(r)
	p.publish()
}

// exited applies the end of the command's process and stops publishing.
func (p *proxy) exited() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.reported {
		p.records.ProcessExited()
		p.publish()
	}
	p.queue.close()
}

// publish queues the status the records map to if it changed. The first
// report is always published: it is what marks the session as driven by
// program status. p.mu must be held.
func (p *proxy) publish() {
	st := p.records.Status()
	if p.reported && st == p.last {
		return
	}
	p.reported = true
	p.last = st
	p.queue.push(st)
}

func (p *proxy) writeInput(b []byte) error {
	p.inputMu.Lock()
	defer p.inputMu.Unlock()

	_, err := p.ptmx.Write(b)
	return err
}

// relayInput copies the terminal's input to the command until either side
// fails, and returns the failure.
func (p *proxy) relayInput(r io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if err := p.writeInput(buf[:n]); err != nil {
				return fmt.Errorf("relay input to the command: %w", err)
			}
		}
		if err != nil {
			return fmt.Errorf("read the terminal: %w", err)
		}
	}
}

// relayOutput copies the command's output to the terminal, consuming the
// program status sequences in it, until reading the pseudo-terminal fails,
// and returns the failure. Once writing to the terminal fails — the pane is
// gone — the output is still read and discarded, so the command never blocks
// on a full terminal and its reports keep being handled.
func (p *proxy) relayOutput(w io.Writer) error {
	var scanner programstatus.Scanner
	buf := make([]byte, 32*1024)
	var out []byte
	// discard is set once writing to the terminal has failed.
	discard := false
	write := func(b []byte) {
		if discard || len(b) == 0 {
			return
		}
		if _, err := w.Write(b); err != nil {
			discard = true
			p.rep.report(fmt.Errorf("relay output to the terminal: %w", err))
		}
	}
	for {
		n, err := p.ptmx.Read(buf)
		if n > 0 {
			out = scanner.Scan(out[:0], buf[:n], p)
			write(out)
		}
		if err != nil {
			write(scanner.Flush(out[:0]))
			return fmt.Errorf("read the command's output: %w", err)
		}
	}
}
