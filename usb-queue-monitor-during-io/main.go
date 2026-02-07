// usb-queue-monitor-during-io: eBPF-based queue depth monitor
//
// Complements usb-queue-monitor-v2 (sysfs polling / system capacity view)
// with an event-driven "during-IO" view: each sample is a real block_rq_issue
// event, recording the device queue depth at the moment new I/O enters the device.
//
// Uses tp_btf/block_rq_issue (increment + emit) and tp_btf/block_rq_complete
// (decrement only) to maintain per-device inflight counters in eBPF.
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -type queue_event bpf bpf/queue_depth.c -- -I/usr/include -I.

package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

const (
	displayInterval = 50 * time.Millisecond // ~20 FPS display refresh
	maxQueuePerDev  = 30
	usbDeviceCount  = 5
	maxQueueUSBAggr = maxQueuePerDev * usbDeviceCount // 150 total
)

// Device groups: SSD/NVMe first, then USB drives
var deviceList = []string{"sda", "nvme0n1", "nvme1n1", "", "sdc", "sdd", "sde", "sdf", "sdg"}
var usbDevices = []string{"sdc", "sdd", "sde", "sdf", "sdg"}

// Configurable percentiles to display (P0 replaced by Util column)
var defaultPercentiles = []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 95, 99, 99.5, 99.9, 99.95, 99.99, 99.995, 99.999, 100}

// ---------- Histogram (exact 256-bucket, from usb-queue-monitor-v2) ----------

// Histogram maintains exact counts for queue depth values 0-255
// Memory: 256 x 8 bytes = 2KB per device
type Histogram struct {
	buckets [256]uint64
	total   uint64
	sum     uint64
	nonZero uint64
	max     int64
}

func NewHistogram() *Histogram {
	return &Histogram{}
}

func (h *Histogram) Add(value int) {
	h.total++
	h.sum += uint64(value)
	if value > 0 {
		h.nonZero++
	}
	if int64(value) > h.max {
		h.max = int64(value)
	}
	if value > 255 {
		value = 255
	}
	h.buckets[value]++
}

func (h *Histogram) GetTotal() uint64   { return h.total }
func (h *Histogram) GetMax() int64      { return h.max }

func (h *Histogram) GetAverage() float64 {
	if h.total == 0 {
		return 0.0
	}
	return float64(h.sum) / float64(h.total)
}

func (h *Histogram) GetUtilization() float64 {
	if h.total == 0 {
		return 0.0
	}
	return float64(h.nonZero) / float64(h.total) * 100.0
}

func (h *Histogram) Percentile(p float64) float64 {
	if h.total == 0 {
		return 0.0
	}
	pos := float64(h.total-1) * p / 100.0
	targetLower := uint64(pos)
	targetUpper := targetLower + 1
	weight := pos - float64(targetLower)

	var cumulative uint64
	var lowerValue, upperValue int
	foundLower := false

	for i := 0; i < 256; i++ {
		cumulative += h.buckets[i]
		if !foundLower && cumulative > targetLower {
			lowerValue = i
			foundLower = true
		}
		if cumulative > targetUpper {
			upperValue = i
			return float64(lowerValue)*(1-weight) + float64(upperValue)*weight
		}
		if foundLower && cumulative > targetUpper {
			return float64(lowerValue)
		}
	}
	return float64(lowerValue)
}

// ---------- Device helpers ----------

func getDeviceSize(device string) string {
	data, err := os.ReadFile(fmt.Sprintf("/sys/block/%s/size", device))
	if err != nil {
		return "?T"
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return "?T"
	}
	sizeBytes := sectors * 512
	tb := float64(sizeBytes) / (1024 * 1024 * 1024 * 1024)
	if tb >= 1.0 {
		return fmt.Sprintf("%.0fT", tb)
	}
	gb := float64(sizeBytes) / (1024 * 1024 * 1024)
	return fmt.Sprintf("%.0fG", gb)
}

func devToMajorMinor(dev uint32) (uint32, uint32) {
	return dev >> 20, dev & 0xFFFFF
}

func majorMinorToDev(major, minor uint32) uint32 {
	return (major << 20) | minor
}

// Device names cache
var (
	devNames   = make(map[uint32]string)
	devNamesMu sync.RWMutex
)

func lookupDevName(dev uint32) string {
	devNamesMu.RLock()
	if name, ok := devNames[dev]; ok {
		devNamesMu.RUnlock()
		return name
	}
	devNamesMu.RUnlock()

	major, minor := devToMajorMinor(dev)
	name := fmt.Sprintf("%d:%d", major, minor)

	sysPath := fmt.Sprintf("/sys/dev/block/%d:%d/device/../block", major, minor)
	if entries, err := os.ReadDir(sysPath); err == nil && len(entries) > 0 {
		name = entries[0].Name()
	} else {
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

// ---------- Formatting helpers ----------

func formatCount(count uint64) string {
	if count >= 1_000_000_000 {
		return fmt.Sprintf("%.1fB", float64(count)/1_000_000_000)
	} else if count >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(count)/1_000_000)
	} else if count >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(count)/1_000)
	}
	return fmt.Sprintf("%d", count)
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	} else if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm%ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh%dm", h, m)
}

func formatPercentileHeader(pct float64) string {
	if pct == float64(int(pct)) {
		return fmt.Sprintf("P%d", int(pct))
	}
	return fmt.Sprintf("P%g", pct)
}

func calcPercentiles(h *Histogram) []float64 {
	results := make([]float64, len(defaultPercentiles))
	for i, pct := range defaultPercentiles {
		results[i] = h.Percentile(pct)
	}
	return results
}

func findP50Index() int {
	for i, pct := range defaultPercentiles {
		if pct == 50 {
			return i
		}
	}
	return -1
}

// ---------- Bar rendering ----------

func makeBar(current, p90, width int) string {
	var bar strings.Builder
	for i := 1; i <= width; i++ {
		if i <= current {
			bar.WriteString("\xe2\x96\x88") // full block
		} else if i <= p90 {
			bar.WriteString("\xe2\x96\x91") // light shade
		} else {
			bar.WriteString("-")
		}
	}
	return bar.String()
}

func renderBucketBar(pct float64, barWidth int) string {
	var fillRatio float64
	if pct <= 0 {
		fillRatio = 0
	} else {
		fillRatio = (math.Log10(pct) + 3) / 5
		if fillRatio < 0 {
			fillRatio = 0
		}
		if fillRatio > 1 {
			fillRatio = 1
		}
	}
	filled := int(fillRatio*float64(barWidth) + 0.5)
	var bar strings.Builder
	for i := 0; i < barWidth; i++ {
		if i < filled {
			bar.WriteString("\xe2\x96\x91") // light shade
		} else {
			bar.WriteString("-")
		}
	}
	return bar.String()
}

const maxHistoRows = 25

func getTopDepths(h *Histogram, maxRows int) []int {
	if h == nil || h.total == 0 {
		return nil
	}
	type dc struct {
		depth int
		count uint64
	}
	var nonEmpty []dc
	for i := 0; i < 256; i++ {
		if h.buckets[i] > 0 {
			nonEmpty = append(nonEmpty, dc{i, h.buckets[i]})
		}
	}
	if len(nonEmpty) == 0 {
		return nil
	}
	if len(nonEmpty) <= maxRows {
		result := make([]int, len(nonEmpty))
		for i, d := range nonEmpty {
			result[i] = d.depth
		}
		return result
	}
	selected := make(map[int]bool)
	for i := 0; i < maxRows; i++ {
		maxIdx := -1
		var maxCount uint64
		for j, d := range nonEmpty {
			if !selected[d.depth] && d.count > maxCount {
				maxCount = d.count
				maxIdx = j
			}
		}
		if maxIdx >= 0 {
			selected[nonEmpty[maxIdx].depth] = true
		}
	}
	var result []int
	for depth := 0; depth < 256; depth++ {
		if selected[depth] {
			result = append(result, depth)
		}
	}
	return result
}

func renderAllHistograms(buf *strings.Builder, histograms map[string]*Histogram, usbAggregate *Histogram, barWidth int) {
	devOrder := []string{"sda", "nvme0n1", "nvme1n1", "sdc", "sdd", "sde", "sdf", "sdg"}
	shortNames := []string{"sda", "nvme0", "nvme1", "sdc", "sdd", "sde", "sdf", "sdg", "USB"}

	allDepths := make([][]int, len(devOrder)+1)
	for i, dev := range devOrder {
		allDepths[i] = getTopDepths(histograms[dev], maxHistoRows)
	}
	allDepths[len(devOrder)] = getTopDepths(usbAggregate, maxHistoRows)

	maxRows := 0
	for _, depths := range allDepths {
		if len(depths) > maxRows {
			maxRows = len(depths)
		}
	}
	if maxRows == 0 {
		return
	}

	colWidth := 4 + barWidth + 2

	for _, name := range shortNames {
		fmt.Fprintf(buf, "%-*s", colWidth, name)
	}
	buf.WriteString("\n")

	for row := 0; row < maxRows; row++ {
		for i := range allDepths {
			if row < len(allDepths[i]) {
				depth := allDepths[i][row]
				var h *Histogram
				if i < len(devOrder) {
					h = histograms[devOrder[i]]
				} else {
					h = usbAggregate
				}
				pct := 0.0
				if h != nil && h.total > 0 {
					pct = float64(h.buckets[depth]) / float64(h.total) * 100.0
				}
				bar := renderBucketBar(pct, barWidth)
				cell := fmt.Sprintf("%3d:%s", depth, bar)
				fmt.Fprintf(buf, "%-*s", colWidth, cell)
			} else {
				fmt.Fprintf(buf, "%-*s", colWidth, "")
			}
		}
		buf.WriteString("\n")
	}
}

// ---------- pendingEvent for batch processing ----------

type pendingEvent struct {
	devName string
	depth   int
}

// ---------- State (shared between reader and display goroutines) ----------

type State struct {
	mu           sync.Mutex
	histograms   map[string]*Histogram
	usbAggregate *Histogram
}

func newState() *State {
	hists := make(map[string]*Histogram)
	for _, dev := range deviceList {
		if dev == "" {
			continue
		}
		hists[dev] = NewHistogram()
	}
	return &State{
		histograms:   hists,
		usbAggregate: NewHistogram(),
	}
}

func (s *State) RecordBatch(batch []pendingEvent) {
	s.mu.Lock()
	for i := range batch {
		e := &batch[i]
		h, ok := s.histograms[e.devName]
		if !ok {
			h = NewHistogram()
			s.histograms[e.devName] = h
		}
		h.Add(e.depth)

		// Update USB aggregate if this is a USB device
		for _, usbDev := range usbDevices {
			if e.devName == usbDev {
				s.usbAggregate.Add(e.depth)
				break
			}
		}
	}
	s.mu.Unlock()
}

// ---------- Current depth tracking (atomics for lock-free display) ----------

type CurrentDepths struct {
	values      map[string]*atomic.Int32
	usbAggrCurr atomic.Int32
}

func newCurrentDepths() *CurrentDepths {
	cd := &CurrentDepths{
		values: make(map[string]*atomic.Int32),
	}
	for _, dev := range deviceList {
		if dev == "" {
			continue
		}
		cd.values[dev] = &atomic.Int32{}
	}
	return cd
}

// ---------- Display ----------

type Display struct {
	batchMode    bool
	p50Index     int
	deviceSizes  map[string]string
	startTime    time.Time
	displayCount uint64
	ring         *RingPollReader
}

func (d *Display) resetCursor() {
	if !d.batchMode {
		fmt.Print("\033[H\033[J")
	}
}

func (d *Display) render(state *State, currents map[string]int, usbAggrCurrent int, totalEvents uint64, totalDrops uint64) {
	d.displayCount++
	var buf strings.Builder

	timestamp := time.Now().Format("Mon Jan 02 15:04:05 2006")

	if d.batchMode {
		fmt.Fprintf(&buf, "[%s] Block I/O Queue Monitor (during-io)\n", timestamp)
	} else {
		fmt.Fprintf(&buf, "Block I/O Queue Monitor (during-io) - %s\n", timestamp)
	}

	lineWidth := 8 + 9 + 9 + len(defaultPercentiles)*9 + 9 + 12 + 2 + maxQueuePerDev + 2 + 10 + 5
	buf.WriteString(strings.Repeat("=", lineWidth))
	buf.WriteString("\n")
	fmt.Fprintf(&buf, "%-8s %8s %8s", "Device", "Current", "Util")
	for i, pct := range defaultPercentiles {
		fmt.Fprintf(&buf, " %8s", formatPercentileHeader(pct))
		if i == d.p50Index {
			fmt.Fprintf(&buf, " %8s", "Avg")
		}
	}
	fmt.Fprintf(&buf, "  %-11s  %-32s  %-8s%4s\n", "Device", "        Utilization", "Device", "Avg")
	buf.WriteString(strings.Repeat("-", lineWidth))
	buf.WriteString("\n")

	// Hold lock while reading histograms and formatting
	state.mu.Lock()

	for _, dev := range deviceList {
		if dev == "" {
			buf.WriteString("\n")
			continue
		}

		hist := state.histograms[dev]
		if hist == nil {
			hist = NewHistogram()
		}
		current := currents[dev]
		pcts := calcPercentiles(hist)
		pcts[len(pcts)-1] = float64(hist.GetMax())
		avg := hist.GetAverage()
		util := hist.GetUtilization()

		p99Int := 0
		for i, pct := range defaultPercentiles {
			if pct == 99 {
				p99Int = int(pcts[i] + 0.5)
				break
			}
		}
		bar := makeBar(current, p99Int, maxQueuePerDev)
		fmt.Fprintf(&buf, "%-8s %4d/%-3d %7.1f%%", dev, current, maxQueuePerDev, util)
		for i, val := range pcts {
			fmt.Fprintf(&buf, " %8.2f", val)
			if i == d.p50Index {
				fmt.Fprintf(&buf, " %8.2f", avg)
			}
		}
		devWithSize := fmt.Sprintf("%s(%s)", dev, d.deviceSizes[dev])
		fmt.Fprintf(&buf, "  %-11s  [%s]  %-8s%4d\n", devWithSize, bar, dev, int(avg+0.5))

		// After last USB device, show aggregate USB stats
		if dev == "sdg" {
			aggrPcts := calcPercentiles(state.usbAggregate)
			aggrPcts[len(aggrPcts)-1] = float64(state.usbAggregate.GetMax())
			aggrAvg := state.usbAggregate.GetAverage()
			aggrUtil := state.usbAggregate.GetUtilization()

			fmt.Fprintf(&buf, "%-8s %4d/%-3d %7.1f%%", "USB", usbAggrCurrent, maxQueueUSBAggr, aggrUtil)
			for i, val := range aggrPcts {
				fmt.Fprintf(&buf, " %8.2f", val)
				if i == d.p50Index {
					fmt.Fprintf(&buf, " %8.2f", aggrAvg)
				}
			}
			scaledCurrent := int(float64(usbAggrCurrent) / float64(maxQueueUSBAggr) * float64(maxQueuePerDev) + 0.5)
			aggrP99 := 0.0
			for i, pct := range defaultPercentiles {
				if pct == 99 {
					aggrP99 = aggrPcts[i]
					break
				}
			}
			scaledP99 := int(aggrP99 / float64(maxQueueUSBAggr) * float64(maxQueuePerDev) + 0.5)
			aggrBar := makeBar(scaledCurrent, scaledP99, maxQueuePerDev)
			scaledAvg := int(aggrAvg / float64(maxQueueUSBAggr) * float64(maxQueuePerDev) + 0.5)
			fmt.Fprintf(&buf, "  %-11s  [%s]  %-8s%4d\n", "", aggrBar, "USB", scaledAvg)
		}
	}

	buf.WriteString("\n")
	if d.batchMode {
		buf.WriteString("Legend: \xe2\x96\x88 = current  \xe2\x96\x91 = p99 (long-term)  - = unused\n")
	} else {
		buf.WriteString("Legend: \xe2\x96\x88= current  \xe2\x96\x91= p99 (long-term)  -= unused\n")
	}

	// Histogram distribution display
	buf.WriteString("\n")
	renderAllHistograms(&buf, state.histograms, state.usbAggregate, 5)
	buf.WriteString("Log scale: \xe2\x96\x91\xe2\x96\x91\xe2\x96\x91\xe2\x96\x91\xe2\x96\x91=100%  \xe2\x96\x91\xe2\x96\x91\xe2\x96\x91--=1%  \xe2\x96\x91\xe2\x96\x91---=0.1%  \xe2\x96\x91----=0.01%\n")

	state.mu.Unlock()

	// Footer: event stats + ring buffer stats (lock-free)
	elapsed := time.Since(d.startTime)
	elapsedSec := elapsed.Seconds()
	eventRate := 0.0
	displayRate := 0.0
	dropRate := 0.0
	if elapsedSec > 0 {
		eventRate = float64(totalEvents) / elapsedSec
		displayRate = float64(d.displayCount) / elapsedSec
		dropRate = float64(totalDrops) / elapsedSec
	}
	fmt.Fprintf(&buf, "Events: %s in %s  |  Rate: %s/s  |  Display: %.1f FPS  |  Drops: %s (%s/s)\n",
		formatCount(totalEvents), formatDuration(elapsed), formatCount(uint64(eventRate)),
		displayRate, formatCount(totalDrops), formatCount(uint64(dropRate)))

	// Ring buffer poll stats
	if d.ring != nil {
		avg1, avg0, last1, last0 := d.ring.PollStats()
		maxPend := d.ring.MaxPending()
		bufSize := d.ring.BufSize()
		fmt.Fprintf(&buf, "Ring: avg1=%.1f avg0=%.1f last1=%d last0=%s max=%s/%s\n",
			avg1, avg0, last1, formatRingDuration(last0),
			formatBytes(maxPend), formatBytes(int64(bufSize)))
	}

	if d.batchMode {
		buf.WriteString("\n")
	}

	d.resetCursor()
	fmt.Print(buf.String())
}

func formatRingDuration(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	if d < time.Millisecond {
		return fmt.Sprintf("%dus", d.Microseconds())
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func formatBytes(b int64) string {
	if b >= 1024*1024 {
		return fmt.Sprintf("%.1fM", float64(b)/(1024*1024))
	}
	if b >= 1024 {
		return fmt.Sprintf("%.1fK", float64(b)/1024)
	}
	return fmt.Sprintf("%d", b)
}

// ---------- eBPF helpers ----------

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

// readCurrentDepths reads the eBPF queue_depth map for accurate current values
func readCurrentDepths(m *ebpf.Map, currents *CurrentDepths) {
	if m == nil {
		return
	}
	var key uint32
	var val int64
	iter := m.Iterate()
	usbSum := int32(0)
	for iter.Next(&key, &val) {
		devName := lookupDevName(key)
		if !isTrackedDevice(devName) {
			continue
		}
		depth := int32(val)
		if depth < 0 {
			depth = 0
		}
		if a, ok := currents.values[devName]; ok {
			a.Store(depth)
		}
		for _, usbDev := range usbDevices {
			if devName == usbDev {
				usbSum += depth
				break
			}
		}
	}
	currents.usbAggrCurr.Store(usbSum)
}

// ---------- main ----------

func main() {
	batchFlag := flag.Bool("batch", false, "batch mode (no screen clearing)")
	devFilter := flag.String("d", "", "comma-separated device filter (e.g., sdc,sdd or 8:32,8:48)")
	pollSleep := flag.Duration("poll-sleep", 50*time.Microsecond, "busy-poll sleep when ring empty")
	flag.Parse()

	// Parse device filter
	filterDevs, err := parseDeviceFilter(*devFilter)
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
		if err := objs.QueueConfig.Put(key, enabled); err != nil {
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

	// Open ring buffer (busy-poll reader)
	rd, err := NewRingPollReader(objs.Events, *pollSleep)
	if err != nil {
		log.Fatalf("Failed to open ring buffer: %v", err)
	}
	defer rd.Cleanup()

	p50Index := findP50Index()
	if p50Index == -1 {
		log.Fatal("P50 must be present in percentiles array")
	}

	// Get device sizes at startup
	deviceSizes := make(map[string]string)
	for _, dev := range deviceList {
		if dev == "" {
			continue
		}
		deviceSizes[dev] = getDeviceSize(dev)
	}

	state := newState()
	currents := newCurrentDepths()
	display := &Display{
		batchMode:   *batchFlag,
		p50Index:    p50Index,
		deviceSizes: deviceSizes,
		startTime:   time.Now(),
		ring:        rd,
	}

	// Signal handling
	done := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig
		signal.Stop(sig)
		close(done)
		rd.Close()
	}()

	// Reader goroutine: busy-polls ring buffer, batches events, flushes under single Lock
	const flushSize = 1024
	const flushInterval = 10 * time.Millisecond

	var totalEvents atomic.Uint64
	var readerDone sync.WaitGroup
	readerDone.Add(1)
	go func() {
		defer readerDone.Done()
		var rec PollRecord
		eventSize := int(unsafe.Sizeof(bpfQueueEvent{}))
		pending := make([]pendingEvent, 0, flushSize)
		lastFlush := time.Now()

		for rd.ReadInto(&rec) {
			if len(rec.RawSample) < eventSize {
				continue
			}
			event := *(*bpfQueueEvent)(unsafe.Pointer(&rec.RawSample[0]))
			devName := lookupDevName(event.Dev)
			if !isTrackedDevice(devName) {
				continue
			}
			pending = append(pending, pendingEvent{devName, int(event.Depth)})

			if len(pending) >= flushSize || time.Since(lastFlush) >= flushInterval {
				state.RecordBatch(pending)
				totalEvents.Add(uint64(len(pending)))
				rd.Commit()
				pending = pending[:0]
				lastFlush = time.Now()
			}
		}
		if len(pending) > 0 {
			state.RecordBatch(pending)
			totalEvents.Add(uint64(len(pending)))
			rd.Commit()
		}
	}()

	// Drop counter
	var totalDrops atomic.Uint64

	// Display goroutine (20 FPS)
	displayTicker := time.NewTicker(displayInterval)
	go func() {
		defer displayTicker.Stop()
		for {
			select {
			case <-done:
				return
			case <-displayTicker.C:
				readDropCount(objs.DropCount, &totalDrops)
				readCurrentDepths(objs.QueueDepth, currents)

				currentMap := make(map[string]int)
				for _, dev := range deviceList {
					if dev == "" {
						continue
					}
					currentMap[dev] = int(currents.values[dev].Load())
				}
				usbAggrCurrent := int(currents.usbAggrCurr.Load())

				display.render(state, currentMap, usbAggrCurrent, totalEvents.Load(), totalDrops.Load())
			}
		}
	}()

	log.Printf("Tracing block I/O queue depth (during-io, poll-sleep=%v)...", *pollSleep)

	// Wait for signal, then drain
	<-done
	readerDone.Wait()

	readDropCount(objs.DropCount, &totalDrops)
	currentMap := make(map[string]int)
	for _, dev := range deviceList {
		if dev == "" {
			continue
		}
		currentMap[dev] = 0
	}
	display.render(state, currentMap, 0, totalEvents.Load(), totalDrops.Load())
}
