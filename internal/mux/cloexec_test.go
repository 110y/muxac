package mux

import (
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMarkListedOnExec(t *testing.T) {
	t.Parallel()

	fd, err := syscall.Open(filepath.Join(t.TempDir(), "inherited"), syscall.O_CREAT|syscall.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Close(fd); err != nil {
			t.Error(err)
		}
	})

	if err := markListedOnExec("/dev/fd"); err != nil {
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Errorf("descriptor %d is not closed on exec", fd)
	}
}
