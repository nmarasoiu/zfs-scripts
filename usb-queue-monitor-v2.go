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
)

const (
	displayInterval = 50 * time.Millisecond // ~20 FPS display refresh
	reservoirSize   = 10000
	maxQueuePerDev  = 30
	usbDeviceCount  = 5
	maxQueueUSBAggr = maxQueuePerDev * usbDeviceCount // 150 total
	sampleBatchSize = 1000                            // samples before acquiring lock
)

// Device indices for slice-based access (eliminates map overhead in hot path)
const (
	devSda = iota
	devNvme0n1
	devNvme1n1
	devSep1 // separator (empty line in display)
	devSdc
	devSdd
	devSde
	devSdf
	devSdg
	numDeviceSlots
)

// Device names indexed by device constants above
var deviceNames = [numDeviceSlots]string{"sda", "nvme0n1", "nvme1n1", "", "sdc", "sdd", "sde", "sdf", "sdg"}

// USB device indices for aggregate calculation
var usbDeviceIndices = [usbDeviceCount]int{devSdc, devSdd, devSde, devSdf, devSdg}


// Configurable percentiles to display (P0 replaced by Util column)
var percentiles = []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 95, 99, 99.5, 99.9, 99.95, 99.99, 99.995, 99.999, 100}

// ReservoirSampler maintains a fixed-size representative sample using reservoir sampling
type ReservoirSampler struct {
	reservoir []int
	count     uint64
	sum       uint64 // running sum for true average
	nonZero   uint64 // count of samples where value > 0
	max       int    // true maximum ever seen (never decreases)
	size      int
	rngState  uint64 // xorshift64 state (faster than rand.Rand)
}

// NewReservoirSampler creates a new reservoir sampler
func NewReservoirSampler(size int) *ReservoirSampler {
	return &ReservoirSampler{
		reservoir: make([]int, 0, size),
		count:     0,
		sum:       0,
		nonZero:   0,
		size:      size,
		rngState:  uint64(time.Now().UnixNano()) | 1, // ensure non-zero
	}
}

// Add adds a value to the reservoir
func (rs *ReservoirSampler) Add(value int) {
	rs.count++
	rs.sum += uint64(value)
	if value > 0 {
		rs.nonZero++
	}
	if value > rs.max {
		rs.max = value
	}
	if len(rs.reservoir) < rs.size {
		rs.reservoir = append(rs.reservoir, value)
	} else {
		// Randomly replace elements with decreasing probability
		// xorshift64: fast PRNG (~3 ops vs rand.Int63n overhead)
		rs.rngState ^= rs.rngState << 13
		rs.rngState ^= rs.rngState >> 7
		rs.rngState ^= rs.rngState << 17
		j := rs.rngState % rs.count
		if j < uint64(rs.size) {
			rs.reservoir[j] = value
		}
	}
}

// GetSamples returns a copy of the reservoir
func (rs *ReservoirSampler) GetSamples() []int {
	samples := make([]int, len(rs.reservoir))
	copy(samples, rs.reservoir)
	return samples
}

// GetCount returns the total number of samples seen
func (rs *ReservoirSampler) GetCount() uint64 {
	return rs.count
}

// GetAverage returns the true running average of all samples ever seen
func (rs *ReservoirSampler) GetAverage() float64 {
	if rs.count == 0 {
		return 0.0
	}
	return float64(rs.sum) / float64(rs.count)
}

// GetUtilization returns the percentage of samples where value > 0
func (rs *ReservoirSampler) GetUtilization() float64 {
	if rs.count == 0 {
		return 0.0
	}
	return float64(rs.nonZero) / float64(rs.count) * 100.0
}

// GetMax returns the true maximum ever seen (never decreases)
func (rs *ReservoirSampler) GetMax() int {
	return rs.max
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

// calcPercentile calculates the percentile from a list of values (returns integer)
func calcPercentile(data []int, pct int) int {
	if len(data) == 0 {
		return 0
	}
	sort.Ints(data)
	idx := len(data) * pct / 100
	if idx >= len(data) {
		idx = len(data) - 1
	}
	return data[idx]
}

// calcPercentileFloat calculates the percentile with linear interpolation
func calcPercentileFloat(sorted []int, pct float64) float64 {
	if len(sorted) == 0 {
		return 0.0
	}

	// Linear interpolation for more accurate percentiles
	pos := float64(len(sorted)-1) * pct / 100.0
	lower := int(pos)
	upper := lower + 1

	if upper >= len(sorted) {
		return float64(sorted[len(sorted)-1])
	}

	// Interpolate between lower and upper values
	weight := pos - float64(lower)
	return float64(sorted[lower])*(1-weight) + float64(sorted[upper])*weight
}

// calcPercentiles calculates all configured percentiles
func calcPercentiles(data []int) []float64 {
	results := make([]float64, len(percentiles))
	if len(data) == 0 {
		return results
	}

	sorted := make([]int, len(data))
	copy(sorted, data)
	sort.Ints(sorted)

	for i, pct := range percentiles {
		results[i] = calcPercentileFloat(sorted, pct)
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
	deviceSizes     [numDeviceSlots]string
	usbAggregate    *ReservoirSampler
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

func (d *Display) render(samplers [numDeviceSlots]*ReservoirSampler, currents [numDeviceSlots]int, usbAggrCurrent int, totalSamples uint64) {
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

	for i := 0; i < numDeviceSlots; i++ {
		dev := deviceNames[i]
		// Empty string means separator line
		if dev == "" {
			buf.WriteString("\n")
			continue
		}

		sampler := samplers[i]
		current := currents[i]
		pcts := calcPercentiles(sampler.GetSamples())
		// Use true max for P100 (last percentile) instead of reservoir max
		pcts[len(pcts)-1] = float64(sampler.GetMax())
		avg := sampler.GetAverage()
		util := sampler.GetUtilization()

		// Find P99 for bar display
		p99Int := 0
		for j, pct := range percentiles {
			if pct == 99 {
				p99Int = int(pcts[j] + 0.5)
				break
			}
		}
		bar := makeBar(current, p99Int, maxQueuePerDev)
		fmt.Fprintf(&buf, "%-8s %4d/%-3d %7.1f%%", dev, current, maxQueuePerDev, util)
		for j, val := range pcts {
			fmt.Fprintf(&buf, " %8.2f", val)
			if j == d.p50Index {
				fmt.Fprintf(&buf, " %8.2f", avg)
			}
		}
		devWithSize := fmt.Sprintf("%s(%s)", dev, d.deviceSizes[i])
		fmt.Fprintf(&buf, "  %-11s  [%s]  %-8s%4d\n", devWithSize, bar, dev, int(avg+0.5))

		// After last USB device, show aggregate USB stats
		if i == devSdg {
			aggrPcts := calcPercentiles(d.usbAggregate.GetSamples())
			// Use true max for P100 (last percentile) instead of reservoir max
			aggrPcts[len(aggrPcts)-1] = float64(d.usbAggregate.GetMax())
			aggrAvg := d.usbAggregate.GetAverage()
			aggrUtil := d.usbAggregate.GetUtilization()

			fmt.Fprintf(&buf, "%-8s %4d/%-3d %7.1f%%", "USB", usbAggrCurrent, maxQueueUSBAggr, aggrUtil)
			for j, val := range aggrPcts {
				fmt.Fprintf(&buf, " %8.2f", val)
				if j == d.p50Index {
					fmt.Fprintf(&buf, " %8.2f", aggrAvg)
				}
			}
			// Scaled utilization bar: scale from 0-150 to 0-30 for display
			scaledCurrent := int(float64(usbAggrCurrent) / float64(maxQueueUSBAggr) * float64(maxQueuePerDev) + 0.5)
			aggrP99 := 0.0
			for j, pct := range percentiles {
				if pct == 99 {
					aggrP99 = aggrPcts[j]
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

	reservoirCount := len(samplers[devSda].GetSamples())
	fmt.Fprintf(&buf, "Samples: %s total (%d in reservoir) @ %.0f/sec\n", formatCount(totalSamples), reservoirCount, d.samplesPerSec)

	if d.batchMode {
		buf.WriteString("\n")
	}

	// Write entire buffer at once (reduces flicker)
	d.resetCursor()
	fmt.Print(buf.String())
}

// SamplerState holds the shared state between sampler and display goroutines
// Using fixed-size array indexed by device constants (no map overhead)
type SamplerState struct {
	mu           sync.RWMutex
	samplers     [numDeviceSlots]*ReservoirSampler
	usbAggregate *ReservoirSampler
}

// Atomics for current values (lock-free access for display)
// Using fixed-size array indexed by device constants (no map overhead)
type CurrentValues struct {
	values      [numDeviceSlots]atomic.Int32
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

	// Initialize samplers for each device (slice indexed by device constants)
	var samplers [numDeviceSlots]*ReservoirSampler
	for i := 0; i < numDeviceSlots; i++ {
		if deviceNames[i] == "" {
			continue
		}
		samplers[i] = NewReservoirSampler(reservoirSize)
	}

	p50Index := findP50Index()
	if p50Index == -1 {
		log.Fatal("P50 must be present in percentiles array")
	}

	// Get device sizes at startup (fixed-size array)
	var deviceSizes [numDeviceSlots]string
	for i := 0; i < numDeviceSlots; i++ {
		if deviceNames[i] == "" {
			continue
		}
		deviceSizes[i] = getDeviceSize(deviceNames[i])
	}

	// Create aggregate sampler for combined USB queue depth
	usbAggregate := NewReservoirSampler(reservoirSize)

	// Initialize atomic current values (fixed-size array, no map)
	currents := &CurrentValues{}

	// Open persistent file handles for fast sysfs reads (slice indexed by device constants)
	var readers [numDeviceSlots]*InflightReader
	for i := 0; i < numDeviceSlots; i++ {
		if deviceNames[i] == "" {
			continue
		}
		reader, err := NewInflightReader(deviceNames[i])
		if err != nil {
			log.Printf("Warning: cannot open inflight file for %s: %v", deviceNames[i], err)
			continue
		}
		readers[i] = reader
	}

	// Shared state protected by RWMutex for sampler data
	state := &SamplerState{
		samplers:     samplers,
		usbAggregate: usbAggregate,
	}

	display := &Display{
		batchMode:    *batchMode,
		p50Index:     p50Index,
		deviceSizes:  deviceSizes,
		usbAggregate: usbAggregate,
	}

	// Setup signal handling for clean shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Channel to signal shutdown to goroutines
	done := make(chan struct{})

	// Initial message
	if !*batchMode {
		fmt.Println("Block I/O Queue Monitor - Ctrl+C to stop")
		fmt.Println("Starting sampler (dedicated CPU) and display (60 FPS)...")
	} else {
		log.Println("Starting sampler (dedicated CPU) and display...")
	}

	// SAMPLER GOROUTINE - runs flat out, no sleep, hogs one CPU core
	var sampleCount atomic.Uint64
	go func() {
		// Pre-allocate batch storage as fixed-size arrays (no map overhead)
		var localBatches [numDeviceSlots][]int
		for i := 0; i < numDeviceSlots; i++ {
			if deviceNames[i] != "" {
				localBatches[i] = make([]int, 0, sampleBatchSize)
			}
		}
		localUsbSums := make([]int, 0, sampleBatchSize)
		batchCount := 0

		for {
			select {
			case <-done:
				return
			default:
				// Phase 1: Read all sysfs values via persistent handles (no locks, just I/O)
				usbSum := 0
				for i := 0; i < numDeviceSlots; i++ {
					if deviceNames[i] == "" {
						continue
					}
					reader := readers[i]
					if reader == nil {
						continue // device not available
					}
					current, err := reader.Read()
					if err != nil {
						current = 0
					}

					// Accumulate in local batch (no lock needed, direct index)
					localBatches[i] = append(localBatches[i], current)

					// Update atomic current value (lock-free for display)
					currents.values[i].Store(int32(current))
				}

				// Calculate USB aggregate using index array (no map lookup)
				for _, idx := range usbDeviceIndices {
					if vals := localBatches[idx]; len(vals) > 0 {
						usbSum += vals[len(vals)-1]
					}
				}
				localUsbSums = append(localUsbSums, usbSum)
				currents.usbAggrCurr.Store(int32(usbSum))

				sampleCount.Add(1)
				batchCount++

				// Phase 2: Flush batch when full (1000x fewer lock acquisitions)
				if batchCount >= sampleBatchSize {
					state.mu.Lock()
					for i := 0; i < numDeviceSlots; i++ {
						if state.samplers[i] == nil {
							continue
						}
						for _, v := range localBatches[i] {
							state.samplers[i].Add(v)
						}
						// Reset slice, reuse backing array
						localBatches[i] = localBatches[i][:0]
					}
					for _, sum := range localUsbSums {
						state.usbAggregate.Add(sum)
					}
					localUsbSums = localUsbSums[:0]
					state.mu.Unlock()
					batchCount = 0
				}
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
				// Read current values (lock-free atomics, direct index access)
				var currentValues [numDeviceSlots]int
				for i := 0; i < numDeviceSlots; i++ {
					currentValues[i] = int(currents.values[i].Load())
				}
				usbAggrCurrent := int(currents.usbAggrCurr.Load())

				// Get sampler snapshots (needs read lock)
				state.mu.RLock()
				// Make copies of samplers for rendering (fixed-size array)
				var samplersCopy [numDeviceSlots]*ReservoirSampler
				for i := 0; i < numDeviceSlots; i++ {
					s := state.samplers[i]
					if s == nil {
						continue
					}
					// Create a temporary sampler with copied data for rendering
					samplersCopy[i] = &ReservoirSampler{
						reservoir: s.GetSamples(), // GetSamples already returns a copy
						count:     s.count,
						sum:       s.sum,
						nonZero:   s.nonZero,
						max:       s.max,
						size:      s.size,
					}
				}
				// Copy USB aggregate
				display.usbAggregate = &ReservoirSampler{
					reservoir: state.usbAggregate.GetSamples(),
					count:     state.usbAggregate.count,
					sum:       state.usbAggregate.sum,
					nonZero:   state.usbAggregate.nonZero,
					max:       state.usbAggregate.max,
					size:      state.usbAggregate.size,
				}
				state.mu.RUnlock()

				// Render (outside of lock)
				display.render(samplersCopy, currentValues, usbAggrCurrent, sampleCount.Load())
			}
		}
	}()

	// Wait for shutdown signal
	<-sigChan
	close(done)

	// Close persistent file handles
	for _, reader := range readers {
		if reader != nil {
			reader.Close()
		}
	}

	if *batchMode {
		log.Printf("Shutting down... Total samples: %d", sampleCount.Load())
	} else {
		fmt.Printf("\nStopped. Total samples: %d\n", sampleCount.Load())
	}
}
