package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"syscall"
)

// cpuTick holds raw jiffy counters from a /proc/stat cpu line.
type cpuTick struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (t cpuTick) total() uint64 {
	return t.user + t.nice + t.system + t.idle + t.iowait + t.irq + t.softirq + t.steal
}

func (t cpuTick) busy() uint64 {
	return t.total() - t.idle - t.iowait
}

// cpuTracker accumulates utilization stats for one core (or the aggregate).
type cpuTracker struct {
	buckets [10]int // [0]=0–10%, [1]=10–20%, ..., [9]=90–100%
	min     float64
	max     float64
	sum     float64
	count   int
	cur     float64
	prev    cpuTick
	hasPrev bool
}

func newCpuTracker() *cpuTracker {
	return &cpuTracker{min: 101} // >100 means no data yet
}

// update computes utilization from delta jiffies and feeds the buckets.
func (t *cpuTracker) update(tick cpuTick) {
	if !t.hasPrev {
		t.prev = tick
		t.hasPrev = true
		return
	}
	dtotal := float64(tick.total() - t.prev.total())
	dbusy := float64(tick.busy() - t.prev.busy())
	t.prev = tick

	if dtotal == 0 {
		t.cur = 0
	} else {
		t.cur = (dbusy / dtotal) * 100.0
	}

	idx := int(t.cur / 10)
	if idx > 9 {
		idx = 9
	}
	t.buckets[idx]++

	t.sum += t.cur
	t.count++
	if t.cur < t.min {
		t.min = t.cur
	}
	if t.cur > t.max {
		t.max = t.cur
	}
}

func (t *cpuTracker) avg() float64 {
	if t.count == 0 {
		return 0
	}
	return t.sum / float64(t.count)
}

// cdf returns the percentage of samples with utilization < threshold*10%.
func (t *cpuTracker) cdf(threshold int) float64 {
	if t.count == 0 {
		return 0
	}
	var cum int
	for i := 0; i < threshold; i++ {
		cum += t.buckets[i]
	}
	return float64(cum) / float64(t.count) * 100
}

// cpuState holds fd and per-core trackers for /proc/stat.
type cpuState struct {
	fd    int
	buf   [4096]byte
	all   *cpuTracker
	cores []*cpuTracker
	ready bool // true after first update (need two ticks to diff)
}

func newCpuState() *cpuState {
	fd, err := syscall.Open("/proc/stat", syscall.O_RDONLY, 0)
	if err != nil {
		return nil
	}
	return &cpuState{fd: fd, all: newCpuTracker()}
}

func parseCpuLine(line string) (cpuTick, bool) {
	fields := strings.Fields(line)
	if len(fields) < 8 {
		return cpuTick{}, false
	}
	vals := make([]uint64, 8)
	for i := 0; i < 8; i++ {
		v, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return cpuTick{}, false
		}
		vals[i] = v
	}
	return cpuTick{vals[0], vals[1], vals[2], vals[3], vals[4], vals[5], vals[6], vals[7]}, true
}

func (cs *cpuState) update() {
	n := pread(cs.fd, cs.buf[:])
	if n <= 0 {
		return
	}

	hadPrev := cs.all.hasPrev

	for _, line := range strings.Split(string(cs.buf[:n]), "\n") {
		if strings.HasPrefix(line, "cpu ") {
			if tick, ok := parseCpuLine(line); ok {
				cs.all.update(tick)
			}
		} else if strings.HasPrefix(line, "cpu") {
			// "cpu0 ...", "cpu1 ...", etc.
			sp := strings.IndexByte(line, ' ')
			if sp < 0 {
				continue
			}
			idxStr := line[3:sp]
			idx, err := strconv.Atoi(idxStr)
			if err != nil {
				continue
			}
			// grow cores slice on first discovery
			for len(cs.cores) <= idx {
				cs.cores = append(cs.cores, newCpuTracker())
			}
			if tick, ok := parseCpuLine(line); ok {
				cs.cores[idx].update(tick)
			}
		}
	}

	// ready once we've completed the first diff (two reads)
	if hadPrev {
		cs.ready = true
	}
}

func printCpuTable(w io.Writer, cs *cpuState) {
	if !cs.ready {
		return
	}

	fmt.Fprintf(w, "%-6s │ %7s │ %7s │ %7s │ %7s │ %7s │ %7s │ %7s │ %7s │ %7s │ %7s\n",
		"CPU%", "current", "≤10%", "≤20%", "≤30%", "≤40%", "≤50%", "≤60%", "≤70%", "≤80%", "≤90%")
	fmt.Fprintln(w, "───────┼─────────┼─────────┼─────────┼─────────┼─────────┼─────────┼─────────┼─────────┼─────────┼─────────")

	printCpuRow(w, "all", cs.all)
	for i, core := range cs.cores {
		printCpuRow(w, fmt.Sprintf("cpu%d", i), core)
	}
	fmt.Fprintln(w)
}

func printCpuRow(w io.Writer, name string, t *cpuTracker) {
	fmt.Fprintf(w, "%-6s │ %s │ %s │ %s │ %s │ %s │ %s │ %s │ %s │ %s │ %s\n",
		name,
		formatPct(t.cur),
		formatPct(t.cdf(1)),
		formatPct(t.cdf(2)),
		formatPct(t.cdf(3)),
		formatPct(t.cdf(4)),
		formatPct(t.cdf(5)),
		formatPct(t.cdf(6)),
		formatPct(t.cdf(7)),
		formatPct(t.cdf(8)),
		formatPct(t.cdf(9)),
	)
}
