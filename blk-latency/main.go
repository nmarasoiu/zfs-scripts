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

var (
	interval   = flag.Duration("i", 10*time.Second, "stats interval")
	devices    = flag.String("d", "", "comma-separated device filter (e.g., sdc,sdd or 8:32,8:48)")
	continuous = flag.Bool("c", false, "continuous output (no clear screen)")
)

// Device names cache: dev -> name
var (
	devNames   = make(map[uint32]string)
	devNamesMu sync.RWMutex
)

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

type deviceStats struct {
	hist    *hdrhistogram.Histogram
	samples int64
}

func newDeviceStats() *deviceStats {
	// 1µs to 60s range, 3 significant digits
	return &deviceStats{
		hist: hdrhistogram.New(1, 60_000_000, 3),
	}
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
		// Enable filter
		var key uint32 = 0
		var enabled uint8 = 1
		if err := objs.LatConfig.Put(key, enabled); err != nil {
			log.Fatalf("Failed to enable filter: %v", err)
		}
		// Add devices to filter
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

	// Per-device histograms
	stats := make(map[uint32]*deviceStats)
	var statsMu sync.Mutex

	// Stats printer
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	printStats := func() {
		statsMu.Lock()
		defer statsMu.Unlock()

		if len(stats) == 0 {
			fmt.Println("No samples collected")
			return
		}

		// Sort devices by name
		var devList []uint32
		for dev := range stats {
			devList = append(devList, dev)
		}
		sort.Slice(devList, func(i, j int) bool {
			return lookupDevName(devList[i]) < lookupDevName(devList[j])
		})

		if !*continuous {
			fmt.Print("\033[H\033[2J") // Clear screen
		}
		fmt.Printf("%-12s %8s %8s %8s %8s %8s %8s %9s\n",
			"device", "p50_us", "p75_us", "p90_us", "p95_us", "p99_us", "max_us", "samples")

		for _, dev := range devList {
			ds := stats[dev]
			name := lookupDevName(dev)
			fmt.Printf("%-12s %8d %8d %8d %8d %8d %8d %9d\n",
				name,
				ds.hist.ValueAtQuantile(50),
				ds.hist.ValueAtQuantile(75),
				ds.hist.ValueAtQuantile(90),
				ds.hist.ValueAtQuantile(95),
				ds.hist.ValueAtQuantile(99),
				ds.hist.Max(),
				ds.samples,
			)
		}
		fmt.Println()

		// Reset histograms
		for _, ds := range stats {
			ds.hist.Reset()
			ds.samples = 0
		}
	}

	// Signal handling
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig
		printStats()
		os.Exit(0)
	}()

	// Timer goroutine
	go func() {
		for range ticker.C {
			printStats()
		}
	}()

	log.Printf("Tracing block I/O latency (interval=%v)...", *interval)

	// Ring buffer consumer
	var event bpfLatencyEvent
	for {
		record, err := rd.Read()
		if err != nil {
			if err == ringbuf.ErrClosed {
				return
			}
			log.Printf("Ring buffer read error: %v", err)
			continue
		}

		if err := binary.Read(bytes.NewReader(record.RawSample), binary.LittleEndian, &event); err != nil {
			log.Printf("Failed to parse event: %v", err)
			continue
		}

		latencyUs := event.LatencyNs / 1000
		if latencyUs == 0 {
			latencyUs = 1 // Minimum 1µs
		}

		statsMu.Lock()
		ds, ok := stats[event.Dev]
		if !ok {
			ds = newDeviceStats()
			stats[event.Dev] = ds
		}
		ds.hist.RecordValue(int64(latencyUs))
		ds.samples++
		statsMu.Unlock()
	}
}
