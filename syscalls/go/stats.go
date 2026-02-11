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

// mergeSimpleStats folds src into dst (min/max/sum/count).
func mergeSimpleStats(dst, src *simpleStats) {
	if src.count == 0 {
		return
	}
	if src.min < dst.min {
		dst.min = src.min
	}
	if src.max > dst.max {
		dst.max = src.max
	}
	dst.sum += src.sum
	dst.count += src.count
}

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

	// Per-(process, syscall) simple stats — unbounded, persistent for program lifetime.
	// ~80 bytes/entry; 10K entries ≈ 800KB.
	stats map[sketchKey]*simpleStats

	// Per-(process, syscall) DDSketches, capped by LRU eviction.
	sketches        *simplelru.LRU[sketchKey, *ddsketch.DDSketch]
	sketchEvictions uint64

	startTime time.Time
}

func newState(maxSketches int, alpha float64) *State {
	s := &State{
		alpha:     alpha,
		stats:     make(map[sketchKey]*simpleStats),
		startTime: time.Now(),
	}
	s.sketches, _ = simplelru.NewLRU[sketchKey, *ddsketch.DDSketch](maxSketches, func(_ sketchKey, _ *ddsketch.DDSketch) {
		s.sketchEvictions++
	})
	return s
}

// StateView provides read access to State internals within a locked scope.
// Only valid for the duration of the callback passed to State.Read.
// All fields are pointer-shared with the live State — no cloning.
type StateView struct {
	StartTime       time.Time
	NSketches       int    // current DDSketches in LRU
	NStats          int    // total persistent (proc, syscall) entries
	SketchEvictions uint64
	GlobalStats     *simpleStats // computed on-the-fly in Read()
	ProcStats       map[string]map[uint32]*syscallStats
}

// Read calls fn with a read-only view of the state under the lock.
// The view contains pointer-shared data — valid only within fn.
func (s *State) Read(fn func(StateView)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Build nested map view from persistent stats, attaching sketches from LRU where available.
	// String conversion of comm happens here at display rate, not on the hot event path.
	// Also fold min/max/sum/count into globalStats in the same pass.
	procStats := make(map[string]map[uint32]*syscallStats)
	globalStats := newSimpleStats()
	for key, st := range s.stats {
		mergeSimpleStats(globalStats, st)
		name := commToString(key.comm)
		syscalls := procStats[name]
		if syscalls == nil {
			syscalls = make(map[uint32]*syscallStats)
			procStats[name] = syscalls
		}
		sk, _ := s.sketches.Peek(key) // nil if evicted
		syscalls[key.syscall] = &syscallStats{
			sketch: sk,
			stats:  st,
		}
	}

	fn(StateView{
		StartTime:       s.startTime,
		NSketches:       s.sketches.Len(),
		NStats:          len(s.stats),
		SketchEvictions: s.sketchEvictions,
		GlobalStats:     globalStats,
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
		ev := &batch[i]
		key := sketchKey{ev.comm, ev.syscallID}

		// Always record in persistent simple stats
		st, ok := s.stats[key]
		if !ok {
			st = newSimpleStats()
			s.stats[key] = st
		}
		st.Record(ev.latencyUs)

		// Record in LRU-capped DDSketch
		sk, ok := s.sketches.Get(key) // Get promotes to most-recent
		if !ok {
			sk, _ = ddsketch.NewDefaultDDSketch(s.alpha)
			s.sketches.Add(key, sk) // evicts LRU if over cap
		}
		sk.Add(float64(ev.latencyUs))
	}
	s.mu.Unlock()
}
