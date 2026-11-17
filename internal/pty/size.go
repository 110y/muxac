package pty

import (
	"os"

	"golang.org/x/sys/unix"
)

// Size is the size of a terminal, in characters and in pixels.
type Size struct {
	Cols, Rows     int
	XPixel, YPixel int
}

// GetSize returns the size of the terminal f.
func GetSize(f *os.File) (Size, error) {
	var ws *unix.Winsize
	err := control(f, func(fd int) error {
		var err error
		ws, err = unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
		return err
	})
	if err != nil {
		return Size{}, err
	}
	return Size{Cols: int(ws.Col), Rows: int(ws.Row), XPixel: int(ws.Xpixel), YPixel: int(ws.Ypixel)}, nil
}

// SetSize changes the size of the terminal f, which sends SIGWINCH to the
// foreground process group of the terminal when it changes.
func SetSize(f *os.File, s Size) error {
	ws := &unix.Winsize{
		Col:    uint16(min(max(s.Cols, 0), 0xffff)),
		Row:    uint16(min(max(s.Rows, 0), 0xffff)),
		Xpixel: uint16(min(max(s.XPixel, 0), 0xffff)),
		Ypixel: uint16(min(max(s.YPixel, 0), 0xffff)),
	}
	return control(f, func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, ws)
	})
}

// control runs fn on the descriptor of f without changing its blocking mode,
// as f.Fd() would.
func control(f *os.File, fn func(fd int) error) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var fnErr error
	if err := conn.Control(func(fd uintptr) { fnErr = fn(int(fd)) }); err != nil {
		return err
	}
	return fnErr
}
