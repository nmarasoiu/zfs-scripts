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

// BPF drop reason indices — must match enum drop_reason in syscall_latency.c.
const (
	dropRingFull    uint32 = 0
	dropLRUEvict    uint32 = 1
	dropStartupMiss uint32 = 2
)

func readDropCounts(m *ebpf.Map, dst *dropCounts) {
	var val uint64
	if err := m.Lookup(dropRingFull, &val); err == nil {
		dst.ringFull.Store(val)
	}
	if err := m.Lookup(dropLRUEvict, &val); err == nil {
		dst.lruEvict.Store(val)
	}
	if err := m.Lookup(dropStartupMiss, &val); err == nil {
		dst.startupMiss.Store(val)
	}
}

// readMapUsed reads the map_used_count BPF array counter.
// Returns approximate start_times occupancy, clamped to [0, cap].
func readMapUsed(m *ebpf.Map, cap int64) int64 {
	var val int64
	if err := m.Lookup(uint32(0), &val); err != nil {
		return 0
	}
	if val < 0 {
		return 0
	}
	if val > cap {
		return cap
	}
	return val
}

