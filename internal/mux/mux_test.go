package mux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/term"

	"github.com/110y/muxac/internal/pty"
)

// The test binary also plays the server of the sessions the tests create,
// and the programs those sessions run.
const (
	// testServerEnv marks the test binary started as `muxac server`.
	testServerEnv = "MUXAC_MUX_TEST_SERVER"
	// programEnv names the program the test binary plays in a session.
	programEnv = "MUXAC_MUX_TEST_PROGRAM"
	// watchEnv names, in the environment of a session, the file in which
	// the watch of the test server tells what it saw of the session.
	watchEnv = "MUXAC_MUX_TEST_WATCH"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "server" && os.Getenv(testServerEnv) != "" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
		err := Serve(ctx, ServeOptions{
			OnError: func(err error) { fmt.Fprintln(os.Stderr, "muxac server:", err) },
			Watch:   watch,
		})
		stop()
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if p := os.Getenv(programEnv); p != "" {
		os.Exit(program(p))
	}

	// Socket paths must stay short, at most 103 bytes on macOS: the
	// directory of the sockets is made in a directory of its own under /tmp,
	// rather than under $TMPDIR, which is long on macOS, or in one named
	// after a test.
	dir, err := os.MkdirTemp("/tmp", "mx")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for k, v := range map[string]string{envTmpDir: dir, testServerEnv: "1", "SHELL": "/bin/sh"} {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	// The tests do not depend on how the user set up muxac, nor on running
	// in a session.
	for _, k := range []string{envPrefix, envHistoryLimit, envSocket} {
		if err := os.Unsetenv(k); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// program plays a program running in a session and returns its exit status.
// Like a TUI, it reads its terminal in raw mode.
func program(name string) (code int) {
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return 100
	}
	defer func() {
		if err := term.Restore(int(os.Stdin.Fd()), state); err != nil && code == 0 {
			code = 108
		}
	}()

	switch name {
	case "echo":
		// Tells its environment and working directory, then echoes what it
		// reads, until it reads q.
		wd, err := os.Getwd()
		if err != nil {
			return 101
		}
		if _, err := fmt.Fprintf(os.Stdout, "ready\r\nenv:%s\r\nwd:%s\r\n", os.Getenv("MUXAC_TEST_VAR"), wd); err != nil {
			return 101
		}
		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return 102
			}
			if bytes.IndexByte(buf[:n], 'q') >= 0 {
				return 0
			}
			if _, err := fmt.Fprintf(os.Stdout, "in:%q\r\n", buf[:n]); err != nil {
				return 103
			}
		}
	case "query":
		// Asks its terminal for the cursor position and the background
		// color, fenced by DA1, shows the answers, and waits to be killed.
		if _, err := os.Stdout.WriteString("abc\x1b[6n\x1b]11;?\x07\x1b[c"); err != nil {
			return 104
		}
		var in []byte
		buf := make([]byte, 256)
		for !bytes.Contains(in, []byte("\x1b[?62;22c")) {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return 105
			}
			in = append(in, buf[:n]...)
		}
		if _, err := fmt.Fprintf(os.Stdout, "\r\nanswers:%q\r\n", in); err != nil {
			return 106
		}
		select {}
	}
	return 107
}

// watch plays the watch of the test server. For a session whose environment
// names a file in watchEnv, it writes there, once the session's screen shows
// "watched", the working directory of the session's command, followed by
// "ended" and whether the session's socket still existed once the session
// ended.
func watch(ctx context.Context, s Session) {
	path := s.Getenv(watchEnv)
	if path == "" {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "watch:", err)
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "watch:", err)
		}
	}()

	for !strings.HasPrefix(s.Screen(), "watched\n") {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := fmt.Fprintf(f, "dir:%s\n", s.Dir()); err != nil {
		fmt.Fprintln(os.Stderr, "watch:", err)
		return
	}
	<-ctx.Done()
	_, err = os.Stat(s.Getenv(envSocket))
	if _, err := fmt.Fprintf(f, "ended, socket:%t\n", err == nil); err != nil {
		fmt.Fprintln(os.Stderr, "watch:", err)
	}
}

// newDetachedSession creates a session that runs command, with env added to
// the environment of the test, without attaching a terminal to it.
func (c *Client) newDetachedSession(ctx context.Context, sessionName string, env []string, command string) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	return c.startServer(ctx, config{
		Name:    sessionName,
		Dir:     dir,
		Env:     sessionEnv(sessionName, env),
		Command: command,
		Size:    winsize{Cols: defaultCols, Rows: defaultRows},
	}, nil)
}

// captureScreen returns what the session's screen shows, as text: every
// line, with the spaces at its end trimmed, followed by a newline.
func (c *Client) captureScreen(ctx context.Context, sessionName string) (string, error) {
	res, err := c.do(ctx, sessionName, opCapture, 5*time.Second)
	return res.Text, err
}

// programCommand returns the command that runs the test binary as the
// program name.
func programCommand(t *testing.T, name string) string {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return programEnv + "=" + name + " exec '" + exe + "'"
}

func newTestClient(t *testing.T) *Client {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Client{Exe: exe}
}

// sessionName returns a session name unique to the test.
func sessionName(t *testing.T) string {
	return "muxac-test-" + strings.NewReplacer("/", "-", " ", "_").Replace(t.Name())
}

// eventually waits for cond to hold.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func killOnCleanup(t *testing.T, c *Client, name string) {
	t.Helper()

	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if c.HasSession(ctx, name) {
			if err := c.KillSession(ctx, name); err != nil {
				t.Errorf("kill session %s: %v", name, err)
			}
		}
	})
}

func TestDetachedSessionLifecycle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	c := newTestClient(t)
	name := sessionName(t)
	killOnCleanup(t, c, name)

	if c.HasSession(ctx, name) {
		t.Fatal("the session exists before it is created")
	}
	if err := c.newDetachedSession(ctx, name, nil, "printf 'hello from the session\\n'; exec sleep 60"); err != nil {
		t.Fatalf("newDetachedSession: %v", err)
	}
	if !c.HasSession(ctx, name) {
		t.Fatal("HasSession = false after creating the session")
	}
	names, err := c.ListSessionNames(ctx)
	if err != nil {
		t.Fatalf("ListSessionNames: %v", err)
	}
	if !slices.Contains(names, name) {
		t.Errorf("ListSessionNames = %q, want it to contain %q", names, name)
	}
	eventually(t, "the output on the screen", func() bool {
		text, err := c.captureScreen(ctx, name)
		return err == nil && strings.HasPrefix(text, "hello from the session\n")
	})
	text, err := c.captureScreen(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(text, "\n"); lines != defaultRows {
		t.Errorf("the screen has %d lines, want %d", lines, defaultRows)
	}

	if err := c.newDetachedSession(ctx, name, nil, "true"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("creating the session again: %v, want an error telling that it exists", err)
	}

	if err := c.KillSession(ctx, name); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if c.HasSession(ctx, name) {
		t.Error("HasSession = true after killing the session")
	}
	names, err = c.ListSessionNames(ctx)
	if err != nil {
		t.Fatalf("ListSessionNames: %v", err)
	}
	if slices.Contains(names, name) {
		t.Errorf("ListSessionNames = %q after killing %q", names, name)
	}
	if _, err := c.captureScreen(ctx, name); !errors.Is(err, errNoSession) {
		t.Errorf("captureScreen after killing: %v, want errNoSession", err)
	}
}

func TestSessionEndsWithItsCommand(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	c := newTestClient(t)
	name := sessionName(t)

	if err := c.newDetachedSession(ctx, name, nil, "sleep 0.2"); err != nil {
		t.Fatalf("newDetachedSession: %v", err)
	}
	eventually(t, "the session to end", func() bool { return !c.HasSession(ctx, name) })
}

func TestWatchRunsAsLongAsTheSession(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		command string
		// end waits for the session, which runs command, to end, ending it
		// if need be.
		end func(t *testing.T, c *Client, name string)
	}{
		{
			name:    "killed",
			command: "echo watched; exec sleep 60",
			end: func(t *testing.T, c *Client, name string) {
				t.Helper()

				if err := c.KillSession(t.Context(), name); err != nil {
					t.Fatalf("KillSession: %v", err)
				}
			},
		},
		{
			name:    "command exits",
			command: "echo watched; exec sleep 2",
			end: func(t *testing.T, c *Client, name string) {
				t.Helper()

				eventually(t, "the session to end", func() bool { return !c.HasSession(t.Context(), name) })
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			c := newTestClient(t)
			name := sessionName(t)
			killOnCleanup(t, c, name)
			path := filepath.Join(t.TempDir(), "watch")
			wd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}

			if err := c.newDetachedSession(ctx, name, []string{watchEnv + "=" + path}, tc.command); err != nil {
				t.Fatalf("newDetachedSession: %v", err)
			}
			started := "dir:" + wd + "\n"
			eventually(t, "the watch to see the session", func() bool {
				b, err := os.ReadFile(path)
				return err == nil && strings.HasPrefix(string(b), started)
			})
			tc.end(t, c, name)
			// Once the session no longer exists, its watch has returned,
			// having seen the session end before its socket was removed.
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if want := started + "ended, socket:true\n"; string(b) != want {
				t.Errorf("the watch wrote %q, want %q", b, want)
			}
		})
	}
}

func TestSessionGetenv(t *testing.T) {
	t.Parallel()

	s := &server{cfg: config{Env: []string{"A=1", "AB=2", "B=", "A=3"}}}
	for key, want := range map[string]string{"A": "3", "AB": "2", "B": "", "C": ""} {
		if got := s.Getenv(key); got != want {
			t.Errorf("Getenv(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestQueriesAnsweredWithoutTerminal(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	c := newTestClient(t)
	name := sessionName(t)
	killOnCleanup(t, c, name)

	if err := c.newDetachedSession(ctx, name, nil, programCommand(t, "query")); err != nil {
		t.Fatalf("newDetachedSession: %v", err)
	}
	// Without a terminal attached, the colors are unknown: only the cursor
	// position and DA1 are answered.
	want := `answers:"\x1b[1;4R\x1b[?62;22c"`
	eventually(t, "the answers on the screen", func() bool {
		text, err := c.captureScreen(ctx, name)
		return err == nil && strings.Contains(text, want)
	})
}

func TestStaleSocketIsReplaced(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	c := newTestClient(t)
	name := sessionName(t)
	killOnCleanup(t, c, name)

	dir, err := socketDir(true)
	if err != nil {
		t.Fatal(err)
	}
	// A socket left by a server that did not end normally.
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath(dir, name), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	if c.HasSession(ctx, name) {
		t.Fatal("HasSession = true for a stale socket")
	}
	if err := c.newDetachedSession(ctx, name, nil, "exec sleep 60"); err != nil {
		t.Fatalf("newDetachedSession over a stale socket: %v", err)
	}
	if !c.HasSession(ctx, name) {
		t.Error("HasSession = false after creating the session")
	}
}

func TestSocketDirPermissions(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "muxac")
	if _, err := checkSocketDir(dir, true); err != nil {
		t.Fatalf("checkSocketDir creating the directory: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("the directory was created with mode %o, want 700", perm)
	}

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := checkSocketDir(dir, true); err == nil {
		t.Error("checkSocketDir accepted a directory other users can read")
	}
}

// fakeTerminal plays the terminal of a user: it is the primary side of a
// pseudo-terminal whose secondary side the client attaches. It answers the
// client's questions about its colors.
type fakeTerminal struct {
	ptmx, tty *os.File

	mu  sync.Mutex
	out []byte
	// err is the first error the terminal ran into in the background.
	err error
}

func (ft *fakeTerminal) fail(err error) {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	if ft.err == nil {
		ft.err = err
	}
}

func newFakeTerminal(t *testing.T, cols, rows int) *fakeTerminal {
	t.Helper()

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.SetSize(tty, pty.Size{Cols: cols, Rows: rows}); err != nil {
		t.Fatal(err)
	}
	ft := &fakeTerminal{ptmx: ptmx, tty: tty}
	// The reads block: closing the pseudo-terminal does not end them while
	// the client still reads the other side, so they are not waited for.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				ft.mu.Lock()
				ft.out = append(ft.out, buf[:n]...)
				ft.mu.Unlock()
				if bytes.Contains(buf[:n], []byte(probeQueries)) {
					if _, err := ptmx.WriteString("\x1b]11;rgb:1111/2222/3333\x1b\\\x1b[1;1R\x1b[?64;1c"); err != nil {
						ft.fail(err)
					}
				}
			}
			if err != nil {
				// Reading fails with EIO once the other side is closed.
				if !errors.Is(err, syscall.EIO) && !errors.Is(err, os.ErrClosed) {
					ft.fail(err)
				}
				return
			}
		}
	}()
	t.Cleanup(func() {
		ft.mu.Lock()
		err := ft.err
		ft.mu.Unlock()
		if err := errors.Join(err, tty.Close(), ptmx.Close()); err != nil {
			t.Error(err)
		}
	})
	return ft
}

// input types s on the terminal.
func (ft *fakeTerminal) input(t *testing.T, s string) {
	t.Helper()

	if _, err := ft.ptmx.WriteString(s); err != nil {
		t.Error(err)
	}
}

func (ft *fakeTerminal) output() string {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	return string(ft.out)
}

// attachFake attaches a fake terminal of the given size to the session through
// c until the test ends, whose context ending detaches it.
func attachFake(t *testing.T, c *Client, name string, cols, rows int) *fakeTerminal {
	t.Helper()

	ft := newFakeTerminal(t, cols, rows)
	attacher := *c
	attacher.Stdin, attacher.Stdout = ft.tty, ft.tty
	done := make(chan struct{})
	go func() {
		defer close(done)
		ft.fail(attacher.AttachSession(t.Context(), name))
	}()
	// Cleanups run in the reverse order of their registration: the client
	// returns before its terminal is closed.
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("timed out waiting for the client to return")
		}
	})
	return ft
}

// waitOutput waits for the terminal to show s.
func (ft *fakeTerminal) waitOutput(t *testing.T, s string) {
	t.Helper()

	eventually(t, fmt.Sprintf("%q on the terminal", s), func() bool {
		return strings.Contains(ft.output(), s)
	})
}

func TestAttach(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	name := sessionName(t)
	c := newTestClient(t)
	killOnCleanup(t, c, name)

	// Creating the session attaches the terminal to it. The terminal is wide
	// enough for the working directory, however long the temporary directory
	// is, to show on a single line.
	ft := newFakeTerminal(t, 400, 20)
	creator := *c
	creator.Stdin, creator.Stdout = ft.tty, ft.tty
	dir, command := t.TempDir(), programCommand(t, "echo")
	result := make(chan error, 1)
	go func() {
		result <- creator.NewSession(ctx, name, dir, []string{"MUXAC_TEST_VAR=set"}, command)
	}()
	ft.waitOutput(t, "ready")
	// The command runs in the session's directory, with the variables added.
	ft.waitOutput(t, "env:set\r\nwd:"+dir+"\r\n")
	ft.input(t, "x")
	ft.waitOutput(t, `in:"x"`)
	// The prefix key pressed twice is passed on.
	ft.input(t, "\x02\x02")
	ft.waitOutput(t, `in:"\x02"`)
	ft.input(t, "\x02d")
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the client to detach")
	}
	ft.waitOutput(t, "[detached (from session "+name+")]")
	if !c.HasSession(ctx, name) {
		t.Fatal("the session ended when the client detached")
	}
	if text, err := c.captureScreen(ctx, name); err != nil || !strings.HasPrefix(text, "ready\nenv:set\nwd:"+dir+"\n"+`in:"x"`) {
		t.Errorf("captureScreen = %q, %v; want the session's output", text, err)
	}

	// Attaching again draws the screen as it is, at the size of the new
	// terminal.
	ft2 := newFakeTerminal(t, 50, 10)
	attacher := *c
	attacher.Stdin, attacher.Stdout = ft2.tty, ft2.tty
	go func() {
		result <- attacher.AttachSession(ctx, name)
	}()
	ft2.waitOutput(t, `in:"x"`)
	eventually(t, "the session to take the new size", func() bool {
		text, err := c.captureScreen(ctx, name)
		return err == nil && strings.Count(text, "\n") == 10
	})
	// The program ends the session: the client tells so and returns.
	ft2.input(t, "q")
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("AttachSession: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the client to return")
	}
	ft2.waitOutput(t, "[exited]")
	eventually(t, "the session to end", func() bool { return !c.HasSession(ctx, name) })
}

func TestAttachedTerminalColorsAnswerQueries(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	name := sessionName(t)
	c := newTestClient(t)
	killOnCleanup(t, c, name)

	ft := newFakeTerminal(t, 80, 24)
	creator := *c
	creator.Stdin, creator.Stdout = ft.tty, ft.tty
	dir, command := t.TempDir(), programCommand(t, "query")
	result := make(chan error, 1)
	go func() {
		result <- creator.NewSession(ctx, name, dir, nil, command)
	}()
	// The color the terminal reported to the client is the session's.
	ft.waitOutput(t, `answers:"\x1b[1;4R\x1b]11;rgb:1111/2222/3333\a\x1b[?62;22c"`)
	// The queries never reach the attached terminal: the session answers
	// them.
	if out := ft.output(); strings.Contains(out, "abc\x1b[6n") {
		t.Errorf("a query reached the terminal: %q", out)
	}

	// Killing the session ends the attachment.
	if err := c.KillSession(ctx, name); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Errorf("NewSession: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the client to return")
	}
	ft.waitOutput(t, "[exited]")
}

func TestAttachToItselfIsRefused(t *testing.T) {
	ctx := t.Context()
	name := sessionName(t)
	c := newTestClient(t)
	killOnCleanup(t, c, name)

	if err := c.newDetachedSession(ctx, name, nil, "exec sleep 60"); err != nil {
		t.Fatalf("newDetachedSession: %v", err)
	}
	dir, err := socketDir(false)
	if err != nil {
		t.Fatal(err)
	}
	// As inside the session itself.
	t.Setenv(envSocket, socketPath(dir, name))
	ft := newFakeTerminal(t, 80, 24)
	attacher := *c
	attacher.Stdin, attacher.Stdout = ft.tty, ft.tty
	if err := attacher.AttachSession(ctx, name); err == nil {
		t.Error("AttachSession attached the session to itself")
	}
}

func TestAttachRequiresTerminal(t *testing.T) {
	t.Parallel()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(r.Close(), w.Close()); err != nil {
			t.Error(err)
		}
	})
	c := &Client{Stdin: r, Stdout: w}
	if err := c.AttachSession(t.Context(), "muxac-no-terminal"); err == nil || !strings.Contains(err.Error(), "not a terminal") {
		t.Errorf("AttachSession without a terminal: %v", err)
	}
}

func TestAttachToMissingSession(t *testing.T) {
	t.Parallel()

	ft := newFakeTerminal(t, 80, 24)
	c := &Client{Stdin: ft.tty, Stdout: ft.tty}
	if err := c.AttachSession(t.Context(), sessionName(t)); err == nil || !strings.Contains(err.Error(), "can't find session") {
		t.Errorf("AttachSession to a missing session: %v", err)
	}
}

// outputPipe returns a pipe to stand for a captured standard output, and a
// function that returns what was written to it once it is closed.
func outputPipe(t *testing.T) (*os.File, func() string) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, err := b.ReadFrom(r)
		read <- b.String() + errorText(err)
	}()
	t.Cleanup(func() {
		if err := errors.Join(w.Close(), r.Close()); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Error(err)
		}
	})
	return w, func() string {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
		return <-read
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return "(error: " + err.Error() + ")"
}

func TestAttachWithCapturedOutput(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	name := sessionName(t)
	c := newTestClient(t)
	killOnCleanup(t, c, name)

	// As in `out=$(muxac new ...)`: only the standard input is a terminal.
	ft := newFakeTerminal(t, 80, 24)
	stdout, captured := outputPipe(t)
	creator := *c
	creator.Stdin, creator.Stdout = ft.tty, stdout
	dir, command := t.TempDir(), programCommand(t, "echo")
	result := make(chan error, 1)
	go func() {
		result <- creator.NewSession(ctx, name, dir, nil, command)
	}()
	// The session is shown on the terminal.
	ft.waitOutput(t, "ready")
	ft.input(t, "\x02d")
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the client to detach")
	}
	// Only the end of the attachment is written to the output.
	if got, want := captured(), "[detached (from session "+name+")]\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestLostServer(t *testing.T) {
	t.Parallel()

	name := sessionName(t)
	dir, err := socketDir(true)
	if err != nil {
		t.Fatal(err)
	}
	// A server that goes away as soon as a client attaches.
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath(dir, name), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Error(err)
		}
	})
	served := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			served <- err
			return
		}
		_, err = readRequest(conn)
		served <- errors.Join(err, conn.Close())
	}()

	ft := newFakeTerminal(t, 80, 24)
	stdout, captured := outputPipe(t)
	c := &Client{Stdin: ft.tty, Stdout: stdout}
	if err := c.AttachSession(t.Context(), name); !errors.Is(err, ErrLostServer) {
		t.Errorf("AttachSession = %v, want ErrLostServer", err)
	}
	if err := <-served; err != nil {
		t.Errorf("fake server: %v", err)
	}
	if got := captured(); got != "[lost server]\n" {
		t.Errorf("output = %q, want %q", got, "[lost server]\n")
	}
}

func TestAttachReplaysHistory(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	c := newTestClient(t)
	name := sessionName(t)
	killOnCleanup(t, c, name)

	// The lines that scrolled off the screen of the session before any
	// terminal attached.
	if err := c.newDetachedSession(ctx, name, nil, "i=1; while [ $i -le 60 ]; do echo line$i; i=$((i+1)); done; exec sleep 60"); err != nil {
		t.Fatalf("newDetachedSession: %v", err)
	}
	eventually(t, "the output on the screen", func() bool {
		text, err := c.captureScreen(ctx, name)
		return err == nil && strings.Contains(text, "line60\n")
	})

	ft := attachFake(t, c, name, 80, 24)
	// The fake terminal answered that its cursor is at the top left, so the
	// history starts there, below which is erased first.
	ft.waitOutput(t, "\x1b[Jline1\r\nline2\r\nline3\r\n")
	ft.waitOutput(t, "line37\r\n")
}

func TestSessionFollowsTheTerminalUsedLast(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	c := newTestClient(t)
	name := sessionName(t)
	killOnCleanup(t, c, name)

	if err := c.newDetachedSession(ctx, name, nil, "exec sleep 60"); err != nil {
		t.Fatalf("newDetachedSession: %v", err)
	}
	// The screen of the session has as many lines as the session has rows.
	rows := func() int {
		text, err := c.captureScreen(ctx, name)
		if err != nil {
			return -1
		}
		return strings.Count(text, "\n")
	}
	waitRows := func(want int, what string) {
		t.Helper()

		eventually(t, what, func() bool { return rows() == want })
	}

	a := attachFake(t, c, name, 80, 20)
	waitRows(20, "the session to take the size of the terminal that attached")
	b := attachFake(t, c, name, 100, 30)
	waitRows(30, "the session to take the size of the terminal that attached last")
	// A terminal typed into is used, although its size did not change, while
	// another one stays attached, as the hidden one of a preview does.
	a.input(t, "x")
	waitRows(20, "the session to take the size of the terminal typed into")
	// A terminal that loses the focus is not being used, one that gets it is.
	b.input(t, focusOutReport)
	time.Sleep(300 * time.Millisecond)
	if got := rows(); got != 20 {
		t.Errorf("the session has %d rows once a terminal lost the focus, want 20", got)
	}
	b.input(t, "\x1b[I")
	waitRows(30, "the session to take the size of the terminal that got the focus")
}

func TestRelayTellsTheResizeSinceAttaching(t *testing.T) {
	t.Parallel()

	// The terminal was resized after its size was read for attaching, before
	// the relay followed its resizes.
	ft := newFakeTerminal(t, 90, 25)
	conn, server := net.Pipe()
	result := make(chan error, 1)
	go func() {
		_, err := relay(t.Context(), conn, readInput(ft.tty), nil, ft.tty, keyScanner{}, winsize{Cols: 80, Rows: 20})
		result <- err
	}()
	if err := server.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	typ, payload, err := readFrame(server)
	if err != nil {
		t.Fatalf("read the first message of the relay: %v", err)
	}
	size, err := decodeSize(payload)
	if typ != msgResize || err != nil || size.Cols != 90 || size.Rows != 25 {
		t.Errorf("the relay sent message %d (%+v, %v) first, want a resize to 90x25", typ, size, err)
	}
	// The server going away ends the relay.
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrLostServer) {
		t.Errorf("relay = %v, want ErrLostServer", err)
	}
}

func TestFocusOut(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]bool{
		"":                 false,
		"\x1b[O":           true,
		"\x1b[O\x1b[O":     true,
		"\x1b[I":           false,
		"\x1b[O\x1b[I":     false,
		"\x1b[Ox":          false,
		"x":                false,
		"\x1b[O\x1b[O\x1b": false,
	} {
		if got := focusOut([]byte(input)); got != want {
			t.Errorf("focusOut(%q) = %t, want %t", input, got, want)
		}
	}
}

func TestTerminalOfAnotherSizeIsCaughtUp(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	c := newTestClient(t)
	name := sessionName(t)
	killOnCleanup(t, c, name)

	// The program echoes what it reads.
	if err := c.newDetachedSession(ctx, name, nil, programCommand(t, "echo")); err != nil {
		t.Fatalf("newDetachedSession: %v", err)
	}

	a := attachFake(t, c, name, 80, 20)
	a.waitOutput(t, "ready")
	b := attachFake(t, c, name, 100, 30)
	b.waitOutput(t, "ready")
	// The terminal of a no longer has the size of the session: it would not
	// show the output right, and gets none.
	before := len(a.output())
	for i := range 40 {
		s := strconv.Itoa(i)
		b.input(t, s)
		b.waitOutput(t, `in:"`+s+`"`)
	}
	if got := len(a.output()); got != before {
		t.Errorf("the terminal of another size got %d bytes of output", got-before)
	}
	// Used again, it is caught up: the lines it missed come first.
	a.input(t, "x")
	a.waitOutput(t, `in:"0"`)
	a.waitOutput(t, `in:"x"`)
}

func TestTerminalShowsLinesWholeAfterANarrowerOne(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	c := newTestClient(t)
	name := sessionName(t)
	killOnCleanup(t, c, name)

	// The line is printed once the session is as wide as the terminal.
	long := strings.Repeat("x", 100) + "END"
	if err := c.newDetachedSession(ctx, name, nil, "read line; echo "+long+"; exec sleep 60"); err != nil {
		t.Fatalf("newDetachedSession: %v", err)
	}
	a := attachFake(t, c, name, 120, 20)
	a.input(t, "\r")
	a.waitOutput(t, long)
	// A narrower terminal is used meanwhile, as the preview of a picker is,
	// cutting the line on the screen of the session.
	b := attachFake(t, c, name, 80, 10)
	b.waitOutput(t, strings.Repeat("x", 80))
	before := len(a.output())
	// Used again, the terminal shows the line whole.
	a.input(t, "z")
	eventually(t, "the line whole on the terminal used again", func() bool {
		return strings.Contains(a.output()[before:], long)
	})
}

func TestTerminalShowsOutputWholeAfterAnotherSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// cols is the width of the terminal used while the lines are
		// printed, and n how long they are.
		cols, n int
	}{
		{name: "wider", cols: 120, n: 100},
		{name: "narrower", cols: 60, n: 90},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			c := newTestClient(t)
			name := sessionName(t)
			killOnCleanup(t, c, name)

			// The lines are printed once the other terminal is used.
			var lines []string
			for i := range 5 {
				lines = append(lines, "L"+strconv.Itoa(i)+strings.Repeat("x", tc.n-5)+"END")
			}
			if err := c.newDetachedSession(ctx, name, nil, "read go; printf '%s\\n' "+strings.Join(lines, " ")+"; exec sleep 60"); err != nil {
				t.Fatalf("newDetachedSession: %v", err)
			}
			a := attachFake(t, c, name, 80, 20)
			a.waitOutput(t, "\x1b[?2026l")
			b := attachFake(t, c, name, tc.cols, 10)
			b.input(t, "\r")
			b.waitOutput(t, lines[len(lines)-1])
			before := len(a.output())
			// Used again, the terminal shows every line whole.
			a.input(t, "z")
			for _, line := range lines {
				eventually(t, "the line whole on the terminal used again", func() bool {
					return strings.Contains(a.output()[before:], line)
				})
			}
		})
	}
}

func TestSessionKeepsNoInheritedFiles(t *testing.T) {
	t.Parallel()

	// A file this process has open without closing it on exec, as the
	// editor muxac runs in may have.
	fd, err := syscall.Open(filepath.Join(t.TempDir(), "inherited"), syscall.O_CREAT|syscall.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Close(fd); err != nil {
			t.Error(err)
		}
	})

	ctx := t.Context()
	c := newTestClient(t)
	name := sessionName(t)
	killOnCleanup(t, c, name)
	command := fmt.Sprintf("if [ -e /dev/fd/%d ]; then echo inherited; else echo not inherited; fi; exec sleep 60", fd)
	if err := c.newDetachedSession(ctx, name, nil, command); err != nil {
		t.Fatalf("newDetachedSession: %v", err)
	}
	var text string
	eventually(t, "the output on the screen", func() bool {
		text, err = c.captureScreen(ctx, name)
		return err == nil && strings.Contains(text, "inherited\n")
	})
	if !strings.HasPrefix(text, "not inherited\n") {
		t.Errorf("the command of the session has descriptor %d open: screen = %q", fd, text)
	}
}
