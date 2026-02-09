package main

import (
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

func configureBPFFilters(objs *bpfObjects, focusList []string) error {
	for _, name := range focusList {
		var comm [16]byte
		copy(comm[:], name)
		var val uint8 = 1
		if err := objs.TargetComms.Put(comm, val); err != nil {
			return fmt.Errorf("add comm filter %q: %w", name, err)
		}
	}
	return nil
}

// ktimeNow returns the current CLOCK_MONOTONIC time in nanoseconds,
// matching bpf_ktime_get_ns() used by the BPF program.
func ktimeNow() uint64 {
	var ts unix.Timespec
	unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return uint64(ts.Sec)*1_000_000_000 + uint64(ts.Nsec)
}

// cleanStaleEntries iterates start_times, counts entries older than staleAge,
// evicts entries older than evictAge, and under pressure (>80% capacity)
// also evicts the oldest entries to get back below the threshold.
// Safe: BPF sys_exit handles missing entries gracefully (silent skip).
// Returns (total entries, stale count, evicted count).
func cleanStaleEntries(startTimes, syscallIds *ebpf.Map, staleAge, evictAge time.Duration, capacity int64) (int, int, int) {
	now := ktimeNow()
	staleThresh := now - uint64(staleAge.Nanoseconds())
	evictThresh := now - uint64(evictAge.Nanoseconds())

	type entry struct {
		tid     uint32
		startNs uint64
	}

	var all []entry
	var tid uint32
	var startNs uint64
	stale := 0

	iter := startTimes.Iterate()
	for iter.Next(&tid, &startNs) {
		all = append(all, entry{tid, startNs})
		if startNs < staleThresh {
			stale++
		}
	}
	total := len(all)

	// Phase 1: evict entries older than evictAge
	var toDelete []uint32
	for i := range all {
		if all[i].startNs < evictThresh {
			toDelete = append(toDelete, all[i].tid)
		}
	}

	// Phase 2: if above 80% capacity, sort by age and evict oldest
	remaining := total - len(toDelete)
	pressureLimit := int(float64(capacity) * 0.8)
	if remaining > pressureLimit {
		sort.Slice(all, func(i, j int) bool { return all[i].startNs < all[j].startNs })
		need := remaining - pressureLimit
		for _, e := range all {
			if need <= 0 {
				break
			}
			if e.startNs >= evictThresh { // not already marked
				toDelete = append(toDelete, e.tid)
				need--
			}
		}
	}

	for _, tid := range toDelete {
		startTimes.Delete(tid)
		syscallIds.Delete(tid)
	}
	return total, stale, len(toDelete)
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
