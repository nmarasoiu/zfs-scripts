package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	batchN := flag.Int("batch", 0, "run N iterations then exit (0 = infinite)")
	flag.Parse()

	loadFd := mustOpen("/proc/loadavg")
	psiFiles := []psiFile{
		{"CPU", mustOpen("/proc/pressure/cpu")},
		{"IO", mustOpen("/proc/pressure/io")},
		{"MEMORY", mustOpen("/proc/pressure/memory")},
	}
	poolFds := discoverPools()
	cpuSt := newCpuState()

	var buf [512]byte
	var w bytes.Buffer

	for i := 0; *batchN == 0 || i < *batchN; i++ {
		refreshZpoolCache()
		updatePoolStates(poolFds, buf[:])
		if cpuSt != nil {
			cpuSt.update()
		}

		w.Reset()
		if *batchN == 0 {
			fmt.Fprint(&w, "\033[H\033[2J")
		}
		printLoadTable(&w, loadFd, buf[:])
		for _, pf := range psiFiles {
			some, full := readPressure(pf.fd, buf[:])
			printTable(&w, pf.name, some, full)
		}
		if cpuSt != nil {
			printCpuTable(&w, cpuSt)
		}
		printZpoolStatus(&w)
		os.Stdout.Write(w.Bytes())

		if *batchN > 0 && i == *batchN-1 {
			break
		}
		time.Sleep(4 * time.Second)
	}
}
