package main

import (
	"bytes"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"
	"github.com/hashicorp/golang-lru/v2/simplelru"
)

// commIntern interns process comm strings to avoid 80K allocations/s.
// Only used by the reader goroutine — no sync needed.
var commIntern = make(map[string]string)

func commString(comm [16]int8) string {
	var buf [16]byte
	for i, c := range comm {
		buf[i] = byte(c)
	}
	var raw string
	if n := bytes.IndexByte(buf[:], 0); n >= 0 {
		raw = string(buf[:n])
	} else {
		raw = string(buf[:])
	}
	if s, ok := commIntern[raw]; ok {
		return s
	}
	commIntern[raw] = raw
	return raw
}

// topN tracks the top N maximum values
type topN struct {
	values []int64
	n      int
}

func newTopN(n int) *topN {
	return &topN{values: make([]int64, 0, n), n: n}
}

func (t *topN) Add(v int64) {
	if len(t.values) < t.n {
		i := sort.Search(len(t.values), func(i int) bool { return t.values[i] >= v })
		t.values = append(t.values, 0)
		copy(t.values[i+1:], t.values[i:])
		t.values[i] = v
		return
	}
	if v > t.values[0] {
		i := sort.Search(len(t.values), func(i int) bool { return t.values[i] >= v })
		if i > 0 {
			copy(t.values[:i-1], t.values[1:i])
			t.values[i-1] = v
		}
	}
}

func (t *topN) Get() []int64 {
	result := make([]int64, len(t.values))
	copy(result, t.values)
	return result
}

func (t *topN) Reset() { t.values = t.values[:0] }

// simpleStats tracks min/max/sum/count explicitly (no sketch overhead)
type simpleStats struct {
	min   int64
	max   int64
	sum   uint64
	count uint64
}

func newSimpleStats() *simpleStats {
	return &simpleStats{min: math.MaxInt64, max: 0}
}

func (s *simpleStats) Record(v int64) {
	if v < s.min {
		s.min = v
	}
	if v > s.max {
		s.max = v
	}
	s.sum += uint64(v)
	s.count++
}

func (s *simpleStats) Avg() int64 {
	if s.count == 0 {
		return 0
	}
	return int64(s.sum / s.count)
}

// syscallStats holds lifetime stats for a syscall.
// Uses DDSketch for percentiles, explicit tracking for min/max/avg.
type syscallStats struct {
	sketch *ddsketch.DDSketch
	stats  *simpleStats
	top    *topN
}

func newSyscallStats() *syscallStats {
	sketch, _ := ddsketch.NewDefaultDDSketch(0.01)
	return &syscallStats{
		sketch: sketch,
		stats:  newSimpleStats(),
		top:    newTopN(5),
	}
}

func (ss *syscallStats) Record(latencyUs int64) {
	ss.sketch.Add(float64(latencyUs))
	ss.stats.Record(latencyUs)
	ss.top.Add(latencyUs)
}

// percentiles holds pre-extracted quantile values (in µs) from a DDSketch.
type percentiles struct {
	P25, P50, P75, P90, P99, P999 int64
}

func sketchPercentiles(sketch *ddsketch.DDSketch) percentiles {
	p25, _ := sketch.GetValueAtQuantile(0.25)
	p50, _ := sketch.GetValueAtQuantile(0.50)
	p75, _ := sketch.GetValueAtQuantile(0.75)
	p90, _ := sketch.GetValueAtQuantile(0.90)
	p99, _ := sketch.GetValueAtQuantile(0.99)
	p999, _ := sketch.GetValueAtQuantile(0.999)
	return percentiles{
		P25: int64(p25), P50: int64(p50), P75: int64(p75),
		P90: int64(p90), P99: int64(p99), P999: int64(p999),
	}
}

// sketchKey is the flat composite key for the LRU sketch cache.
type sketchKey struct {
	proc    string
	syscall uint32
}

// State holds all per-process and per-syscall stats
type State struct {
	mu sync.Mutex

	// Per-(process, syscall) stats, capped by LRU eviction.
	sketches        *simplelru.LRU[sketchKey, *syscallStats]
	sketchEvictions uint64

	// Global lifetime sketch across all processes and syscalls
	globalSketch *ddsketch.DDSketch
	globalStats  *simpleStats

	startTime time.Time
}

func newState(maxSketches int) *State {
	sketch, _ := ddsketch.NewDefaultDDSketch(0.01)
	s := &State{
		globalSketch: sketch,
		globalStats:  newSimpleStats(),
		startTime:    time.Now(),
	}
	s.sketches, _ = simplelru.NewLRU[sketchKey, *syscallStats](maxSketches, func(_ sketchKey, _ *syscallStats) {
		s.sketchEvictions++
	})
	return s
}

// StateView provides read access to State internals within a locked scope.
// Only valid for the duration of the callback passed to State.Read.
// All fields are pointer-shared with the live State — no cloning.
type StateView struct {
	StartTime       time.Time
	NSketches       int
	SketchEvictions uint64
	GlobalStats     *simpleStats
	GlobalSketch    *ddsketch.DDSketch
	ProcStats       map[string]map[uint32]*syscallStats
}

// Read calls fn with a read-only view of the state under the lock.
// The view contains pointer-shared data — valid only within fn.
func (s *State) Read(fn func(StateView)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Build nested map view from LRU (pointer copies only, no cloning).
	procStats := make(map[string]map[uint32]*syscallStats)
	for _, key := range s.sketches.Keys() {
		val, ok := s.sketches.Peek(key)
		if !ok {
			continue
		}
		fm := procStats[key.proc]
		if fm == nil {
			fm = make(map[uint32]*syscallStats)
			procStats[key.proc] = fm
		}
		fm[key.syscall] = val
	}

	fn(StateView{
		StartTime:       s.startTime,
		NSketches:       s.sketches.Len(),
		SketchEvictions: s.sketchEvictions,
		GlobalStats:     s.globalStats,
		GlobalSketch:    s.globalSketch,
		ProcStats:       procStats,
	})
}

type pendingEvent struct {
	comm      string
	syscallID uint32
	latencyUs int64
}

func (s *State) RecordBatch(batch []pendingEvent) {
	s.mu.Lock()
	for i := range batch {
		e := &batch[i]
		key := sketchKey{e.comm, e.syscallID}
		ss, ok := s.sketches.Get(key) // Get promotes to most-recent
		if !ok {
			ss = newSyscallStats()
			s.sketches.Add(key, ss) // evicts LRU if over cap
		}
		ss.Record(e.latencyUs)
		s.globalSketch.Add(float64(e.latencyUs))
		s.globalStats.Record(e.latencyUs)
	}
	s.mu.Unlock()
}

// runtimeMetrics groups atomic counters shared between goroutines.
type runtimeMetrics struct {
	drops    atomic.Uint64
	evicted  atomic.Uint64
	mapUsed  atomic.Int64
	mapStale atomic.Int64

	// Map avg/max tracking (updated each cleanup tick)
	mapMaxUsed atomic.Int64
	mapSumUsed atomic.Int64
	mapSamples atomic.Int64
}

// ringStats is a point-in-time snapshot of ring buffer metrics,
// decoupling display from the ringpoll.Reader type.
type ringStats struct {
	pending    int
	avgPending int64 // running average of pending bytes
	capBytes   int
	maxPending int64
	avg1       float64
	avg0       float64
	last1      int64
	last0      time.Duration
}

// ringAvg accumulates ring pending samples across display ticks
// to compute a true running average (like mapSumUsed/mapSamples).
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

type processSummary struct {
	name  string
	count uint64
	rate  float64
}

type tableEntry struct {
	label string
	ss    *syscallStats
}

// collectEntries builds a sorted list of table entries from per-process stats.
// When alwaysPrefix is true, labels are always "proc/syscall"; otherwise the
// proc prefix is omitted when there is exactly one process.
func collectEntries(procStats map[string]map[uint32]*syscallStats, alwaysPrefix bool) []tableEntry {
	singleProc := !alwaysPrefix && len(procStats) == 1

	var entries []tableEntry
	for proc, fm := range procStats {
		for id, ss := range fm {
			label := syscallName(id)
			if !singleProc {
				label = proc + "/" + label
			}
			entries = append(entries, tableEntry{label, ss})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		ci := entries[i].ss.stats.count
		cj := entries[j].ss.stats.count
		if ci != cj {
			return ci > cj
		}
		return entries[i].label < entries[j].label
	})

	return entries
}

// filterStatsGeneral returns a filtered copy of procStats where entries match
// the text against process name or syscall name (case-insensitive substring).
// Process name matches include all syscalls; syscall matches are per-entry.
func filterStatsGeneral(procStats map[string]map[uint32]*syscallStats, text string) map[string]map[uint32]*syscallStats {
	lower := strings.ToLower(text)
	filtered := make(map[string]map[uint32]*syscallStats)
	for proc, fm := range procStats {
		if strings.HasPrefix(strings.ToLower(proc), lower) {
			filtered[proc] = fm
			continue
		}
		matched := make(map[uint32]*syscallStats)
		for id, ss := range fm {
			if strings.HasPrefix(syscallName(id), lower) {
				matched[id] = ss
			}
		}
		if len(matched) > 0 {
			filtered[proc] = matched
		}
	}
	return filtered
}

// collectProcessSummaries aggregates per-process totals.
func collectProcessSummaries(procStats map[string]map[uint32]*syscallStats, elapsedSecs float64) []processSummary {
	summaries := make([]processSummary, 0, len(procStats))
	for proc, fm := range procStats {
		var total uint64
		for _, ss := range fm {
			total += ss.stats.count
		}
		rate := float64(0)
		if elapsedSecs > 0 {
			rate = float64(total) / elapsedSecs
		}
		summaries = append(summaries, processSummary{name: proc, count: total, rate: rate})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].count != summaries[j].count {
			return summaries[i].count > summaries[j].count
		}
		return summaries[i].name < summaries[j].name
	})
	return summaries
}
