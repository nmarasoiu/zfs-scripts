// slow-devices: Identify slow ZFS devices by latency percentiles
//
// Parses zpool iostat -wvv and computes P50, P90, P99, P99.9 for read/write
// across all devices (pool, vdevs, partitions), sorted by P99 write descending.
//
// Usage: slow-devices [pool]

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Bucket midpoints in microseconds
var bucketMidpointsUs = []float64{
	0.001, 0.003, 0.007, 0.015, 0.031, 0.063, 0.127, 0.255, 0.511,
	1, 2, 4, 8, 16, 32, 65, 131, 262, 524,
	1000, 2000, 4000, 8000, 16000, 33000, 67000, 134000, 268000, 536000,
	1000000, 2000000, 4000000, 8000000, 17000000, 34000000, 68000000, 137000000,
}

var bucketLabels = []string{
	"1ns", "3ns", "7ns", "15ns", "31ns", "63ns", "127ns", "255ns", "511ns",
	"1us", "2us", "4us", "8us", "16us", "32us", "65us", "131us", "262us", "524us",
	"1ms", "2ms", "4ms", "8ms", "16ms", "33ms", "67ms", "134ms", "268ms", "536ms",
	"1s", "2s", "4s", "8s", "17s", "34s", "68s", "137s",
}

var bucketLabelIndex = make(map[string]int)

func init() {
	for i, label := range bucketLabels {
		bucketLabelIndex[label] = i
	}
}

const (
	colTotalRead  = 0
	colTotalWrite = 1
	numColumns    = 11
	numBuckets    = 37
)

type DeviceHistogram struct {
	Name    string
	Buckets [numBuckets][numColumns]uint64
}

type Histogram struct {
	counts []uint64
	total  uint64
}

func newHistogram(buckets []uint64) *Histogram {
	h := &Histogram{counts: make([]uint64, len(buckets))}
	copy(h.counts, buckets)
	for _, c := range buckets {
		h.total += c
	}
	return h
}

func (h *Histogram) Count() uint64 { return h.total }

func (h *Histogram) Percentile(p float64) float64 {
	if h.total == 0 {
		return 0
	}
	target := uint64(float64(h.total) * p / 100.0)
	if target == 0 {
		target = 1
	}
	cumulative := uint64(0)
	for i, c := range h.counts {
		cumulative += c
		if cumulative >= target {
			return bucketMidpointsUs[i]
		}
	}
	return bucketMidpointsUs[len(bucketMidpointsUs)-1]
}

type DeviceStats struct {
	Name       string
	ReadCount  uint64
	WriteCount uint64
	ReadP50    float64
	ReadP90    float64
	ReadP99    float64
	ReadP999   float64
	WriteP50   float64
	WriteP90   float64
	WriteP99   float64
	WriteP999  float64
	TotalP50   float64
	TotalP90   float64
	TotalP99   float64
	TotalP999  float64
}

func parseCount(s string) uint64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return 0
	}
	mult := 1.0
	if strings.HasSuffix(s, "K") {
		mult, s = 1000, s[:len(s)-1]
	} else if strings.HasSuffix(s, "M") {
		mult, s = 1000000, s[:len(s)-1]
	} else if strings.HasSuffix(s, "B") {
		mult, s = 1000000000, s[:len(s)-1]
	}
	v, _ := strconv.ParseFloat(s, 64)
	return uint64(v * mult)
}

func formatLatency(us float64) string {
	if us == 0 {
		return "-"
	}
	if us < 1 {
		return fmt.Sprintf("%dns", int(us*1000+0.5))
	}
	if us < 1000 {
		return fmt.Sprintf("%dµs", int(us+0.5))
	}
	if us < 1_000_000 {
		ms := us / 1000
		if ms < 10 {
			return fmt.Sprintf("%.1fms", ms)
		}
		return fmt.Sprintf("%dms", int(ms+0.5))
	}
	return fmt.Sprintf("%.1fs", us/1_000_000)
}

func formatCount(n uint64) string {
	if n >= 1_000_000_000 {
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	}
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

func shortenName(name string) string {
	if strings.HasPrefix(name, "usb-Seagate_Expansion_HDD_") {
		parts := strings.Split(name, "_")
		if len(parts) >= 4 {
			serial := parts[3]
			if idx := strings.Index(serial, "-0:"); idx > 0 {
				serial = serial[:idx]
				if len(serial) > 8 {
					serial = serial[len(serial)-8:]
				}
				return "usb:" + serial
			}
		}
	}
	if strings.HasPrefix(name, "nvme-") {
		short := strings.TrimPrefix(name, "nvme-")
		partSuffix := ""
		if idx := strings.LastIndex(short, "-part"); idx > 0 {
			partSuffix, short = short[idx:], short[:idx]
		}
		parts := strings.Split(short, "_")
		if len(parts) >= 2 {
			serial := parts[len(parts)-1]
			if len(serial) > 8 {
				serial = serial[len(serial)-8:]
			}
			return "nvme:" + serial + partSuffix
		}
	}
	if strings.HasPrefix(name, "wwn-") {
		if idx := strings.LastIndex(name, "-part"); idx > 0 {
			return "wwn" + name[idx:]
		}
	}
	return name
}

func parseZpoolOutput(data []byte) map[string]*DeviceHistogram {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	histograms := make(map[string]*DeviceHistogram)
	var current *DeviceHistogram

	devPattern := regexp.MustCompile(`^(\S+)\s+total_wait`)
	latPattern := regexp.MustCompile(`^\s*(\d+(?:ns|us|ms|s))\s+(.+)`)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "total_wait") {
			if m := devPattern.FindStringSubmatch(line); m != nil {
				current = &DeviceHistogram{Name: m[1]}
				histograms[m[1]] = current
			}
			continue
		}
		if current != nil {
			if m := latPattern.FindStringSubmatch(line); m != nil {
				if idx, ok := bucketLabelIndex[m[1]]; ok {
					vals := strings.Fields(m[2])
					for col := 0; col < numColumns && col < len(vals); col++ {
						current.Buckets[idx][col] = parseCount(vals[col])
					}
				}
			}
		}
	}
	return histograms
}

func computeStats(histograms map[string]*DeviceHistogram) []DeviceStats {
	var stats []DeviceStats

	for name, hist := range histograms {
		readB := make([]uint64, numBuckets)
		writeB := make([]uint64, numBuckets)
		totalB := make([]uint64, numBuckets)

		for i := 0; i < numBuckets; i++ {
			readB[i] = hist.Buckets[i][colTotalRead]
			writeB[i] = hist.Buckets[i][colTotalWrite]
			totalB[i] = readB[i] + writeB[i]
		}

		rh := newHistogram(readB)
		wh := newHistogram(writeB)
		th := newHistogram(totalB)

		stats = append(stats, DeviceStats{
			Name:       name,
			ReadCount:  rh.Count(),
			WriteCount: wh.Count(),
			ReadP50:    rh.Percentile(50),
			ReadP90:    rh.Percentile(90),
			ReadP99:    rh.Percentile(99),
			ReadP999:   rh.Percentile(99.9),
			WriteP50:   wh.Percentile(50),
			WriteP90:   wh.Percentile(90),
			WriteP99:   wh.Percentile(99),
			WriteP999:  wh.Percentile(99.9),
			TotalP50:   th.Percentile(50),
			TotalP90:   th.Percentile(90),
			TotalP99:   th.Percentile(99),
			TotalP999:  th.Percentile(99.9),
		})
	}

	// Sort by write latencies, then read latencies, then name for stability
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].WriteP99 != stats[j].WriteP99 {
			return stats[i].WriteP99 > stats[j].WriteP99
		}
		if stats[i].WriteP999 != stats[j].WriteP999 {
			return stats[i].WriteP999 > stats[j].WriteP999
		}
		if stats[i].WriteP90 != stats[j].WriteP90 {
			return stats[i].WriteP90 > stats[j].WriteP90
		}
		if stats[i].ReadP99 != stats[j].ReadP99 {
			return stats[i].ReadP99 > stats[j].ReadP99
		}
		if stats[i].ReadP999 != stats[j].ReadP999 {
			return stats[i].ReadP999 > stats[j].ReadP999
		}
		if stats[i].ReadP90 != stats[j].ReadP90 {
			return stats[i].ReadP90 > stats[j].ReadP90
		}
		return stats[i].Name < stats[j].Name
	})

	return stats
}

func printTable(stats []DeviceStats) {
	fmt.Println("ZFS Device Latency Analysis (total_wait)")
	fmt.Println("Sorted by Write P99 (descending)")
	fmt.Println()

	// Header line 1 - column groups
	fmt.Printf("%-24s │             READ               │             WRITE              │   Samples\n", "")
	// Header line 2 - percentile labels
	fmt.Printf("%-24s │ %7s %7s %7s %7s │ %7s %7s %7s %7s │ %8s %8s\n",
		"Device", "P50", "P90", "P99", "P99.9", "P50", "P90", "P99", "P99.9", "Read", "Write")
	fmt.Println(strings.Repeat("─", 120))

	for _, s := range stats {
		displayName := shortenName(s.Name)
		if len(displayName) > 24 {
			displayName = displayName[:21] + "..."
		}

		// Skip devices with no I/O
		if s.ReadCount == 0 && s.WriteCount == 0 {
			continue
		}

		fmt.Printf("%-24s │ %7s %7s %7s %7s │ %7s %7s %7s %7s │ %8s %8s\n",
			displayName,
			formatLatency(s.ReadP50), formatLatency(s.ReadP90), formatLatency(s.ReadP99), formatLatency(s.ReadP999),
			formatLatency(s.WriteP50), formatLatency(s.WriteP90), formatLatency(s.WriteP99), formatLatency(s.WriteP999),
			formatCount(s.ReadCount), formatCount(s.WriteCount))
	}
}

func main() {
	var args []string
	if len(os.Args) > 1 {
		args = []string{"iostat", "-wvv", os.Args[1]}
	} else {
		args = []string{"iostat", "-wvv"}
	}

	out, err := exec.Command("zpool", args...).Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error running zpool iostat: %v\n", err)
		os.Exit(1)
	}

	histograms := parseZpoolOutput(out)
	if len(histograms) == 0 {
		fmt.Fprintln(os.Stderr, "No devices found")
		os.Exit(1)
	}

	stats := computeStats(histograms)
	printTable(stats)
}
