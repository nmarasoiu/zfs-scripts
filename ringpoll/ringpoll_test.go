package ringpoll

import (
	"testing"

	"github.com/cilium/ebpf"
)

func TestNewReader_RejectsNonRingBuf(t *testing.T) {
	spec := &ebpf.MapSpec{
		Type:       ebpf.Hash,
		KeySize:    4,
		ValueSize:  4,
		MaxEntries: 16,
	}
	m, err := ebpf.NewMap(spec)
	if err != nil {
		t.Skipf("cannot create BPF map (need root/CAP_BPF): %v", err)
	}
	defer m.Close()

	_, err = NewReader(m)
	if err == nil {
		t.Fatal("expected error for non-RingBuf map, got nil")
	}
}

func TestNewReader_RingBuf(t *testing.T) {
	spec := &ebpf.MapSpec{
		Type:       ebpf.RingBuf,
		MaxEntries: 64 * 1024, // 64 KB (must be power of 2 * page size)
	}
	m, err := ebpf.NewMap(spec)
	if err != nil {
		t.Skipf("cannot create RingBuf map (need root/CAP_BPF + kernel 5.8+): %v", err)
	}
	defer m.Close()

	rd, err := NewReader(m)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer rd.Cleanup()

	if rd.BufSize() != 64*1024 {
		t.Errorf("BufSize = %d, want %d", rd.BufSize(), 64*1024)
	}
	if rd.Pending() != 0 {
		t.Errorf("Pending = %d, want 0 on empty ring", rd.Pending())
	}
	if rd.MaxPending() != 0 {
		t.Errorf("MaxPending = %d, want 0 on fresh reader", rd.MaxPending())
	}
	if snap := rd.Snapshot(); snap != nil {
		t.Errorf("Snapshot = %+v, want nil on fresh reader", snap)
	}
}

func TestClose_Poll(t *testing.T) {
	spec := &ebpf.MapSpec{
		Type:       ebpf.RingBuf,
		MaxEntries: 64 * 1024,
	}
	m, err := ebpf.NewMap(spec)
	if err != nil {
		t.Skipf("cannot create RingBuf map (need root/CAP_BPF + kernel 5.8+): %v", err)
	}
	defer m.Close()

	rd, err := NewReader(m)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer rd.Cleanup()

	// Close before polling — Poll should return false immediately
	rd.Close()

	var rec Record
	if rd.Poll(&rec) {
		t.Error("Poll returned true after Close")
	}
}
