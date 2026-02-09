package main

import (
	"bytes"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"
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

// State holds all per-process and per-syscall stats
type State struct {
	mu sync.Mutex

	// Per-(process, syscall) stats for all processes
	procSyscallStats map[string]map[uint32]*syscallStats

	// Global lifetime sketch across all processes and syscalls
	globalSketch *ddsketch.DDSketch
	globalStats  *simpleStats

	startTime time.Time
}

func newState() *State {
	sketch, _ := ddsketch.NewDefaultDDSketch(0.01)
	return &State{
		procSyscallStats: make(map[string]map[uint32]*syscallStats),
		globalSketch:     sketch,
		globalStats:      newSimpleStats(),
		startTime:        time.Now(),
	}
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
		fm, ok := s.procSyscallStats[e.comm]
		if !ok {
			fm = make(map[uint32]*syscallStats)
			s.procSyscallStats[e.comm] = fm
		}
		ss, ok := fm[e.syscallID]
		if !ok {
			ss = newSyscallStats()
			fm[e.syscallID] = ss
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
}
