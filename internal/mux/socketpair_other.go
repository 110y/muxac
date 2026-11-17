//go:build !linux

package mux

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// socketpair returns a connected pair of sockets, both closed on exec.
//
// Without SOCK_CLOEXEC, the descriptors are marked while holding
// syscall.ForkLock, so that no process started meanwhile inherits them.
func socketpair() ([2]int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return fds, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	return fds, nil
}
