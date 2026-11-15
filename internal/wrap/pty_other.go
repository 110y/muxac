//go:build !linux

package wrap

import (
	"os"

	"github.com/creack/pty"
)

// openPTY opens a new pseudo-terminal and returns its primary and secondary
// sides.
func openPTY() (*os.File, *os.File, error) {
	return pty.Open()
}
