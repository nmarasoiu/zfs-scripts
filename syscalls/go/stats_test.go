package main

import (
	"testing"
)

func testComm(s string) [16]int8 {
	var c [16]int8
	for i, b := range []byte(s) {
		if i >= 16 {
			break
		}
		c[i] = int8(b)
	}
	return c
}

// --- commToString ---

func TestCommToString_NullTerminated(t *testing.T) {
	got := commToString(testComm("tor"))
	if got != "tor" {
		t.Errorf("commToString = %q, want %q", got, "tor")
	}
}

func TestCommToString_FullBuffer(t *testing.T) {
	got := commToString(testComm("0123456789abcdef"))
	if got != "0123456789abcdef" {
		t.Errorf("commToString = %q, want %q", got, "0123456789abcdef")
	}
}

// --- simpleStats ---

func TestSimpleStats_Empty(t *testing.T) {
	s := newSimpleStats()
	if s.Avg() != 0 {
		t.Errorf("empty Avg = %d, want 0", s.Avg())
	}
	if s.count != 0 {
		t.Errorf("empty count = %d, want 0", s.count)
	}
}

func TestSimpleStats_SingleValue(t *testing.T) {
	s := newSimpleStats()
	s.Record(42)
	if s.max != 42 {
		t.Errorf("max=%d, want 42", s.max)
	}
	if s.Avg() != 42 {
		t.Errorf("Avg = %d, want 42", s.Avg())
	}
	if s.count != 1 {
		t.Errorf("count = %d, want 1", s.count)
	}
}

func TestSimpleStats_MaxAvg(t *testing.T) {
	s := newSimpleStats()
	for _, v := range []int64{10, 20, 30, 40, 50} {
		s.Record(v)
	}
	if s.max != 50 {
		t.Errorf("max = %d, want 50", s.max)
	}
	if s.Avg() != 30 {
		t.Errorf("Avg = %d, want 30", s.Avg())
	}
	if s.count != 5 {
		t.Errorf("count = %d, want 5", s.count)
	}
}

// --- syscallStats ---

func TestSyscallStats_RecordUpdatesSketchAndStats(t *testing.T) {
	ss := newTestSyscallStats(0.25)
	testRecord(ss, 100)
	testRecord(ss, 200)
	testRecord(ss, 300)

	if ss.stats.count != 3 {
		t.Errorf("count = %d, want 3", ss.stats.count)
	}
	if ss.stats.max != 300 {
		t.Errorf("max = %d, want 300", ss.stats.max)
	}

	// DDSketch should have recorded 3 values
	p50, _ := ss.sketch.GetValueAtQuantile(0.50)
	if p50 < 150 || p50 > 250 {
		t.Errorf("p50 = %.0f, expected near 200", p50)
	}
}

func TestSketchPercentiles_Monotonic(t *testing.T) {
	ss := newTestSyscallStats(0.25)
	for i := int64(1); i <= 1000; i++ {
		testRecord(ss, i)
	}
	d := &Display{quantiles: []float64{0.25, 0.50, 0.75, 0.90, 0.99, 0.999}}
	pcts := d.sketchPercentiles(ss.sketch)
	for i := 1; i < len(pcts); i++ {
		if pcts[i-1] > pcts[i] {
			t.Errorf("percentiles not monotonic at index %d: %d > %d", i, pcts[i-1], pcts[i])
		}
	}
}

// --- State ---

func TestState_RecordBatchAndRead(t *testing.T) {
	state := newState(100, 0.25)
	batch := []pendingEvent{
		{testComm("tor"), 0, 100},  // read
		{testComm("tor"), 0, 200},  // read
		{testComm("tor"), 1, 50},   // write
		{testComm("sshd"), 0, 300}, // read
	}
	state.RecordBatch(batch)

	state.Read(func(v StateView) {
		if v.NSketches != 3 {
			t.Errorf("NSketches = %d, want 3", v.NSketches)
		}
		if v.GlobalStats.count != 4 {
			t.Errorf("global count = %d, want 4", v.GlobalStats.count)
		}

		torSyscalls := v.ProcStats["tor"]
		if torSyscalls == nil {
			t.Fatal("missing tor in ProcStats")
		}
		torRead := torSyscalls[0]
		if torRead == nil {
			t.Fatal("missing tor/read in ProcStats")
		}
		if torRead.stats.count != 2 {
			t.Errorf("tor/read count = %d, want 2", torRead.stats.count)
		}

		sshdSyscalls := v.ProcStats["sshd"]
		if sshdSyscalls == nil {
			t.Fatal("missing sshd in ProcStats")
		}
		sshdRead := sshdSyscalls[0]
		if sshdRead.stats.count != 1 {
			t.Errorf("sshd/read count = %d, want 1", sshdRead.stats.count)
		}
	})
}

func TestState_LRUEviction(t *testing.T) {
	state := newState(2, 0.25) // only 2 sketches allowed
	state.RecordBatch([]pendingEvent{
		{testComm("a"), 0, 10},
		{testComm("b"), 1, 20},
		{testComm("c"), 2, 30}, // should evict "a"/0 sketch
	})

	state.Read(func(v StateView) {
		if v.NSketches != 2 {
			t.Errorf("NSketches = %d, want 2", v.NSketches)
		}
		if v.NStats != 3 {
			t.Errorf("NStats = %d, want 3 (all entries persist)", v.NStats)
		}
		if v.SketchEvictions != 1 {
			t.Errorf("evictions = %d, want 1", v.SketchEvictions)
		}
		// "a" stats persist but sketch is evicted
		aStats := v.ProcStats["a"]
		if aStats == nil {
			t.Fatal("expected 'a' to be present (stats persist)")
		}
		aSyscall := aStats[0]
		if aSyscall == nil {
			t.Fatal("expected 'a' syscall 0 to be present")
		}
		if aSyscall.sketch != nil {
			t.Error("expected 'a' sketch to be nil (evicted)")
		}
		if aSyscall.stats.count != 1 {
			t.Errorf("'a' count = %d, want 1", aSyscall.stats.count)
		}
		// "b" and "c" have both stats and sketches
		if v.ProcStats["b"] == nil {
			t.Error("expected 'b' to be present")
		}
		if v.ProcStats["b"][1].sketch == nil {
			t.Error("expected 'b' sketch to be present")
		}
		if v.ProcStats["c"] == nil {
			t.Error("expected 'c' to be present")
		}
	})
}

func TestState_GlobalStatsTrackAllEvents(t *testing.T) {
	state := newState(100, 0.25)
	state.RecordBatch([]pendingEvent{
		{testComm("p1"), 0, 5},
		{testComm("p2"), 1, 15},
	})
	state.RecordBatch([]pendingEvent{
		{testComm("p1"), 0, 25},
	})

	state.Read(func(v StateView) {
		if v.GlobalStats.count != 3 {
			t.Errorf("global count = %d, want 3", v.GlobalStats.count)
		}
		if v.GlobalStats.max != 25 {
			t.Errorf("global max = %d, want 25", v.GlobalStats.max)
		}
	})
}
