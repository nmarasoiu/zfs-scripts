package main

import (
	"fmt"
	"io"
	"sort"
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
// Buckets: [0]=0–10% .. [8]=80–90%, [9]=90–95%, [10]=95–99%, [11]=99–100%
type cpuTracker struct {
	buckets [12]int
	min     float64
	max     float64
	sum     float64
	count   int
	cur     float64
	prev    cpuTick
	hasPrev bool
	ring    []float64
	ringPos int
	ringN   int // number of populated entries (grows until ring is full)
}

func newCpuTracker(windowSize int) *cpuTracker {
	return &cpuTracker{min: 101, ring: make([]float64, windowSize)}
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

	t.ring[t.ringPos] = t.cur
	t.ringPos = (t.ringPos + 1) % len(t.ring)
	if t.ringN < len(t.ring) {
		t.ringN++
	}

	var idx int
	switch {
	case t.cur >= 99:
		idx = 11
	case t.cur >= 95:
		idx = 10
	case t.cur >= 90:
		idx = 9
	default:
		idx = int(t.cur / 10)
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

// smoothCur returns the average of the last windowSize samples.
func (t *cpuTracker) smoothCur() float64 {
	if t.ringN == 0 {
		return t.cur
	}
	var sum float64
	for i := 0; i < t.ringN; i++ {
		sum += t.ring[i]
	}
	return sum / float64(t.ringN)
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
	fd         int
	buf        [4096]byte
	all        *cpuTracker
	cores      []*cpuTracker
	ready      bool // true after first update (need two ticks to diff)
	windowSize int
}

func newCpuState(windowSize int) *cpuState {
	fd, err := syscall.Open("/proc/stat", syscall.O_RDONLY, 0)
	if err != nil {
		return nil
	}
	return &cpuState{fd: fd, all: newCpuTracker(windowSize), windowSize: windowSize}
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
				cs.cores = append(cs.cores, newCpuTracker(cs.windowSize))
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

// cpuSortKey maps a sort flag value to a function returning the sort key.
// For CDF columns, lower value = busier core, so we negate to get descending.
// For current/avg, higher value = busier core, returned as-is (sorted descending).
var cpuSortKeys = map[string]func(t *cpuTracker) float64{
	"index":   nil, // sentinel: preserve CPU index order
	"current": func(t *cpuTracker) float64 { return t.smoothCur() },
	"avg":     func(t *cpuTracker) float64 { return t.avg() },
	"le10":    func(t *cpuTracker) float64 { return -t.cdf(1) },
	"le20":    func(t *cpuTracker) float64 { return -t.cdf(2) },
	"le30":    func(t *cpuTracker) float64 { return -t.cdf(3) },
	"le40":    func(t *cpuTracker) float64 { return -t.cdf(4) },
	"le50":    func(t *cpuTracker) float64 { return -t.cdf(5) },
	"le60":    func(t *cpuTracker) float64 { return -t.cdf(6) },
	"le70":    func(t *cpuTracker) float64 { return -t.cdf(7) },
	"le80":    func(t *cpuTracker) float64 { return -t.cdf(8) },
	"le90":    func(t *cpuTracker) float64 { return -t.cdf(9) },
	"le95":    func(t *cpuTracker) float64 { return -t.cdf(10) },
	"le99":    func(t *cpuTracker) float64 { return -t.cdf(11) },
}

func cpuSortUsage() string {
	return "sort CPU rows: index, current, avg, le10..le90, le95, le99"
}

// buildSortChain returns the key functions for multi-key sorting.
// The chain is: user's choice, then le99, avg as tiebreakers (deduped).
// The caller adds cpu index as the final tiebreaker.
func buildSortChain(sortBy string) []func(t *cpuTracker) float64 {
	var chain []func(t *cpuTracker) float64
	seen := map[string]bool{}
	for _, key := range []string{sortBy, "le99", "avg"} {
		if key == "index" || seen[key] {
			continue
		}
		seen[key] = true
		chain = append(chain, cpuSortKeys[key])
	}
	return chain
}

func printCpuTable(w io.Writer, cs *cpuState, sortBy string) {
	if !cs.ready {
		return
	}

	fmt.Fprintf(w, "%-6s │ %7s │ %7s │ %7s │ %7s │ %7s │ %7s │ %7s │ %7s │ %7s │ %7s │ %7s │ %7s │ %7s\n",
		"CPU%", "current", "avg", "≤10%", "≤20%", "≤30%", "≤40%", "≤50%", "≤60%", "≤70%", "≤80%", "≤90%", "≤95%", "≤99%")
	fmt.Fprintln(w, "───────┼─────────┼─────────┼─────────┼─────────┼─────────┼─────────┼─────────┼─────────┼─────────┼─────────┼─────────┼─────────┼─────────")

	printCpuRow(w, "all", cs.all)

	type cpuEntry struct {
		idx  int
		core *cpuTracker
	}
	entries := make([]cpuEntry, len(cs.cores))
	for i, core := range cs.cores {
		entries[i] = cpuEntry{i, core}
	}

	chain := buildSortChain(sortBy)
	if len(chain) > 0 {
		sort.SliceStable(entries, func(i, j int) bool {
			for _, keyFn := range chain {
				vi, vj := keyFn(entries[i].core), keyFn(entries[j].core)
				if vi != vj {
					return vi > vj
				}
			}
			return entries[i].idx < entries[j].idx
		})
	}

	for _, e := range entries {
		printCpuRow(w, fmt.Sprintf("cpu%d", e.idx), e.core)
	}
	fmt.Fprintln(w)
}

func printCpuRow(w io.Writer, name string, t *cpuTracker) {
	fmt.Fprintf(w, "%-6s │ %s │ %s │ %s │ %s │ %s │ %s │ %s │ %s │ %s │ %s │ %s │ %s │ %s\n",
		name,
		formatPct(t.smoothCur()),
		formatPct(t.avg()),
		formatPct(t.cdf(1)),
		formatPct(t.cdf(2)),
		formatPct(t.cdf(3)),
		formatPct(t.cdf(4)),
		formatPct(t.cdf(5)),
		formatPct(t.cdf(6)),
		formatPct(t.cdf(7)),
		formatPct(t.cdf(8)),
		formatPct(t.cdf(9)),
		formatPct(t.cdf(10)),
		formatPct(t.cdf(11)),
	)
}
