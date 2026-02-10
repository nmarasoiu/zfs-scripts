package ringpoll

import (
	"testing"
	"time"

	"github.com/cilium/ebpf"
)

func createTestRing(t *testing.T) (*ebpf.Map, *Reader) {
	t.Helper()
	spec := &ebpf.MapSpec{
		Type:       ebpf.RingBuf,
		MaxEntries: 64 * 1024,
	}
	m, err := ebpf.NewMap(spec)
	if err != nil {
		t.Skipf("cannot create RingBuf map (need root/CAP_BPF + kernel 5.8+): %v", err)
	}
	rd, err := NewReader(m)
	if err != nil {
		m.Close()
		t.Fatalf("NewReader: %v", err)
	}
	t.Cleanup(func() {
		rd.Cleanup()
		m.Close()
	})
	return m, rd
}

func TestPollerClosedEmpty(t *testing.T) {
	_, rd := createTestRing(t)
	p := NewPoller(rd)

	if p.Closed() {
		t.Error("Poller.Closed() should be false on fresh reader")
	}
	rd.Close()
	if !p.Closed() {
		t.Error("Poller.Closed() should be true after reader closed")
	}
}

func TestPollerQuietOnEmpty(t *testing.T) {
	_, rd := createTestRing(t)
	p := NewPoller(rd)

	if !p.Quiet(0.05) {
		t.Error("Poller.Quiet should be true on empty ring")
	}
}

func TestPollerFillBatchEmpty(t *testing.T) {
	_, rd := createTestRing(t)
	p := NewPoller(rd)

	buf := make([]Record, 16)
	n := p.FillBatch(buf)
	if n != 0 {
		t.Errorf("FillBatch on empty ring returned %d, want 0", n)
	}
}

func TestPollerCommitAll(t *testing.T) {
	_, rd := createTestRing(t)
	p := NewPoller(rd)

	// CommitAll should not panic on fresh reader
	p.CommitAll()

	snap := rd.Snapshot()
	if snap == nil {
		t.Fatal("expected snapshot after CommitAll")
	}
	if snap.PollCount != 1 {
		t.Errorf("PollCount = %d, want 1", snap.PollCount)
	}
}

func TestDrainerRunExitsOnClose(t *testing.T) {
	_, rd := createTestRing(t)
	p := NewPoller(rd)
	d := NewDrainer(p, DrainOpts{
		MaxBatch:  16,
		PollSleep: time.Millisecond,
	})

	// Close reader immediately — Run should return promptly
	rd.Close()

	calls := 0
	d.Run(func(batch []Record) {
		calls++
	})
	// Should reach here without blocking; callback not called on empty ring
	if calls != 0 {
		t.Errorf("callback called %d times on empty ring, want 0", calls)
	}
}

func TestDrainerDefaultOpts(t *testing.T) {
	_, rd := createTestRing(t)
	p := NewPoller(rd)
	d := NewDrainer(p, DrainOpts{}) // all zero → defaults

	if len(d.buf) != 1024 {
		t.Errorf("default MaxBatch: buf len = %d, want 1024", len(d.buf))
	}
	if d.opts.pollSleep() != 3*time.Millisecond {
		t.Errorf("default PollSleep = %v, want 3ms", d.opts.pollSleep())
	}
	if d.opts.quietThreshold() != 0.05 {
		t.Errorf("default QuietThreshold = %v, want 0.05", d.opts.quietThreshold())
	}
	rd.Close()
}

func TestPollerMultipleReaders(t *testing.T) {
	_, rd1 := createTestRing(t)
	_, rd2 := createTestRing(t)
	p := NewPoller(rd1, rd2)

	if p.Closed() {
		t.Error("Poller.Closed() should be false with 2 open readers")
	}
	rd1.Close()
	if p.Closed() {
		t.Error("Poller.Closed() should be false with 1 open reader")
	}
	rd2.Close()
	if !p.Closed() {
		t.Error("Poller.Closed() should be true with all readers closed")
	}
}
