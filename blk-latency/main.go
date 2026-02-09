// blk-latency: Per-IO latency percentile tracker using eBPF
//
// Traces block_rq_issue/complete to compute per-request latency,
// maintains HDR histograms per device, emits percentiles on interval.
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -type latency_event bpf bpf/latency.c -- -I/usr/include -I.

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/HdrHistogram/hdrhistogram-go"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/nmarasoiu/zfs-scripts/ringpoll"
)

const (
	displayInterval = 100 * time.Millisecond // 10 FPS display refresh
	// HDR histogram range: 1us to 60s, 3 significant figures
	histMin    = 1
	histMax    = 60_000_000
	histSigFig = 3
)

var (
	interval  = flag.Duration("i", 10*time.Second, "stats interval for interval view")
	devices   = flag.String("d", "", "comma-separated device filter (e.g., sdc,sdd or 8:32,8:48)")
	batch     = flag.Bool("batch", false, "batch mode (no screen clearing)")
	pollSleep = flag.Duration("poll-sleep", 50*time.Microsecond, "ring buffer poll sleep when empty")
)

// Device names cache: dev -> name
var (
	devNames   = make(map[uint32]string)
	devNamesMu sync.RWMutex
)

// formatLatency formats a latency value (in us) to human-readable string
func formatLatency(us int64) string {
	if us < 100_000 {
		return fmt.Sprintf("%dus", us)
	}
	if us < 1_000_000 {
		ms := (us + 500) / 1000
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

func formatBytes(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	}
	if n >= 1<<10 {
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

func formatMicro(d time.Duration) string {
	us := d.Microseconds()
	if us < 1000 {
		return fmt.Sprintf("%dus", us)
	}
	if us < 1000000 {
		return fmt.Sprintf("%.1fms", float64(us)/1000)
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
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

// topN tracks the top N maximum values
type topN struct {
	values []int64
	n      int
}

func newTopN(n int) *topN {
	return &topN{
		values: make([]int64, 0, n),
		n:      n,
	}
}

// Add inserts a value if it belongs in the top N
func (t *topN) Add(v int64) {
	if len(t.values) < t.n {
		// Still filling up - insert in sorted position
		i := sort.Search(len(t.values), func(i int) bool { return t.values[i] >= v })
		t.values = append(t.values, 0)
		copy(t.values[i+1:], t.values[i:])
		t.values[i] = v
		return
	}
	// Full - only insert if larger than smallest (first element)
	if v > t.values[0] {
		i := sort.Search(len(t.values), func(i int) bool { return t.values[i] >= v })
		if i > 0 {
			copy(t.values[:i-1], t.values[1:i])
			t.values[i-1] = v
		}
	}
}

// Get returns values sorted ascending (index 0 = smallest of top N, last = max)
func (t *topN) Get() []int64 {
	result := make([]int64, len(t.values))
	copy(result, t.values)
	return result
}

// Reset clears all values
func (t *topN) Reset() {
	t.values = t.values[:0]
}

// bottomN tracks the bottom N minimum values
type bottomN struct {
	values []int64
	n      int
}

func newBottomN(n int) *bottomN {
	return &bottomN{
		values: make([]int64, 0, n),
		n:      n,
	}
}

// Add inserts a value if it belongs in the bottom N
func (b *bottomN) Add(v int64) {
	if len(b.values) < b.n {
		// Still filling up - insert in sorted position (descending)
		i := sort.Search(len(b.values), func(i int) bool { return b.values[i] <= v })
		b.values = append(b.values, 0)
		copy(b.values[i+1:], b.values[i:])
		b.values[i] = v
		return
	}
	// Full - only insert if smaller than largest (first element)
	if v < b.values[0] {
		i := sort.Search(len(b.values), func(i int) bool { return b.values[i] <= v })
		if i > 0 {
			copy(b.values[:i-1], b.values[1:i])
			b.values[i-1] = v
		}
	}
}

// Get returns values sorted ascending (index 0 = min, last = largest of bottom N)
func (b *bottomN) Get() []int64 {
	result := make([]int64, len(b.values))
	// Reverse to get ascending order
	for i, v := range b.values {
		result[len(b.values)-1-i] = v
	}
	return result
}

// Reset clears all values
func (b *bottomN) Reset() {
	b.values = b.values[:0]
}

// deviceStats holds both interval and lifetime histograms for a device
type deviceStats struct {
	interval    *hdrhistogram.Histogram // Current interval (reset each period)
	lifetime    *hdrhistogram.Histogram // All-time accumulation
	intervalTop *topN                   // Top 10 for current interval
	lifetimeTop *topN                   // Top 10 all-time
	intervalBot *bottomN                // Bottom 5 for current interval
	lifetimeBot *bottomN                // Bottom 5 all-time
}

func newDeviceStats() *deviceStats {
	return &deviceStats{
		interval:    hdrhistogram.New(histMin, histMax, histSigFig),
		lifetime:    hdrhistogram.New(histMin, histMax, histSigFig),
		intervalTop: newTopN(10),
		lifetimeTop: newTopN(10),
		intervalBot: newBottomN(5),
		lifetimeBot: newBottomN(5),
	}
}

// Record adds a latency sample to both histograms and top/bottom trackers
func (ds *deviceStats) Record(latencyUs int64) {
	ds.interval.RecordValue(latencyUs)
	ds.lifetime.RecordValue(latencyUs)
	ds.intervalTop.Add(latencyUs)
	ds.lifetimeTop.Add(latencyUs)
	ds.intervalBot.Add(latencyUs)
	ds.lifetimeBot.Add(latencyUs)
}

// ResetInterval clears the interval histogram (lifetime persists)
func (ds *deviceStats) ResetInterval() {
	ds.interval.Reset()
	ds.intervalTop.Reset()
	ds.intervalBot.Reset()
}

// pendingEvent holds a decoded event for batch processing
type pendingEvent struct {
	dev       uint32
	latencyUs int64
}

// State holds all device stats with mutex protection
type State struct {
	mu        sync.Mutex
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

func (s *State) RecordBatch(batch []pendingEvent) {
	s.mu.Lock()
	for i := range batch {
		e := &batch[i]
		ds, ok := s.stats[e.dev]
		if !ok {
			ds = newDeviceStats()
			s.stats[e.dev] = ds
		}
		ds.Record(e.latencyUs)
	}
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

// Display handles rendering
type Display struct {
	batchMode bool
	ring      *ringpoll.Reader
}

func (d *Display) resetCursor() {
	if !d.batchMode {
		fmt.Print("\033[H\033[J")
	}
}

// formatTop10 formats top 10 values as fixed columns (max-9 through max)
// Missing values shown as "-"
func formatTop10(top *topN) string {
	vals := top.Get() // ascending: index 0 = max-9, last = max
	var parts []string
	numMissing := 10 - len(vals)
	// Pad missing with "-"
	for i := 0; i < numMissing; i++ {
		parts = append(parts, fmt.Sprintf("%8s", "-"))
	}
	// Add actual values
	for _, v := range vals {
		parts = append(parts, formatLatencyPadded(v))
	}
	return strings.Join(parts, " ")
}

// formatBot5 formats bottom 5 values as fixed columns (min through min+4)
// Missing values shown as "-"
func formatBot5(bot *bottomN) string {
	vals := bot.Get() // ascending: index 0 = min, last = min+4
	var parts []string
	// Add actual values first
	for _, v := range vals {
		parts = append(parts, formatLatencyPadded(v))
	}
	// Pad missing with "-"
	numMissing := 5 - len(vals)
	for i := 0; i < numMissing; i++ {
		parts = append(parts, fmt.Sprintf("%8s", "-"))
	}
	return strings.Join(parts, " ")
}

const lineWidth = 258

func (d *Display) render(state *State, intervalDur time.Duration, drops uint64) {
	var buf strings.Builder
	now := time.Now()

	// Hold lock while reading state and formatting strings -- no copies/clones.
	state.mu.Lock()

	if len(state.stats) == 0 {
		state.mu.Unlock()
		return
	}

	// Sort devices by name
	var devList []uint32
	for dev := range state.stats {
		devList = append(devList, dev)
	}
	sort.Slice(devList, func(i, j int) bool {
		return lookupDevName(devList[i]) < lookupDevName(devList[j])
	})

	elapsed := now.Sub(state.startTime)
	intervalElapsed := now.Sub(state.lastReset)

	fmt.Fprintf(&buf, "Block I/O Latency Monitor - %s (uptime: %s, interval: %s/%s)\n",
		now.Format("15:04:05"), formatDuration(elapsed), formatDuration(intervalElapsed), formatDuration(intervalDur))
	buf.WriteString(strings.Repeat("=", lineWidth))
	buf.WriteString("\n")

	// Header - fixed columns: percentiles, bottom 5 min, top 10 max
	fmt.Fprintf(&buf, "%-10s | %8s %8s %8s %8s %8s %8s %8s %8s | %8s %8s %8s %8s %8s | %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s | %9s\n",
		"INTERVAL", "avg", "p50", "p90", "p95", "p99", "p99.9", "p99.99", "p99.999",
		"min", "min+1", "min+2", "min+3", "min+4",
		"max-9", "max-8", "max-7", "max-6", "max-5", "max-4", "max-3", "max-2", "max-1", "max", "samples")
	buf.WriteString(strings.Repeat("-", lineWidth))
	buf.WriteString("\n")

	// Interval stats
	for _, dev := range devList {
		ds := state.stats[dev]
		name := lookupDevName(dev)
		h := ds.interval
		n := h.TotalCount()
		if n == 0 {
			fmt.Fprintf(&buf, "%-10s | %8s %8s %8s %8s %8s %8s %8s %8s | %8s %8s %8s %8s %8s | %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s | %9s\n",
				name, "-", "-", "-", "-", "-", "-", "-", "-",
				"-", "-", "-", "-", "-",
				"-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "0")
			continue
		}
		fmt.Fprintf(&buf, "%-10s | %s %s %s %s %s %s %s %s | %s | %s | %9s\n",
			name,
			formatLatencyPadded(int64(h.Mean())),
			formatLatencyPadded(h.ValueAtQuantile(50)),
			formatLatencyPadded(h.ValueAtQuantile(90)),
			formatLatencyPadded(h.ValueAtQuantile(95)),
			formatLatencyPadded(h.ValueAtQuantile(99)),
			formatLatencyPadded(h.ValueAtQuantile(99.9)),
			formatLatencyPadded(h.ValueAtQuantile(99.99)),
			formatLatencyPadded(h.ValueAtQuantile(99.999)),
			formatBot5(ds.intervalBot),
			formatTop10(ds.intervalTop),
			formatCount(n),
		)
	}

	buf.WriteString("\n")
	fmt.Fprintf(&buf, "%-10s | %8s %8s %8s %8s %8s %8s %8s %8s | %8s %8s %8s %8s %8s | %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s | %9s\n",
		"LIFETIME", "avg", "p50", "p90", "p95", "p99", "p99.9", "p99.99", "p99.999",
		"min", "min+1", "min+2", "min+3", "min+4",
		"max-9", "max-8", "max-7", "max-6", "max-5", "max-4", "max-3", "max-2", "max-1", "max", "samples")
	buf.WriteString(strings.Repeat("-", lineWidth))
	buf.WriteString("\n")

	// Lifetime stats
	var totalSamples int64
	for _, dev := range devList {
		ds := state.stats[dev]
		name := lookupDevName(dev)
		h := ds.lifetime
		n := h.TotalCount()
		totalSamples += n
		if n == 0 {
			fmt.Fprintf(&buf, "%-10s | %8s %8s %8s %8s %8s %8s %8s %8s | %8s %8s %8s %8s %8s | %8s %8s %8s %8s %8s %8s %8s %8s %8s %8s | %9s\n",
				name, "-", "-", "-", "-", "-", "-", "-", "-",
				"-", "-", "-", "-", "-",
				"-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "0")
			continue
		}
		fmt.Fprintf(&buf, "%-10s | %s %s %s %s %s %s %s %s | %s | %s | %9s\n",
			name,
			formatLatencyPadded(int64(h.Mean())),
			formatLatencyPadded(h.ValueAtQuantile(50)),
			formatLatencyPadded(h.ValueAtQuantile(90)),
			formatLatencyPadded(h.ValueAtQuantile(95)),
			formatLatencyPadded(h.ValueAtQuantile(99)),
			formatLatencyPadded(h.ValueAtQuantile(99.9)),
			formatLatencyPadded(h.ValueAtQuantile(99.99)),
			formatLatencyPadded(h.ValueAtQuantile(99.999)),
			formatBot5(ds.lifetimeBot),
			formatTop10(ds.lifetimeTop),
			formatCount(n),
		)
	}

	buf.WriteString(strings.Repeat("=", lineWidth))
	buf.WriteString("\n")

	nDevices := len(state.stats)

	state.mu.Unlock()

	// Everything below is lock-free: computed values + ring stats (atomic reads)
	rate := float64(0)
	if elapsed.Seconds() > 0 {
		rate = float64(totalSamples) / elapsed.Seconds()
	}
	dropRate := float64(0)
	if elapsed.Seconds() > 0 {
		dropRate = float64(drops) / elapsed.Seconds()
	}

	ringInfo := ""
	if d.ring != nil {
		pending := d.ring.Pending()
		capBytes := d.ring.BufSize()
		maxPend := d.ring.MaxPending()
		pctFull := float64(pending) / float64(capBytes) * 100
		maxPct := float64(maxPend) / float64(capBytes) * 100
		avg1, avg0, last1, last0 := d.ring.PollStats()
		last0Str := "-"
		if last0 > 0 {
			last0Str = formatMicro(last0)
		}
		ringInfo = fmt.Sprintf(" | Ring: %s/%s (%.1f%%) max: %s/%s (%.1f%%) avg1:%.0f avg0:%.1f last1:%s last0:%s",
			formatBytes(int64(pending)), formatBytes(int64(capBytes)), pctFull,
			formatBytes(maxPend), formatBytes(int64(capBytes)), maxPct,
			avg1, avg0, formatCount(last1), last0Str)
	}

	fmt.Fprintf(&buf, "Total: %s samples | Rate: %s/s | Devices: %d | Drops: %s (%s/s) | HDR: ~40KB/dev%s\n",
		formatCount(totalSamples), formatCount(int64(rate)), nDevices,
		formatCount(int64(drops)), formatCount(int64(dropRate)), ringInfo)

	if d.batchMode {
		buf.WriteString("\n")
	}

	d.resetCursor()
	fmt.Print(buf.String())
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "blk-latency: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flag.Parse()

	// Parse device filter
	filterDevs, err := parseDeviceFilter(*devices)
	if err != nil {
		return fmt.Errorf("invalid device filter: %w", err)
	}

	// Remove memlock limit for eBPF
	if err := rlimit.RemoveMemlock(); err != nil {
		return fmt.Errorf("remove memlock limit: %w", err)
	}

	// Load eBPF objects
	objs := bpfObjects{}
	if err := loadBpfObjects(&objs, nil); err != nil {
		return fmt.Errorf("load eBPF objects: %w", err)
	}
	defer objs.Close()

	// Set up device filter if specified
	if len(filterDevs) > 0 {
		var key uint32 = 0
		var enabled uint8 = 1
		if err := objs.LatConfig.Put(key, enabled); err != nil {
			return fmt.Errorf("enable device filter: %w", err)
		}
		for _, dev := range filterDevs {
			var val uint8 = 1
			if err := objs.DevFilter.Put(dev, val); err != nil {
				return fmt.Errorf("add device to filter: %w", err)
			}
		}
		log.Printf("Filtering %d device(s)", len(filterDevs))
	}

	// Attach to tracepoints
	tpIssue, err := link.AttachTracing(link.TracingOptions{
		Program: objs.BlockRqIssue,
	})
	if err != nil {
		return fmt.Errorf("attach block_rq_issue: %w", err)
	}
	defer tpIssue.Close()

	tpComplete, err := link.AttachTracing(link.TracingOptions{
		Program: objs.BlockRqComplete,
	})
	if err != nil {
		return fmt.Errorf("attach block_rq_complete: %w", err)
	}
	defer tpComplete.Close()

	// Open ring buffer (busy-poll reader -- no epoll)
	rd, err := ringpoll.NewReader(objs.Events, *pollSleep)
	if err != nil {
		return fmt.Errorf("open ring buffer: %w", err)
	}
	defer rd.Cleanup()

	state := newState()
	display := &Display{batchMode: *batch, ring: rd}

	// Signal handling
	done := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig
		signal.Stop(sig) // restore default handler so second Ctrl+C force-kills
		close(done)
		rd.Close()
	}()

	// Reader goroutine: busy-polls ring buffer, batches events, flushes under single Lock
	const flushSize = 1024
	const flushInterval = 10 * time.Millisecond

	var readerDone sync.WaitGroup
	readerDone.Add(1)
	go func() {
		defer readerDone.Done()
		var rec ringpoll.Record
		eventSize := int(unsafe.Sizeof(bpfLatencyEvent{}))
		pending := make([]pendingEvent, 0, flushSize)
		lastFlush := time.Now()

		for rd.ReadInto(&rec) {
			if len(rec.RawSample) < eventSize {
				continue
			}
			event := *(*bpfLatencyEvent)(unsafe.Pointer(&rec.RawSample[0]))
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
			pending = append(pending, pendingEvent{event.Dev, latencyUs})

			if len(pending) >= flushSize || time.Since(lastFlush) >= flushInterval {
				state.RecordBatch(pending)
				rd.Commit()
				pending = pending[:0]
				lastFlush = time.Now()
			}
		}
		if len(pending) > 0 {
			state.RecordBatch(pending)
			rd.Commit()
		}
	}()

	// Drop counter: read from kernel map periodically by display goroutine
	var totalDrops atomic.Uint64

	// Display goroutine (10 FPS)
	displayTicker := time.NewTicker(displayInterval)
	go func() {
		defer displayTicker.Stop()
		for {
			select {
			case <-done:
				return
			case <-displayTicker.C:
				readDropCount(objs.DropCount, &totalDrops)
				display.render(state, *interval, totalDrops.Load())
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

	log.Printf("Tracing block I/O latency (interval=%v, display=10fps, poll-sleep=%v)...", *interval, *pollSleep)

	// Wait for signal, then drain
	<-done
	readerDone.Wait()

	readDropCount(objs.DropCount, &totalDrops)
	display.render(state, *interval, totalDrops.Load())
	return nil
}

func readDropCount(m *ebpf.Map, dst *atomic.Uint64) {
	if m == nil {
		return
	}
	var key uint32
	var val uint64
	if err := m.Lookup(key, &val); err == nil {
		dst.Store(val)
	}
}
