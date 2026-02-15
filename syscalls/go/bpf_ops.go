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

// counterResults holds the aggregated per-CPU counter values.
type counterResults struct {
	mapUsed    int64
	dropRing   uint64
	dropMiss   uint64
	probeTotal uint64
	probeExits uint64
}

// readCounters reads the per-CPU counters map, sums across all CPUs,
// and returns aggregate values with map_used clamped to [0, mapCap].
func readCounters(m *ebpf.Map, mapCap int64) counterResults {
	var vals []bpfPercpuCounters
	if err := m.Lookup(uint32(0), &vals); err != nil {
		return counterResults{}
	}
	var r counterResults
	for _, v := range vals {
		r.mapUsed += v.MapUsed
		r.dropRing += v.DropRing
		r.dropMiss += v.DropMiss
		r.probeTotal += v.ProbeTotal
		r.probeExits += v.ProbeExits
	}
	if r.mapUsed < 0 {
		r.mapUsed = 0
	}
	if r.mapUsed > mapCap {
		r.mapUsed = mapCap
	}
	return r
}
