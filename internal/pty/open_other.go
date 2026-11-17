//go:build !linux

package pty

import (
	"os"

	"github.com/creack/pty"
)

// Open opens a new pseudo-terminal and returns its primary and secondary
// sides.
func Open() (*os.File, *os.File, error) {
	return pty.Open()
}
