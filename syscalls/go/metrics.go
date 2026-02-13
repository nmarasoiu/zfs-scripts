package main

import (
	"fmt"
	"sync/atomic"
	"syscall"
	"time"
)

// capacityStats captures avg/max occupancy of a bounded resource.
type capacityStats struct {
	avg int64
	max int64
	cap int64
}

// formatUsage returns "avg: V/C (P%)  max: V/C (P%)" using formatVal for values.
func (cs capacityStats) formatUsage(formatVal func(int64) string) string {
	avgPct := float64(cs.avg) / float64(cs.cap) * 100
	maxPct := float64(cs.max) / float64(cs.cap) * 100
	return fmt.Sprintf("avg: %6s/%s (%4.1f%%)  max: %6s/%s (%4.1f%%)",
		formatVal(cs.avg), formatVal(cs.cap), avgPct,
		formatVal(cs.max), formatVal(cs.cap), maxPct)
}

// dropCounts holds per-reason BPF drop counters (read from kernel).
type dropCounts struct {
	ringFull    atomic.Uint64 // bpf_ringbuf_reserve failed
	lruEvict    atomic.Uint64 // sys_exit miss while map at capacity
	startupMiss atomic.Uint64 // sys_exit miss while map below capacity (benign)
}

// runtimeMetrics groups atomic counters shared between goroutines.
type runtimeMetrics struct {
	bpfDrops dropCounts
}

func snapshotDrops(metrics *runtimeMetrics) frameDrops {
	return frameDrops{
		ringFull:    metrics.bpfDrops.ringFull.Load(),
		lruEvict:    metrics.bpfDrops.lruEvict.Load(),
		startupMiss: metrics.bpfDrops.startupMiss.Load(),
	}
}

// ringStats is a point-in-time snapshot of ring buffer metrics,
// decoupling display from the ringpoll.Reader type.
type ringStats struct {
	capacityStats
	pending int     // current pending bytes
	avg1    float64 // avg batch size (non-empty polls)
}

// frameDrops holds per-reason drop counts for a single display frame.
type frameDrops struct {
	ringFull    uint64 // BPF: ring buffer full
	lruEvict    uint64 // BPF: LRU eviction — map undersized
	startupMiss uint64 // BPF: benign startup race
}

func (d frameDrops) total() uint64 {
	return d.ringFull + d.lruEvict + d.startupMiss
}

// frameMetrics bundles the per-frame runtime metrics passed to render.
type frameMetrics struct {
	drops     frameDrops
	mapUsed   int64 // current start_times occupancy (from BPF counter)
	mapCap    int64 // start_times max_entries
	ringStats *ringStats
	cpuTime   time.Duration
}

// getCPUTime returns the process's cumulative user+system CPU time.
func getCPUTime() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	user := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond
	sys := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
	return user + sys
}

// ringAvg accumulates ring pending samples across display ticks
// to compute a true running average.
type ringAvg struct {
	sum     int64
	samples int64
}

func (ra *ringAvg) add(pending int) {
	ra.sum += int64(pending)
	ra.samples++
}

func (ra *ringAvg) avg() int64 {
	if ra.samples == 0 {
		return 0
	}
	return ra.sum / ra.samples
}
