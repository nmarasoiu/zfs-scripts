package main

import (
	"testing"
	"time"
)

func TestFormatLatency(t *testing.T) {
	tests := []struct {
		us   int64
		want string
	}{
		{0, "0µs"},
		{1, "1µs"},
		{999, "999µs"},
		{99_999, "99999µs"},
		{100_000, "100ms"},
		{500_000, "500ms"},
		{999_500, "1000ms"},
		{1_000_000, "1.0s"},
		{1_500_000, "1.5s"},
		{60_000_000, "60.0s"},
	}
	for _, tt := range tests {
		got := formatLatency(tt.us)
		if got != tt.want {
			t.Errorf("formatLatency(%d) = %q, want %q", tt.us, got, tt.want)
		}
	}
}

func TestFormatCount(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1_000, "1.0K"},
		{1_500, "1.5K"},
		{1_000_000, "1.0M"},
		{2_500_000, "2.5M"},
		{1_000_000_000, "1.0B"},
	}
	for _, tt := range tests {
		got := formatCount(tt.n)
		if got != tt.want {
			t.Errorf("formatCount(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.0K"},
		{1024 * 1024, "1.0M"},
		{1024 * 1024 * 1024, "1.0G"},
		{3 * 1024 * 1024 * 1024, "3.0G"},
	}
	for _, tt := range tests {
		got := formatBytes(tt.n)
		if got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0.0s"},
		{30 * time.Second, "30.0s"},
		{59*time.Second + 900*time.Millisecond, "59.9s"},
		{2*time.Minute + 30*time.Second, "2m30s"},
		{59*time.Minute + 59*time.Second, "59m59s"},
		{time.Hour, "1h0m"},
		{2*time.Hour + 30*time.Minute, "2h30m"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestFormatMicro(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{50 * time.Microsecond, "50µs"},
		{999 * time.Microsecond, "999µs"},
		{time.Millisecond, "1.0ms"},
		{1500 * time.Microsecond, "1.5ms"},
		{time.Second, "1.0s"},
		{2500 * time.Millisecond, "2.5s"},
	}
	for _, tt := range tests {
		got := formatMicro(tt.d)
		if got != tt.want {
			t.Errorf("formatMicro(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestFormatRate(t *testing.T) {
	tests := []struct {
		count uint64
		secs  float64
		want  string
	}{
		{0, 1.0, "-"},
		{100, 0, "-"},
		{100, -1.0, "-"},
		{1, 10, "0.1/s"},
		{1000, 1.0, "1.0K/s"},
		{1_000_000, 1.0, "1.0M/s"},
	}
	for _, tt := range tests {
		got := formatRate(tt.count, tt.secs)
		if got != tt.want {
			t.Errorf("formatRate(%d, %.1f) = %q, want %q", tt.count, tt.secs, got, tt.want)
		}
	}
}

func TestAdvanceCols(t *testing.T) {
	tests := []struct {
		s       string
		maxCols int
		wantOff int
		wantCol int
	}{
		{"hello", 3, 3, 3},
		{"hello", 10, 5, 5},
		{"abc", 3, 3, 3},
		{"", 5, 0, 0},
		{"héllo", 3, 4, 3}, // é is 2 bytes
	}
	for _, tt := range tests {
		off, cols := advanceCols(tt.s, tt.maxCols)
		if off != tt.wantOff || cols != tt.wantCol {
			t.Errorf("advanceCols(%q, %d) = (%d, %d), want (%d, %d)",
				tt.s, tt.maxCols, off, cols, tt.wantOff, tt.wantCol)
		}
	}
}

func TestPadOrTrunc(t *testing.T) {
	tests := []struct {
		s     string
		width int
		want  string
	}{
		{"hi", 5, "hi   "},
		{"hello world", 5, "hello"},
		{"abc", 3, "abc"},
	}
	for _, tt := range tests {
		got := padOrTrunc(tt.s, tt.width)
		if got != tt.want {
			t.Errorf("padOrTrunc(%q, %d) = %q, want %q", tt.s, tt.width, got, tt.want)
		}
	}
}

func TestDisplayWidth(t *testing.T) {
	tests := []struct {
		s    string
		want int
	}{
		{"hello", 5},
		{"", 0},
		{"héllo", 5},    // é is 1 display column
		{"── title", 8}, // ─ is 1 display column (3 UTF-8 bytes)
		{"abc│def", 7},  // │ is 1 display column
	}
	for _, tt := range tests {
		got := displayWidth(tt.s)
		if got != tt.want {
			t.Errorf("displayWidth(%q) = %d, want %d", tt.s, got, tt.want)
		}
	}
}

func TestFormatLatencyPadded(t *testing.T) {
	got := formatLatencyPadded(42)
	if got != "    42µs" {
		t.Errorf("formatLatencyPadded(42) = %q, want %q", got, "    42µs")
	}
	// 8 display columns: 4 spaces + "42" + "µs" (µ is 2 bytes but 1 display column)
	if displayWidth(got) != 8 {
		t.Errorf("formatLatencyPadded(42) display width = %d, want 8", displayWidth(got))
	}
}
