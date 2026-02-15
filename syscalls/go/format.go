package main

import (
	"fmt"
	"strings"
	"time"
)

func formatLatency(ns int64) string {
	if ns < 1_000 {
		return fmt.Sprintf("%dns", ns)
	}
	us := ns / 1000
	if us < 10_000 {
		return fmt.Sprintf("%dµs", us)
	}
	if us < 1_000_000 {
		ms := (us + 500) / 1000
		return fmt.Sprintf("%dms", ms)
	}
	s := float64(us) / 1_000_000
	if s < 1000 {
		return fmt.Sprintf("%.1fs", s)
	}
	m := s / 60
	if m < 100 {
		return fmt.Sprintf("%.1fm", m)
	}
	h := m / 60
	return fmt.Sprintf("%.1fh", h)
}

func formatLatencyPadded(ns int64) string {
	return fmt.Sprintf("%8s", formatLatency(ns))
}

func formatCount(n int64) string {
	if n >= 1_000_000_000 {
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	}
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

func formatBytes(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	}
	if n >= 1<<20 {
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	}
	if n >= 1<<10 {
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm%ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh%dm", h, m)
}

func formatRate(count uint64, secs float64) string {
	if secs <= 0 || count == 0 {
		return "-"
	}
	rate := float64(count) / secs
	if rate < 1 {
		return fmt.Sprintf("%.1f/s", rate)
	}
	return formatCount(int64(rate)) + "/s"
}

// advanceCols walks s up to maxCols display columns, returning the byte offset
// and column count reached. Each rune counts as 1 column.
func advanceCols(s string, maxCols int) (byteOff, cols int) {
	for byteOff < len(s) && cols < maxCols {
		if s[byteOff] < 0x80 {
			byteOff++
		} else {
			byteOff++
			for byteOff < len(s) && s[byteOff]&0xC0 == 0x80 {
				byteOff++
			}
		}
		cols++
	}
	return
}

// displayWidth returns the number of display columns a string occupies.
func displayWidth(s string) int {
	_, cols := advanceCols(s, len(s)) // len(s) >= rune count, so walks all
	return cols
}

// truncateProcLabel shortens the process part of a "proc/syscall" label so the
// total length fits within maxWidth. The syscall portion is never truncated.
func truncateProcLabel(label string, maxWidth int) string {
	if len(label) <= maxWidth {
		return label
	}
	slash := strings.Index(label, "/")
	if slash < 0 {
		return label[:maxWidth]
	}
	syscall := label[slash:] // includes "/"
	procMax := maxWidth - len(syscall)
	if procMax <= 0 {
		return label[:maxWidth]
	}
	return label[:procMax] + syscall
}

// padOrTrunc pads with spaces or truncates to exactly width display columns.
func padOrTrunc(s string, width int) string {
	off, cols := advanceCols(s, width)
	if cols >= width {
		return s[:off]
	}
	return s + strings.Repeat(" ", width-cols)
}
