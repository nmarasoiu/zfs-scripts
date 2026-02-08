package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
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

func readPressure(resource string) (some, full pressure, err error) {
	f, err := os.Open("/proc/pressure/" + resource)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "some ") {
			some = parseLine(line)
		} else if strings.HasPrefix(line, "full ") {
			full = parseLine(line)
		}
	}
	return
}

func formatTotal(t string) string {
	// total is in microseconds, convert to seconds
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

func printLoadTable() {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading loadavg: %v\n", err)
		return
	}
	fields := strings.Fields(string(data))
	// fields: 1min 5min 15min running/total last_pid
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

func main() {
	resources := []string{"cpu", "io", "memory"}

	for {
		fmt.Print("\033[H\033[2J")
		printLoadTable()
		for _, r := range resources {
			some, full, err := readPressure(r)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error reading %s: %v\n", r, err)
				continue
			}
			printTable(strings.ToUpper(r), some, full)
		}
		time.Sleep(2 * time.Second)
	}
}
