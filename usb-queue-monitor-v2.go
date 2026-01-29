package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	sampleInterval  = 100 * time.Millisecond
	reservoirSize   = 10000
	maxQueuePerDev  = 30
	usbDeviceCount  = 5
	maxQueueUSBAggr = maxQueuePerDev * usbDeviceCount // 150 total
)

// Device groups: SSD/NVMe first, then internal rotational, then USB drives
var devices = []string{"sda", "nvme0n1", "nvme1n1", "", "sdb", "", "sdc", "sdd", "sde", "sdf", "sdg"}

// Configurable percentiles to display (P0 replaced by Util column)
var percentiles = []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 95, 99, 99.5, 99.9, 99.95, 99.99, 99.995, 99.999, 100}

// ReservoirSampler maintains a fixed-size representative sample using reservoir sampling
type ReservoirSampler struct {
	reservoir []int
	count     uint64
	sum       uint64 // running sum for true average
	nonZero   uint64 // count of samples where value > 0
	size      int
	rng       *rand.Rand
}

// NewReservoirSampler creates a new reservoir sampler
func NewReservoirSampler(size int) *ReservoirSampler {
	return &ReservoirSampler{
		reservoir: make([]int, 0, size),
		count:     0,
		sum:       0,
		nonZero:   0,
		size:      size,
		rng:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Add adds a value to the reservoir
func (rs *ReservoirSampler) Add(value int) {
	rs.count++
	rs.sum += uint64(value)
	if value > 0 {
		rs.nonZero++
	}
	if len(rs.reservoir) < rs.size {
		rs.reservoir = append(rs.reservoir, value)
	} else {
		// Randomly replace elements with decreasing probability
		j := rs.rng.Int63n(int64(rs.count))
		if j < int64(rs.size) {
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

// getInflight reads the current in-flight IO count for a device
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
	batchMode    bool
	p50Index     int
	deviceSizes  map[string]string
	usbAggregate *ReservoirSampler
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

func (d *Display) render(samplers map[string]*ReservoirSampler, currents map[string]int, usbAggrCurrent int) {
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

		sampler := samplers[dev]
		current := currents[dev]
		pcts := calcPercentiles(sampler.GetSamples())
		avg := sampler.GetAverage()
		util := sampler.GetUtilization()

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
			aggrPcts := calcPercentiles(d.usbAggregate.GetSamples())
			aggrAvg := d.usbAggregate.GetAverage()
			aggrUtil := d.usbAggregate.GetUtilization()

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

	// Use the first device's sampler for total count (they're all the same)
	sampleCount := samplers[devices[0]].GetCount()
	reservoirCount := len(samplers[devices[0]].GetSamples())
	fmt.Fprintf(&buf, "Samples: %s total (%d in reservoir)\n", formatCount(sampleCount), reservoirCount)

	if d.batchMode {
		buf.WriteString("\n")
	}

	// Write entire buffer at once (reduces flicker)
	d.resetCursor()
	fmt.Print(buf.String())
}

func main() {
	batchMode := flag.Bool("batch", false, "Enable batch mode (no screen clearing, suitable for nohup)")
	flag.Parse()

	// Setup logging
	if *batchMode {
		log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
		log.Println("Block I/O Queue Monitor starting in batch mode")
	}

	// Initialize samplers for each device
	samplers := make(map[string]*ReservoirSampler)
	for _, dev := range devices {
		if dev == "" {
			continue
		}
		samplers[dev] = NewReservoirSampler(reservoirSize)
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

	// Create aggregate sampler for combined USB queue depth
	usbAggregate := NewReservoirSampler(reservoirSize)

	display := &Display{
		batchMode:    *batchMode,
		p50Index:     p50Index,
		deviceSizes:  deviceSizes,
		usbAggregate: usbAggregate,
	}

	// Setup signal handling for clean shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Initial delay
	if !*batchMode {
		fmt.Println("Block I/O Queue Monitor - Ctrl+C to stop")
		fmt.Println("Building initial sample set...")
	} else {
		log.Println("Building initial sample set...")
	}
	time.Sleep(500 * time.Millisecond)

	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sigChan:
			if *batchMode {
				log.Println("Received interrupt signal, shutting down...")
			} else {
				fmt.Println("\nStopped.")
			}
			return

		case <-ticker.C:
			currents := make(map[string]int)

			// Collect samples for each device
			for _, dev := range devices {
				if dev == "" {
					continue
				}
				current, err := getInflight(dev)
				if err != nil {
					if *batchMode {
						log.Printf("ERROR: Failed to read inflight for %s: %v", dev, err)
					}
					current = 0
				}
				currents[dev] = current
				samplers[dev].Add(current)
			}

			// Calculate and sample aggregate USB queue depth
			usbAggrCurrent := 0
			for _, usbDev := range usbDevices {
				usbAggrCurrent += currents[usbDev]
			}
			usbAggregate.Add(usbAggrCurrent)

			// Display current state
			display.render(samplers, currents, usbAggrCurrent)
		}
	}
}
