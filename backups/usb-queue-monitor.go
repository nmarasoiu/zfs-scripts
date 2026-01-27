package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	sampleInterval = 100 * time.Millisecond
	reservoirSize  = 10000
	maxQueue       = 30
)

var devices = []string{"sdc", "sdd", "sde", "sdf", "sdg", "sdh", "sdb", "sdi"}

// Configurable percentiles to display
var percentiles = []float64{0, 10, 20, 30, 40, 50, 60, 70, 80, 90, 95, 99, 99.5, 99.9, 99.99, 100}

// ReservoirSampler maintains a fixed-size representative sample using reservoir sampling
type ReservoirSampler struct {
	reservoir []int
	count     uint64
	size      int
	rng       *rand.Rand
}

// NewReservoirSampler creates a new reservoir sampler
func NewReservoirSampler(size int) *ReservoirSampler {
	return &ReservoirSampler{
		reservoir: make([]int, 0, size),
		count:     0,
		size:      size,
		rng:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Add adds a value to the reservoir
func (rs *ReservoirSampler) Add(value int) {
	rs.count++
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

// calcAverage calculates the arithmetic mean of the samples
func calcAverage(data []int) float64 {
	if len(data) == 0 {
		return 0.0
	}
	sum := 0
	for _, v := range data {
		sum += v
	}
	return float64(sum) / float64(len(data))
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
	batchMode bool
}

func (d *Display) clear() {
	if !d.batchMode {
		cmd := exec.Command("clear")
		cmd.Stdout = os.Stdout
		cmd.Run()
	}
}

// formatPercentileHeader returns the header label for a percentile
func formatPercentileHeader(pct float64) string {
	if pct == float64(int(pct)) {
		return fmt.Sprintf("P%d", int(pct))
	}
	return fmt.Sprintf("P%.1f", pct)
}

func (d *Display) render(samplers map[string]*ReservoirSampler, currents map[string]int) {
	d.clear()

	timestamp := time.Now().Format("Mon Jan 02 15:04:05 2006")

	if d.batchMode {
		fmt.Printf("[%s] USB Queue Monitor\n", timestamp)
	} else {
		fmt.Printf("USB Queue Monitor - %s\n", timestamp)
	}

	// Build dynamic header
	lineWidth := 8 + 9 + len(percentiles)*9 + 2 + maxQueue + 2
	fmt.Println(strings.Repeat("=", lineWidth))
	fmt.Printf("%-8s %8s", "Device", "Current")
	for _, pct := range percentiles {
		fmt.Printf(" %8s", formatPercentileHeader(pct))
	}
	fmt.Printf("  Utilization\n")
	fmt.Println(strings.Repeat("-", lineWidth))

	for _, dev := range devices {
		current := currents[dev]
		pcts := calcPercentiles(samplers[dev].GetSamples())
		// Find P90 for bar display (index 3 if using default percentiles)
		p90Int := 0
		for i, pct := range percentiles {
			if pct == 90 {
				p90Int = int(pcts[i] + 0.5)
				break
			}
		}
		bar := makeBar(current, p90Int, maxQueue)
		fmt.Printf("%-8s %4d/%-3d", dev, current, maxQueue)
		for _, val := range pcts {
			fmt.Printf(" %8.2f", val)
		}
		fmt.Printf("  [%s]\n", bar)
	}

	fmt.Println()
	if d.batchMode {
		fmt.Println("Legend: █ = current  ░ = p90 (long-term)  - = unused")
	} else {
		fmt.Printf("Legend: █= current  ░= p90 (long-term)  -= unused\n")
	}

	// Use the first device's sampler for total count (they're all the same)
	sampleCount := samplers[devices[0]].GetCount()
	reservoirCount := len(samplers[devices[0]].GetSamples())
	fmt.Printf("Samples: %s total (%d in reservoir)\n", formatCount(sampleCount), reservoirCount)

	if d.batchMode {
		fmt.Println()
	}
}

func main() {
	batchMode := flag.Bool("batch", false, "Enable batch mode (no screen clearing, suitable for nohup)")
	flag.Parse()

	// Setup logging
	if *batchMode {
		log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
		log.Println("USB Queue Monitor starting in batch mode")
	}

	// Initialize samplers for each device
	samplers := make(map[string]*ReservoirSampler)
	for _, dev := range devices {
		samplers[dev] = NewReservoirSampler(reservoirSize)
	}

	display := &Display{batchMode: *batchMode}

	// Setup signal handling for clean shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Initial delay
	if !*batchMode {
		fmt.Println("USB Queue Monitor - Ctrl+C to stop")
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

			// Collect samples
			for _, dev := range devices {
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

			// Display current state
			display.render(samplers, currents)
		}
	}
}
