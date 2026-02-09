package main

import (
	"strings"
	"testing"
)

// --- ringAvg ---

func TestRingAvg_Empty(t *testing.T) {
	var ra ringAvg
	if ra.avg() != 0 {
		t.Errorf("empty avg = %d, want 0", ra.avg())
	}
}

func TestRingAvg_SingleSample(t *testing.T) {
	var ra ringAvg
	ra.add(100)
	if ra.avg() != 100 {
		t.Errorf("avg = %d, want 100", ra.avg())
	}
}

func TestRingAvg_MultipleSamples(t *testing.T) {
	var ra ringAvg
	ra.add(10)
	ra.add(20)
	ra.add(30)
	if ra.avg() != 20 {
		t.Errorf("avg = %d, want 20", ra.avg())
	}
}

// --- capacityStats ---

func TestCapacityStats_FormatUsage(t *testing.T) {
	cs := capacityStats{avg: 4096, max: 8192, cap: 8 * 1024 * 1024}
	s := cs.formatUsage()
	if !strings.Contains(s, "avg:") {
		t.Errorf("missing 'avg:' in %q", s)
	}
	if !strings.Contains(s, "max:") {
		t.Errorf("missing 'max:' in %q", s)
	}
	if !strings.Contains(s, "%") {
		t.Errorf("missing percentage in %q", s)
	}
}
