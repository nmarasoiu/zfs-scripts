package psiparse

import (
	"strconv"
	"strings"
	"syscall"
)

// Pressure holds the kernel-computed PSI averages and cumulative total.
type Pressure struct {
	Avg10  float64
	Avg60  float64
	Avg300 float64
	Total  uint64 // cumulative stall microseconds
}

func parseLine(line string) Pressure {
	var p Pressure
	for _, f := range strings.Fields(line)[1:] {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case "avg10":
			p.Avg10, _ = strconv.ParseFloat(v, 64)
		case "avg60":
			p.Avg60, _ = strconv.ParseFloat(v, 64)
		case "avg300":
			p.Avg300, _ = strconv.ParseFloat(v, 64)
		case "total":
			p.Total, _ = strconv.ParseUint(v, 10, 64)
		}
	}
	return p
}

// Read performs a pread(2) on fd and parses the PSI pressure file format.
// Returns the "some" and "full" pressure lines. buf should be >= 512 bytes.
func Read(fd int, buf []byte) (some, full Pressure) {
	n, _ := syscall.Pread(fd, buf, 0)
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		if strings.HasPrefix(line, "some ") {
			some = parseLine(line)
		} else if strings.HasPrefix(line, "full ") {
			full = parseLine(line)
		}
	}
	return
}
