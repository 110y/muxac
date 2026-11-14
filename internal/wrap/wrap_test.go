package wrap

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/term"

	"github.com/110y/muxac/internal/status"
)

// fakeAgentEnv makes the test binary act as an agent running under Run
// instead of running the tests.
const fakeAgentEnv = "MUXAC_WRAP_TEST_AGENT"

func TestMain(m *testing.M) {
	if scenario := os.Getenv(fakeAgentEnv); scenario != "" {
		os.Exit(fakeAgent(scenario))
	}
	os.Exit(m.Run())
}

const (
	programStatusReply = "\x1b]7501;?\x07"
	da1Request         = "\x1b[c"
	da1Reply           = "\x1b[?62;22c"
)

// agentReports are the reports the "reports" scenario sends, in the shape
// Claude Code writes them, each followed by waiting for an acknowledgement.
var agentReports = []string{
	"state=idle:app=claude-code",
	"state=working:app=claude-code",
	"state=blocked:app=claude-code:kind=permission:msg=" + base64.StdEncoding.EncodeToString([]byte("Bash(go test ./...)")),
	"state=working:app=claude-code",
	"state=done:app=claude-code:msg=" + base64.StdEncoding.EncodeToString([]byte("All tests pass")),
	"state=working:app=claude-code",
}

// fakeAgent behaves like a program running under Run and returns its exit
// status. Unexpected failures exit with 100 and above.
func fakeAgent(scenario string) int {
	switch scenario {
	case "plain":
		if _, err := os.Stdout.WriteString("no program status here\r\n"); err != nil {
			return 100
		}
		return 0
	case "killed":
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			return 101
		}
		select {}
	case "reports":
		// Like a TUI, the agent reads its terminal in raw mode.
		fd := int(os.Stdin.Fd())
		state, err := term.MakeRaw(fd)
		if err != nil {
			return 102
		}
		code := reportingAgent()
		if err := term.Restore(fd, state); err != nil {
			return 103
		}
		return code
	default:
		return 104
	}
}

// reportingAgent speaks the Program Status Protocol like Claude Code and
// exits with status 3 while still reporting working, without clearing.
func reportingAgent() int {
	// Ask whether the terminal speaks the protocol and send DA1 in the same
	// write. Only a reply to the query that arrives before the DA1 reply
	// enables the protocol.
	if _, err := os.Stdout.WriteString("\x1b]7501;?\x07" + da1Request); err != nil {
		return 110
	}
	var in []byte
	buf := make([]byte, 256)
	for !bytes.Contains(in, []byte(programStatusReply)) || !bytes.Contains(in, []byte(da1Reply)) {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return 111
		}
		in = append(in, buf[:n]...)
	}
	if bytes.Index(in, []byte(programStatusReply)) > bytes.Index(in, []byte(da1Reply)) {
		return 112
	}
	if _, err := os.Stdout.WriteString("order:ok\r\n"); err != nil {
		return 113
	}

	for _, r := range agentReports {
		if _, err := os.Stdout.WriteString("\x1b]7501;" + r + "\x07"); err != nil {
			return 114
		}
		if _, err := os.Stdin.Read(buf[:1]); err != nil {
			return 115
		}
	}
	return 3
}

// fakeTerminal plays the terminal (tmux) on the other side of Run: it collects
// Run's output and answers DA1 requests.
type fakeTerminal struct {
	stdinR, stdinW   *os.File
	stdoutR, stdoutW *os.File

	mu     sync.Mutex
	output []byte
	err    error
	done   chan struct{}
}

func newFakeTerminal(t *testing.T) *fakeTerminal {
	t.Helper()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ft := &fakeTerminal{
		stdinR: stdinR, stdinW: stdinW,
		stdoutR: stdoutR, stdoutW: stdoutW,
		done: make(chan struct{}),
	}
	t.Cleanup(func() {
		for _, f := range []*os.File{stdinR, stdinW, stdoutR, stdoutW} {
			if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				t.Errorf("close %s: %v", f.Name(), err)
			}
		}
	})

	go func() {
		defer close(ft.done)
		ft.setErr(ft.serve())
	}()
	return ft
}

// serve reads Run's output until it ends and answers every DA1 request in it.
func (ft *fakeTerminal) serve() error {
	buf := make([]byte, 4096)
	for {
		n, err := ft.stdoutR.Read(buf)
		if n > 0 {
			ft.mu.Lock()
			before := bytes.Count(ft.output, []byte(da1Request))
			ft.output = append(ft.output, buf[:n]...)
			after := bytes.Count(ft.output, []byte(da1Request))
			ft.mu.Unlock()
			for range after - before {
				if _, err := ft.stdinW.WriteString(da1Reply); err != nil {
					return err
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (ft *fakeTerminal) setErr(err error) {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	ft.err = err
}

// finish closes Run's side of the output and returns everything Run wrote.
func (ft *fakeTerminal) finish(t *testing.T) string {
	t.Helper()

	if err := ft.stdoutW.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("close the output: %v", err)
	}
	select {
	case <-ft.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out reading the output")
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if ft.err != nil {
		t.Errorf("fake terminal: %v", ft.err)
	}
	return string(ft.output)
}

type runResult struct {
	code int
	err  error
}

// errorRecorder collects the errors Run reports.
type errorRecorder struct {
	mu   sync.Mutex
	errs []error
}

func (r *errorRecorder) record(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.errs = append(r.errs, err)
}

func (r *errorRecorder) check(t *testing.T) {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.errs) != 0 {
		t.Errorf("Run reported errors: %v", r.errs)
	}
}

func runFakeAgent(t *testing.T, ft *fakeTerminal, scenario string, onStatus func(status.Status), errs *errorRecorder) <-chan runResult {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan runResult, 1)
	go func() {
		code, err := Run(Options{
			Argv:     []string{"env", fakeAgentEnv + "=" + scenario, exe},
			Stdin:    ft.stdinR,
			Stdout:   ft.stdoutW,
			OnStatus: onStatus,
			OnError:  errs.record,
		})
		result <- runResult{code: code, err: err}
	}()
	return result
}

func waitResult(t *testing.T, result <-chan runResult) runResult {
	t.Helper()

	select {
	case r := <-result:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for Run to return")
		return runResult{}
	}
}

func TestRunTracksProgramStatus(t *testing.T) {
	t.Parallel()

	ft := newFakeTerminal(t)
	var errs errorRecorder
	statuses := make(chan status.Status, 16)
	result := runFakeAgent(t, ft, "reports", func(st status.Status) { statuses <- st }, &errs)

	next := func() status.Status {
		t.Helper()
		select {
		case st := <-statuses:
			return st
		case r := <-result:
			t.Fatalf("Run returned early: code=%d err=%v output=%q", r.code, r.err, ft.finish(t))
		case <-time.After(30 * time.Second):
			t.Fatal("timed out waiting for a status")
		}
		return ""
	}

	// One status per report, acknowledged so the agent sends the next one.
	want := []status.Status{status.Idle, status.Running, status.Waiting, status.Running, status.Idle, status.Running}
	for i, w := range want {
		if got := next(); got != w {
			t.Fatalf("status after report %q = %q, want %q", agentReports[i], got, w)
		}
		if _, err := ft.stdinW.WriteString("n"); err != nil {
			t.Fatal(err)
		}
	}

	// The agent exits while working: its working record ends with its process.
	if got := next(); got != status.Idle {
		t.Errorf("status after the agent exited = %q, want %q", got, status.Idle)
	}

	r := waitResult(t, result)
	if r.err != nil {
		t.Fatalf("Run error: %v", r.err)
	}
	if r.code != 3 {
		t.Errorf("exit status = %d, want 3", r.code)
	}

	output := ft.finish(t)
	if !strings.Contains(output, "order:ok") {
		t.Errorf("the query was not answered before the DA1 reply; output = %q", output)
	}
	if strings.Contains(output, "7501") {
		t.Errorf("program status sequences reached the terminal: %q", output)
	}
	if !strings.Contains(output, da1Request) {
		t.Errorf("the DA1 request did not reach the terminal: %q", output)
	}
	select {
	case st := <-statuses:
		t.Errorf("unexpected status after exit: %q", st)
	default:
	}
	errs.check(t)
}

func TestRunWithoutProgramStatus(t *testing.T) {
	t.Parallel()

	ft := newFakeTerminal(t)
	var errs errorRecorder
	var mu sync.Mutex
	var got []status.Status
	result := runFakeAgent(t, ft, "plain", func(st status.Status) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, st)
	}, &errs)

	r := waitResult(t, result)
	if r.err != nil {
		t.Fatalf("Run error: %v", r.err)
	}
	if r.code != 0 {
		t.Errorf("exit status = %d, want 0", r.code)
	}
	if output := ft.finish(t); !strings.Contains(output, "no program status here") {
		t.Errorf("output = %q, want the command's output", output)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 0 {
		t.Errorf("statuses = %q, want none from a command that never reports", got)
	}
	errs.check(t)
}

func TestRunKilledCommand(t *testing.T) {
	t.Parallel()

	ft := newFakeTerminal(t)
	var errs errorRecorder
	result := runFakeAgent(t, ft, "killed", nil, &errs)

	r := waitResult(t, result)
	if r.err != nil {
		t.Fatalf("Run error: %v", r.err)
	}
	if want := 128 + int(syscall.SIGKILL); r.code != want {
		t.Errorf("exit status = %d, want %d", r.code, want)
	}
	ft.finish(t)
	errs.check(t)
}

func TestRunCommandNotFound(t *testing.T) {
	t.Parallel()

	ft := newFakeTerminal(t)
	_, err := Run(Options{
		Argv:   []string{"muxac-wrap-test-no-such-command"},
		Stdin:  ft.stdinR,
		Stdout: ft.stdoutW,
	})
	if err == nil {
		t.Error("expected an error for a command that cannot be started")
	}
	ft.finish(t)
}

func TestRunRequiresCommand(t *testing.T) {
	t.Parallel()

	if _, err := Run(Options{}); err == nil {
		t.Error("expected an error without a command")
	}
	if _, err := Run(Options{Argv: []string{"true"}}); err == nil {
		t.Error("expected an error without a terminal")
	}
}

func TestEndOfRelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "end of input", err: io.EOF, want: true},
		{name: "pseudo-terminal closed by the command", err: &os.PathError{Op: "read", Path: "/dev/ptmx", Err: syscall.EIO}, want: true},
		{name: "file closed by Run", err: os.ErrClosed, want: true},
		{name: "command already exited", err: os.ErrProcessDone, want: true},
		{name: "unexpected failure", err: &os.PathError{Op: "write", Path: "/dev/stdout", Err: syscall.ENOSPC}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := endOfRelay(tt.err); got != tt.want {
				t.Errorf("endOfRelay(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
