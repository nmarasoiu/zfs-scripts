package main

import (
	"log"
	"sync/atomic"
	"time"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

func configureBPFFilters(objs *bpfObjects, focusList []string) {
	for _, name := range focusList {
		var comm [16]byte
		copy(comm[:], name)
		var val uint8 = 1
		if err := objs.TargetComms.Put(comm, val); err != nil {
			log.Fatalf("Failed to add comm filter %q: %v", name, err)
		}
	}
}

// ktimeNow returns the current CLOCK_MONOTONIC time in nanoseconds,
// matching bpf_ktime_get_ns() used by the BPF program.
func ktimeNow() uint64 {
	var ts unix.Timespec
	unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return uint64(ts.Sec)*1_000_000_000 + uint64(ts.Nsec)
}

// cleanStaleEntries iterates start_times, counts entries older than staleAge,
// and evicts at most one entry older than evictAge per call.
// Converges naturally over repeated cleanup ticks.
// Returns (total entries, stale count, evicted count 0 or 1).
func cleanStaleEntries(startTimes, syscallIds *ebpf.Map, staleAge, evictAge time.Duration) (int, int, int) {
	now := ktimeNow()
	staleThresh := now - uint64(staleAge.Nanoseconds())
	evictThresh := now - uint64(evictAge.Nanoseconds())

	var tid uint32
	var startNs uint64
	total := 0
	stale := 0
	var evictTid uint32
	evictFound := false

	iter := startTimes.Iterate()
	for iter.Next(&tid, &startNs) {
		total++
		if startNs < staleThresh {
			stale++
		}
		if !evictFound && startNs < evictThresh {
			evictTid = tid
			evictFound = true
		}
	}

	evicted := 0
	if evictFound {
		startTimes.Delete(evictTid)
		syscallIds.Delete(evictTid)
		evicted = 1
	}
	return total, stale, evicted
}

func readDropCount(m *ebpf.Map, dst *atomic.Uint64) {
	if m == nil {
		return
	}
	var key uint32
	var val uint64
	if err := m.Lookup(key, &val); err == nil {
		dst.Store(val)
	}
}
