package mux

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// markListedOnExec marks the descriptors from 3 on that dir, which lists the
// open descriptors of the process, names as closed on exec.
func markListedOnExec(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("list the open files: %w", err)
	}
	names, err := d.Readdirnames(-1)
	if err := errors.Join(err, d.Close()); err != nil {
		return fmt.Errorf("list the open files: %w", err)
	}
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			return fmt.Errorf("list the open files: %w", err)
		}
		if fd > 2 {
			syscall.CloseOnExec(fd)
		}
	}
	return nil
}
