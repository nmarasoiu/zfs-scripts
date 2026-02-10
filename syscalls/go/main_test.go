package main

import (
	"testing"
)

func TestParseFocusList_Empty(t *testing.T) {
	old := *focusProcs
	defer func() { *focusProcs = old }()

	*focusProcs = ""
	got := parseFocusList()
	if got != nil {
		t.Errorf("empty string should return nil, got %v", got)
	}
}

func TestParseFocusList_SingleName(t *testing.T) {
	old := *focusProcs
	defer func() { *focusProcs = old }()

	*focusProcs = "tor"
	got := parseFocusList()
	if len(got) != 1 || got[0] != "tor" {
		t.Errorf("got %v, want [tor]", got)
	}
}

func TestParseFocusList_MultipleNames(t *testing.T) {
	old := *focusProcs
	defer func() { *focusProcs = old }()

	*focusProcs = "tor,sshd,proxy"
	got := parseFocusList()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0] != "tor" || got[1] != "sshd" || got[2] != "proxy" {
		t.Errorf("got %v, want [tor sshd proxy]", got)
	}
}

func TestParseFocusList_Dedup(t *testing.T) {
	old := *focusProcs
	defer func() { *focusProcs = old }()

	*focusProcs = "tor,sshd,tor"
	got := parseFocusList()
	if len(got) != 2 {
		t.Errorf("len = %d, want 2 (dedup)", len(got))
	}
}

func TestParseFocusList_TruncatesLongNames(t *testing.T) {
	old := *focusProcs
	defer func() { *focusProcs = old }()

	*focusProcs = "1234567890123456789" // 19 chars, should truncate to 15
	got := parseFocusList()
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if len(got[0]) != 15 {
		t.Errorf("name len = %d, want 15", len(got[0]))
	}
}

func TestParseFocusList_TrimsWhitespace(t *testing.T) {
	old := *focusProcs
	defer func() { *focusProcs = old }()

	*focusProcs = " tor , sshd "
	got := parseFocusList()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0] != "tor" || got[1] != "sshd" {
		t.Errorf("got %v, want [tor sshd]", got)
	}
}

func TestParseFocusList_SkipsEmptyEntries(t *testing.T) {
	old := *focusProcs
	defer func() { *focusProcs = old }()

	*focusProcs = "tor,,sshd,"
	got := parseFocusList()
	if len(got) != 2 {
		t.Errorf("len = %d, want 2 (skip empty)", len(got))
	}
}

// --- parseSize ---

func TestParseSize_Megabytes(t *testing.T) {
	got, err := parseSize("2M")
	if err != nil || got != 2*1024*1024 {
		t.Errorf("parseSize(2M) = %d, %v; want %d", got, err, 2*1024*1024)
	}
}

func TestParseSize_Kilobytes(t *testing.T) {
	got, err := parseSize("512K")
	if err != nil || got != 512*1024 {
		t.Errorf("parseSize(512K) = %d, %v; want %d", got, err, 512*1024)
	}
}

func TestParseSize_WithBSuffix(t *testing.T) {
	got, err := parseSize("4MB")
	if err != nil || got != 4*1024*1024 {
		t.Errorf("parseSize(4MB) = %d, %v; want %d", got, err, 4*1024*1024)
	}
}

func TestParseSize_PlainBytes(t *testing.T) {
	got, err := parseSize("4096")
	if err != nil || got != 4096 {
		t.Errorf("parseSize(4096) = %d, %v; want 4096", got, err)
	}
}

func TestParseSize_InvalidReturnsError(t *testing.T) {
	_, err := parseSize("abc")
	if err == nil {
		t.Error("expected error for invalid size")
	}
}

// --- isPowerOf2 ---

func TestIsPowerOf2(t *testing.T) {
	tests := []struct {
		n    uint32
		want bool
	}{
		{0, false},
		{1, true},
		{2, true},
		{3, false},
		{4, true},
		{4096, true},
		{5000, false},
		{1 << 20, true},
	}
	for _, tt := range tests {
		if got := isPowerOf2(tt.n); got != tt.want {
			t.Errorf("isPowerOf2(%d) = %v, want %v", tt.n, got, tt.want)
		}
	}
}

// --- parsePercentiles ---

func TestParsePercentiles_Default(t *testing.T) {
	qs, err := parsePercentiles(defaultPercentiles)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(qs) != 3 {
		t.Fatalf("len = %d, want 3", len(qs))
	}
	// Should be sorted and in 0-1 range
	for i, q := range qs {
		if q <= 0 || q >= 1 {
			t.Errorf("quantile[%d] = %g, out of (0,1) range", i, q)
		}
		if i > 0 && q < qs[i-1] {
			t.Errorf("quantiles not sorted at index %d", i)
		}
	}
}

func TestParsePercentiles_Simple(t *testing.T) {
	qs, err := parsePercentiles("50,99")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(qs) != 2 {
		t.Fatalf("len = %d, want 2", len(qs))
	}
	if qs[0] != 0.50 || qs[1] != 0.99 {
		t.Errorf("got %v, want [0.5 0.99]", qs)
	}
}

func TestParsePercentiles_InvalidValue(t *testing.T) {
	_, err := parsePercentiles("0,50")
	if err == nil {
		t.Error("expected error for percentile=0")
	}
	_, err = parsePercentiles("50,100")
	if err == nil {
		t.Error("expected error for percentile=100")
	}
}

func TestParsePercentiles_Empty(t *testing.T) {
	_, err := parsePercentiles("")
	if err == nil {
		t.Error("expected error for empty string")
	}
}

// --- quantileHeader ---

func TestQuantileHeader(t *testing.T) {
	tests := []struct {
		q    float64
		want string
	}{
		{0.25, "p25"},
		{0.50, "p50"},
		{0.90, "p90"},
		{0.99, "p99"},
		{0.999, "p99.9"},
	}
	for _, tt := range tests {
		got := quantileHeader(tt.q)
		if got != tt.want {
			t.Errorf("quantileHeader(%g) = %q, want %q", tt.q, got, tt.want)
		}
	}
}
