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

// --- DDSketch count/sum/avg/max ---

func TestSketch_CountSumAvg(t *testing.T) {
	sk := newTestSketch(0.25)
	sk.Add(10)
	sk.Add(20)
	sk.Add(30)

	if uint64(sk.GetCount()) != 3 {
		t.Errorf("count = %d, want 3", uint64(sk.GetCount()))
	}
	// GetSum() is approximate (bucket centroids), within ±alpha per value
	sum := sk.GetSum()
	if sum < 45 || sum > 75 {
		t.Errorf("sum = %f, want ~60 (within ±25%%)", sum)
	}
	avg := sum / sk.GetCount()
	if avg < 15 || avg > 25 {
		t.Errorf("avg = %f, want ~20 (within ±25%%)", avg)
	}
}

func TestSketch_MaxValue(t *testing.T) {
	sk := newTestSketch(0.25)
	for _, v := range []float64{10, 20, 30, 40, 50} {
		sk.Add(v)
	}
	maxVal, err := sk.GetMaxValue()
	if err != nil {
		t.Fatalf("GetMaxValue error: %v", err)
	}
	// DDSketch max is approximate within ±alpha
	if maxVal < 40 || maxVal > 65 {
		t.Errorf("max = %f, expected near 50 (within ±25%%)", maxVal)
	}
}

func TestSketchPercentiles_Monotonic(t *testing.T) {
	sk := newTestSketch(0.25)
	for i := int64(1); i <= 1000; i++ {
		sk.Add(float64(i))
	}
	d := &Display{quantiles: []float64{0.25, 0.50, 0.75, 0.90, 0.99, 0.999}}
	pcts := d.sketchPercentiles(sk)
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

		torSyscalls := v.ProcStats["tor"]
		if torSyscalls == nil {
			t.Fatal("missing tor in ProcStats")
		}
		torRead := torSyscalls[0]
		if torRead == nil {
			t.Fatal("missing tor/read in ProcStats")
		}
		if uint64(torRead.GetCount()) != 2 {
			t.Errorf("tor/read count = %d, want 2", uint64(torRead.GetCount()))
		}

		sshdSyscalls := v.ProcStats["sshd"]
		if sshdSyscalls == nil {
			t.Fatal("missing sshd in ProcStats")
		}
		sshdRead := sshdSyscalls[0]
		if uint64(sshdRead.GetCount()) != 1 {
			t.Errorf("sshd/read count = %d, want 1", uint64(sshdRead.GetCount()))
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
		if v.SketchEvictions != 1 {
			t.Errorf("evictions = %d, want 1", v.SketchEvictions)
		}
		// "a" is evicted from LRU — no longer in ProcStats
		if v.ProcStats["a"] != nil {
			t.Error("expected 'a' to be absent (evicted from LRU)")
		}
		// "b" and "c" should be present
		if v.ProcStats["b"] == nil {
			t.Error("expected 'b' to be present")
		}
		if v.ProcStats["b"][1] == nil {
			t.Error("expected 'b' sketch to be present")
		}
		if v.ProcStats["c"] == nil {
			t.Error("expected 'c' to be present")
		}
	})
}

func TestState_ProcCountersSurviveEviction(t *testing.T) {
	state := newState(2, 0.25) // only 2 sketches allowed
	state.RecordBatch([]pendingEvent{
		{testComm("a"), 0, 10},
		{testComm("b"), 1, 20},
		{testComm("c"), 2, 30}, // evicts a sketch from cache
	})

	state.Read(func(v StateView) {
		// "a" is evicted from sketch cache
		if v.ProcStats["a"] != nil {
			t.Error("expected 'a' to be absent from sketch cache (evicted)")
		}
		// But "a" survives in persistent procCounters
		pcA := v.ProcCounters["a"]
		if pcA == nil {
			t.Fatal("expected 'a' in ProcCounters (survives eviction)")
		}
		if pcA.count != 1 {
			t.Errorf("a counter count = %d, want 1", pcA.count)
		}
		if pcA.sum != 10 {
			t.Errorf("a counter sum = %d, want 10", pcA.sum)
		}
	})
}

func TestState_ProcCountersAccumulateAcrossSyscalls(t *testing.T) {
	state := newState(100, 0.25)
	state.RecordBatch([]pendingEvent{
		{testComm("tor"), 0, 100},  // read
		{testComm("tor"), 1, 200},  // write
		{testComm("tor"), 0, 300},  // read again
		{testComm("sshd"), 0, 50},
	})

	state.Read(func(v StateView) {
		torPC := v.ProcCounters["tor"]
		if torPC == nil {
			t.Fatal("missing tor in ProcCounters")
		}
		if torPC.count != 3 {
			t.Errorf("tor count = %d, want 3", torPC.count)
		}
		if torPC.sum != 600 {
			t.Errorf("tor sum = %d, want 600", torPC.sum)
		}

		sshdPC := v.ProcCounters["sshd"]
		if sshdPC == nil {
			t.Fatal("missing sshd in ProcCounters")
		}
		if sshdPC.count != 1 {
			t.Errorf("sshd count = %d, want 1", sshdPC.count)
		}
		if sshdPC.sum != 50 {
			t.Errorf("sshd sum = %d, want 50", sshdPC.sum)
		}
	})
}

func TestState_RecordBatchPreservesLRUOrder(t *testing.T) {
	state := newState(2, 0.25)
	// Add a, b
	state.RecordBatch([]pendingEvent{
		{testComm("a"), 0, 10},
		{testComm("b"), 1, 20},
	})
	// Touch a again (promotes to most-recent), then add c (evicts b)
	state.RecordBatch([]pendingEvent{
		{testComm("a"), 0, 15},
		{testComm("c"), 2, 30},
	})

	state.Read(func(v StateView) {
		if v.NSketches != 2 {
			t.Errorf("NSketches = %d, want 2", v.NSketches)
		}
		// "b" should be evicted (a was promoted by the second batch)
		if v.ProcStats["b"] != nil {
			t.Error("expected 'b' to be absent (evicted)")
		}
		if v.ProcStats["a"] == nil {
			t.Error("expected 'a' to be present (was promoted)")
		}
		if v.ProcStats["c"] == nil {
			t.Error("expected 'c' to be present")
		}
		// "a" should have both samples
		if v.ProcStats["a"] != nil {
			count := uint64(v.ProcStats["a"][0].GetCount())
			if count != 2 {
				t.Errorf("a count = %d, want 2", count)
			}
		}
	})
}
