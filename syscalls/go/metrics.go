package main

import (
	"fmt"
	"syscall"
	"time"
)

// dropCounts holds BPF drop counters summed across all CPUs.
type dropCounts struct {
	ringFull uint64
	miss     uint64
}

func (d dropCounts) total() uint64 {
	return d.ringFull + d.miss
}

// ringStats is a point-in-time snapshot of ring buffer occupancy.
type ringStats struct {
	avg int64
	max int64
	cap int64
}

// formatUsage returns "avg: V/C (P%)  max: V/C (P%)" using formatVal for values.
func (rs ringStats) formatUsage(formatVal func(int64) string) string {
	avgPct := float64(rs.avg) / float64(rs.cap) * 100
	maxPct := float64(rs.max) / float64(rs.cap) * 100
	return fmt.Sprintf("avg: %6s/%s (%4.1f%%)  max: %6s/%s (%4.1f%%)",
		formatVal(rs.avg), formatVal(rs.cap), avgPct,
		formatVal(rs.max), formatVal(rs.cap), maxPct)
}

// frameMetrics bundles the per-frame runtime metrics passed to render.
type frameMetrics struct {
	drops     dropCounts
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
