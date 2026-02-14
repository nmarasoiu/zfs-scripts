package main

import (
	"fmt"

	"github.com/cilium/ebpf"
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

// readCounters reads the per-CPU counters map, sums across all CPUs,
// and returns aggregate map_used (clamped to [0, mapCap]), dropRing, dropMiss.
func readCounters(m *ebpf.Map, mapCap int64) (mapUsed int64, dropRing, dropMiss uint64) {
	var vals []bpfPercpuCounters
	if err := m.Lookup(uint32(0), &vals); err != nil {
		return 0, 0, 0
	}
	var used int64
	for _, v := range vals {
		used += v.MapUsed
		dropRing += v.DropRing
		dropMiss += v.DropMiss
	}
	if used < 0 {
		used = 0
	}
	if used > mapCap {
		used = mapCap
	}
	return used, dropRing, dropMiss
}
