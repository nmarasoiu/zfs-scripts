package main

import (
	"fmt"
	"io"
	"strings"
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

func printTable(w io.Writer, name string, some, full pressure) {
	fmt.Fprintf(w, "%-6s │ %7s │ %7s │ %7s │ %10s\n", name, "avg10", "avg60", "avg300", "total")
	fmt.Fprintf(w, "───────┼─────────┼─────────┼─────────┼───────────\n")
	fmt.Fprintf(w, "%-6s │ %6s%% │ %6s%% │ %6s%% │ %10s\n", "some", some.avg10, some.avg60, some.avg300, formatTotal(some.total))
	fmt.Fprintf(w, "%-6s │ %6s%% │ %6s%% │ %6s%% │ %10s\n", "full", full.avg10, full.avg60, full.avg300, formatTotal(full.total))
	fmt.Fprintln(w)
}
