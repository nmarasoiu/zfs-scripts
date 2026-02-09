package main

import "testing"

func TestParseLine(t *testing.T) {
	tests := []struct {
		line string
		want pressure
	}{
		{
			"some avg10=0.00 avg60=0.00 avg300=0.00 total=123456",
			pressure{"0.00", "0.00", "0.00", "123456"},
		},
		{
			"full avg10=12.34 avg60=5.67 avg300=1.23 total=9999999",
			pressure{"12.34", "5.67", "1.23", "9999999"},
		},
		{
			"some avg10=100.00 avg60=99.99 avg300=50.00 total=0",
			pressure{"100.00", "99.99", "50.00", "0"},
		},
		{
			"some nofields",
			pressure{},
		},
	}
	for _, tt := range tests {
		got := parseLine(tt.line)
		if got != tt.want {
			t.Errorf("parseLine(%q) = %+v, want %+v", tt.line, got, tt.want)
		}
	}
}

func TestFormatTotal(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"0", "0.0s"},
		{"1000000", "1.0s"},
		{"59000000", "59.0s"},
		{"60000000", "1.0m"},
		{"3599000000", "60.0m"},
		{"3600000000", "1.0h"},
		{"7200000000", "2.0h"},
	}
	for _, tt := range tests {
		got := formatTotal(tt.input)
		if got != tt.want {
			t.Errorf("formatTotal(%q) = %q, want %q", tt.input, got, tt.want)
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
