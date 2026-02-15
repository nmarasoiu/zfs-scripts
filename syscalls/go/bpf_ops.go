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

// readCounters reads the per-CPU counters map and sums across all CPUs.
func readCounters(m *ebpf.Map) dropCounts {
	var vals []bpfPercpuCounters
	if err := m.Lookup(uint32(0), &vals); err != nil {
		return dropCounts{}
	}
	var r dropCounts
	for _, v := range vals {
		r.ringFull += v.DropRing
		r.miss += v.DropMiss
	}
	return r
}
