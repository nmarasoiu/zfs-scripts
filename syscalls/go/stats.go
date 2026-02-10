package main

import (
	"bytes"
	"math"
	"sync"
	"time"
	"unsafe"

	"github.com/DataDog/sketches-go/ddsketch"
	"github.com/hashicorp/golang-lru/v2/simplelru"
)

// commToString converts a kernel TASK_COMM_LEN array to a Go string.
// Only used at display time (~2-4 FPS), never on the hot event path.
func commToString(comm [16]int8) string {
	buf := *(*[16]byte)(unsafe.Pointer(&comm))
	if n := bytes.IndexByte(buf[:], 0); n >= 0 {
		return string(buf[:n])
	}
	return string(buf[:])
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

func newSyscallStats(alpha float64) *syscallStats {
	sketch, _ := ddsketch.NewDefaultDDSketch(alpha)
	return &syscallStats{
		sketch: sketch,
		stats:  newSimpleStats(),
	}
}

func (ss *syscallStats) Record(latencyUs int64) {
	ss.sketch.Add(float64(latencyUs))
	ss.stats.Record(latencyUs)
}

// sketchPercentiles extracts the given quantiles from a DDSketch.
// quantiles are in 0.0–1.0 form (e.g. 0.50, 0.99). Returns values in µs.
func sketchPercentiles(sketch *ddsketch.DDSketch, quantiles []float64) []int64 {
	result := make([]int64, len(quantiles))
	for i, q := range quantiles {
		v, _ := sketch.GetValueAtQuantile(q)
		result[i] = int64(v)
	}
	return result
}

// sketchKey is the flat composite key for the LRU sketch cache.
// Uses [16]int8 (value type, no pointers) so the hot path avoids heap allocations.
type sketchKey struct {
	comm    [16]int8
	syscall uint32
}

// State holds all per-process and per-syscall stats
type State struct {
	mu    sync.Mutex
	alpha float64 // DDSketch relative accuracy

	// Per-(process, syscall) stats, capped by LRU eviction.
	sketches        *simplelru.LRU[sketchKey, *syscallStats]
	sketchEvictions uint64

	// Global lifetime sketch across all processes and syscalls
	globalSketch *ddsketch.DDSketch
	globalStats  *simpleStats

	startTime time.Time
}

func newState(maxSketches int, alpha float64) *State {
	sketch, _ := ddsketch.NewDefaultDDSketch(alpha)
	s := &State{
		alpha:        alpha,
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
	// String conversion of comm happens here at display rate, not on the hot event path.
	procStats := make(map[string]map[uint32]*syscallStats)
	for _, key := range s.sketches.Keys() {
		val, ok := s.sketches.Peek(key)
		if !ok {
			continue
		}
		name := commToString(key.comm)
		fm := procStats[name]
		if fm == nil {
			fm = make(map[uint32]*syscallStats)
			procStats[name] = fm
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
	comm      [16]int8
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
			ss = newSyscallStats(s.alpha)
			s.sketches.Add(key, ss) // evicts LRU if over cap
		}
		ss.Record(e.latencyUs)
		s.globalSketch.Add(float64(e.latencyUs))
		s.globalStats.Record(e.latencyUs)
	}
	s.mu.Unlock()
}
