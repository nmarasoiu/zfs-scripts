package main

import "testing"

func TestFormatTotal(t *testing.T) {
	tests := []struct {
		input uint64
		want  string
	}{
		{0, "0.0s"},
		{1000000, "1.0s"},
		{59000000, "59.0s"},
		{60000000, "1.0m"},
		{3599000000, "60.0m"},
		{3600000000, "1.0h"},
		{7200000000, "2.0h"},
	}
	for _, tt := range tests {
		got := formatTotal(tt.input)
		if got != tt.want {
			t.Errorf("formatTotal(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatPct(t *testing.T) {
	tests := []struct {
		val  float64
		want string
	}{
		{0.0, "  0.00%"},
		{50.5, " 50.50%"},
		{100.0, "100.00%"},
		{99.99, " 99.99%"},
	}
	for _, tt := range tests {
		got := formatPct(tt.val)
		if got != tt.want {
			t.Errorf("formatPct(%v) = %q, want %q", tt.val, got, tt.want)
		}
	}
}
