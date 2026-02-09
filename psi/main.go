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
	psiInterval := flag.Duration("psi", 4*time.Second, "PSI/load refresh interval")
	cpuInterval := flag.Duration("cpu", 4*time.Second, "CPU utilization refresh interval")
	zpoolInterval := flag.Duration("zpool", 30*time.Second, "zpool status subprocess interval")
	flag.Parse()

	zpRefreshInterval = *zpoolInterval

	loadFd := mustOpen("/proc/loadavg")
	psiFiles := []psiFile{
		{"CPU", mustOpen("/proc/pressure/cpu")},
		{"IO", mustOpen("/proc/pressure/io")},
		{"MEMORY", mustOpen("/proc/pressure/memory")},
	}
	poolFds := discoverPools()
	cpuSt := newCpuState()

	// Main loop ticks at the smallest component interval.
	tick := *psiInterval
	if *cpuInterval < tick {
		tick = *cpuInterval
	}
	if *zpoolInterval < tick {
		tick = *zpoolInterval
	}

	var buf [512]byte
	var w bytes.Buffer

	// Cached PSI readings — re-read only on psiInterval.
	type psiReading struct {
		name       string
		some, full pressure
	}
	psiReadings := make([]psiReading, len(psiFiles))
	var lastPsi, lastCpu time.Time

	for i := 0; *batchN == 0 || i < *batchN; i++ {
		now := time.Now()

		refreshZpoolCache()
		updatePoolStates(poolFds, buf[:])

		if i == 0 || now.Sub(lastPsi) >= *psiInterval {
			for j, pf := range psiFiles {
				some, full := readPressure(pf.fd, buf[:])
				psiReadings[j] = psiReading{pf.name, some, full}
			}
			lastPsi = now
		}

		if cpuSt != nil && (i == 0 || now.Sub(lastCpu) >= *cpuInterval) {
			cpuSt.update()
			lastCpu = now
		}

		w.Reset()
		if *batchN == 0 {
			fmt.Fprint(&w, "\033[H\033[2J")
		}
		printLoadTable(&w, loadFd, buf[:])
		for _, r := range psiReadings {
			printTable(&w, r.name, r.some, r.full)
		}
		if cpuSt != nil {
			printCpuTable(&w, cpuSt)
		}
		printZpoolStatus(&w)
		os.Stdout.Write(w.Bytes())

		if *batchN > 0 && i == *batchN-1 {
			break
		}
		time.Sleep(tick)
	}
}
