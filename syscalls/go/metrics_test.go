package main

import (
	"strings"
	"testing"
)

// --- ringOccupancy ---

func TestRingOccupancy_Empty(t *testing.T) {
	ro := newRingOccupancy(0.01)
	if ro.avg() != 0 {
		t.Errorf("empty avg = %d, want 0", ro.avg())
	}
	if ro.p99() != 0 {
		t.Errorf("empty p99 = %d, want 0", ro.p99())
	}
}

func TestRingOccupancy_SingleSample(t *testing.T) {
	ro := newRingOccupancy(0.01)
	ro.add(100)
	if ro.avg() != 100 {
		t.Errorf("avg = %d, want 100", ro.avg())
	}
}

func TestRingOccupancy_MultipleSamples(t *testing.T) {
	ro := newRingOccupancy(0.01)
	ro.add(10)
	ro.add(20)
	ro.add(30)
	if ro.avg() != 20 {
		t.Errorf("avg = %d, want 20", ro.avg())
	}
}

func TestRingOccupancy_P99(t *testing.T) {
	ro := newRingOccupancy(0.01)
	for i := 1; i <= 100; i++ {
		ro.add(i * 1000)
	}
	p99 := ro.p99()
	// p99 should be close to 99000 (within DDSketch accuracy)
	if p99 < 95000 || p99 > 103000 {
		t.Errorf("p99 = %d, want ~99000", p99)
	}
}

// --- infraSketches ---

func TestInfraSketches_Empty(t *testing.T) {
	is := newInfraSketches(0.01)
	is.Read(func(iv infraView) {
		if iv.RingAll.GetCount() != 0 {
			t.Errorf("ringAll count = %g, want 0", iv.RingAll.GetCount())
		}
		if iv.SleepAll.GetCount() != 0 {
			t.Errorf("sleepAll count = %g, want 0", iv.SleepAll.GetCount())
		}
	})
}

func TestInfraSketches_Record(t *testing.T) {
	is := newInfraSketches(0.01)
	cap := 2 * 1024 * 1024 // 2MB

	// Simulate 100 drain cycles
	for i := 0; i < 100; i++ {
		maxPending := cap / 100 * (i + 1) // 1% to 100%
		avgPending := maxPending / 2
		sleepNs := int64(50_000) // 50µs
		if i%10 == 0 {
			sleepNs = 0 // busy-poll every 10th cycle
		}
		is.Record(maxPending, avgPending, cap, sleepNs)
	}

	is.Read(func(iv infraView) {
		if iv.RingAll.GetCount() != 100 {
			t.Errorf("ringAll count = %g, want 100", iv.RingAll.GetCount())
		}
		if iv.RingMax.GetCount() != 100 {
			t.Errorf("ringMax count = %g, want 100", iv.RingMax.GetCount())
		}
		if iv.SleepAll.GetCount() != 100 {
			t.Errorf("sleepAll count = %g, want 100", iv.SleepAll.GetCount())
		}
		// 10 of 100 cycles were busy-poll (sleepNs=0)
		if iv.SleepPure.GetCount() != 90 {
			t.Errorf("sleepPure count = %g, want 90", iv.SleepPure.GetCount())
		}
		// Ring max ranges from 1% to 100%; p50 should be ~50%
		p50, _ := iv.RingMax.GetValueAtQuantile(0.50)
		if p50 < 30 || p50 > 70 {
			t.Errorf("ringMax p50 = %.1f%%, want ~50%%", p50)
		}
	})
}

// --- ringStats ---

func TestRingStats_FormatUsage(t *testing.T) {
	rs := ringStats{avg: 4096, p99: 6144, max: 8192, cap: 8 * 1024 * 1024}
	s := rs.formatUsage(formatBytes)
	if !strings.Contains(s, "avg:") {
		t.Errorf("missing 'avg:' in %q", s)
	}
	if !strings.Contains(s, "p99:") {
		t.Errorf("missing 'p99:' in %q", s)
	}
	if !strings.Contains(s, "max:") {
		t.Errorf("missing 'max:' in %q", s)
	}
	if !strings.Contains(s, "%") {
		t.Errorf("missing percentage in %q", s)
	}
}
