package main

import (
	"fmt"
	"io"
	"strings"
)

func printLoadTable(w io.Writer, fd int, buf []byte) {
	n := pread(fd, buf)
	fields := strings.Fields(string(buf[:n]))
	min1, min5, min15 := fields[0], fields[1], fields[2]
	procs := ""
	if len(fields) >= 4 {
		procs = fields[3]
	}
	fmt.Fprintf(w, "%-6s │ %7s │ %7s │ %7s │ %10s\n", "LOAD", "1min", "5min", "15min", "procs")
	fmt.Fprintf(w, "───────┼─────────┼─────────┼─────────┼───────────\n")
	fmt.Fprintf(w, "%-6s │ %7s │ %7s │ %7s │ %10s\n", "", min1, min5, min15, procs)
	fmt.Fprintln(w)
}
