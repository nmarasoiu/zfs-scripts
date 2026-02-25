package main

import (
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"
	"github.com/nmarasoiu/zfs-scripts/ringpoll"
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
	p99 int64
	max int64
	cap int64
}

// formatUsage returns "avg: V/C (P%)  p99: V/C (P%)  max: V/C (P%)" using formatVal for values.
func (rs ringStats) formatUsage(formatVal func(int64) string) string {
	avgPct := float64(rs.avg) / float64(rs.cap) * 100
	p99Pct := float64(rs.p99) / float64(rs.cap) * 100
	maxPct := float64(rs.max) / float64(rs.cap) * 100
	return fmt.Sprintf("avg: %6s/%s (%4.1f%%)  p99: %6s/%s (%4.1f%%)  max: %6s/%s (%4.1f%%)",
		formatVal(rs.avg), formatVal(rs.cap), avgPct,
		formatVal(rs.p99), formatVal(rs.cap), p99Pct,
		formatVal(rs.max), formatVal(rs.cap), maxPct)
}

// frameMetrics bundles the per-frame runtime metrics passed to render.
type frameMetrics struct {
	drops       dropCounts
	ringStats   *ringStats
	cpuTime     time.Duration
	sleepStats  ringpoll.SleepStats
	totalEvents uint64 // events delivered + all drops
	infra       *infraSketches
}

// getCPUTime returns the process's cumulative user+system CPU time.
func getCPUTime() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	user := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond
	sys := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
	return user + sys
}

// ringOccupancy tracks ring pending bytes across display ticks using
// a DDSketch for quantiles and a running sum/count for the mean.
type ringOccupancy struct {
	sketch  *ddsketch.DDSketch
	sum     int64
	samples int64
}

func newRingOccupancy(alpha float64) *ringOccupancy {
	sk, _ := ddsketch.NewDefaultDDSketch(alpha)
	return &ringOccupancy{sketch: sk}
}

func (ro *ringOccupancy) add(pending int) {
	ro.sum += int64(pending)
	ro.samples++
	ro.sketch.Add(float64(pending))
}

func (ro *ringOccupancy) avg() int64 {
	if ro.samples == 0 {
		return 0
	}
	return ro.sum / ro.samples
}

func (ro *ringOccupancy) p99() int64 {
	if ro.samples == 0 {
		return 0
	}
	v, _ := ro.sketch.GetValueAtQuantile(0.99)
	if v < 0 {
		return 0
	}
	return int64(v)
}

// infraSketches tracks ring occupancy and pacer sleep as DDSketches,
// sampled on every drain cycle for full-resolution percentiles.
type infraSketches struct {
	mu        sync.Mutex
	ringAll   *ddsketch.DDSketch // avg fill % across rings
	ringMax   *ddsketch.DDSketch // worst ring fill %
	sleepAll  *ddsketch.DDSketch // all sleep durations (ns)
	sleepPure *ddsketch.DDSketch // non-zero sleep only (ns)
}

func newInfraSketches(alpha float64) *infraSketches {
	mk := func() *ddsketch.DDSketch {
		s, _ := ddsketch.NewDefaultDDSketch(alpha)
		return s
	}
	return &infraSketches{
		ringAll:   mk(),
		ringMax:   mk(),
		sleepAll:  mk(),
		sleepPure: mk(),
	}
}

// Record samples ring fill and sleep duration from one drain cycle.
func (is *infraSketches) Record(maxPending, avgPending, capacity int, sleepNs int64) {
	is.mu.Lock()
	if capacity > 0 {
		is.ringMax.Add(float64(maxPending) / float64(capacity) * 100)
		is.ringAll.Add(float64(avgPending) / float64(capacity) * 100)
	}
	is.sleepAll.Add(float64(sleepNs))
	if sleepNs > 0 {
		is.sleepPure.Add(float64(sleepNs))
	}
	is.mu.Unlock()
}

// infraView provides locked read access to the infra sketches.
type infraView struct {
	RingAll   *ddsketch.DDSketch
	RingMax   *ddsketch.DDSketch
	SleepAll  *ddsketch.DDSketch
	SleepPure *ddsketch.DDSketch
}

func (is *infraSketches) Read(fn func(infraView)) {
	is.mu.Lock()
	defer is.mu.Unlock()
	fn(infraView{is.ringAll, is.ringMax, is.sleepAll, is.sleepPure})
}
