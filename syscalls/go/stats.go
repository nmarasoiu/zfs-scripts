package main

import (
	"bytes"
	"sync"
	"time"
	"unsafe"

	"github.com/DataDog/sketches-go/ddsketch"
	lru "github.com/hashicorp/golang-lru/v2"
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

// sketchKey is the flat composite key for the sketch cache.
// Uses [16]int8 (value type, no pointers) so the hot path avoids heap allocations.
type sketchKey struct {
	comm    [16]int8
	syscall uint32
}

// processCounter tracks persistent per-process-name totals that survive sketch eviction.
// Typically ~30-50 entries, never evicted, ~1KB total.
type processCounter struct {
	count uint64
}

// State holds all per-process and per-syscall stats via DDSketch.
// DDSketch provides count, sum, avg, max, and percentiles — no separate tracking needed.
type State struct {
	mu    sync.Mutex
	alpha float64 // DDSketch relative accuracy

	// Per-(process, syscall) DDSketches, capped by 2Q eviction.
	sketches        *lru.TwoQueueCache[sketchKey, *ddsketch.DDSketch]
	sketchEvictions uint64

	// Per-process-name persistent counters (never evicted).
	procCounters map[[16]int8]*processCounter

	startTime time.Time
}

func newState(maxSketches int, alpha float64) *State {
	s := &State{
		alpha:        alpha,
		procCounters: make(map[[16]int8]*processCounter),
		startTime:    time.Now(),
	}
	s.sketches, _ = lru.New2Q[sketchKey, *ddsketch.DDSketch](maxSketches)
	return s
}

// StateView provides read access to State internals within a locked scope.
// Only valid for the duration of the callback passed to State.Read.
type StateView struct {
	StartTime       time.Time
	NSketches       int    // current DDSketches in cache
	SketchEvictions uint64
	ProcStats       map[string]map[uint32]*ddsketch.DDSketch
	ProcCounters    map[string]*processCounter
}

// Read calls fn with a read-only view of the state under the lock.
// The view contains pointer-shared data — valid only within fn.
func (s *State) Read(fn func(StateView)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	procStats := make(map[string]map[uint32]*ddsketch.DDSketch)
	for _, key := range s.sketches.Keys() {
		sk, _ := s.sketches.Peek(key) // Peek: no promotion during display
		name := commToString(key.comm)
		syscalls := procStats[name]
		if syscalls == nil {
			syscalls = make(map[uint32]*ddsketch.DDSketch)
			procStats[name] = syscalls
		}
		syscalls[key.syscall] = sk
	}

	procCounters := make(map[string]*processCounter, len(s.procCounters))
	for comm, pc := range s.procCounters {
		procCounters[commToString(comm)] = pc
	}

	fn(StateView{
		StartTime:       s.startTime,
		NSketches:       s.sketches.Len(),
		SketchEvictions: s.sketchEvictions,
		ProcStats:       procStats,
		ProcCounters:    procCounters,
	})
}

type pendingEvent struct {
	comm      [16]int8
	syscallID uint32
	latencyNs int64
}

func (s *State) RecordBatch(batch []pendingEvent) {
	s.mu.Lock()
	for i := range batch {
		ev := &batch[i]

		// Persistent per-process counters (never evicted)
		pc := s.procCounters[ev.comm]
		if pc == nil {
			pc = &processCounter{}
			s.procCounters[ev.comm] = pc
		}
		pc.count++

		key := sketchKey{ev.comm, ev.syscallID}

		sk, ok := s.sketches.Get(key) // Get promotes to most-recent
		if !ok {
			sk, _ = ddsketch.NewDefaultDDSketch(s.alpha)
			lenBefore := s.sketches.Len()
			s.sketches.Add(key, sk)
			if s.sketches.Len() <= lenBefore {
				s.sketchEvictions++
			}
		}
		sk.Add(float64(ev.latencyNs))
	}
	s.mu.Unlock()
}
