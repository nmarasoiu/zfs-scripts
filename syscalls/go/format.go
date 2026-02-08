package main

import (
	"fmt"
	"time"
)

func formatLatency(us int64) string {
	if us < 100_000 {
		return fmt.Sprintf("%dµs", us)
	}
	if us < 1_000_000 {
		ms := (us + 500) / 1000
		return fmt.Sprintf("%dms", ms)
	}
	s := float64(us) / 1_000_000
	return fmt.Sprintf("%.1fs", s)
}

func formatLatencyPadded(us int64) string {
	return fmt.Sprintf("%8s", formatLatency(us))
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

func formatMicro(d time.Duration) string {
	us := d.Microseconds()
	if us < 1000 {
		return fmt.Sprintf("%dµs", us)
	}
	if us < 1000000 {
		return fmt.Sprintf("%.1fms", float64(us)/1000)
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
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
