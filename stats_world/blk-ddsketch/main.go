// blk-ddsketch: Per-IO latency percentile tracker using eBPF + DDSketch
//
// Improvement over blk-latency: Uses DDSketch for provable relative error
// guarantees on tail latencies (p99.9, p99.99, p99.999).
//
// DDSketch provides:
// - Relative value error: true p99 is within ±α% of reported value
// - ~2-10KB memory per sketch (vs ~40KB for HDR)
// - Mergeable sketches (useful for aggregation)
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -type latency_event bpf bpf/latency.c -- -I/usr/include -I.

package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"
	"github.com/DataDog/sketches-go/ddsketch/mapping"
	"github.com/DataDog/sketches-go/ddsketch/store"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

const (
	displayInterval = 100 * time.Millisecond // 10 FPS display refresh
	// DDSketch relative accuracy: 1% means true value within ±1% of reported
	sketchAlpha = 0.01
)

var (
	interval = flag.Duration("i", 10*time.Second, "stats interval for interval view")
	devices  = flag.String("d", "", "comma-separated device filter (e.g., sdc,sdd or 8:32,8:48)")
	batch    = flag.Bool("batch", false, "batch mode (no screen clearing)")
	alpha    = flag.Float64("alpha", 0.01, "DDSketch relative accuracy (0.01 = 1%)")
)

// Device names cache: dev -> name
var (
	devNames   = make(map[uint32]string)
	devNamesMu sync.RWMutex
)

// formatLatency formats a latency value (in µs) to human-readable string
func formatLatency(us float64) string {
	if us < 1000 {
		return fmt.Sprintf("%.0fµs", us)
	}
	if us < 1_000_000 {
		ms := us / 1000
		return fmt.Sprintf("%.0fms", ms)
	}
	s := us / 1_000_000
	return fmt.Sprintf("%.1fs", s)
}

// formatLatencyPadded formats latency right-aligned in 8 chars
func formatLatencyPadded(us float64) string {
	return fmt.Sprintf("%8s", formatLatency(us))
}

// formatCount formats sample counts
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

// formatDuration formats elapsed time
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
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

func devToMajorMinor(dev uint32) (uint32, uint32) {
	return dev >> 20, dev & 0xFFFFF
}

func majorMinorToDev(major, minor uint32) uint32 {
	return (major << 20) | minor
}

func lookupDevName(dev uint32) string {
	devNamesMu.RLock()
	if name, ok := devNames[dev]; ok {
		devNamesMu.RUnlock()
		return name
	}
	devNamesMu.RUnlock()

	major, minor := devToMajorMinor(dev)
	name := fmt.Sprintf("%d:%d", major, minor)

	// Try to resolve from /sys/dev/block/
	sysPath := fmt.Sprintf("/sys/dev/block/%d:%d/device/../block", major, minor)
	if entries, err := os.ReadDir(sysPath); err == nil && len(entries) > 0 {
		name = entries[0].Name()
	} else {
		// Try uevent for disk name
		ueventPath := fmt.Sprintf("/sys/dev/block/%d:%d/uevent", major, minor)
		if data, err := os.ReadFile(ueventPath); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "DEVNAME=") {
					name = strings.TrimPrefix(line, "DEVNAME=")
					break
				}
			}
		}
	}

	devNamesMu.Lock()
	devNames[dev] = name
	devNamesMu.Unlock()
	return name
}

// isTrackedDevice returns true if device should be tracked (nvme* or sd* only)
func isTrackedDevice(name string) bool {
	return strings.HasPrefix(name, "nvme") || strings.HasPrefix(name, "sd")
}

func parseDeviceFilter(filter string) ([]uint32, error) {
	if filter == "" {
		return nil, nil
	}

	var devs []uint32
	for _, d := range strings.Split(filter, ",") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}

		// Try major:minor format
		if strings.Contains(d, ":") {
			parts := strings.Split(d, ":")
			if len(parts) != 2 {
				return nil, fmt.Errorf("invalid device: %s", d)
			}
			major, err := strconv.ParseUint(parts[0], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid major: %s", parts[0])
			}
			minor, err := strconv.ParseUint(parts[1], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid minor: %s", parts[1])
			}
			devs = append(devs, majorMinorToDev(uint32(major), uint32(minor)))
			continue
		}

		// Try device name (e.g., sdc)
		ueventPath := fmt.Sprintf("/sys/block/%s/uevent", d)
		data, err := os.ReadFile(ueventPath)
		if err != nil {
			return nil, fmt.Errorf("device not found: %s", d)
		}
		var major, minor uint64
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MAJOR=") {
				major, _ = strconv.ParseUint(strings.TrimPrefix(line, "MAJOR="), 10, 32)
			}
			if strings.HasPrefix(line, "MINOR=") {
				minor, _ = strconv.ParseUint(strings.TrimPrefix(line, "MINOR="), 10, 32)
			}
		}
		devs = append(devs, majorMinorToDev(uint32(major), uint32(minor)))
	}
	return devs, nil
}

// preciseStats tracks sum/count with full precision for exact average
type preciseStats struct {
	sum   float64
	count uint64
}

func (p *preciseStats) Add(v float64) {
	p.sum += v
	p.count++
}

func (p *preciseStats) Avg() float64 {
	if p.count == 0 {
		return 0
	}
	return p.sum / float64(p.count)
}

func (p *preciseStats) Reset() {
	p.sum = 0
	p.count = 0
}

func (p *preciseStats) Clone() preciseStats {
	return preciseStats{sum: p.sum, count: p.count}
}

// newSketch creates a DDSketch with given alpha
func newSketch(alpha float64) *ddsketch.DDSketch {
	m, _ := mapping.NewLogarithmicMapping(alpha)
	s := ddsketch.NewDDSketch(m, store.NewDenseStore(), store.NewDenseStore())
	return s
}

// copySketch creates a deep copy of a DDSketch
func copySketch(src *ddsketch.DDSketch) *ddsketch.DDSketch {
	dst := newSketch(*alpha)
	dst.MergeWith(src)
	return dst
}

// deviceStats holds both interval and lifetime sketches for a device
type deviceStats struct {
	interval         *ddsketch.DDSketch // Current interval (reset each period)
	lifetime         *ddsketch.DDSketch // All-time accumulation
	intervalPrecise  preciseStats       // Precise sum/count for interval avg
	lifetimePrecise  preciseStats       // Precise sum/count for lifetime avg
	alpha            float64            // Relative accuracy
}

func newDeviceStats(alpha float64) *deviceStats {
	return &deviceStats{
		interval: newSketch(alpha),
		lifetime: newSketch(alpha),
		alpha:    alpha,
	}
}

// Record adds a latency sample (in microseconds)
func (ds *deviceStats) Record(latencyUs float64) {
	ds.interval.Add(latencyUs)
	ds.lifetime.Add(latencyUs)
	ds.intervalPrecise.Add(latencyUs)
	ds.lifetimePrecise.Add(latencyUs)
}

// ResetInterval clears the interval sketch (lifetime persists)
func (ds *deviceStats) ResetInterval() {
	ds.interval = newSketch(ds.alpha)
	ds.intervalPrecise.Reset()
}

// Snapshot creates deep copies for lock-free display
func (ds *deviceStats) Snapshot() *deviceStats {
	return &deviceStats{
		interval:        copySketch(ds.interval),
		lifetime:        copySketch(ds.lifetime),
		intervalPrecise: ds.intervalPrecise.Clone(),
		lifetimePrecise: ds.lifetimePrecise.Clone(),
		alpha:           ds.alpha,
	}
}

// State holds all device stats with mutex protection
type State struct {
	mu        sync.RWMutex
	stats     map[uint32]*deviceStats
	startTime time.Time
	lastReset time.Time
	alpha     float64
}

func newState(alpha float64) *State {
	now := time.Now()
	return &State{
		stats:     make(map[uint32]*deviceStats),
		startTime: now,
		lastReset: now,
		alpha:     alpha,
	}
}

func (s *State) Record(dev uint32, latencyUs float64) {
	s.mu.Lock()
	ds, ok := s.stats[dev]
	if !ok {
		ds = newDeviceStats(s.alpha)
		s.stats[dev] = ds
	}
	ds.Record(latencyUs)
	s.mu.Unlock()
}

func (s *State) ResetIntervals() {
	s.mu.Lock()
	for _, ds := range s.stats {
		ds.ResetInterval()
	}
	s.lastReset = time.Now()
	s.mu.Unlock()
}

func (s *State) Snapshot() (map[uint32]*deviceStats, time.Time, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := make(map[uint32]*deviceStats)
	for dev, ds := range s.stats {
		snap[dev] = ds.Snapshot()
	}
	return snap, s.startTime, s.lastReset
}

// Display handles rendering
type Display struct {
	batchMode bool
}

func (d *Display) resetCursor() {
	if !d.batchMode {
		fmt.Print("\033[H\033[J")
	}
}


// getQuantileSafe returns quantile value, handling empty sketches
func getQuantileSafe(s *ddsketch.DDSketch, q float64) (float64, bool) {
	if s.GetCount() == 0 {
		return 0, false
	}
	v, err := s.GetValueAtQuantile(q)
	if err != nil {
		return 0, false
	}
	return v, true
}

const lineWidth = 196

func (d *Display) render(stats map[uint32]*deviceStats, startTime, lastReset time.Time, intervalDur time.Duration, alpha float64) {
	var buf strings.Builder
	now := time.Now()

	// Sort devices by name
	var devList []uint32
	for dev := range stats {
		devList = append(devList, dev)
	}
	sort.Slice(devList, func(i, j int) bool {
		return lookupDevName(devList[i]) < lookupDevName(devList[j])
	})

	timestamp := now.Format("15:04:05")
	elapsed := now.Sub(startTime)
	intervalElapsed := now.Sub(lastReset)

	fmt.Fprintf(&buf, "Block I/O Latency (DDSketch α=%.2f%%) - %s (uptime: %s, interval: %s/%s)\n",
		alpha*100, timestamp, formatDuration(elapsed), formatDuration(intervalElapsed), formatDuration(intervalDur))
	buf.WriteString(strings.Repeat("=", lineWidth))
	buf.WriteString("\n")

	// Header: min, avg, p10-p90, p99, p99.9, p99.99, p99.999, max, samples
	fmt.Fprintf(&buf, "%-10s │ %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s │ %9s\n",
		"INTERVAL", "min", "avg", "p10", "p20", "p30", "p40", "p50", "p60", "p70", "p80", "p90", "p99", "p99.9", "p99.99", "p99.999", "max", "samples")
	buf.WriteString(strings.Repeat("-", lineWidth))
	buf.WriteString("\n")

	// Interval stats
	for _, dev := range devList {
		ds := stats[dev]
		name := lookupDevName(dev)
		s := ds.interval
		n := ds.intervalPrecise.count
		if n == 0 {
			fmt.Fprintf(&buf, "%-10s │ %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s │ %9s\n",
				name, "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "0")
			continue
		}

		avg := ds.intervalPrecise.Avg()
		min, _ := getQuantileSafe(s, 0.0)
		p10, _ := getQuantileSafe(s, 0.10)
		p20, _ := getQuantileSafe(s, 0.20)
		p30, _ := getQuantileSafe(s, 0.30)
		p40, _ := getQuantileSafe(s, 0.40)
		p50, _ := getQuantileSafe(s, 0.50)
		p60, _ := getQuantileSafe(s, 0.60)
		p70, _ := getQuantileSafe(s, 0.70)
		p80, _ := getQuantileSafe(s, 0.80)
		p90, _ := getQuantileSafe(s, 0.90)
		p99, _ := getQuantileSafe(s, 0.99)
		p999, _ := getQuantileSafe(s, 0.999)
		p9999, _ := getQuantileSafe(s, 0.9999)
		p99999, _ := getQuantileSafe(s, 0.99999)
		max, _ := getQuantileSafe(s, 1.0)

		fmt.Fprintf(&buf, "%-10s │ %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s │ %9s\n",
			name,
			formatLatencyPadded(min),
			formatLatencyPadded(avg),
			formatLatencyPadded(p10),
			formatLatencyPadded(p20),
			formatLatencyPadded(p30),
			formatLatencyPadded(p40),
			formatLatencyPadded(p50),
			formatLatencyPadded(p60),
			formatLatencyPadded(p70),
			formatLatencyPadded(p80),
			formatLatencyPadded(p90),
			formatLatencyPadded(p99),
			formatLatencyPadded(p999),
			formatLatencyPadded(p9999),
			formatLatencyPadded(p99999),
			formatLatencyPadded(max),
			formatCount(n),
		)
	}

	buf.WriteString("\n")
	fmt.Fprintf(&buf, "%-10s │ %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s │ %9s\n",
		"LIFETIME", "min", "avg", "p10", "p20", "p30", "p40", "p50", "p60", "p70", "p80", "p90", "p99", "p99.9", "p99.99", "p99.999", "max", "samples")
	buf.WriteString(strings.Repeat("-", lineWidth))
	buf.WriteString("\n")

	// Lifetime stats
	var totalSamples uint64
	for _, dev := range devList {
		ds := stats[dev]
		name := lookupDevName(dev)
		s := ds.lifetime
		n := ds.lifetimePrecise.count
		totalSamples += n
		if n == 0 {
			fmt.Fprintf(&buf, "%-10s │ %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s │ %9s\n",
				name, "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "0")
			continue
		}

		avg := ds.lifetimePrecise.Avg()
		min, _ := getQuantileSafe(s, 0.0)
		p10, _ := getQuantileSafe(s, 0.10)
		p20, _ := getQuantileSafe(s, 0.20)
		p30, _ := getQuantileSafe(s, 0.30)
		p40, _ := getQuantileSafe(s, 0.40)
		p50, _ := getQuantileSafe(s, 0.50)
		p60, _ := getQuantileSafe(s, 0.60)
		p70, _ := getQuantileSafe(s, 0.70)
		p80, _ := getQuantileSafe(s, 0.80)
		p90, _ := getQuantileSafe(s, 0.90)
		p99, _ := getQuantileSafe(s, 0.99)
		p999, _ := getQuantileSafe(s, 0.999)
		p9999, _ := getQuantileSafe(s, 0.9999)
		p99999, _ := getQuantileSafe(s, 0.99999)
		max, _ := getQuantileSafe(s, 1.0)

		fmt.Fprintf(&buf, "%-10s │ %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s │ %9s\n",
			name,
			formatLatencyPadded(min),
			formatLatencyPadded(avg),
			formatLatencyPadded(p10),
			formatLatencyPadded(p20),
			formatLatencyPadded(p30),
			formatLatencyPadded(p40),
			formatLatencyPadded(p50),
			formatLatencyPadded(p60),
			formatLatencyPadded(p70),
			formatLatencyPadded(p80),
			formatLatencyPadded(p90),
			formatLatencyPadded(p99),
			formatLatencyPadded(p999),
			formatLatencyPadded(p9999),
			formatLatencyPadded(p99999),
			formatLatencyPadded(max),
			formatCount(n),
		)
	}

	buf.WriteString(strings.Repeat("=", lineWidth))
	buf.WriteString("\n")

	// Stats summary
	rate := float64(0)
	if elapsed.Seconds() > 0 {
		rate = float64(totalSamples) / elapsed.Seconds()
	}
	fmt.Fprintf(&buf, "Total: %s samples | Rate: %s/s | DDSketch: ~2-10KB/device (α=%.2f%% relative error)\n",
		formatCount(totalSamples), formatCount(uint64(rate)), alpha*100)

	if d.batchMode {
		buf.WriteString("\n")
	}

	d.resetCursor()
	fmt.Print(buf.String())
}

func main() {
	flag.Parse()

	// Parse device filter
	filterDevs, err := parseDeviceFilter(*devices)
	if err != nil {
		log.Fatalf("Invalid device filter: %v", err)
	}

	// Validate alpha
	if *alpha <= 0 || *alpha >= 1 {
		log.Fatalf("Alpha must be between 0 and 1 (got %.4f)", *alpha)
	}

	// Remove memlock limit for eBPF
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatalf("Failed to remove memlock limit: %v", err)
	}

	// Load eBPF objects
	objs := bpfObjects{}
	if err := loadBpfObjects(&objs, nil); err != nil {
		log.Fatalf("Failed to load eBPF objects: %v", err)
	}
	defer objs.Close()

	// Set up device filter if specified
	if len(filterDevs) > 0 {
		var key uint32 = 0
		var enabled uint8 = 1
		if err := objs.LatConfig.Put(key, enabled); err != nil {
			log.Fatalf("Failed to enable filter: %v", err)
		}
		for _, dev := range filterDevs {
			var val uint8 = 1
			if err := objs.DevFilter.Put(dev, val); err != nil {
				log.Fatalf("Failed to add device to filter: %v", err)
			}
		}
		log.Printf("Filtering %d device(s)", len(filterDevs))
	}

	// Attach to tracepoints
	tpIssue, err := link.AttachTracing(link.TracingOptions{
		Program: objs.BlockRqIssue,
	})
	if err != nil {
		log.Fatalf("Failed to attach block_rq_issue: %v", err)
	}
	defer tpIssue.Close()

	tpComplete, err := link.AttachTracing(link.TracingOptions{
		Program: objs.BlockRqComplete,
	})
	if err != nil {
		log.Fatalf("Failed to attach block_rq_complete: %v", err)
	}
	defer tpComplete.Close()

	// Open ring buffer
	rd, err := ringbuf.NewReader(objs.Events)
	if err != nil {
		log.Fatalf("Failed to open ring buffer: %v", err)
	}
	defer rd.Close()

	state := newState(*alpha)
	display := &Display{batchMode: *batch}

	// Signal handling
	done := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig
		close(done)
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
				stats, startTime, lastReset := state.Snapshot()
				if len(stats) > 0 {
					display.render(stats, startTime, lastReset, *interval, *alpha)
				}
			}
		}
	}()

	// Interval reset goroutine
	intervalTicker := time.NewTicker(*interval)
	go func() {
		defer intervalTicker.Stop()
		for {
			select {
			case <-done:
				return
			case <-intervalTicker.C:
				state.ResetIntervals()
			}
		}
	}()

	log.Printf("Tracing block I/O latency with DDSketch (α=%.2f%%, interval=%v)...", *alpha*100, *interval)

	// Ring buffer consumer (main loop)
	var event bpfLatencyEvent
	for {
		select {
		case <-done:
			// Final stats
			stats, startTime, lastReset := state.Snapshot()
			display.render(stats, startTime, lastReset, *interval, *alpha)
			return
		default:
		}

		record, err := rd.Read()
		if err != nil {
			if err == ringbuf.ErrClosed {
				return
			}
			continue
		}

		if err := binary.Read(bytes.NewReader(record.RawSample), binary.LittleEndian, &event); err != nil {
			continue
		}

		// Only track nvme* and sd* devices
		devName := lookupDevName(event.Dev)
		if !isTrackedDevice(devName) {
			continue
		}

		latencyUs := float64(event.LatencyNs) / 1000.0
		if latencyUs < 1 {
			latencyUs = 1
		}

		state.Record(event.Dev, latencyUs)
	}
}
