package main

import (
	"bytes"
	"math"
	"sync"
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
}

func newSyscallStats() *syscallStats {
	sketch, _ := ddsketch.NewDefaultDDSketch(0.01)
	return &syscallStats{
		sketch: sketch,
		stats:  newSimpleStats(),
	}
}

func (ss *syscallStats) Record(latencyUs int64) {
	ss.sketch.Add(float64(latencyUs))
	ss.stats.Record(latencyUs)
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
