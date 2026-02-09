package main

import (
	"math"
	"testing"
)

func TestHistogramEmpty(t *testing.T) {
	h := NewHistogram()
	if h.GetTotal() != 0 {
		t.Errorf("empty total = %d, want 0", h.GetTotal())
	}
	if h.GetAverage() != 0 {
		t.Errorf("empty average = %f, want 0", h.GetAverage())
	}
	if h.GetUtilization() != 0 {
		t.Errorf("empty utilization = %f, want 0", h.GetUtilization())
	}
	if h.GetMax() != 0 {
		t.Errorf("empty max = %d, want 0", h.GetMax())
	}
	if h.Percentile(50) != 0 {
		t.Errorf("empty p50 = %f, want 0", h.Percentile(50))
	}
}

func TestHistogramSingleValue(t *testing.T) {
	h := NewHistogram()
	h.Add(5)

	if h.GetTotal() != 1 {
		t.Errorf("total = %d, want 1", h.GetTotal())
	}
	if h.GetAverage() != 5.0 {
		t.Errorf("average = %f, want 5.0", h.GetAverage())
	}
	if h.GetMax() != 5 {
		t.Errorf("max = %d, want 5", h.GetMax())
	}
	// Any percentile of a single value should return that value
	if h.Percentile(0) != 5 {
		t.Errorf("p0 = %f, want 5", h.Percentile(0))
	}
	if h.Percentile(50) != 5 {
		t.Errorf("p50 = %f, want 5", h.Percentile(50))
	}
	if h.Percentile(100) != 5 {
		t.Errorf("p100 = %f, want 5", h.Percentile(100))
	}
}

func TestHistogramUtilization(t *testing.T) {
	h := NewHistogram()
	// 3 zeros and 7 non-zeros = 70% utilization
	for i := 0; i < 3; i++ {
		h.Add(0)
	}
	for i := 0; i < 7; i++ {
		h.Add(1)
	}
	util := h.GetUtilization()
	if util != 70.0 {
		t.Errorf("utilization = %f, want 70.0", util)
	}
}

func TestHistogramPercentiles(t *testing.T) {
	h := NewHistogram()
	// Add 100 samples: values 0..99
	for i := 0; i < 100; i++ {
		h.Add(i)
	}

	// P0 should be ~0
	p0 := h.Percentile(0)
	if p0 != 0 {
		t.Errorf("p0 = %f, want 0", p0)
	}

	// P50 should be ~49.5 (interpolated between 49 and 50)
	p50 := h.Percentile(50)
	if math.Abs(p50-49.5) > 0.6 {
		t.Errorf("p50 = %f, want ~49.5", p50)
	}

	// P99 should be ~98
	p99 := h.Percentile(99)
	if p99 < 97 || p99 > 99 {
		t.Errorf("p99 = %f, want ~98", p99)
	}

	// P100 should be 99
	p100 := h.Percentile(100)
	if p100 != 99 {
		t.Errorf("p100 = %f, want 99", p100)
	}
}

func TestHistogramClamp(t *testing.T) {
	h := NewHistogram()
	h.Add(300) // > 255, should clamp bucket to 255

	if h.GetMax() != 300 {
		t.Errorf("max = %d, want 300 (unbounded)", h.GetMax())
	}
	// Bucket[255] should have the count
	if h.buckets[255] != 1 {
		t.Errorf("bucket[255] = %d, want 1", h.buckets[255])
	}
}

func TestHistogramSnapshot(t *testing.T) {
	h := NewHistogram()
	h.Add(5)
	h.Add(10)

	snap := h.Snapshot()
	if snap.GetTotal() != h.GetTotal() {
		t.Errorf("snapshot total mismatch: %d vs %d", snap.GetTotal(), h.GetTotal())
	}
	if snap.GetAverage() != h.GetAverage() {
		t.Errorf("snapshot average mismatch: %f vs %f", snap.GetAverage(), h.GetAverage())
	}

	// Modifying original shouldn't affect snapshot
	h.Add(100)
	if snap.GetTotal() == h.GetTotal() {
		t.Error("snapshot should be independent of original")
	}
}

func TestHistogramAverage(t *testing.T) {
	h := NewHistogram()
	h.Add(10)
	h.Add(20)
	h.Add(30)

	avg := h.GetAverage()
	if avg != 20.0 {
		t.Errorf("average = %f, want 20.0", avg)
	}
}
