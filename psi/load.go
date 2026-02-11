package main

import (
	"fmt"
	"io"
	"strings"
)

type loadSnapshot struct {
	min1, min5, min15, procs string
}

func readLoad(fd int, buf []byte) loadSnapshot {
	n := pread(fd, buf)
	fields := strings.Fields(string(buf[:n]))
	snap := loadSnapshot{min1: fields[0], min5: fields[1], min15: fields[2]}
	if len(fields) >= 4 {
		snap.procs = fields[3]
	}
	return snap
}

func printLoadTable(w io.Writer, snap loadSnapshot) {
	fmt.Fprintf(w, "%-6s │ %7s │ %7s │ %7s │ %10s\n", "LOAD", "1min", "5min", "15min", "procs")
	fmt.Fprintf(w, "───────┼─────────┼─────────┼─────────┼───────────\n")
	fmt.Fprintf(w, "%-6s │ %7s │ %7s │ %7s │ %10s\n", "", snap.min1, snap.min5, snap.min15, snap.procs)
	fmt.Fprintln(w)
}
