package psiparse

import "testing"

func TestParseLine(t *testing.T) {
	tests := []struct {
		line string
		want Pressure
	}{
		{
			"some avg10=0.00 avg60=0.00 avg300=0.00 total=123456",
			Pressure{0.00, 0.00, 0.00, 123456},
		},
		{
			"full avg10=12.34 avg60=5.67 avg300=1.23 total=9999999",
			Pressure{12.34, 5.67, 1.23, 9999999},
		},
		{
			"some avg10=100.00 avg60=99.99 avg300=50.00 total=0",
			Pressure{100.00, 99.99, 50.00, 0},
		},
		{
			"some nofields",
			Pressure{},
		},
	}
	for _, tt := range tests {
		got := parseLine(tt.line)
		if got != tt.want {
			t.Errorf("parseLine(%q) = %+v, want %+v", tt.line, got, tt.want)
		}
	}
}
