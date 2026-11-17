// Package mux is muxac's terminal multiplexer. Every session runs in a
// server process of its own: a `muxac server` that runs the session's command
// on a pseudo-terminal, keeps track of its screen with a virtual terminal
// (package vt), and serves the clients that connect to the session's Unix
// socket, in a directory private to the user. Clients attach a terminal to
// the session, ask what the screen shows, or kill the session.
//
// An attached terminal shows the session's output as the command writes it:
// the server passes the output on rather than drawing the screen itself, so
// the terminal's own features, such as its scrollback, keep working. The
// server only draws the screen when a terminal attaches, or when it has to
// catch up, and answers the command's queries about its terminal itself, so
// that the command sees the same terminal whether one is attached or not.
package mux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"
)

// Runner is the interface for session operations.
type Runner interface {
	HasSession(ctx context.Context, sessionName string) bool
	AttachSession(ctx context.Context, sessionName string) error
	NewSession(ctx context.Context, sessionName, dir string, env []string, command string) error
	ListSessionNames(ctx context.Context) ([]string, error)
	KillSession(ctx context.Context, sessionName string) error
}

// startTimeout bounds how long starting the server of a session may take.
const startTimeout = 30 * time.Second

// Client implements Runner by talking to the servers of the sessions, which
// it starts as `muxac server` processes.
type Client struct {
	// Exe is the muxac executable to start servers with, the running one if
	// empty.
	Exe string
	// Stdin and Stdout are the terminal that attaches to sessions, os.Stdin
	// and os.Stdout if nil.
	Stdin, Stdout *os.File
}

var _ Runner = (*Client)(nil)

// errNoSession tells that a session does not exist.
var errNoSession = errors.New("no such session")

// dial connects to the server of the session.
func (c *Client) dial(ctx context.Context, sessionName string) (net.Conn, error) {
	dir, err := socketDir(false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNoSession
		}
		return nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath(dir, sessionName))
	if err != nil {
		if stale(err) {
			return nil, errNoSession
		}
		return nil, err
	}
	return conn, nil
}

// do sends a request other than attaching to the server of the session and
// returns its response.
func (c *Client) do(ctx context.Context, sessionName, op string, timeout time.Duration) (response, error) {
	conn, err := c.dial(ctx, sessionName)
	if err != nil {
		return response{}, err
	}
	res, err := roundTrip(ctx, conn, request{Version: protocolVersion, Op: op}, timeout)
	return res, errors.Join(err, conn.Close())
}

func roundTrip(ctx context.Context, conn net.Conn, req request, timeout time.Duration) (response, error) {
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return response{}, err
	}
	if err := writeJSON(conn, msgRequest, req); err != nil {
		return response{}, err
	}
	typ, payload, err := readFrame(conn)
	if err != nil {
		return response{}, err
	}
	if typ != msgResponse {
		return response{}, fmt.Errorf("unexpected message %d from the session", typ)
	}
	var res response
	if err := json.Unmarshal(payload, &res); err != nil {
		return response{}, err
	}
	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	return res, nil
}

// HasSession reports whether the session runs.
func (c *Client) HasSession(ctx context.Context, sessionName string) bool {
	res, err := c.do(ctx, sessionName, opInfo, time.Second)
	return err == nil && res.Name == sessionName
}

// ListSessionNames returns the names of the running sessions. A session
// whose server does not answer in time makes it fail, rather than being
// left out as if it did not run.
func (c *Client) ListSessionNames(ctx context.Context) ([]string, error) {
	dir, err := socketDir(false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type()&os.ModeSocket == 0 {
			continue
		}
		path := filepath.Join(dir, e.Name())
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if stale(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		res, err := roundTrip(ctx, conn, request{Version: protocolVersion, Op: opInfo}, time.Second)
		if err := errors.Join(err, conn.Close()); err != nil {
			if gone(err) {
				// The server went away before answering: its session ended.
				continue
			}
			return nil, fmt.Errorf("ask the session at %s: %w", path, err)
		}
		names = append(names, res.Name)
	}
	slices.Sort(names)
	return names, nil
}

// KillSession ends the session, hanging up its command's terminal. Once it
// returns, the session no longer exists.
func (c *Client) KillSession(ctx context.Context, sessionName string) error {
	_, err := c.do(ctx, sessionName, opKill, 30*time.Second)
	if gone(err) {
		// The server went away before answering: the session ended anyway.
		return nil
	}
	return err
}

// envHistoryLimit names the environment variable that sets how many of the
// lines that went off the top of its screen a session keeps, to show them
// again to the terminals that attach.
const envHistoryLimit = "MUXAC_HISTORY_LIMIT"

// defaultHistoryLimit is the number of lines a session keeps by default.
const defaultHistoryLimit = 10000

// historyLimit returns the number of lines a session keeps, as envHistoryLimit
// sets it.
func historyLimit() (int, error) {
	s := os.Getenv(envHistoryLimit)
	if s == "" {
		return defaultHistoryLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: want a number of lines: %w", envHistoryLimit, s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s=%q: want a number of lines, not a negative one", envHistoryLimit, s)
	}
	return n, nil
}

// sessionEnv returns the environment of the command of a session: muxac's
// own, with env added.
func sessionEnv(sessionName string, env []string) []string {
	out := os.Environ()
	if os.Getenv("TERM") == "" {
		out = append(out, "TERM=xterm-256color")
	}
	out = append(out, env...)
	return append(out, envSocket+"="+socketPath(socketDirPath(), sessionName))
}

// startServer starts the server of a session and waits until the session
// runs. If creator is not nil, it is the connection over which the calling
// client attaches.
func (c *Client) startServer(ctx context.Context, cfg config, creator *os.File) error {
	limit, err := historyLimit()
	if err != nil {
		return err
	}
	cfg.HistoryLimit = limit
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	cfgR, cfgW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer cfgW.Close()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return errors.Join(err, cfgR.Close())
	}
	defer readyR.Close()

	exe := c.Exe
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return errors.Join(err, cfgR.Close(), readyW.Close())
		}
	}
	// The server gets the files passed to it, and none of the ones this
	// process inherited, such as the ones of an editor whose terminal runs
	// muxac: the server and the session's command would keep them open as
	// long as the session lasts.
	if err := closeInheritedOnExec(); err != nil {
		return errors.Join(fmt.Errorf("keep the inherited files from the server: %w", err), cfgR.Close(), readyW.Close())
	}
	cmd := exec.Command(exe, "server")
	cmd.Stdin = cfgR
	cmd.ExtraFiles = []*os.File{readyW}
	if creator != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, creator)
	}
	// The server runs in a session of its own, without a controlling
	// terminal, so that it outlives the terminal it was started from.
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	startErr := cmd.Start()
	if err := errors.Join(startErr, cfgR.Close(), readyW.Close()); err != nil {
		return err
	}
	// The server outlives this process, which does not wait for it: whether
	// it started the session, it reports through the ready pipe.
	if err := cmd.Process.Release(); err != nil {
		return err
	}

	if _, err := cfgW.Write(b); err != nil {
		return fmt.Errorf("send the session config: %w", err)
	}
	if err := cfgW.Close(); err != nil {
		return err
	}

	deadline := time.Now().Add(startTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := readyR.SetReadDeadline(deadline); err != nil {
		return err
	}
	// The session goes on starting if ctx is done; only waiting for it
	// stops.
	stop := context.AfterFunc(ctx, func() { readyR.Close() })
	defer stop()
	result, err := io.ReadAll(readyR)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("start the session: %w", err)
	}
	var res startResult
	if err := json.Unmarshal(result, &res); err != nil {
		return fmt.Errorf("start the session: the server exited without starting it: %w", err)
	}
	if res.Error != "" {
		return errors.New(res.Error)
	}
	return nil
}
