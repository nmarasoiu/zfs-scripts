package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	displayInterval = 50 * time.Millisecond // ~20 FPS display refresh
	maxQueuePerDev  = 30
	usbDeviceCount  = 5
	maxQueueUSBAggr = maxQueuePerDev * usbDeviceCount // 150 total
)

// Device groups: SSD/NVMe first, then USB drives
var devices = []string{"sda", "nvme0n1", "nvme1n1", "", "sdc", "sdd", "sde", "sdf", "sdg"}

// Configurable percentiles to display (P0 replaced by Util column)
var percentiles = []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 95, 99, 99.5, 99.9, 99.95, 99.99, 99.995, 99.999, 100}

// Histogram maintains exact counts for queue depth values 0-255
// Memory: 256 × 8 bytes = 2KB per device (vs 10MB reservoir)
type Histogram struct {
	buckets [256]uint64 // count of samples at each queue depth
	total   uint64      // total samples seen
	sum     uint64      // running sum for average
	nonZero uint64      // count of samples where value > 0
	max     int64       // true maximum ever seen (unbounded, for display)
}

// NewHistogram creates a new histogram (zero-initialized by Go)
func NewHistogram() *Histogram {
	return &Histogram{}
}

// Add records a queue depth sample
func (h *Histogram) Add(value int) {
	h.total++
	h.sum += uint64(value)
	if value > 0 {
		h.nonZero++
	}
	if int64(value) > h.max {
		h.max = int64(value)
	}
	// Clamp for histogram bucket (values >255 are ultra-rare on NVMe)
	if value > 255 {
		value = 255
	}
	h.buckets[value]++
}

// GetTotal returns the total number of samples seen
func (h *Histogram) GetTotal() uint64 {
	return h.total
}

// GetAverage returns the true running average
func (h *Histogram) GetAverage() float64 {
	if h.total == 0 {
		return 0.0
	}
	return float64(h.sum) / float64(h.total)
}

// GetUtilization returns the percentage of samples where value > 0
func (h *Histogram) GetUtilization() float64 {
	if h.total == 0 {
		return 0.0
	}
	return float64(h.nonZero) / float64(h.total) * 100.0
}

// GetMax returns the true maximum ever seen
func (h *Histogram) GetMax() int64 {
	return h.max
}

// Percentile calculates the percentile value (0-100) with linear interpolation
func (h *Histogram) Percentile(p float64) float64 {
	if h.total == 0 {
		return 0.0
	}

	// Target position in the sorted sequence (0-indexed, fractional)
	pos := float64(h.total-1) * p / 100.0
	targetLower := uint64(pos)
	targetUpper := targetLower + 1
	weight := pos - float64(targetLower)

	// Walk buckets to find values at targetLower and targetUpper positions
	var cumulative uint64
	var lowerValue, upperValue int
	foundLower := false

	for i := 0; i < 256; i++ {
		prevCumulative := cumulative
		cumulative += h.buckets[i]

		if !foundLower && cumulative > targetLower {
			lowerValue = i
			foundLower = true
		}
		if cumulative > targetUpper {
			upperValue = i
			// Interpolate between lower and upper values
			return float64(lowerValue)*(1-weight) + float64(upperValue)*weight
		}
		// Handle case where targetLower and targetUpper are in same bucket
		if foundLower && cumulative > targetUpper {
			return float64(lowerValue)
		}
		_ = prevCumulative // silence unused warning
	}

	return float64(lowerValue)
}

// Snapshot returns a copy of the histogram for lock-free display
func (h *Histogram) Snapshot() *Histogram {
	snap := &Histogram{
		total:   h.total,
		sum:     h.sum,
		nonZero: h.nonZero,
		max:     h.max,
	}
	copy(snap.buckets[:], h.buckets[:])
	return snap
}

// getDeviceSize reads the device size and returns it as a human-readable string (e.g., "4TB")
func getDeviceSize(device string) string {
	data, err := os.ReadFile(fmt.Sprintf("/sys/block/%s/size", device))
	if err != nil {
		return "?TB"
	}

	sectors, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return "?TB"
	}

	// Each sector is 512 bytes
	bytes := sectors * 512
	tb := float64(bytes) / (1024 * 1024 * 1024 * 1024)

	if tb >= 1.0 {
		return fmt.Sprintf("%.0fT", tb)
	}
	gb := float64(bytes) / (1024 * 1024 * 1024)
	return fmt.Sprintf("%.0fG", gb)
}

// InflightReader holds an open file handle for fast repeated reads
type InflightReader struct {
	file *os.File
	buf  []byte
}

// NewInflightReader opens a persistent file handle for the device's inflight file
func NewInflightReader(device string) (*InflightReader, error) {
	path := fmt.Sprintf("/sys/block/%s/inflight", device)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &InflightReader{
		file: f,
		buf:  make([]byte, 64), // inflight file is small: "0 0\n" typically
	}, nil
}

// Read uses pread to read from offset 0 in a single syscall (avoids seek+read overhead)
func (ir *InflightReader) Read() (int, error) {
	// Single syscall: pread(fd, buf, len, offset=0) instead of lseek + read
	n, err := syscall.Pread(int(ir.file.Fd()), ir.buf, 0)
	if err != nil {
		return 0, err
	}

	// Zero-allocation parse of "read write\n" format
	// Hand-rolled ASCII→int for hot path (avoids strings.Fields + strconv.Atoi allocations)
	read, write := 0, 0
	i := 0

	// Parse first number (read count)
	for i < n && ir.buf[i] >= '0' && ir.buf[i] <= '9' {
		read = read*10 + int(ir.buf[i]-'0')
		i++
	}

	// Skip whitespace
	for i < n && (ir.buf[i] == ' ' || ir.buf[i] == '\t') {
		i++
	}

	// Parse second number (write count)
	for i < n && ir.buf[i] >= '0' && ir.buf[i] <= '9' {
		write = write*10 + int(ir.buf[i]-'0')
		i++
	}

	return read + write, nil
}

// Close closes the file handle
func (ir *InflightReader) Close() error {
	return ir.file.Close()
}

// getInflight reads the current in-flight IO count for a device (legacy, used at startup)
func getInflight(device string) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/sys/block/%s/inflight", device))
	if err != nil {
		return 0, err
	}

	parts := strings.Fields(strings.TrimSpace(string(data)))
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid inflight format for %s", device)
	}

	read, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}

	write, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, err
	}

	return read + write, nil
}

// calcPercentiles calculates all configured percentiles from a histogram
func calcPercentiles(h *Histogram) []float64 {
	results := make([]float64, len(percentiles))
	for i, pct := range percentiles {
		results[i] = h.Percentile(pct)
	}
	return results
}

// findP50Index returns the index of P50 in the percentiles array
func findP50Index() int {
	for i, pct := range percentiles {
		if pct == 50 {
			return i
		}
	}
	return -1
}

// makeBar creates a visualization bar
func makeBar(current, p90, width int) string {
	var bar strings.Builder
	for i := 1; i <= width; i++ {
		if i <= current {
			bar.WriteString("█")
		} else if i <= p90 {
			bar.WriteString("░")
		} else {
			bar.WriteString("-")
		}
	}
	return bar.String()
}

// formatCount formats large numbers in human-readable format
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

// Display renders the current state
type Display struct {
	batchMode       bool
	p50Index        int
	deviceSizes     map[string]string
	lastSampleCount uint64
	lastTime        time.Time
	samplesPerSec   float64
}

func (d *Display) resetCursor() {
	if !d.batchMode {
		// Move cursor to home position and clear screen (less flicker than exec clear)
		fmt.Print("\033[H\033[J")
	}
}

// formatPercentileHeader returns the header label for a percentile
func formatPercentileHeader(pct float64) string {
	if pct == float64(int(pct)) {
		return fmt.Sprintf("P%d", int(pct))
	}
	// Use %g to preserve precision without trailing zeros
	return fmt.Sprintf("P%g", pct)
}

var usbDevices = []string{"sdc", "sdd", "sde", "sdf", "sdg"}

func (d *Display) render(histograms map[string]*Histogram, usbAggregate *Histogram, currents map[string]int, usbAggrCurrent int, totalSamples uint64) {
	// Calculate samples/sec
	now := time.Now()
	if !d.lastTime.IsZero() {
		elapsed := now.Sub(d.lastTime).Seconds()
		if elapsed > 0 {
			d.samplesPerSec = float64(totalSamples-d.lastSampleCount) / elapsed
		}
	}
	d.lastSampleCount = totalSamples
	d.lastTime = now

	var buf strings.Builder

	timestamp := time.Now().Format("Mon Jan 02 15:04:05 2006")

	if d.batchMode {
		fmt.Fprintf(&buf, "[%s] Block I/O Queue Monitor\n", timestamp)
	} else {
		fmt.Fprintf(&buf, "Block I/O Queue Monitor - %s\n", timestamp)
	}

	// Build dynamic header
	// 8=Device, 9=Current, 9=Util, percentiles*9, 9=Avg, 12=Device(size), 2+30+2=bar, 10=Device, 5=Avg
	lineWidth := 8 + 9 + 9 + len(percentiles)*9 + 9 + 12 + 2 + maxQueuePerDev + 2 + 10 + 5
	buf.WriteString(strings.Repeat("=", lineWidth))
	buf.WriteString("\n")
	fmt.Fprintf(&buf, "%-8s %8s %8s", "Device", "Current", "Util")
	for i, pct := range percentiles {
		fmt.Fprintf(&buf, " %8s", formatPercentileHeader(pct))
		if i == d.p50Index {
			fmt.Fprintf(&buf, " %8s", "Avg")
		}
	}
	fmt.Fprintf(&buf, "  %-11s  %-32s  %-8s%4s\n", "Device", "        Utilization", "Device", "Avg")
	buf.WriteString(strings.Repeat("-", lineWidth))
	buf.WriteString("\n")

	for _, dev := range devices {
		// Empty string means separator line
		if dev == "" {
			buf.WriteString("\n")
			continue
		}

		hist := histograms[dev]
		current := currents[dev]
		pcts := calcPercentiles(hist)
		// Use true max for P100 (last percentile)
		pcts[len(pcts)-1] = float64(hist.GetMax())
		avg := hist.GetAverage()
		util := hist.GetUtilization()

		// Find P99 for bar display
		p99Int := 0
		for i, pct := range percentiles {
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
			aggrPcts := calcPercentiles(usbAggregate)
			// Use true max for P100 (last percentile)
			aggrPcts[len(aggrPcts)-1] = float64(usbAggregate.GetMax())
			aggrAvg := usbAggregate.GetAverage()
			aggrUtil := usbAggregate.GetUtilization()

			fmt.Fprintf(&buf, "%-8s %4d/%-3d %7.1f%%", "USB", usbAggrCurrent, maxQueueUSBAggr, aggrUtil)
			for i, val := range aggrPcts {
				fmt.Fprintf(&buf, " %8.2f", val)
				if i == d.p50Index {
					fmt.Fprintf(&buf, " %8.2f", aggrAvg)
				}
			}
			// Scaled utilization bar: scale from 0-150 to 0-30 for display
			scaledCurrent := int(float64(usbAggrCurrent) / float64(maxQueueUSBAggr) * float64(maxQueuePerDev) + 0.5)
			aggrP99 := 0.0
			for i, pct := range percentiles {
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
		buf.WriteString("Legend: █ = current  ░ = p99 (long-term)  - = unused\n")
	} else {
		buf.WriteString("Legend: █= current  ░= p99 (long-term)  -= unused\n")
	}

	fmt.Fprintf(&buf, "Samples: %s total @ %.0f/sec\n", formatCount(totalSamples), d.samplesPerSec)

	if d.batchMode {
		buf.WriteString("\n")
	}

	// Write entire buffer at once (reduces flicker)
	d.resetCursor()
	fmt.Print(buf.String())
}

// SamplerState holds the shared state between sampler and display goroutines
type SamplerState struct {
	mu           sync.RWMutex
	histograms   map[string]*Histogram
	usbAggregate *Histogram
}

// Atomics for current values (lock-free access for display)
type CurrentValues struct {
	values      map[string]*atomic.Int32
	usbAggrCurr atomic.Int32
}

func main() {
	batchMode := flag.Bool("batch", false, "Enable batch mode (no screen clearing, suitable for nohup)")
	flag.Parse()

	// Setup logging
	if *batchMode {
		log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
		log.Println("Block I/O Queue Monitor starting in batch mode")
	}

	// Initialize histograms for each device (2KB each vs 10MB reservoir)
	histograms := make(map[string]*Histogram)
	for _, dev := range devices {
		if dev == "" {
			continue
		}
		histograms[dev] = NewHistogram()
	}

	p50Index := findP50Index()
	if p50Index == -1 {
		log.Fatal("P50 must be present in percentiles array")
	}

	// Get device sizes at startup
	deviceSizes := make(map[string]string)
	for _, dev := range devices {
		if dev == "" {
			continue
		}
		deviceSizes[dev] = getDeviceSize(dev)
	}

	// Create aggregate histogram for combined USB queue depth
	usbAggregate := NewHistogram()

	// Initialize atomic current values
	currents := &CurrentValues{
		values: make(map[string]*atomic.Int32),
	}
	for _, dev := range devices {
		if dev == "" {
			continue
		}
		currents.values[dev] = &atomic.Int32{}
	}

	// Open persistent file handles for fast sysfs reads (seek+read vs open/read/close)
	readers := make(map[string]*InflightReader)
	for _, dev := range devices {
		if dev == "" {
			continue
		}
		reader, err := NewInflightReader(dev)
		if err != nil {
			log.Printf("Warning: cannot open inflight file for %s: %v", dev, err)
			continue
		}
		readers[dev] = reader
	}

	// Shared state protected by RWMutex for histogram data
	state := &SamplerState{
		histograms:   histograms,
		usbAggregate: usbAggregate,
	}

	display := &Display{
		batchMode:   *batchMode,
		p50Index:    p50Index,
		deviceSizes: deviceSizes,
	}

	// Setup signal handling for clean shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Channel to signal shutdown to goroutines
	done := make(chan struct{})

	// Initial message
	if !*batchMode {
		fmt.Println("Block I/O Queue Monitor - Ctrl+C to stop")
		fmt.Println("Starting sampler (dedicated CPU) and display (20 FPS)...")
	} else {
		log.Println("Starting sampler (dedicated CPU) and display...")
	}

	// SAMPLER GOROUTINE - runs flat out, no sleep, hogs one CPU core
	var sampleCount atomic.Uint64
	go func() {
		// Pre-allocate slice for batch updates
		batch := make(map[string]int, len(devices))

		for {
			select {
			case <-done:
				return
			default:
				// Phase 1: Read all sysfs values via persistent handles (no locks, just I/O)
				usbSum := int32(0)
				for _, dev := range devices {
					if dev == "" {
						continue
					}
					reader := readers[dev]
					if reader == nil {
						continue // device not available
					}
					current, err := reader.Read()
					if err != nil {
						current = 0
					}
					batch[dev] = current

					// Update atomic current value (lock-free for display)
					currents.values[dev].Store(int32(current))
				}

				// Calculate USB aggregate
				for _, usbDev := range usbDevices {
					usbSum += int32(batch[usbDev])
				}
				currents.usbAggrCurr.Store(usbSum)

				// Phase 2: Batch update all histograms under single lock
				state.mu.Lock()
				for dev, current := range batch {
					state.histograms[dev].Add(current)
				}
				state.usbAggregate.Add(int(usbSum))
				state.mu.Unlock()

				sampleCount.Add(1)
			}
		}
	}()

	// DISPLAY GOROUTINE - runs at ~20 FPS (50ms)
	displayTicker := time.NewTicker(displayInterval)
	go func() {
		defer displayTicker.Stop()
		for {
			select {
			case <-done:
				return
			case <-displayTicker.C:
				// Read current values (lock-free atomics)
				currentMap := make(map[string]int)
				for _, dev := range devices {
					if dev == "" {
						continue
					}
					currentMap[dev] = int(currents.values[dev].Load())
				}
				usbAggrCurrent := int(currents.usbAggrCurr.Load())

				// Snapshot histograms under lock (fast: 2KB copy per device)
				histogramsCopy := make(map[string]*Histogram)
				state.mu.RLock()
				for dev, h := range state.histograms {
					histogramsCopy[dev] = h.Snapshot()
				}
				usbAggrCopy := state.usbAggregate.Snapshot()
				state.mu.RUnlock()

				// Render (outside of lock)
				display.render(histogramsCopy, usbAggrCopy, currentMap, usbAggrCurrent, sampleCount.Load())
			}
		}
	}()

	// Wait for shutdown signal
	<-sigChan
	close(done)

	// Close persistent file handles
	for _, reader := range readers {
		reader.Close()
	}

	if *batchMode {
		log.Printf("Shutting down... Total samples: %d", sampleCount.Load())
	} else {
		fmt.Printf("\nStopped. Total samples: %d\n", sampleCount.Load())
	}
}
