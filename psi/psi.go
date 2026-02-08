package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
)

type pressure struct {
	avg10  string
	avg60  string
	avg300 string
	total  string
}

func parseLine(line string) pressure {
	p := pressure{}
	fields := strings.Fields(line)
	for _, f := range fields[1:] {
		parts := strings.SplitN(f, "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "avg10":
			p.avg10 = parts[1]
		case "avg60":
			p.avg60 = parts[1]
		case "avg300":
			p.avg300 = parts[1]
		case "total":
			p.total = parts[1]
		}
	}
	return p
}

func pread(fd int, buf []byte) int {
	n, _ := syscall.Pread(fd, buf, 0)
	return n
}

func readPressure(fd int, buf []byte) (some, full pressure) {
	n := pread(fd, buf)
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		if strings.HasPrefix(line, "some ") {
			some = parseLine(line)
		} else if strings.HasPrefix(line, "full ") {
			full = parseLine(line)
		}
	}
	return
}

func formatTotal(t string) string {
	var us uint64
	fmt.Sscanf(t, "%d", &us)
	secs := float64(us) / 1000000.0
	if secs >= 3600 {
		return fmt.Sprintf("%.1fh", secs/3600)
	} else if secs >= 60 {
		return fmt.Sprintf("%.1fm", secs/60)
	}
	return fmt.Sprintf("%.1fs", secs)
}

func printTable(name string, some, full pressure) {
	fmt.Printf("%-6s │ %7s │ %7s │ %7s │ %10s\n", name, "avg10", "avg60", "avg300", "total")
	fmt.Printf("───────┼─────────┼─────────┼─────────┼───────────\n")
	fmt.Printf("%-6s │ %6s%% │ %6s%% │ %6s%% │ %10s\n", "some", some.avg10, some.avg60, some.avg300, formatTotal(some.total))
	fmt.Printf("%-6s │ %6s%% │ %6s%% │ %6s%% │ %10s\n", "full", full.avg10, full.avg60, full.avg300, formatTotal(full.total))
	fmt.Println()
}

func printLoadTable(fd int, buf []byte) {
	n := pread(fd, buf)
	fields := strings.Fields(string(buf[:n]))
	min1, min5, min15 := fields[0], fields[1], fields[2]
	procs := ""
	if len(fields) >= 4 {
		procs = fields[3]
	}
	fmt.Printf("%-6s │ %7s │ %7s │ %7s │ %10s\n", "LOAD", "1min", "5min", "15min", "procs")
	fmt.Printf("───────┼─────────┼─────────┼─────────┼───────────\n")
	fmt.Printf("%-6s │ %7s │ %7s │ %7s │ %10s\n", "", min1, min5, min15, procs)
	fmt.Println()
}

type psiFile struct {
	name string
	fd   int
}

func mustOpen(path string) int {
	fd, err := syscall.Open(path, syscall.O_RDONLY, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", path, err)
		os.Exit(1)
	}
	return fd
}

func main() {
	loadFd := mustOpen("/proc/loadavg")
	psiFiles := []psiFile{
		{"CPU", mustOpen("/proc/pressure/cpu")},
		{"IO", mustOpen("/proc/pressure/io")},
		{"MEMORY", mustOpen("/proc/pressure/memory")},
	}

	var buf [512]byte

	for {
		fmt.Print("\033[H\033[2J")
		printLoadTable(loadFd, buf[:])
		for _, pf := range psiFiles {
			some, full := readPressure(pf.fd, buf[:])
			printTable(pf.name, some, full)
		}
		time.Sleep(1 * time.Second)
	}
}
