package main

import (
	"os"

	"golang.org/x/sys/unix"
)

type keyKind int

const (
	keyChar keyKind = iota
	keyEnter
	keyBackspace
)

type keyEvent struct {
	kind keyKind
	ch   byte
}

// isTerminal returns true if the given fd refers to a terminal.
func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

// enableRawMode puts stdin into raw mode (no ICANON/ECHO) but keeps ISIG
// so Ctrl+C still triggers the signal handler. Returns the original termios
// for later restore, or nil if stdin is not a terminal.
func enableRawMode() *unix.Termios {
	orig, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS)
	if err != nil {
		return nil
	}
	raw := *orig
	raw.Lflag &^= unix.ICANON | unix.ECHO
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TCSETS, &raw)
	return orig
}

// restoreTermMode restores the original terminal settings.
func restoreTermMode(orig *unix.Termios) {
	if orig != nil {
		unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TCSETS, orig)
	}
}

// termSize returns the terminal dimensions (cols, rows). Returns 0,0 if not a terminal.
func termSize() (int, int) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0
	}
	return int(ws.Col), int(ws.Row)
}

// runInput reads stdin byte-by-byte and sends parsed key events to ch.
// Exits on read error (stdin closed or program shutting down).
func runInput(ch chan<- keyEvent) {
	var buf [1]byte
	for {
		n, err := os.Stdin.Read(buf[:])
		if err != nil || n == 0 {
			return
		}
		b := buf[0]
		switch {
		case b == 0x0d || b == 0x0a:
			ch <- keyEvent{kind: keyEnter}
		case b == 0x7f || b == 0x08:
			ch <- keyEvent{kind: keyBackspace}
		case b >= 0x20 && b <= 0x7e:
			ch <- keyEvent{kind: keyChar, ch: b}
		}
	}
}
