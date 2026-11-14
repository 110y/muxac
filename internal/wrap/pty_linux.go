package wrap

import (
	"errors"
	"os"
	"strconv"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// openPTYAttempts is how many times openPTY tries to get a consistent pair.
const openPTYAttempts = 3

// openPTY opens a new pseudo-terminal and returns its primary and secondary
// sides.
//
// creack/pty v1.1.20 to v1.1.24 can return the wrong secondary side on Linux:
// it asks the kernel for the device number through an ioctl whose result is
// lost when the goroutine's stack moves during the call, and then opens
// /dev/pts/0 instead (https://github.com/creack/pty/pull/215, fixed but not
// released yet). The number is therefore read again with x/sys/unix, and the
// pair is opened anew when the two disagree, so the command is never attached
// to someone else's terminal.
func openPTY() (*os.File, *os.File, error) {
	for range openPTYAttempts {
		ptmx, tty, err := pty.Open()
		if err != nil {
			return nil, nil, err
		}
		n, err := unix.IoctlGetUint32(int(ptmx.Fd()), unix.TIOCGPTN)
		if err != nil {
			return nil, nil, errors.Join(err, tty.Close(), ptmx.Close())
		}
		if tty.Name() == "/dev/pts/"+strconv.FormatUint(uint64(n), 10) {
			return ptmx, tty, nil
		}
		if err := errors.Join(tty.Close(), ptmx.Close()); err != nil {
			return nil, nil, err
		}
	}
	return nil, nil, errors.New("open pseudo-terminal: secondary device does not match the primary")
}
