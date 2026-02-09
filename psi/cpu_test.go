package main

import "testing"

func TestParseCpuLine(t *testing.T) {
	tests := []struct {
		line   string
		want   cpuTick
		wantOk bool
	}{
		{
			"cpu  1000 200 300 5000 100 50 25 10",
			cpuTick{1000, 200, 300, 5000, 100, 50, 25, 10},
			true,
		},
		{
			"cpu0 500 100 150 2500 50 25 12 5",
			cpuTick{500, 100, 150, 2500, 50, 25, 12, 5},
			true,
		},
		{
			"cpu  0 0 0 0 0 0 0 0",
			cpuTick{},
			true,
		},
		{
			"cpu  too few",
			cpuTick{},
			false,
		},
		{
			"cpu  1 2 3 notanumber 5 6 7 8",
			cpuTick{},
			false,
		},
	}
	for _, tt := range tests {
		got, ok := parseCpuLine(tt.line)
		if ok != tt.wantOk {
			t.Errorf("parseCpuLine(%q) ok=%v, want %v", tt.line, ok, tt.wantOk)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("parseCpuLine(%q) = %+v, want %+v", tt.line, got, tt.want)
		}
	}
}

func TestCpuTickTotal(t *testing.T) {
	tick := cpuTick{1000, 200, 300, 5000, 100, 50, 25, 10}
	if got := tick.total(); got != 6685 {
		t.Errorf("total() = %d, want 6685", got)
	}
}

func TestCpuTickBusy(t *testing.T) {
	tick := cpuTick{1000, 200, 300, 5000, 100, 50, 25, 10}
	// busy = total - idle - iowait = 6685 - 5000 - 100 = 1585
	if got := tick.busy(); got != 1585 {
		t.Errorf("busy() = %d, want 1585", got)
	}
}

func TestCpuTrackerUpdate(t *testing.T) {
	tr := newCpuTracker()

	// First update: no delta yet
	tr.update(cpuTick{100, 0, 50, 800, 50, 0, 0, 0})
	if tr.count != 0 {
		t.Errorf("after first update, count = %d, want 0", tr.count)
	}

	// Second update: delta = 100 busy out of 200 total = 50%
	tr.update(cpuTick{200, 0, 100, 900, 100, 0, 0, 0})
	if tr.count != 1 {
		t.Errorf("after second update, count = %d, want 1", tr.count)
	}
	if tr.cur < 49.9 || tr.cur > 50.1 {
		t.Errorf("cur = %.2f, want ~50.0", tr.cur)
	}
}

func TestCpuTrackerAvg(t *testing.T) {
	tr := newCpuTracker()
	if tr.avg() != 0 {
		t.Errorf("avg of empty tracker = %f, want 0", tr.avg())
	}
}
