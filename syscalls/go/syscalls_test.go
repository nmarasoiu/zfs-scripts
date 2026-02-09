package main

import (
	"strings"
	"testing"
)

func TestSyscallName_KnownSyscalls(t *testing.T) {
	tests := []struct {
		id   uint32
		want string
	}{
		{0, "read"},
		{1, "write"},
		{2, "open"},
		{3, "close"},
		{9, "mmap"},
		{202, "futex"},
		{281, "epoll_pwait"},
		{321, "bpf"},
	}
	for _, tt := range tests {
		got := syscallName(tt.id)
		if got != tt.want {
			t.Errorf("syscallName(%d) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestSyscallName_UnknownReturnsNumeric(t *testing.T) {
	got := syscallName(9999)
	if !strings.HasPrefix(got, "sys_") {
		t.Errorf("syscallName(9999) = %q, want prefix 'sys_'", got)
	}
}

func TestSyscallName_GapInTable(t *testing.T) {
	// Syscall 156 is not defined (gap between 155 and 157)
	got := syscallName(156)
	if !strings.HasPrefix(got, "sys_") {
		t.Errorf("syscallName(156) = %q, want prefix 'sys_' for gap", got)
	}
}
