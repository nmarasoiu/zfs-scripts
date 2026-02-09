package main

import (
	"fmt"
	"sync/atomic"
	"time"
)

// capacityStats captures avg/max occupancy of a bounded resource.
type capacityStats struct {
	avg int64
	max int64
	cap int64
}

// formatUsage returns "avg: V/C (P%)  max: V/C (P%)" using fmtVal for values.
func (cs capacityStats) formatUsage(fmtVal func(int64) string) string {
	avgPct := float64(cs.avg) / float64(cs.cap) * 100
	maxPct := float64(cs.max) / float64(cs.cap) * 100
	return fmt.Sprintf("avg: %6s/%s (%4.1f%%)  max: %6s/%s (%4.1f%%)",
		fmtVal(cs.avg), fmtVal(cs.cap), avgPct,
		fmtVal(cs.max), fmtVal(cs.cap), maxPct)
}

// runtimeMetrics groups atomic counters shared between goroutines.
type runtimeMetrics struct {
	drops atomic.Uint64
}

// ringStats is a point-in-time snapshot of ring buffer metrics,
// decoupling display from the ringpoll.Reader type.
type ringStats struct {
	capacityStats
	pending int         // current pending bytes
	avg1    float64     // poll stats
	avg0    float64
	last1   int64
	last0   time.Duration
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
