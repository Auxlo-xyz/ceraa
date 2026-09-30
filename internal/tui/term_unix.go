package tui

import (
	"os"
	"syscall"
	"unsafe"
)

// terminalSize asks the kernel for the window size.
//
// A raw ioctl rather than a dependency: the request is one struct and the
// fallback is already handled by width, so a package that only ever needs this
// one value should not pull a syscall wrapper in for it.
func terminalSize(fd uintptr) (int, int, error) {
	var ws struct {
		rows, cols, x, y uint16
	}
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		fd,
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno != 0 {
		return 0, 0, errno
	}
	return int(ws.cols), int(ws.rows), nil
}

// StdinIsTerminal reports whether input is a terminal, which decides whether
// prompts and the interactive loop are offered at all.
func StdinIsTerminal() bool { return term.isTerminal(os.Stdin.Fd()) }
