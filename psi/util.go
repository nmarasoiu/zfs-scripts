package main

import (
	"fmt"
	"os"
	"syscall"
)

type psiFile struct {
	name string
	fd   int
}

func pread(fd int, buf []byte) int {
	n, _ := syscall.Pread(fd, buf, 0)
	return n
}

func mustOpen(path string) int {
	fd, err := syscall.Open(path, syscall.O_RDONLY, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", path, err)
		os.Exit(1)
	}
	return fd
}

const (
	ansiRed   = "\033[31m"
	ansiBold  = "\033[1m"
	ansiReset = "\033[0m"
)
