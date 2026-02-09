// zpool-latency: Real-time ZFS pool latency viewer (physical disk layer)
//
// Displays per-device disk_wait latency percentiles from zpool iostat -wvv
//
// Usage: zpool-latency <pool> [-i interval]

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
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
	displayInterval  = 100 * time.Millisecond // 10 FPS
	lifetimePollFreq = 2 * time.Second
)

var interval = flag.Int("i", 1, "interval in seconds")

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
	colDiskRead  = 2
	colDiskWrite = 3
	numColumns   = 11
)

type DeviceHistogram struct {
	Name    string
	Buckets [37][11]uint64
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

func (h *Histogram) Count() uint64     { return h.total }
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

type State struct {
	mu          sync.RWMutex
	histograms  map[string]*DeviceHistogram
	lastUpdate  time.Time
	updateCount uint64
}

func newState() *State {
	return &State{histograms: make(map[string]*DeviceHistogram), lastUpdate: time.Now()}
}

func (s *State) Update(h map[string]*DeviceHistogram) {
	s.mu.Lock()
	s.histograms = h
	s.lastUpdate = time.Now()
	s.updateCount++
	s.mu.Unlock()
}

func (s *State) Snapshot() (map[string]*DeviceHistogram, time.Time, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := make(map[string]*DeviceHistogram)
	for k, v := range s.histograms {
		c := &DeviceHistogram{Name: v.Name}
		c.Buckets = v.Buckets
		snap[k] = c
	}
	return snap, s.lastUpdate, s.updateCount
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

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
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

func sortKey(name string) string {
	switch {
	case !strings.Contains(name, "-") && !strings.HasPrefix(name, "draid") &&
		!strings.HasPrefix(name, "mirror") && !strings.HasPrefix(name, "raidz"):
		return "0_" + name
	case strings.HasPrefix(name, "draid"), strings.HasPrefix(name, "mirror"), strings.HasPrefix(name, "raidz"):
		return "1_" + name
	case strings.HasPrefix(name, "nvme-"):
		return "2_" + name
	case strings.HasPrefix(name, "wwn-"):
		return "3_" + name
	case strings.HasPrefix(name, "usb-"):
		return "4_" + name
	}
	return "5_" + name
}

func sortedDevices(h map[string]*DeviceHistogram) []string {
	var list []string
	for name := range h {
		list = append(list, name)
	}
	sort.Slice(list, func(i, j int) bool { return sortKey(list[i]) < sortKey(list[j]) })
	return list
}

func parseZpoolOutput(r io.Reader) map[string]*DeviceHistogram {
	scanner := bufio.NewScanner(r)
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

func fetchLifetime(pool string) map[string]*DeviceHistogram {
	out, err := exec.Command("zpool", "iostat", "-wvv", pool).Output()
	if err != nil {
		return nil
	}
	return parseZpoolOutput(bytes.NewReader(out))
}

type IntervalParser struct {
	current   *State // ongoing interval (accumulating)
	previous  *State // last completed interval (stable snapshot)
	seenFirst bool
}

func (p *IntervalParser) Parse(r io.Reader) {
	scanner := bufio.NewScanner(r)
	histograms := make(map[string]*DeviceHistogram)
	var currentDev *DeviceHistogram

	devPattern := regexp.MustCompile(`^(\S+)\s+total_wait`)
	latPattern := regexp.MustCompile(`^\s*(\d+(?:ns|us|ms|s))\s+(.+)`)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "total_wait") {
			if m := devPattern.FindStringSubmatch(line); m != nil {
				if _, exists := histograms[m[1]]; exists {
					// Interval complete - save to previous, start new current
					if p.seenFirst {
						p.previous.Update(histograms)
					}
					p.seenFirst = true
					histograms = make(map[string]*DeviceHistogram)
				}
				currentDev = &DeviceHistogram{Name: m[1]}
				histograms[m[1]] = currentDev
			}
			continue
		}
		if currentDev != nil {
			if m := latPattern.FindStringSubmatch(line); m != nil {
				if idx, ok := bucketLabelIndex[m[1]]; ok {
					vals := strings.Fields(m[2])
					for col := 0; col < numColumns && col < len(vals); col++ {
						currentDev.Buckets[idx][col] = parseCount(vals[col])
					}
					// Update current state with ongoing data
					if p.seenFirst {
						p.current.Update(histograms)
					}
				}
			}
		}
	}
}

func render(currentH, previousH, lifetimeH map[string]*DeviceHistogram,
	currentT, previousT, lifetimeT time.Time, intervalCount uint64, startTime time.Time, intervalSec int) {
	var buf strings.Builder
	now := time.Now()

	devList := sortedDevices(lifetimeH)
	if len(devList) == 0 {
		devList = sortedDevices(currentH)
	}

	fmt.Fprintf(&buf, "ZFS Pool Latency (disk_wait) - %s (uptime: %s, interval: %ds)\n",
		now.Format("15:04:05"), formatDuration(now.Sub(startTime)), intervalSec)

	const w = 145
	buf.WriteString(strings.Repeat("=", w) + "\n")

	// Section header helper
	writeHeader := func(label string, age time.Duration) {
		fmt.Fprintf(&buf, "%-20s │            READ                               │            WRITE                              │  samples\n",
			fmt.Sprintf("%s (%s ago)", label, formatDuration(age)))
		fmt.Fprintf(&buf, "%-20s │ %7s %7s %7s %7s %7s %7s │ %7s %7s %7s %7s %7s %7s │\n",
			"", "avg", "p50", "p90", "p99", "p99.9", "max", "avg", "p50", "p90", "p99", "p99.9", "max")
		buf.WriteString(strings.Repeat("-", w) + "\n")
	}

	// ONGOING INTERVAL
	writeHeader("ONGOING", now.Sub(currentT))
	for _, name := range devList {
		renderDevice(&buf, name, currentH[name])
	}

	// LAST INTERVAL (only if we have data)
	if len(previousH) > 0 {
		buf.WriteString("\n")
		writeHeader("LAST INTERVAL", now.Sub(previousT))
		for _, name := range devList {
			renderDevice(&buf, name, previousH[name])
		}
	}

	// LIFETIME
	buf.WriteString("\n")
	writeHeader("LIFETIME", now.Sub(lifetimeT))
	for _, name := range devList {
		renderDevice(&buf, name, lifetimeH[name])
	}

	buf.WriteString(strings.Repeat("=", w) + "\n")

	var total uint64
	for _, h := range lifetimeH {
		for i := 0; i < 37; i++ {
			total += h.Buckets[i][colDiskRead] + h.Buckets[i][colDiskWrite]
		}
	}
	fmt.Fprintf(&buf, "Total I/O: %s | Intervals: %d | disk_wait = physical disk service time\n",
		formatCount(total), intervalCount)

	fmt.Print("\033[H\033[J")
	fmt.Print(buf.String())
}

func renderDevice(buf *strings.Builder, name string, hist *DeviceHistogram) {
	if hist == nil {
		hist = &DeviceHistogram{Name: name}
	}

	readB, writeB := make([]uint64, 37), make([]uint64, 37)
	for i := 0; i < 37; i++ {
		readB[i], writeB[i] = hist.Buckets[i][colDiskRead], hist.Buckets[i][colDiskWrite]
	}
	rh, wh := newHistogram(readB), newHistogram(writeB)

	displayName := shortenName(name)
	if len(displayName) > 20 {
		displayName = displayName[:17] + "..."
	}

	total := rh.Count() + wh.Count()
	if total == 0 {
		fmt.Fprintf(buf, "%-20s │ %7s %7s %7s %7s %7s %7s │ %7s %7s %7s %7s %7s %7s │ %8s\n",
			displayName, "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "0")
		return
	}

	f := func(us float64) string { return fmt.Sprintf("%7s", formatLatency(us)) }

	ra, rp50, rp90, rp99, rp999, rm := "      -", "      -", "      -", "      -", "      -", "      -"
	if rh.Count() > 0 {
		ra, rp50, rp90, rp99, rp999, rm = f(rh.Mean()), f(rh.Percentile(50)), f(rh.Percentile(90)),
			f(rh.Percentile(99)), f(rh.Percentile(99.9)), f(rh.Max())
	}
	wa, wp50, wp90, wp99, wp999, wm := "      -", "      -", "      -", "      -", "      -", "      -"
	if wh.Count() > 0 {
		wa, wp50, wp90, wp99, wp999, wm = f(wh.Mean()), f(wh.Percentile(50)), f(wh.Percentile(90)),
			f(wh.Percentile(99)), f(wh.Percentile(99.9)), f(wh.Max())
	}

	fmt.Fprintf(buf, "%-20s │ %s %s %s %s %s %s │ %s %s %s %s %s %s │ %8s\n",
		displayName, ra, rp50, rp90, rp99, rp999, rm, wa, wp50, wp90, wp99, wp999, wm, formatCount(total))
}

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: zpool-latency [-i interval] <pool>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Real-time per-device disk latency percentiles (physical layer)")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  -i N    zpool iostat interval in seconds (default: 1)")
	}

	// Reorder args so flags can appear after positional args
	args := os.Args[1:]
	var reordered []string
	var positional []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			reordered = append(reordered, args[i])
			if args[i] == "-i" && i+1 < len(args) {
				i++
				reordered = append(reordered, args[i])
			}
		} else {
			positional = append(positional, args[i])
		}
	}
	os.Args = append([]string{os.Args[0]}, append(reordered, positional...)...)

	flag.Parse()

	pool := flag.Arg(0)
	if pool == "" {
		flag.Usage()
		os.Exit(1)
	}

	currentState, previousState, lifetimeState := newState(), newState(), newState()
	parser := &IntervalParser{current: currentState, previous: previousState}
	startTime := time.Now()

	done := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; close(done) }()

	cmd := exec.Command("zpool", "iostat", "-wvv", pool, fmt.Sprintf("%d", *interval))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "zpool-latency: %v\n", err)
		os.Exit(1)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "zpool-latency: %v\n", err)
		os.Exit(1)
	}

	go parser.Parse(stdout)

	go func() {
		if h := fetchLifetime(pool); h != nil {
			lifetimeState.Update(h)
		}
		ticker := time.NewTicker(lifetimePollFreq)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if h := fetchLifetime(pool); h != nil {
					lifetimeState.Update(h)
				}
			}
		}
	}()

	displayTicker := time.NewTicker(displayInterval)
	go func() {
		defer displayTicker.Stop()
		for {
			select {
			case <-done:
				return
			case <-displayTicker.C:
				ch, ct, _ := currentState.Snapshot()
				ph, pt, pc := previousState.Snapshot()
				lh, lt, _ := lifetimeState.Snapshot()
				if len(lh) > 0 {
					render(ch, ph, lh, ct, pt, lt, pc, startTime, *interval)
				}
			}
		}
	}()

	<-done
	cmd.Process.Kill()
	cmd.Wait()
}
