// zpool-latency: Real-time ZFS pool latency histogram viewer
//
// Parses zpool iostat -wvv output and displays per-device latency percentiles
// in a real-time updating view similar to blk-latency.
//
// Architecture:
//   - Interval stats: streaming from `zpool iostat -wvv pool <interval>`
//   - Lifetime stats: periodic exec of `zpool iostat -wvv pool` (cumulative)
//
// Usage: go run zpool-latency.go [pool] [-i interval]

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	displayInterval  = 100 * time.Millisecond // 10 FPS display refresh
	lifetimePollFreq = 2 * time.Second        // How often to fetch lifetime stats
)

var (
	poolName = flag.String("pool", "", "ZFS pool name (required, or as first positional arg)")
	interval = flag.Int("i", 10, "zpool iostat interval in seconds")
	batch    = flag.Bool("batch", false, "batch mode (no screen clearing)")
	showDisk = flag.Bool("disk", false, "show disk_wait instead of total_wait")
)

// Latency bucket definitions (nanoseconds)
// These are the exact bucket labels from zpool iostat -wvv
var bucketLabels = []string{
	"1ns", "3ns", "7ns", "15ns", "31ns", "63ns", "127ns", "255ns", "511ns",
	"1us", "2us", "4us", "8us", "16us", "32us", "65us", "131us", "262us", "524us",
	"1ms", "2ms", "4ms", "8ms", "16ms", "33ms", "67ms", "134ms", "268ms", "536ms",
	"1s", "2s", "4s", "8s", "17s", "34s", "68s", "137s",
}

// Bucket midpoints in microseconds (for percentile calculation)
var bucketMidpointsUs = []float64{
	0.001, 0.003, 0.007, 0.015, 0.031, 0.063, 0.127, 0.255, 0.511,
	1, 2, 4, 8, 16, 32, 65, 131, 262, 524,
	1000, 2000, 4000, 8000, 16000, 33000, 67000, 134000, 268000, 536000,
	1000000, 2000000, 4000000, 8000000, 17000000, 34000000, 68000000, 137000000,
}

// Column indices in the histogram data
const (
	colTotalRead  = 0
	colTotalWrite = 1
	colDiskRead   = 2
	colDiskWrite  = 3
	colSyncRead   = 4
	colSyncWrite  = 5
	colAsyncRead  = 6
	colAsyncWrite = 7
	colScrub      = 8
	colTrim       = 9
	colRebuild    = 10
	numColumns    = 11
)

// DeviceHistogram holds histogram data for one device
type DeviceHistogram struct {
	Name    string
	Buckets [37][11]uint64 // 37 latency buckets × 11 columns
}

// parseCount parses a count value like "1.23K", "4.56M", or plain number
func parseCount(s string) uint64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return 0
	}

	multiplier := 1.0
	if strings.HasSuffix(s, "K") {
		multiplier = 1000
		s = s[:len(s)-1]
	} else if strings.HasSuffix(s, "M") {
		multiplier = 1000000
		s = s[:len(s)-1]
	} else if strings.HasSuffix(s, "B") {
		multiplier = 1000000000
		s = s[:len(s)-1]
	}

	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return uint64(val * multiplier)
}

// Histogram provides percentile calculations from bucket counts
type Histogram struct {
	counts []uint64
	total  uint64
}

func newHistogramFromBuckets(buckets []uint64) *Histogram {
	h := &Histogram{counts: make([]uint64, len(buckets))}
	copy(h.counts, buckets)
	for _, c := range buckets {
		h.total += c
	}
	return h
}

func (h *Histogram) TotalCount() uint64 {
	return h.total
}

func (h *Histogram) Mean() float64 {
	if h.total == 0 {
		return 0
	}
	sum := 0.0
	for i, c := range h.counts {
		sum += float64(c) * bucketMidpointsUs[i]
	}
	return sum / float64(h.total)
}

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

func (h *Histogram) Max() float64 {
	for i := len(h.counts) - 1; i >= 0; i-- {
		if h.counts[i] > 0 {
			return bucketMidpointsUs[i]
		}
	}
	return 0
}

// State holds parsed histogram data for either interval or lifetime
type State struct {
	mu         sync.RWMutex
	histograms map[string]*DeviceHistogram
	lastUpdate time.Time
	updateCount uint64
}

func newState() *State {
	return &State{
		histograms: make(map[string]*DeviceHistogram),
		lastUpdate: time.Now(),
	}
}

func (s *State) Update(histograms map[string]*DeviceHistogram) {
	s.mu.Lock()
	s.histograms = histograms
	s.lastUpdate = time.Now()
	s.updateCount++
	s.mu.Unlock()
}

func (s *State) Snapshot() (map[string]*DeviceHistogram, time.Time, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := make(map[string]*DeviceHistogram)
	for k, v := range s.histograms {
		copyHist := &DeviceHistogram{Name: v.Name}
		copyHist.Buckets = v.Buckets
		snap[k] = copyHist
	}
	return snap, s.lastUpdate, s.updateCount
}

// formatLatency formats a latency value (in µs) to human-readable string
func formatLatency(us float64) string {
	if us < 1 {
		ns := us * 1000
		return fmt.Sprintf("%dns", int(ns+0.5))
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
	s := us / 1_000_000
	return fmt.Sprintf("%.1fs", s)
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

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm%ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh%dm", h, m)
}

// Display handles rendering
type Display struct {
	batchMode bool
	showDisk  bool
	startTime time.Time
}

func (d *Display) resetCursor() {
	if !d.batchMode {
		fmt.Print("\033[H\033[J")
	}
}

// shortenDeviceName returns a more readable short version of the device name
func shortenDeviceName(name string) string {
	// USB Seagate drives: extract just the serial
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
	// NVMe drives: shorten model name
	if strings.HasPrefix(name, "nvme-") {
		short := strings.TrimPrefix(name, "nvme-")
		partSuffix := ""
		if idx := strings.LastIndex(short, "-part"); idx > 0 {
			partSuffix = short[idx:]
			short = short[:idx]
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
	// WWN drives: use partition number
	if strings.HasPrefix(name, "wwn-") {
		if idx := strings.LastIndex(name, "-part"); idx > 0 {
			return "wwn" + name[idx:]
		}
	}
	return name
}

// deviceSortKey returns a sortable key for devices
func deviceSortKey(name string) string {
	if !strings.Contains(name, "-") && !strings.HasPrefix(name, "draid") &&
		!strings.HasPrefix(name, "mirror") && !strings.HasPrefix(name, "raidz") {
		return "0_" + name
	}
	if strings.HasPrefix(name, "draid") || strings.HasPrefix(name, "mirror") ||
		strings.HasPrefix(name, "raidz") {
		return "1_" + name
	}
	if strings.HasPrefix(name, "nvme-") {
		return "2_" + name
	}
	if strings.HasPrefix(name, "wwn-") {
		return "3_" + name
	}
	if strings.HasPrefix(name, "usb-") {
		return "4_" + name
	}
	return "5_" + name
}

// getSortedDevices returns device names sorted consistently
func getSortedDevices(histograms map[string]*DeviceHistogram) []string {
	var devList []string
	for name := range histograms {
		devList = append(devList, name)
	}
	sort.Slice(devList, func(i, j int) bool {
		return deviceSortKey(devList[i]) < deviceSortKey(devList[j])
	})
	return devList
}

func (d *Display) render(intervalHist map[string]*DeviceHistogram, intervalUpdate time.Time, intervalCount uint64,
	lifetimeHist map[string]*DeviceHistogram, lifetimeUpdate time.Time, intervalSec int) {
	var buf strings.Builder
	now := time.Now()

	// Use lifetime devices as the canonical list (more complete)
	devList := getSortedDevices(lifetimeHist)
	if len(devList) == 0 {
		devList = getSortedDevices(intervalHist)
	}

	timestamp := now.Format("15:04:05")
	elapsed := now.Sub(d.startTime)
	sinceInterval := now.Sub(intervalUpdate)
	sinceLifetime := now.Sub(lifetimeUpdate)

	fmt.Fprintf(&buf, "ZFS Pool Latency Monitor - %s (uptime: %s, interval: %ds)\n",
		timestamp, formatDuration(elapsed), intervalSec)

	lineWidth := 145
	buf.WriteString(strings.Repeat("=", lineWidth))
	buf.WriteString("\n")

	waitLabel := "TOTAL_WAIT"
	if d.showDisk {
		waitLabel = "DISK_WAIT"
	}

	// INTERVAL SECTION
	fmt.Fprintf(&buf, "INTERVAL (%s ago)    │          %s READ                      │          %s WRITE                     │  samples\n",
		formatDuration(sinceInterval), waitLabel, waitLabel)
	fmt.Fprintf(&buf, "%-20s │ %7s %7s %7s %7s %7s %7s │ %7s %7s %7s %7s %7s %7s │\n",
		"", "avg", "p50", "p90", "p99", "p99.9", "max",
		"avg", "p50", "p90", "p99", "p99.9", "max")
	buf.WriteString(strings.Repeat("-", lineWidth))
	buf.WriteString("\n")

	for _, name := range devList {
		hist := intervalHist[name]
		if hist == nil {
			hist = &DeviceHistogram{Name: name}
		}
		d.renderDevice(&buf, name, hist)
	}

	buf.WriteString("\n")

	// LIFETIME SECTION
	fmt.Fprintf(&buf, "LIFETIME (%s ago)    │          %s READ                      │          %s WRITE                     │  samples\n",
		formatDuration(sinceLifetime), waitLabel, waitLabel)
	fmt.Fprintf(&buf, "%-20s │ %7s %7s %7s %7s %7s %7s │ %7s %7s %7s %7s %7s %7s │\n",
		"", "avg", "p50", "p90", "p99", "p99.9", "max",
		"avg", "p50", "p90", "p99", "p99.9", "max")
	buf.WriteString(strings.Repeat("-", lineWidth))
	buf.WriteString("\n")

	for _, name := range devList {
		hist := lifetimeHist[name]
		if hist == nil {
			hist = &DeviceHistogram{Name: name}
		}
		d.renderDevice(&buf, name, hist)
	}

	buf.WriteString(strings.Repeat("=", lineWidth))
	buf.WriteString("\n")

	// Stats summary
	var totalSamples uint64
	for _, h := range lifetimeHist {
		for i := 0; i < 37; i++ {
			totalSamples += h.Buckets[i][colTotalRead] + h.Buckets[i][colTotalWrite]
		}
	}
	modeHint := "total_wait = queue + disk"
	if d.showDisk {
		modeHint = "disk_wait = disk only"
	}
	fmt.Fprintf(&buf, "Total I/O: %s | Interval updates: %d | %s\n",
		formatCount(totalSamples), intervalCount, modeHint)

	if d.batchMode {
		buf.WriteString("\n")
	}

	d.resetCursor()
	fmt.Print(buf.String())
}

func (d *Display) renderDevice(buf *strings.Builder, name string, hist *DeviceHistogram) {
	readCol, writeCol := colTotalRead, colTotalWrite
	if d.showDisk {
		readCol, writeCol = colDiskRead, colDiskWrite
	}

	readBuckets := make([]uint64, 37)
	writeBuckets := make([]uint64, 37)
	for i := 0; i < 37; i++ {
		readBuckets[i] = hist.Buckets[i][readCol]
		writeBuckets[i] = hist.Buckets[i][writeCol]
	}

	readHist := newHistogramFromBuckets(readBuckets)
	writeHist := newHistogramFromBuckets(writeBuckets)

	displayName := shortenDeviceName(name)
	if len(displayName) > 20 {
		displayName = displayName[:17] + "..."
	}

	totalOps := readHist.TotalCount() + writeHist.TotalCount()

	if readHist.TotalCount() == 0 && writeHist.TotalCount() == 0 {
		fmt.Fprintf(buf, "%-20s │ %7s %7s %7s %7s %7s %7s │ %7s %7s %7s %7s %7s %7s │ %8s\n",
			displayName, "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "0")
		return
	}

	fmtLat := func(us float64) string { return fmt.Sprintf("%7s", formatLatency(us)) }

	readAvg, readP50, readP90, readP99, readP999, readMax := "      -", "      -", "      -", "      -", "      -", "      -"
	if readHist.TotalCount() > 0 {
		readAvg = fmtLat(readHist.Mean())
		readP50 = fmtLat(readHist.Percentile(50))
		readP90 = fmtLat(readHist.Percentile(90))
		readP99 = fmtLat(readHist.Percentile(99))
		readP999 = fmtLat(readHist.Percentile(99.9))
		readMax = fmtLat(readHist.Max())
	}

	writeAvg, writeP50, writeP90, writeP99, writeP999, writeMax := "      -", "      -", "      -", "      -", "      -", "      -"
	if writeHist.TotalCount() > 0 {
		writeAvg = fmtLat(writeHist.Mean())
		writeP50 = fmtLat(writeHist.Percentile(50))
		writeP90 = fmtLat(writeHist.Percentile(90))
		writeP99 = fmtLat(writeHist.Percentile(99))
		writeP999 = fmtLat(writeHist.Percentile(99.9))
		writeMax = fmtLat(writeHist.Max())
	}

	fmt.Fprintf(buf, "%-20s │ %s %s %s %s %s %s │ %s %s %s %s %s %s │ %8s\n",
		displayName,
		readAvg, readP50, readP90, readP99, readP999, readMax,
		writeAvg, writeP50, writeP90, writeP99, writeP999, writeMax,
		formatCount(totalOps))
}

// bucketLabelIndex maps bucket labels to indices
var bucketLabelIndex = make(map[string]int)

func init() {
	for i, label := range bucketLabels {
		bucketLabelIndex[label] = i
	}
}

// parseZpoolOutput parses zpool iostat -wvv output from a reader
func parseZpoolOutput(reader io.Reader) map[string]*DeviceHistogram {
	scanner := bufio.NewScanner(reader)
	histograms := make(map[string]*DeviceHistogram)
	var currentDevice *DeviceHistogram

	deviceHeaderPattern := regexp.MustCompile(`^(\S+)\s+total_wait`)
	latencyLinePattern := regexp.MustCompile(`^\s*(\d+(?:ns|us|ms|s))\s+(.+)`)
	separatorPattern := regexp.MustCompile(`^[-]+$`)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || separatorPattern.MatchString(trimmed) {
			continue
		}

		if strings.HasPrefix(trimmed, "latency") {
			continue
		}

		if strings.Contains(line, "total_wait") {
			if matches := deviceHeaderPattern.FindStringSubmatch(line); matches != nil {
				deviceName := matches[1]
				currentDevice = &DeviceHistogram{Name: deviceName}
				histograms[deviceName] = currentDevice
			}
			continue
		}

		if currentDevice != nil {
			if matches := latencyLinePattern.FindStringSubmatch(line); matches != nil {
				bucketLabel := matches[1]
				valuesStr := matches[2]

				bucketIdx, ok := bucketLabelIndex[bucketLabel]
				if !ok {
					continue
				}

				values := strings.Fields(valuesStr)
				for col := 0; col < numColumns && col < len(values); col++ {
					currentDevice.Buckets[bucketIdx][col] = parseCount(values[col])
				}
			}
		}
	}

	return histograms
}

// IntervalParser parses streaming zpool iostat output for interval stats
type IntervalParser struct {
	state       *State
	skipFirst   bool // Skip first output (it's lifetime, not interval)
	seenFirst   bool
}

func newIntervalParser(state *State) *IntervalParser {
	return &IntervalParser{state: state, skipFirst: true}
}

func (p *IntervalParser) Parse(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	histograms := make(map[string]*DeviceHistogram)
	var currentDevice *DeviceHistogram

	deviceHeaderPattern := regexp.MustCompile(`^(\S+)\s+total_wait`)
	latencyLinePattern := regexp.MustCompile(`^\s*(\d+(?:ns|us|ms|s))\s+(.+)`)
	separatorPattern := regexp.MustCompile(`^[-]+$`)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || separatorPattern.MatchString(trimmed) {
			continue
		}

		if strings.HasPrefix(trimmed, "latency") {
			continue
		}

		if strings.Contains(line, "total_wait") {
			if matches := deviceHeaderPattern.FindStringSubmatch(line); matches != nil {
				deviceName := matches[1]

				// Check if this is the start of a new interval
				if _, exists := histograms[deviceName]; exists && len(histograms) > 0 {
					// Completed one full snapshot
					if p.skipFirst && !p.seenFirst {
						// Skip the first snapshot (it's lifetime stats)
						p.seenFirst = true
					} else {
						p.state.Update(histograms)
					}
					histograms = make(map[string]*DeviceHistogram)
				}

				currentDevice = &DeviceHistogram{Name: deviceName}
				histograms[deviceName] = currentDevice
			}
			continue
		}

		if currentDevice != nil {
			if matches := latencyLinePattern.FindStringSubmatch(line); matches != nil {
				bucketLabel := matches[1]
				valuesStr := matches[2]

				bucketIdx, ok := bucketLabelIndex[bucketLabel]
				if !ok {
					continue
				}

				values := strings.Fields(valuesStr)
				for col := 0; col < numColumns && col < len(values); col++ {
					currentDevice.Buckets[bucketIdx][col] = parseCount(values[col])
				}
			}
		}
	}
}

// fetchLifetimeStats runs zpool iostat once to get cumulative stats
func fetchLifetimeStats(pool string) (map[string]*DeviceHistogram, error) {
	cmd := exec.Command("zpool", "iostat", "-wvv", pool)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseZpoolOutput(bytes.NewReader(output)), nil
}

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: zpool-latency [-i interval] [-batch] [-disk] <pool>")
		fmt.Fprintln(os.Stderr, "  pool:   ZFS pool name (required)")
		fmt.Fprintln(os.Stderr, "  -i:     zpool iostat interval in seconds (default: 10)")
		fmt.Fprintln(os.Stderr, "  -batch: batch mode, no screen clearing")
		fmt.Fprintln(os.Stderr, "  -disk:  show disk_wait instead of total_wait")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Wait types:")
		fmt.Fprintln(os.Stderr, "  total_wait = time in queue + disk service time")
		fmt.Fprintln(os.Stderr, "  disk_wait  = actual disk service time only")
	}
	flag.Parse()

	pool := *poolName
	if pool == "" && flag.NArg() > 0 {
		pool = flag.Arg(0)
	}
	if pool == "" {
		flag.Usage()
		os.Exit(1)
	}

	intervalState := newState()
	lifetimeState := newState()
	intervalParser := newIntervalParser(intervalState)
	display := &Display{batchMode: *batch, showDisk: *showDisk, startTime: time.Now()}

	// Signal handling
	done := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig
		close(done)
	}()

	// Start streaming zpool iostat for interval stats
	cmd := exec.Command("zpool", "iostat", "-wvv", pool, fmt.Sprintf("%d", *interval))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("Failed to get stdout pipe: %v", err)
	}

	if !*batch {
		fmt.Printf("Starting zpool iostat on pool '%s' (interval: %ds)...\n", pool, *interval)
		fmt.Println("Waiting for first data snapshot...")
	}

	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start zpool iostat: %v", err)
	}

	// Parse interval output in goroutine
	go func() {
		intervalParser.Parse(stdout)
	}()

	// Lifetime stats fetcher goroutine
	go func() {
		// Fetch immediately on start
		if hists, err := fetchLifetimeStats(pool); err == nil && len(hists) > 0 {
			lifetimeState.Update(hists)
		}

		ticker := time.NewTicker(lifetimePollFreq)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if hists, err := fetchLifetimeStats(pool); err == nil && len(hists) > 0 {
					lifetimeState.Update(hists)
				}
			}
		}
	}()

	// Display goroutine (10 FPS)
	displayTicker := time.NewTicker(displayInterval)
	go func() {
		defer displayTicker.Stop()
		for {
			select {
			case <-done:
				return
			case <-displayTicker.C:
				intervalHist, intervalUpdate, intervalCount := intervalState.Snapshot()
				lifetimeHist, lifetimeUpdate, _ := lifetimeState.Snapshot()

				// Need at least lifetime stats to display
				if len(lifetimeHist) > 0 {
					display.render(intervalHist, intervalUpdate, intervalCount,
						lifetimeHist, lifetimeUpdate, *interval)
				}
			}
		}
	}()

	// Wait for shutdown
	<-done
	cmd.Process.Kill()
	cmd.Wait()

	if !*batch {
		fmt.Println("\nStopped.")
	}
}
