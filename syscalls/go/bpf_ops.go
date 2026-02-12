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
	dropRingFull   uint32 = 0
	dropNoStartTS  uint32 = 1
)

func readDropCounts(m *ebpf.Map, dst *dropCounts) {
	var val uint64
	if err := m.Lookup(dropRingFull, &val); err == nil {
		dst.ringFull.Store(val)
	}
	if err := m.Lookup(dropNoStartTS, &val); err == nil {
		dst.noStartTS.Store(val)
	}
}

