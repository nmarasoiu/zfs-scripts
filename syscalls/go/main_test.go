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
