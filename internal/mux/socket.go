package mux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// envTmpDir names the environment variable that overrides the directory in
// which muxac makes the directory of its sockets, /tmp by default.
const envTmpDir = "MUXAC_TMPDIR"

// envSocket names the environment variable that tells the programs of a
// session the path of its socket, so that muxac never attaches a session to
// itself.
const envSocket = "MUXAC_SOCKET"

// lockName is the file in the socket directory that serializes the creation
// and removal of sockets.
const lockName = "lock"

// socketDirPath returns the path of the directory holding the sockets of the
// sessions of the current user.
func socketDirPath() string {
	base := os.Getenv(envTmpDir)
	if base == "" {
		base = "/tmp"
	}
	return filepath.Join(base, fmt.Sprintf("muxac-%d", os.Getuid()))
}

// socketDir returns the directory holding the sockets of the sessions of the
// current user, creating it if create is set.
func socketDir(create bool) (string, error) {
	return checkSocketDir(socketDirPath(), create)
}

// checkSocketDir returns dir, creating it if create is set. As tmux does, it
// refuses a directory that other users could tamper with.
func checkSocketDir(dir string, create bool) (string, error) {
	if create {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() || fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("directory %s has unsafe permissions", dir)
	}
	return dir, nil
}

// socketPath returns the path of the socket of the session named name in
// dir. Session names can be longer than a socket path may be, so the file is
// named after a hash of the name.
func socketPath(dir, name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(dir, hex.EncodeToString(sum[:16]))
}

// lockDir takes the lock of the socket directory dir, and returns the
// function that releases it.
func lockDir(dir string) (func() error, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	conn, err := f.SyscallConn()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	var flockErr error
	if err := conn.Control(func(fd uintptr) {
		for {
			flockErr = unix.Flock(int(fd), unix.LOCK_EX)
			if !errors.Is(flockErr, unix.EINTR) {
				return
			}
		}
	}); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if flockErr != nil {
		return nil, errors.Join(fmt.Errorf("lock %s: %w", dir, flockErr), f.Close())
	}
	// Closing the file releases the lock.
	return f.Close, nil
}

// alive reports whether a server listens on the socket at path.
func alive(path string) (bool, error) {
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(context.Background(), "unix", path)
	switch {
	case stale(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, conn.Close()
}

// stale reports whether err, from connecting to the socket of a session,
// tells that no server listens on it: there is no socket, or one left by a
// server that did not end normally.
func stale(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}
