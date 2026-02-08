package ringpoll

import (
	"testing"
	"time"

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

	_, err = NewReader(m, 50*time.Microsecond)
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

	rd, err := NewReader(m, 50*time.Microsecond)
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
	if rd.LastBatch() != 0 {
		t.Errorf("LastBatch = %d, want 0 on fresh reader", rd.LastBatch())
	}

	avg1, avg0, last1, _ := rd.PollStats()
	if avg1 != 0 || avg0 != 0 || last1 != 0 {
		t.Errorf("PollStats = (%f, %f, %d), want all zeros on fresh reader", avg1, avg0, last1)
	}
}

func TestClose(t *testing.T) {
	spec := &ebpf.MapSpec{
		Type:       ebpf.RingBuf,
		MaxEntries: 64 * 1024,
	}
	m, err := ebpf.NewMap(spec)
	if err != nil {
		t.Skipf("cannot create RingBuf map (need root/CAP_BPF + kernel 5.8+): %v", err)
	}
	defer m.Close()

	rd, err := NewReader(m, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer rd.Cleanup()

	// Close before reading — ReadInto should return false promptly
	rd.Close()

	done := make(chan bool, 1)
	go func() {
		var rec Record
		done <- rd.ReadInto(&rec)
	}()

	select {
	case got := <-done:
		if got {
			t.Error("ReadInto returned true after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadInto did not return within 2s after Close")
	}
}
