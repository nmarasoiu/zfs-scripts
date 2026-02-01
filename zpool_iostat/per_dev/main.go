package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// LatencyBucket represents a histogram bucket
type LatencyBucket struct {
	LowerBound uint64 // in nanoseconds
	Label      string
	ReadCount  uint64
	WriteCount uint64
}

// DeviceStats holds latency histogram for a device
type DeviceStats struct {
	Name    string
	Buckets []LatencyBucket
}

// Percentiles holds calculated percentile values
type Percentiles struct {
	P50   uint64
	P90   uint64
	P95   uint64
	P99   uint64
	P999  uint64
}

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
)

// Latency thresholds in nanoseconds
const (
	thresholdGreen  = 1_000_000      // 1ms
	thresholdYellow = 50_000_000     // 50ms
)

func main() {
	var output []byte
	var err error

	// Check if stdin has data (pipe mode)
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) == 0 {
		// Reading from pipe
		output, err = io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading stdin: %v\n", err)
			os.Exit(1)
		}
	} else {
		// Run zpool iostat -wvv
		cmd := exec.Command("zpool", "iostat", "-wvv")
		output, err = cmd.Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error running zpool iostat: %v\n", err)
			os.Exit(1)
		}
	}

	devices := parseOutput(string(output))
	if len(devices) == 0 {
		fmt.Println("No device data found")
		os.Exit(0)
	}

	printTable(devices)
}

// parseOutput parses zpool iostat -wvv output
func parseOutput(output string) []DeviceStats {
	var devices []DeviceStats
	var currentDevice *DeviceStats

	scanner := bufio.NewScanner(strings.NewReader(output))

	// Regex to match device header line (device name followed by "total_wait")
	deviceHeaderRe := regexp.MustCompile(`^(\S+)\s+total_wait`)

	// Regex to match latency data line
	// Format: latency  total_r total_w disk_r disk_w sync_r sync_w async_r async_w scrub trim rebuild
	latencyLineRe := regexp.MustCompile(`^(\d+(?:ns|us|ms|s))\s+`)

	inDevice := false

	for scanner.Scan() {
		line := scanner.Text()

		// Check for device header
		if matches := deviceHeaderRe.FindStringSubmatch(line); matches != nil {
			deviceName := matches[1]

			// Skip pool-level entries (they don't have specific device identifiers)
			// Pool names are typically short and don't contain dashes/underscores in device-like patterns
			if isPhysicalDevice(deviceName) {
				if currentDevice != nil {
					devices = append(devices, *currentDevice)
				}
				currentDevice = &DeviceStats{
					Name:    deviceName,
					Buckets: make([]LatencyBucket, 0),
				}
				inDevice = true
			} else {
				inDevice = false
			}
			continue
		}

		// Check for end-of-device separator (long line of ONLY dashes)
		// The column header separator has spaces between dash groups: "--- ----- -----"
		// The end-of-device separator is pure dashes: "----------------"
		trimmed := strings.TrimSpace(line)
		if len(trimmed) > 50 && strings.Trim(trimmed, "-") == "" {
			if currentDevice != nil && len(currentDevice.Buckets) > 0 {
				devices = append(devices, *currentDevice)
				currentDevice = nil
			}
			inDevice = false
			continue
		}

		// Parse latency line if we're in a device section
		if inDevice && currentDevice != nil {
			if matches := latencyLineRe.FindStringSubmatch(line); matches != nil {
				bucket := parseLatencyLine(line)
				if bucket != nil {
					currentDevice.Buckets = append(currentDevice.Buckets, *bucket)
				}
			}
		}
	}

	// Add last device if any
	if currentDevice != nil && len(currentDevice.Buckets) > 0 {
		devices = append(devices, *currentDevice)
	}

	return devices
}

// isPhysicalDevice determines if a name represents a physical device
func isPhysicalDevice(name string) bool {
	// Physical devices typically have patterns like:
	// - nvme-...
	// - usb-...
	// - wwn-...
	// - sd[a-z]
	// - draid... (dRAID vdev, treat as device)
	// - ata-...
	// - scsi-...

	nameLower := strings.ToLower(name)

	// Skip obvious pool names and vdev types
	// These are either vdev types or common pool naming patterns
	poolPatterns := []string{
		"mirror",
		"raidz",
		"spare",
		"log",
		"cache",
		"special",
	}

	for _, pattern := range poolPatterns {
		if strings.HasPrefix(nameLower, pattern) {
			return false
		}
	}

	// Accept names that look like device identifiers
	devicePrefixes := []string{
		"nvme-", "nvme_",
		"usb-",
		"wwn-",
		"ata-",
		"scsi-",
		"draid",
	}

	for _, prefix := range devicePrefixes {
		if strings.HasPrefix(nameLower, prefix) {
			return true
		}
	}

	// If name contains common device patterns
	if strings.Contains(name, "-part") || strings.Contains(name, ":0") {
		return true
	}

	// Short names without dashes are likely pool names (e.g., "hddpool", "tank", "data")
	if !strings.Contains(name, "-") {
		return false
	}

	return true
}

// parseLatencyLine parses a single latency histogram line
func parseLatencyLine(line string) *LatencyBucket {
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return nil
	}

	latencyLabel := fields[0]
	latencyNs := parseLatencyToNs(latencyLabel)
	if latencyNs == 0 && latencyLabel != "1ns" {
		return nil
	}

	// Fields: latency total_r total_w disk_r disk_w ...
	// We want disk_r (index 3) and disk_w (index 4)
	diskRead := parseCount(fields[3])
	diskWrite := parseCount(fields[4])

	return &LatencyBucket{
		LowerBound: latencyNs,
		Label:      latencyLabel,
		ReadCount:  diskRead,
		WriteCount: diskWrite,
	}
}

// parseLatencyToNs converts latency string to nanoseconds
func parseLatencyToNs(s string) uint64 {
	s = strings.TrimSpace(s)

	var multiplier uint64 = 1
	var numStr string

	if strings.HasSuffix(s, "ns") {
		multiplier = 1
		numStr = strings.TrimSuffix(s, "ns")
	} else if strings.HasSuffix(s, "us") {
		multiplier = 1_000
		numStr = strings.TrimSuffix(s, "us")
	} else if strings.HasSuffix(s, "ms") {
		multiplier = 1_000_000
		numStr = strings.TrimSuffix(s, "ms")
	} else if strings.HasSuffix(s, "s") {
		multiplier = 1_000_000_000
		numStr = strings.TrimSuffix(s, "s")
	} else {
		return 0
	}

	num, err := strconv.ParseUint(numStr, 10, 64)
	if err != nil {
		return 0
	}

	return num * multiplier
}

// parseCount parses count values like "1.23M", "45.6K", or plain numbers
func parseCount(s string) uint64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0
	}

	var multiplier float64 = 1

	if strings.HasSuffix(s, "K") {
		multiplier = 1_000
		s = strings.TrimSuffix(s, "K")
	} else if strings.HasSuffix(s, "M") {
		multiplier = 1_000_000
		s = strings.TrimSuffix(s, "M")
	} else if strings.HasSuffix(s, "G") {
		multiplier = 1_000_000_000
		s = strings.TrimSuffix(s, "G")
	}

	num, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}

	return uint64(num * multiplier)
}

// calculatePercentiles computes percentiles from histogram buckets
func calculatePercentiles(buckets []LatencyBucket, useRead bool) Percentiles {
	// Calculate total count
	var total uint64
	for _, b := range buckets {
		if useRead {
			total += b.ReadCount
		} else {
			total += b.WriteCount
		}
	}

	if total == 0 {
		return Percentiles{}
	}

	// Calculate percentile thresholds
	p50Target := total * 50 / 100
	p90Target := total * 90 / 100
	p95Target := total * 95 / 100
	p99Target := total * 99 / 100
	p999Target := total * 999 / 1000

	var cumulative uint64
	var result Percentiles

	for _, b := range buckets {
		var count uint64
		if useRead {
			count = b.ReadCount
		} else {
			count = b.WriteCount
		}

		cumulative += count

		if result.P50 == 0 && cumulative >= p50Target {
			result.P50 = b.LowerBound
		}
		if result.P90 == 0 && cumulative >= p90Target {
			result.P90 = b.LowerBound
		}
		if result.P95 == 0 && cumulative >= p95Target {
			result.P95 = b.LowerBound
		}
		if result.P99 == 0 && cumulative >= p99Target {
			result.P99 = b.LowerBound
		}
		if result.P999 == 0 && cumulative >= p999Target {
			result.P999 = b.LowerBound
		}
	}

	return result
}

// formatLatency formats nanoseconds as human-readable latency
func formatLatency(ns uint64) string {
	if ns == 0 {
		return "-"
	}

	if ns < 1_000 {
		return fmt.Sprintf("%dns", ns)
	} else if ns < 1_000_000 {
		return fmt.Sprintf("%dus", ns/1_000)
	} else if ns < 1_000_000_000 {
		return fmt.Sprintf("%dms", ns/1_000_000)
	} else {
		return fmt.Sprintf("%ds", ns/1_000_000_000)
	}
}

// colorize adds ANSI color based on latency threshold
func colorize(ns uint64, s string) string {
	if ns == 0 {
		return s
	}

	var color string
	if ns < thresholdGreen {
		color = colorGreen
	} else if ns < thresholdYellow {
		color = colorYellow
	} else {
		color = colorRed
	}

	return color + s + colorReset
}

// shortenDeviceName truncates long device names
func shortenDeviceName(name string, maxLen int) string {
	if len(name) <= maxLen {
		return name
	}

	// Try to preserve the end (usually has unique identifiers)
	return "..." + name[len(name)-(maxLen-3):]
}

// printTable outputs the formatted table
func printTable(devices []DeviceStats) {
	// Sort devices by name
	sort.Slice(devices, func(i, j int) bool {
		return devices[i].Name < devices[j].Name
	})

	// Calculate column widths
	maxNameLen := 40
	colWidth := 10

	// Print READ table
	fmt.Println()
	fmt.Printf("%sDISK LATENCY - READ%s\n", colorGreen, colorReset)
	printHeader(maxNameLen, colWidth)

	for _, dev := range devices {
		pct := calculatePercentiles(dev.Buckets, true)
		printDeviceRow(dev.Name, pct, maxNameLen, colWidth)
	}

	// Print WRITE table
	fmt.Println()
	fmt.Printf("%sDISK LATENCY - WRITE%s\n", colorGreen, colorReset)
	printHeader(maxNameLen, colWidth)

	for _, dev := range devices {
		pct := calculatePercentiles(dev.Buckets, false)
		printDeviceRow(dev.Name, pct, maxNameLen, colWidth)
	}

	fmt.Println()
}

func printHeader(nameWidth, colWidth int) {
	fmt.Printf("%-*s %*s %*s %*s %*s %*s\n",
		nameWidth, "Device",
		colWidth, "P50",
		colWidth, "P90",
		colWidth, "P95",
		colWidth, "P99",
		colWidth, "P99.9")
	fmt.Println(strings.Repeat("─", nameWidth+5*colWidth+5))
}

func printDeviceRow(name string, pct Percentiles, nameWidth, colWidth int) {
	shortName := shortenDeviceName(name, nameWidth)

	p50 := formatLatency(pct.P50)
	p90 := formatLatency(pct.P90)
	p95 := formatLatency(pct.P95)
	p99 := formatLatency(pct.P99)
	p999 := formatLatency(pct.P999)

	fmt.Printf("%-*s %s %s %s %s %s\n",
		nameWidth, shortName,
		colorize(pct.P50, fmt.Sprintf("%*s", colWidth, p50)),
		colorize(pct.P90, fmt.Sprintf("%*s", colWidth, p90)),
		colorize(pct.P95, fmt.Sprintf("%*s", colWidth, p95)),
		colorize(pct.P99, fmt.Sprintf("%*s", colWidth, p99)),
		colorize(pct.P999, fmt.Sprintf("%*s", colWidth, p999)))
}
