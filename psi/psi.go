package main

import (
	"fmt"
	"io"

	"psiparse"
)

func formatTotal(us uint64) string {
	secs := float64(us) / 1000000.0
	if secs >= 3600 {
		return fmt.Sprintf("%.1fh", secs/3600)
	} else if secs >= 60 {
		return fmt.Sprintf("%.1fm", secs/60)
	}
	return fmt.Sprintf("%.1fs", secs)
}

func printTable(w io.Writer, name string, some, full psiparse.Pressure) {
	fmt.Fprintf(w, "%-6s │ %7s │ %7s │ %7s │ %10s\n", name, "avg10", "avg60", "avg300", "total")
	fmt.Fprintf(w, "───────┼─────────┼─────────┼─────────┼───────────\n")
	fmt.Fprintf(w, "%-6s │ %6.2f%% │ %6.2f%% │ %6.2f%% │ %10s\n", "some", some.Avg10, some.Avg60, some.Avg300, formatTotal(some.Total))
	fmt.Fprintf(w, "%-6s │ %6.2f%% │ %6.2f%% │ %6.2f%% │ %10s\n", "full", full.Avg10, full.Avg60, full.Avg300, formatTotal(full.Total))
	fmt.Fprintln(w)
}
