package mux

import (
	"errors"
	"math"

	"golang.org/x/sys/unix"
)

// closeInheritedOnExec marks the descriptors from 3 on as closed on exec
// (see Client.startServer).
func closeInheritedOnExec() error {
	rangeErr := unix.CloseRange(3, math.MaxUint32, unix.CLOSE_RANGE_CLOEXEC)
	if rangeErr == nil {
		return nil
	}
	// Kernels before 5.11, and sandboxes that do not let the call through,
	// do not mark them all at once.
	if err := markListedOnExec("/proc/self/fd"); err != nil {
		return errors.Join(rangeErr, err)
	}
	return nil
}
