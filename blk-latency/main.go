// blk-latency: Per-IO latency percentile tracker using eBPF
//
// Traces block_rq_issue/complete to compute per-request latency,
// maintains HDR histograms per device, emits percentiles on interval.
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

	"github.com/HdrHistogram/hdrhistogram-go"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

const (
	displayInterval = 100 * time.Millisecond // 10 FPS display refresh
	// HDR histogram range: 1µs to 60s, 3 significant figures
	histMin    = 1
	histMax    = 60_000_000
	histSigFig = 3
)

var (
	interval   = flag.Duration("i", 10*time.Second, "stats interval for interval view")
	devices    = flag.String("d", "", "comma-separated device filter (e.g., sdc,sdd or 8:32,8:48)")
	batch      = flag.Bool("batch", false, "batch mode (no screen clearing)")
)

// Device names cache: dev -> name
var (
	devNames   = make(map[uint32]string)
	devNamesMu sync.RWMutex
)

// formatLatency formats a latency value (in µs) to human-readable string
func formatLatency(us int64) string {
	if us < 1000 {
		return fmt.Sprintf("%dµs", us)
	}
	if us < 1_000_000 {
		ms := (us + 500) / 1000 // round to nearest ms
		return fmt.Sprintf("%dms", ms)
	}
	s := float64(us) / 1_000_000
	return fmt.Sprintf("%.1fs", s)
}

// formatLatencyPadded formats latency right-aligned in 8 chars
func formatLatencyPadded(us int64) string {
	return fmt.Sprintf("%8s", formatLatency(us))
}

// formatCount formats sample counts
func formatCount(n int64) string {
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

// deviceStats holds both interval and lifetime histograms for a device
type deviceStats struct {
	interval *hdrhistogram.Histogram // Current interval (reset each period)
	lifetime *hdrhistogram.Histogram // All-time accumulation
}

func newDeviceStats() *deviceStats {
	return &deviceStats{
		interval: hdrhistogram.New(histMin, histMax, histSigFig),
		lifetime: hdrhistogram.New(histMin, histMax, histSigFig),
	}
}

// Record adds a latency sample to both histograms
func (ds *deviceStats) Record(latencyUs int64) {
	ds.interval.RecordValue(latencyUs)
	ds.lifetime.RecordValue(latencyUs)
}

// ResetInterval clears the interval histogram (lifetime persists)
func (ds *deviceStats) ResetInterval() {
	ds.interval.Reset()
}

// Snapshot creates deep copies of both histograms for lock-free display
func (ds *deviceStats) Snapshot() *deviceStats {
	return &deviceStats{
		interval: hdrhistogram.Import(ds.interval.Export()),
		lifetime: hdrhistogram.Import(ds.lifetime.Export()),
	}
}

// State holds all device stats with mutex protection
type State struct {
	mu        sync.RWMutex
	stats     map[uint32]*deviceStats
	startTime time.Time
	lastReset time.Time
}

func newState() *State {
	now := time.Now()
	return &State{
		stats:     make(map[uint32]*deviceStats),
		startTime: now,
		lastReset: now,
	}
}

func (s *State) Record(dev uint32, latencyUs int64) {
	s.mu.Lock()
	ds, ok := s.stats[dev]
	if !ok {
		ds = newDeviceStats()
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

func (d *Display) render(stats map[uint32]*deviceStats, startTime, lastReset time.Time, intervalDur time.Duration) {
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

	fmt.Fprintf(&buf, "Block I/O Latency Monitor - %s (uptime: %s, interval: %s/%s)\n",
		timestamp, formatDuration(elapsed), formatDuration(intervalElapsed), formatDuration(intervalDur))
	buf.WriteString(strings.Repeat("=", 120))
	buf.WriteString("\n")

	// Header
	fmt.Fprintf(&buf, "%-10s │ %8s %8s %8s %8s %8s %8s %8s │ %9s\n",
		"INTERVAL", "avg", "p50", "p90", "p95", "p99", "p99.9", "max", "samples")
	buf.WriteString(strings.Repeat("-", 120))
	buf.WriteString("\n")

	// Interval stats
	for _, dev := range devList {
		ds := stats[dev]
		name := lookupDevName(dev)
		h := ds.interval
		n := h.TotalCount()
		if n == 0 {
			fmt.Fprintf(&buf, "%-10s │ %8s %8s %8s %8s %8s %8s %8s │ %9s\n",
				name, "-", "-", "-", "-", "-", "-", "-", "0")
			continue
		}
		fmt.Fprintf(&buf, "%-10s │ %s %s %s %s %s %s %s │ %9s\n",
			name,
			formatLatencyPadded(int64(h.Mean())),
			formatLatencyPadded(h.ValueAtQuantile(50)),
			formatLatencyPadded(h.ValueAtQuantile(90)),
			formatLatencyPadded(h.ValueAtQuantile(95)),
			formatLatencyPadded(h.ValueAtQuantile(99)),
			formatLatencyPadded(h.ValueAtQuantile(99.9)),
			formatLatencyPadded(h.Max()),
			formatCount(n),
		)
	}

	buf.WriteString("\n")
	fmt.Fprintf(&buf, "%-10s │ %8s %8s %8s %8s %8s %8s %8s │ %9s\n",
		"LIFETIME", "avg", "p50", "p90", "p95", "p99", "p99.9", "max", "samples")
	buf.WriteString(strings.Repeat("-", 120))
	buf.WriteString("\n")

	// Lifetime stats
	var totalSamples int64
	for _, dev := range devList {
		ds := stats[dev]
		name := lookupDevName(dev)
		h := ds.lifetime
		n := h.TotalCount()
		totalSamples += n
		if n == 0 {
			fmt.Fprintf(&buf, "%-10s │ %8s %8s %8s %8s %8s %8s %8s │ %9s\n",
				name, "-", "-", "-", "-", "-", "-", "-", "0")
			continue
		}
		fmt.Fprintf(&buf, "%-10s │ %s %s %s %s %s %s %s │ %9s\n",
			name,
			formatLatencyPadded(int64(h.Mean())),
			formatLatencyPadded(h.ValueAtQuantile(50)),
			formatLatencyPadded(h.ValueAtQuantile(90)),
			formatLatencyPadded(h.ValueAtQuantile(95)),
			formatLatencyPadded(h.ValueAtQuantile(99)),
			formatLatencyPadded(h.ValueAtQuantile(99.9)),
			formatLatencyPadded(h.Max()),
			formatCount(n),
		)
	}

	buf.WriteString(strings.Repeat("=", 120))
	buf.WriteString("\n")

	// Stats summary
	rate := float64(0)
	if elapsed.Seconds() > 0 {
		rate = float64(totalSamples) / elapsed.Seconds()
	}
	fmt.Fprintf(&buf, "Total: %s samples | Rate: %s/s | HDR histograms: ~40KB/device\n",
		formatCount(totalSamples), formatCount(int64(rate)))

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

	state := newState()
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
					display.render(stats, startTime, lastReset, *interval)
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

	log.Printf("Tracing block I/O latency (interval=%v, display=10fps)...", *interval)

	// Ring buffer consumer (main loop)
	var event bpfLatencyEvent
	for {
		select {
		case <-done:
			// Final stats
			stats, startTime, lastReset := state.Snapshot()
			display.render(stats, startTime, lastReset, *interval)
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

		latencyUs := int64(event.LatencyNs / 1000)
		if latencyUs < 1 {
			latencyUs = 1
		}
		if latencyUs > histMax {
			latencyUs = histMax
		}

		state.Record(event.Dev, latencyUs)
	}
}
