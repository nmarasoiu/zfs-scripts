package main

import (
	"bytes"
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

// sketchKey is the flat composite key for the LRU sketch cache.
// Uses [16]int8 (value type, no pointers) so the hot path avoids heap allocations.
type sketchKey struct {
	comm    [16]int8
	syscall uint32
}

// State holds all per-process and per-syscall stats via DDSketch.
// DDSketch provides count, sum, avg, max, and percentiles — no separate tracking needed.
type State struct {
	mu    sync.Mutex
	alpha float64 // DDSketch relative accuracy

	// Per-(process, syscall) DDSketches, capped by LRU eviction.
	sketches        *simplelru.LRU[sketchKey, *ddsketch.DDSketch]
	sketchEvictions uint64

	startTime time.Time
}

func newState(maxSketches int, alpha float64) *State {
	s := &State{
		alpha:     alpha,
		startTime: time.Now(),
	}
	s.sketches, _ = simplelru.NewLRU[sketchKey, *ddsketch.DDSketch](maxSketches, func(_ sketchKey, _ *ddsketch.DDSketch) {
		s.sketchEvictions++
	})
	return s
}

// StateView provides read access to State internals within a locked scope.
// Only valid for the duration of the callback passed to State.Read.
type StateView struct {
	StartTime       time.Time
	NSketches       int    // current DDSketches in LRU
	SketchEvictions uint64
	ProcStats       map[string]map[uint32]*ddsketch.DDSketch
}

// Read calls fn with a read-only view of the state under the lock.
// The view contains pointer-shared data — valid only within fn.
func (s *State) Read(fn func(StateView)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	procStats := make(map[string]map[uint32]*ddsketch.DDSketch)
	for _, key := range s.sketches.Keys() {
		sk, _ := s.sketches.Peek(key) // Peek: no LRU promotion during display
		name := commToString(key.comm)
		syscalls := procStats[name]
		if syscalls == nil {
			syscalls = make(map[uint32]*ddsketch.DDSketch)
			procStats[name] = syscalls
		}
		syscalls[key.syscall] = sk
	}

	fn(StateView{
		StartTime:       s.startTime,
		NSketches:       s.sketches.Len(),
		SketchEvictions: s.sketchEvictions,
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

		sk, ok := s.sketches.Get(key) // Get promotes to most-recent
		if !ok {
			sk, _ = ddsketch.NewDefaultDDSketch(s.alpha)
			s.sketches.Add(key, sk) // evicts LRU if over cap
		}
		sk.Add(float64(ev.latencyUs))
	}
	s.mu.Unlock()
}
