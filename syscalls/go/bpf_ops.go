package main

import (
	"fmt"
	"sync/atomic"

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
