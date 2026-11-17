package mux

import "golang.org/x/sys/unix"

// socketpair returns a connected pair of sockets, both closed on exec.
func socketpair() ([2]int, error) {
	return unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
}
