package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

var (
	version = "dev"
	commit  = ""
)

type psiReading struct {
	name       string
	some, full pressure
}

func main() {
	batchN := flag.Int("batch", 0, "run N iterations then exit (0 = infinite)")
	psiInterval := flag.Duration("psi", 5*time.Second, "PSI/load/zpool-state refresh interval")
	cpuInterval := flag.Duration("cpu", 40*time.Millisecond, "CPU utilization sample interval")
	zpoolInterval := flag.Duration("zpool", 15*time.Second, "zpool status subprocess interval")
	displayInterval := flag.Duration("display", 100*time.Millisecond, "display refresh interval")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		if commit != "" {
			fmt.Printf("psi %s (%s)\n", version, commit)
		} else {
			fmt.Printf("psi %s\n", version)
		}
		return
	}

	// Open file descriptors.
	loadFd := mustOpen("/proc/loadavg")
	defer syscall.Close(loadFd)
	psiFiles := []psiFile{
		{"CPU", mustOpen("/proc/pressure/cpu")},
		{"IO", mustOpen("/proc/pressure/io")},
		{"MEMORY", mustOpen("/proc/pressure/memory")},
	}
	defer func() {
		for _, pf := range psiFiles {
			syscall.Close(pf.fd)
		}
	}()
	poolFds := discoverPools()
	defer func() {
		for _, pf := range poolFds {
			syscall.Close(pf.fd)
		}
	}()
	cpuWindow := int(time.Second / *cpuInterval)
	if cpuWindow < 1 {
		cpuWindow = 1
	}
	cpuSt := newCpuState(cpuWindow)
	if cpuSt != nil {
		defer syscall.Close(cpuSt.fd)
	}

	// Shared state protected by mu.
	var mu sync.Mutex
	psiReadings := make([]psiReading, len(psiFiles))
	var load loadSnapshot
	var zpPools []zpPool
	var zpLastRefresh time.Time

	done := make(chan struct{})

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	// Initial data collection (before starting goroutines, no lock needed).
	{
		var buf [512]byte
		for j, pf := range psiFiles {
			some, full := readPressure(pf.fd, buf[:])
			psiReadings[j] = psiReading{pf.name, some, full}
		}
		load = readLoad(loadFd, buf[:])
		refreshZpoolCache(&zpPools, &zpLastRefresh, *zpoolInterval)
		updatePoolStates(poolFds, buf[:], zpPools)
		if cpuSt != nil {
			cpuSt.update() // seed first sample
		}
	}

	// --- CPU collector goroutine ---
	if cpuSt != nil {
		go func() {
			ticker := time.NewTicker(*cpuInterval)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
				}
				mu.Lock()
				cpuSt.update()
				mu.Unlock()
			}
		}()
	}

	// --- PSI/load/zpool collector goroutine ---
	go func() {
		var buf [512]byte
		ticker := time.NewTicker(*psiInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			mu.Lock()
			for j, pf := range psiFiles {
				some, full := readPressure(pf.fd, buf[:])
				psiReadings[j] = psiReading{pf.name, some, full}
			}
			load = readLoad(loadFd, buf[:])
			refreshZpoolCache(&zpPools, &zpLastRefresh, *zpoolInterval)
			updatePoolStates(poolFds, buf[:], zpPools)
			mu.Unlock()
		}
	}()

	// --- Display loop (main goroutine) ---
	var w bytes.Buffer
	displayTicker := time.NewTicker(*displayInterval)
	defer displayTicker.Stop()

	for i := 0; *batchN == 0 || i < *batchN; i++ {
		mu.Lock()
		w.Reset()
		if *batchN == 0 {
			fmt.Fprint(&w, "\033[H\033[2J")
		}
		printLoadTable(&w, load)
		for _, r := range psiReadings {
			printTable(&w, r.name, r.some, r.full)
		}
		if cpuSt != nil {
			printCpuTable(&w, cpuSt)
		}
		printZpoolStatus(&w, zpPools)
		mu.Unlock()
		os.Stdout.Write(w.Bytes())

		if *batchN > 0 && i == *batchN-1 {
			break
		}
		select {
		case <-sig:
			close(done)
			return
		case <-displayTicker.C:
		}
	}
	close(done)
}
