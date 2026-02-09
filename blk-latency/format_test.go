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
		{0, "0us"},
		{500, "500us"},
		{99_999, "99999us"},
		{100_000, "100ms"},
		{500_000, "500ms"},
		{1_000_000, "1.0s"},
		{2_500_000, "2.5s"},
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
		{1_000_000, "1.0M"},
		{1_000_000_000, "1.0B"},
	}
	for _, tt := range tests {
		got := formatCount(tt.n)
		if got != tt.want {
			t.Errorf("formatCount(%d) = %q, want %q", tt.n, got, tt.want)
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
		{2*time.Minute + 30*time.Second, "2m30s"},
		{2*time.Hour + 15*time.Minute, "2h15m"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestDeviceEncoding(t *testing.T) {
	tests := []struct {
		major, minor uint32
	}{
		{8, 0},
		{8, 32},
		{259, 0},
		{259, 1},
		{0, 0},
	}
	for _, tt := range tests {
		dev := majorMinorToDev(tt.major, tt.minor)
		gotMaj, gotMin := devToMajorMinor(dev)
		if gotMaj != tt.major || gotMin != tt.minor {
			t.Errorf("roundtrip(%d, %d) -> dev=%d -> (%d, %d)",
				tt.major, tt.minor, dev, gotMaj, gotMin)
		}
	}
}

func TestIsTrackedDevice(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"sda", true},
		{"sdb1", true},
		{"nvme0n1", true},
		{"nvme0n1p1", true},
		{"dm-0", false},
		{"loop0", false},
		{"8:0", false},
	}
	for _, tt := range tests {
		got := isTrackedDevice(tt.name)
		if got != tt.want {
			t.Errorf("isTrackedDevice(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}
