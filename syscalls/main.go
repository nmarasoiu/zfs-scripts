// syscall-latency: Per-syscall latency percentile tracker using eBPF
//
// Traces syscall enter/exit to compute per-syscall latency, grouped by process.
// Uses DDSketch for percentiles (P25/P50/P75/P90/P99/P99.9) with explicit
// min/max/avg tracking (sum+count). Emits stats on configurable interval.
//
// Focus processes (-c) get full per-syscall breakdown.
// All processes are shown in a summary table (top N by sample count).
//
// Usage: syscall-latency [-c focus_procs] [-n top_n] [-s syscalls] [-i interval]
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -type latency_event bpf bpf/syscall_latency.c -- -I/usr/include -I.

package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

const (
	displayInterval = 100 * time.Millisecond // 10 FPS display refresh
	maxLatencyUs    = 60_000_000             // 60 seconds in µs - clamp values above this
)

var (
	interval    = flag.Duration("i", 10*time.Second, "stats reset interval")
	focusProcs  = flag.String("c", "", "focus processes for per-syscall detail (comma-separated, empty=none)")
	topProcs    = flag.Int("n", 28, "top N process/syscall rows per column in summary")
	syscallList = flag.String("s", "all", "comma-separated syscalls to trace (or 'all')")
	batch       = flag.Bool("batch", false, "batch mode (no screen clearing)")
	pollSleep   = flag.Duration("poll-sleep", 50*time.Microsecond, "ring buffer poll sleep when empty")
)

// x86_64 syscall numbers
var syscallNums = map[string]uint32{
	"read":         0,
	"write":        1,
	"open":         2,
	"close":        3,
	"stat":         4,
	"fstat":        5,
	"lstat":        6,
	"poll":         7,
	"lseek":        8,
	"mmap":         9,
	"mprotect":     10,
	"munmap":       11,
	"brk":          12,
	"pread64":      17,
	"pwrite64":     18,
	"readv":        19,
	"writev":       20,
	"access":       21,
	"pipe":         22,
	"select":       23,
	"dup":          32,
	"dup2":         33,
	"socket":       41,
	"connect":      42,
	"accept":       43,
	"sendto":       44,
	"recvfrom":     45,
	"sendmsg":      46,
	"recvmsg":      47,
	"shutdown":     48,
	"bind":         49,
	"listen":       50,
	"clone":        56,
	"fork":         57,
	"vfork":        58,
	"execve":       59,
	"exit":         60,
	"wait4":        61,
	"kill":         62,
	"fcntl":        72,
	"flock":        73,
	"fsync":        74,
	"fdatasync":    75,
	"truncate":     76,
	"ftruncate":    77,
	"getdents":     78,
	"getcwd":       79,
	"chdir":        80,
	"rename":       82,
	"mkdir":        83,
	"rmdir":        84,
	"creat":        85,
	"link":         86,
	"unlink":       87,
	"symlink":      88,
	"readlink":     89,
	"chmod":        90,
	"fchmod":       91,
	"chown":        92,
	"fchown":       93,
	"lchown":       94,
	"umask":        95,
	"openat":       257,
	"mkdirat":      258,
	"fstatat":      262,
	"unlinkat":     263,
	"renameat":     264,
	"faccessat":    269,
	"splice":       275,
	"sync":         162,
	"syncfs":       306,
	"fallocate":    285,
	"epoll_wait":   232,
	"epoll_pwait":  281,
	"futex":        202,
	"nanosleep":    35,
	"accept4":      288,
	"recvmmsg":     299,
	"sendmmsg":     307,
	"getdents64":   217,
	"ioctl":        16,
	"madvise":      28,
	"sched_yield":  24,
	"clock_gettime": 228,
	"gettimeofday": 96,
	"getpid":       39,
	"gettid":       186,
	"getuid":       102,
	"getgid":       104,
	"rt_sigaction": 13,
	"rt_sigprocmask": 14,
	"rt_sigreturn": 15,
	"pselect6":     270,
	"ppoll":        271,
	"eventfd2":     290,
	"timerfd_create": 283,
	"timerfd_settime": 286,
	"signalfd4":    289,
	"epoll_create1": 291,
	"epoll_ctl":    233,
	"pipe2":        293,
	"inotify_init1": 294,
	"inotify_add_watch": 254,
	"inotify_rm_watch": 255,
	"statfs":       137,
	"fstatfs":      138,
	"prctl":        157,
	"arch_prctl":   158,
	"set_tid_address": 218,
	"set_robust_list": 273,
	"getrandom":    318,
	"memfd_create": 319,
	"statx":        332,
	"io_uring_setup": 425,
	"io_uring_enter": 426,
	"io_uring_register": 427,
	"copy_file_range": 326,
	"preadv2":      327,
	"pwritev2":     328,
	"setsockopt":   54,
	"getsockopt":   55,
	"getsockname":  51,
	"getpeername":  52,
	"socketpair":   53,
	"exit_group":   231,
	"waitid":       247,
	"tgkill":       234,
	"clock_nanosleep": 230,
}

// Reverse map for display
var syscallNames = make(map[uint32]string)

func init() {
	for name, num := range syscallNums {
		syscallNames[num] = name
	}
}

func commString(comm [16]int8) string {
	var buf [16]byte
	for i, c := range comm {
		buf[i] = byte(c)
	}
	if n := bytes.IndexByte(buf[:], 0); n >= 0 {
		return string(buf[:n])
	}
	return string(buf[:])
}

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

// topN tracks the top N maximum values
type topN struct {
	values []int64
	n      int
}

func newTopN(n int) *topN {
	return &topN{values: make([]int64, 0, n), n: n}
}

func (t *topN) Add(v int64) {
	if len(t.values) < t.n {
		i := sort.Search(len(t.values), func(i int) bool { return t.values[i] >= v })
		t.values = append(t.values, 0)
		copy(t.values[i+1:], t.values[i:])
		t.values[i] = v
		return
	}
	if v > t.values[0] {
		i := sort.Search(len(t.values), func(i int) bool { return t.values[i] >= v })
		if i > 0 {
			copy(t.values[:i-1], t.values[1:i])
			t.values[i-1] = v
		}
	}
}

func (t *topN) Get() []int64 {
	result := make([]int64, len(t.values))
	copy(result, t.values)
	return result
}

func (t *topN) Reset() { t.values = t.values[:0] }

func (t *topN) Clone() *topN {
	clone := &topN{values: make([]int64, len(t.values)), n: t.n}
	copy(clone.values, t.values)
	return clone
}

// simpleStats tracks min/max/sum/count explicitly (no sketch overhead)
type simpleStats struct {
	min   int64
	max   int64
	sum   uint64
	count uint64
}

func newSimpleStats() *simpleStats {
	return &simpleStats{min: math.MaxInt64, max: 0}
}

func (s *simpleStats) Record(v int64) {
	if v < s.min {
		s.min = v
	}
	if v > s.max {
		s.max = v
	}
	s.sum += uint64(v)
	s.count++
}

func (s *simpleStats) Reset() {
	s.min = math.MaxInt64
	s.max = 0
	s.sum = 0
	s.count = 0
}

func (s *simpleStats) Avg() int64 {
	if s.count == 0 {
		return 0
	}
	return int64(s.sum / s.count)
}

func (s *simpleStats) Clone() *simpleStats {
	return &simpleStats{min: s.min, max: s.max, sum: s.sum, count: s.count}
}

func mergeSimpleStats(dst, src *simpleStats) {
	if src.count == 0 {
		return
	}
	if dst.count == 0 {
		dst.min = src.min
		dst.max = src.max
	} else {
		if src.min < dst.min {
			dst.min = src.min
		}
		if src.max > dst.max {
			dst.max = src.max
		}
	}
	dst.sum += src.sum
	dst.count += src.count
}

// syscallStats holds both interval and lifetime stats for a syscall
// Uses DDSketch for percentiles only, explicit tracking for min/max/avg
type syscallStats struct {
	intervalSketch *ddsketch.DDSketch
	lifetimeSketch *ddsketch.DDSketch
	intervalStats  *simpleStats
	lifetimeStats  *simpleStats
	intervalTop    *topN
	lifetimeTop    *topN
}

func newSyscallStats() *syscallStats {
	intervalSketch, _ := ddsketch.NewDefaultDDSketch(0.01)
	lifetimeSketch, _ := ddsketch.NewDefaultDDSketch(0.01)
	return &syscallStats{
		intervalSketch: intervalSketch,
		lifetimeSketch: lifetimeSketch,
		intervalStats:  newSimpleStats(),
		lifetimeStats:  newSimpleStats(),
		intervalTop:    newTopN(5),
		lifetimeTop:    newTopN(5),
	}
}

func (ss *syscallStats) Record(latencyUs int64) {
	v := float64(latencyUs)
	ss.intervalSketch.Add(v)
	ss.lifetimeSketch.Add(v)
	ss.intervalStats.Record(latencyUs)
	ss.lifetimeStats.Record(latencyUs)
	ss.intervalTop.Add(latencyUs)
	ss.lifetimeTop.Add(latencyUs)
}

func (ss *syscallStats) ResetInterval() {
	ss.intervalSketch, _ = ddsketch.NewDefaultDDSketch(0.01)
	ss.intervalStats.Reset()
	ss.intervalTop.Reset()
}

func (ss *syscallStats) Snapshot() *syscallStats {
	return &syscallStats{
		intervalSketch: ss.intervalSketch.Copy(),
		lifetimeSketch: ss.lifetimeSketch.Copy(),
		intervalStats:  ss.intervalStats.Clone(),
		lifetimeStats:  ss.lifetimeStats.Clone(),
		intervalTop:    ss.intervalTop.Clone(),
		lifetimeTop:    ss.lifetimeTop.Clone(),
	}
}

// MergeFrom merges another syscallStats into this one (for "[N others]" bucket)
func (ss *syscallStats) MergeFrom(other *syscallStats) {
	ss.intervalSketch.MergeWith(other.intervalSketch)
	ss.lifetimeSketch.MergeWith(other.lifetimeSketch)
	mergeSimpleStats(ss.intervalStats, other.intervalStats)
	mergeSimpleStats(ss.lifetimeStats, other.lifetimeStats)
	// topN not merged - not used for summary rows
}

// State holds all per-process and per-syscall stats
type State struct {
	mu sync.RWMutex

	// Per-(process, syscall) stats for all processes
	procSyscallStats map[string]map[uint32]*syscallStats

	focusProcesses map[string]bool

	startTime time.Time
	lastReset time.Time
}

func newState(focus map[string]bool) *State {
	now := time.Now()
	return &State{
		procSyscallStats: make(map[string]map[uint32]*syscallStats),
		focusProcesses:   focus,
		startTime:        now,
		lastReset:        now,
	}
}

func (s *State) Record(comm string, syscallID uint32, latencyUs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	fm, ok := s.procSyscallStats[comm]
	if !ok {
		fm = make(map[uint32]*syscallStats)
		s.procSyscallStats[comm] = fm
	}
	ss, ok := fm[syscallID]
	if !ok {
		ss = newSyscallStats()
		fm[syscallID] = ss
	}
	ss.Record(latencyUs)
}

func (s *State) ResetIntervals() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, fm := range s.procSyscallStats {
		for _, ss := range fm {
			ss.ResetInterval()
		}
	}
	s.lastReset = time.Now()
}

type stateSnapshot struct {
	procSyscallStats map[string]map[uint32]*syscallStats
	startTime        time.Time
	lastReset        time.Time
}

func (s *State) Snapshot() *stateSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := &stateSnapshot{
		procSyscallStats: make(map[string]map[uint32]*syscallStats, len(s.procSyscallStats)),
		startTime:        s.startTime,
		lastReset:        s.lastReset,
	}

	for name, fm := range s.procSyscallStats {
		snap.procSyscallStats[name] = make(map[uint32]*syscallStats, len(fm))
		for id, ss := range fm {
			snap.procSyscallStats[name][id] = ss.Snapshot()
		}
	}

	return snap
}

// Display handles rendering
type Display struct {
	batchMode      bool
	focusProcesses []string // ordered list of focus process names
	topN           int
	ring           *RingPollReader
}

func (d *Display) resetCursor() {
	if !d.batchMode {
		fmt.Print("\033[H\033[J")
	}
}

func formatTop5(top *topN) string {
	vals := top.Get()
	var parts []string
	for i := 0; i < 5-len(vals); i++ {
		parts = append(parts, fmt.Sprintf("%8s", "-"))
	}
	for _, v := range vals {
		parts = append(parts, formatLatencyPadded(v))
	}
	return strings.Join(parts, " ")
}

const (
	focusLineWidth   = 155
	summaryLineWidth = 97
)

func sectionHeader(buf *strings.Builder, title string, width int) {
	// "── title ───..."
	displayWidth := 3 + len(title) + 1 // "── " + title + " "
	remaining := width - displayWidth
	if remaining < 0 {
		remaining = 0
	}
	buf.WriteString("── ")
	buf.WriteString(title)
	buf.WriteString(" ")
	buf.WriteString(strings.Repeat("─", remaining))
	buf.WriteString("\n")
}

func (d *Display) render(snap *stateSnapshot, intervalDur time.Duration, drops uint64) {
	var buf strings.Builder
	now := time.Now()

	elapsed := now.Sub(snap.startTime)
	intervalElapsed := now.Sub(snap.lastReset)

	fmt.Fprintf(&buf, "Syscall Latency Monitor - %s (uptime: %s, interval: %s/%s)\n",
		now.Format("15:04:05"), formatDuration(elapsed),
		formatDuration(intervalElapsed), formatDuration(intervalDur))

	// Focus sections
	for _, name := range d.focusProcesses {
		fm := snap.procSyscallStats[name]
		if fm == nil {
			continue
		}
		d.renderFocusSection(&buf, name, fm)
	}

	// Process+syscall summary (lifetime only)
	d.renderProcessSummary(&buf, snap.procSyscallStats, elapsed)

	// Footer
	var totalSamples uint64
	for _, fm := range snap.procSyscallStats {
		for _, ss := range fm {
			totalSamples += ss.lifetimeStats.count
		}
	}
	rate := float64(0)
	if elapsed.Seconds() > 0 {
		rate = float64(totalSamples) / elapsed.Seconds()
	}
	dropRate := float64(0)
	if elapsed.Seconds() > 0 {
		dropRate = float64(drops) / elapsed.Seconds()
	}
	ringInfo := ""
	if d.ring != nil {
		pending := d.ring.Pending()
		capBytes := d.ring.BufSize()
		batch := d.ring.LastBatch()
		pctFull := float64(pending) / float64(capBytes) * 100
		ringInfo = fmt.Sprintf(" | Ring: %s/%s (%.1f%%) batch %s",
			formatBytes(int64(pending)), formatBytes(int64(capBytes)), pctFull,
			formatCount(batch))
	}
	fmt.Fprintf(&buf, "Total: %s syscalls | Rate: %s/s | Processes: %d | Drops: %s (%s/s)%s\n",
		formatCount(int64(totalSamples)), formatCount(int64(rate)), len(snap.procSyscallStats),
		formatCount(int64(drops)), formatCount(int64(dropRate)), ringInfo)

	if d.batchMode {
		buf.WriteString("\n")
	}

	d.resetCursor()
	fmt.Print(buf.String())
}

func (d *Display) renderFocusSection(buf *strings.Builder, name string, stats map[uint32]*syscallStats) {
	sectionHeader(buf, name, focusLineWidth)

	// Sort syscalls by name
	var ids []uint32
	for id := range stats {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		ni := syscallNames[ids[i]]
		nj := syscallNames[ids[j]]
		if ni == "" {
			ni = fmt.Sprintf("syscall_%d", ids[i])
		}
		if nj == "" {
			nj = fmt.Sprintf("syscall_%d", ids[j])
		}
		return ni < nj
	})

	// Interval
	fmt.Fprintf(buf, "%-12s │ %8s %8s %8s %8s %8s %8s %8s %8s %8s │ %8s %8s %8s %8s %8s │ %9s\n",
		"INTERVAL", "min", "avg", "p25", "p50", "p75", "p90", "p99", "p99.9", "max",
		"max-4", "max-3", "max-2", "max-1", "max", "samples")
	buf.WriteString(strings.Repeat("-", focusLineWidth))
	buf.WriteString("\n")

	for _, id := range ids {
		ss := stats[id]
		sname := syscallNames[id]
		if sname == "" {
			sname = fmt.Sprintf("sys_%d", id)
		}
		renderDetailRow(buf, sname, ss.intervalStats, ss.intervalSketch, ss.intervalTop)
	}

	buf.WriteString("\n")

	// Lifetime
	fmt.Fprintf(buf, "%-12s │ %8s %8s %8s %8s %8s %8s %8s %8s %8s │ %8s %8s %8s %8s %8s │ %9s\n",
		"LIFETIME", "min", "avg", "p25", "p50", "p75", "p90", "p99", "p99.9", "max",
		"max-4", "max-3", "max-2", "max-1", "max", "samples")
	buf.WriteString(strings.Repeat("-", focusLineWidth))
	buf.WriteString("\n")

	for _, id := range ids {
		ss := stats[id]
		sname := syscallNames[id]
		if sname == "" {
			sname = fmt.Sprintf("sys_%d", id)
		}
		renderDetailRow(buf, sname, ss.lifetimeStats, ss.lifetimeSketch, ss.lifetimeTop)
	}

	buf.WriteString("\n")
}

func renderDetailRow(buf *strings.Builder, name string, st *simpleStats, sketch *ddsketch.DDSketch, top *topN) {
	n := st.count
	if n == 0 {
		fmt.Fprintf(buf, "%-12s │ %8s %8s %8s %8s %8s %8s %8s %8s %8s │ %8s %8s %8s %8s %8s │ %9s\n",
			name, "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "0")
		return
	}
	p25, _ := sketch.GetValueAtQuantile(0.25)
	p50, _ := sketch.GetValueAtQuantile(0.50)
	p75, _ := sketch.GetValueAtQuantile(0.75)
	p90, _ := sketch.GetValueAtQuantile(0.90)
	p99, _ := sketch.GetValueAtQuantile(0.99)
	p999, _ := sketch.GetValueAtQuantile(0.999)
	fmt.Fprintf(buf, "%-12s │ %s %s %s %s %s %s %s %s %s │ %s │ %9s\n",
		name,
		formatLatencyPadded(st.min),
		formatLatencyPadded(st.Avg()),
		formatLatencyPadded(int64(p25)),
		formatLatencyPadded(int64(p50)),
		formatLatencyPadded(int64(p75)),
		formatLatencyPadded(int64(p90)),
		formatLatencyPadded(int64(p99)),
		formatLatencyPadded(int64(p999)),
		formatLatencyPadded(st.max),
		formatTop5(top),
		formatCount(int64(n)),
	)
}

type procSyscallEntry struct {
	label string
	stats *syscallStats
}

func (d *Display) renderProcessSummary(buf *strings.Builder, procSyscallStats map[string]map[uint32]*syscallStats, totalElapsed time.Duration) {
	var entries []procSyscallEntry
	for name, fm := range procSyscallStats {
		for id, ss := range fm {
			sname := syscallNames[id]
			if sname == "" {
				sname = fmt.Sprintf("sys_%d", id)
			}
			entries = append(entries, procSyscallEntry{
				label: name + "/" + sname,
				stats: ss,
			})
		}
	}

	// Sort by lifetime count desc, then alphabetically by label
	sort.Slice(entries, func(i, j int) bool {
		ci := entries[i].stats.lifetimeStats.count
		cj := entries[j].stats.lifetimeStats.count
		if ci != cj {
			return ci > cj
		}
		return entries[i].label < entries[j].label
	})

	totalSecs := totalElapsed.Seconds()
	nPerCol := d.topN // rows per column
	totalShown := nPerCol * 2
	if totalShown > len(entries) {
		totalShown = len(entries)
	}

	// Two-column width: left column + " │ " + right column
	dualWidth := summaryLineWidth + 3 + summaryLineWidth

	sectionHeader(buf, fmt.Sprintf("Process × Syscall (top %d)", totalShown), dualWidth)

	// Header row (two columns)
	hdr := fmt.Sprintf("%-28s │ %8s %8s %8s %8s %8s │ %9s %9s",
		"LIFETIME", "avg", "p50", "p90", "p99", "max", "samples", "rate")
	fmt.Fprintf(buf, "%s │ %s\n", hdr, hdr)
	buf.WriteString(strings.Repeat("-", dualWidth))
	buf.WriteString("\n")

	// Split entries into left and right columns
	leftEnd := nPerCol
	if leftEnd > len(entries) {
		leftEnd = len(entries)
	}
	rightStart := nPerCol
	rightEnd := nPerCol * 2
	if rightEnd > len(entries) {
		rightEnd = len(entries)
	}

	leftSlice := entries[:leftEnd]
	var rightSlice []procSyscallEntry
	if rightStart < len(entries) {
		rightSlice = entries[rightStart:rightEnd]
	}

	// Determine "others" for entries beyond shown
	var otherSlice []procSyscallEntry
	if rightEnd < len(entries) {
		otherSlice = entries[rightEnd:]
	}

	// Render rows side by side
	maxRows := len(leftSlice)
	if len(rightSlice) > maxRows {
		maxRows = len(rightSlice)
	}
	// Add one row for "others" if needed
	hasOthers := len(otherSlice) > 0
	if hasOthers {
		maxRows++ // extra row for [N others] on right side
	}

	for i := 0; i < maxRows; i++ {
		var leftStr, rightStr string

		if i < len(leftSlice) {
			leftStr = formatSummaryRow(leftSlice[i].label, leftSlice[i].stats.lifetimeStats, leftSlice[i].stats.lifetimeSketch, totalSecs)
		} else {
			leftStr = strings.Repeat(" ", summaryLineWidth)
		}

		if i < len(rightSlice) {
			rightStr = formatSummaryRow(rightSlice[i].label, rightSlice[i].stats.lifetimeStats, rightSlice[i].stats.lifetimeSketch, totalSecs)
		} else if i == len(rightSlice) && hasOthers {
			// Show [N others] as last row on right
			merged := newSyscallStats()
			for _, e := range otherSlice {
				merged.MergeFrom(e.stats)
			}
			rightStr = formatSummaryRow(fmt.Sprintf("[%d others]", len(otherSlice)), merged.lifetimeStats, merged.lifetimeSketch, totalSecs)
		} else {
			rightStr = strings.Repeat(" ", summaryLineWidth)
		}

		fmt.Fprintf(buf, "%s │ %s\n", leftStr, rightStr)
	}

	buf.WriteString(strings.Repeat("=", dualWidth))
	buf.WriteString("\n")
}

func formatSummaryRow(name string, st *simpleStats, sketch *ddsketch.DDSketch, secs float64) string {
	n := st.count
	if n == 0 {
		return fmt.Sprintf("%-28s │ %8s %8s %8s %8s %8s │ %9s %9s",
			name, "-", "-", "-", "-", "-", "0", "-")
	}
	p50, _ := sketch.GetValueAtQuantile(0.50)
	p90, _ := sketch.GetValueAtQuantile(0.90)
	p99, _ := sketch.GetValueAtQuantile(0.99)
	return fmt.Sprintf("%-28s │ %s %s %s %s %s │ %9s %9s",
		name,
		formatLatencyPadded(st.Avg()),
		formatLatencyPadded(int64(p50)),
		formatLatencyPadded(int64(p90)),
		formatLatencyPadded(int64(p99)),
		formatLatencyPadded(st.max),
		formatCount(int64(n)),
		formatRate(n, secs),
	)
}

func main() {
	flag.Parse()

	// Parse focus processes
	focusSet := make(map[string]bool)
	var focusList []string
	if *focusProcs != "" {
		for _, name := range strings.Split(*focusProcs, ",") {
			name = strings.TrimSpace(name)
			if name != "" && !focusSet[name] {
				focusSet[name] = true
				focusList = append(focusList, name)
			}
		}
	}

	// Parse syscall list
	traceAll := false
	var traceSyscalls []uint32
	trimmed := strings.TrimSpace(*syscallList)
	if strings.EqualFold(trimmed, "all") {
		traceAll = true
	} else {
		for _, name := range strings.Split(trimmed, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if num, ok := syscallNums[name]; ok {
				traceSyscalls = append(traceSyscalls, num)
			} else {
				log.Fatalf("Unknown syscall: %s", name)
			}
		}
		if len(traceSyscalls) == 0 {
			log.Fatal("No syscalls to trace")
		}
	}

	// Remove memlock limit for eBPF
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatalf("Failed to remove memlock limit: %v", err)
	}

	// Load eBPF objects
	objs := bpfObjects{}
	if err := loadBpfObjects(&objs, nil); err != nil {
		log.Fatalf("Failed to load eBPF objects: %v", err)
	}
	defer objs.Close()

	// Set up syscall filter
	if traceAll {
		var key uint32
		var enabled uint8 = 1
		if err := objs.TraceAll.Put(key, enabled); err != nil {
			log.Fatalf("Failed to set trace_all flag: %v", err)
		}
	} else {
		for _, num := range traceSyscalls {
			var enabled uint8 = 1
			if err := objs.SyscallFilter.Put(num, enabled); err != nil {
				log.Fatalf("Failed to add syscall to filter: %v", err)
			}
		}
	}

	// No BPF comm filter - we trace all processes and group in userspace

	// Attach tracepoints
	tpEnter, err := link.Tracepoint("raw_syscalls", "sys_enter", objs.TraceSyscallEnter, nil)
	if err != nil {
		log.Fatalf("Failed to attach sys_enter: %v", err)
	}
	defer tpEnter.Close()

	tpExit, err := link.Tracepoint("raw_syscalls", "sys_exit", objs.TraceSyscallExit, nil)
	if err != nil {
		log.Fatalf("Failed to attach sys_exit: %v", err)
	}
	defer tpExit.Close()

	// Open ring buffer (busy-poll reader — no epoll)
	rd, err := NewRingPollReader(objs.Events, *pollSleep)
	if err != nil {
		log.Fatalf("Failed to open ring buffer: %v", err)
	}
	defer rd.Cleanup()

	state := newState(focusSet)
	display := &Display{
		batchMode:      *batch,
		focusProcesses: focusList,
		topN:           *topProcs,
		ring:           rd,
	}

	// Signal handling
	done := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig
		signal.Stop(sig) // restore default handler so second Ctrl+C force-kills
		close(done)
		rd.Close()
	}()

	// Event channel: drainer → consumer pipeline
	eventCh := make(chan bpfLatencyEvent, 4096)

	// Drainer goroutine: busy-polls ring buffer, no epoll
	go func() {
		defer close(eventCh)
		var rec PollRecord
		var event bpfLatencyEvent
		for rd.ReadInto(&rec) {
			if err := binary.Read(bytes.NewReader(rec.RawSample), binary.LittleEndian, &event); err != nil {
				continue
			}
			select {
			case eventCh <- event:
			case <-done:
				return
			}
		}
	}()

	// Consumer goroutine: processes events into State
	var consumerDone sync.WaitGroup
	consumerDone.Add(1)
	go func() {
		defer consumerDone.Done()
		for event := range eventCh {
			latencyUs := int64(event.LatencyNs / 1000)
			if latencyUs < 1 {
				latencyUs = 1
			}
			if latencyUs > maxLatencyUs {
				latencyUs = maxLatencyUs
			}
			state.Record(commString(event.Comm), event.SyscallId, latencyUs)
		}
	}()

	// Drop counter: read from kernel map periodically by display goroutine
	var totalDrops atomic.Uint64

	// Display goroutine (10 FPS)
	displayTicker := time.NewTicker(displayInterval)
	go func() {
		defer displayTicker.Stop()
		for {
			select {
			case <-done:
				return
			case <-displayTicker.C:
				// Read drop counter from kernel
				readDropCount(objs.DropCount, &totalDrops)
				snap := state.Snapshot()
				if len(snap.procSyscallStats) > 0 {
					display.render(snap, *interval, totalDrops.Load())
				}
			}
		}
	}()

	// Interval reset goroutine
	intervalTicker := time.NewTicker(*interval)
	go func() {
		defer intervalTicker.Stop()
		for {
			select {
			case <-done:
				return
			case <-intervalTicker.C:
				state.ResetIntervals()
			}
		}
	}()

	var syscallLabel string
	if traceAll {
		syscallLabel = "ALL"
	} else {
		syscallStr := make([]string, len(traceSyscalls))
		for i, num := range traceSyscalls {
			syscallStr[i] = syscallNames[num]
		}
		syscallLabel = strings.Join(syscallStr, ",")
	}
	if len(focusList) > 0 {
		log.Printf("Tracing syscalls: %s | focus: %s | top %d processes (interval=%v)",
			syscallLabel, strings.Join(focusList, ","), *topProcs, *interval)
	} else {
		log.Printf("Tracing syscalls: %s | top %d processes (interval=%v)",
			syscallLabel, *topProcs, *interval)
	}

	// Wait for signal, then drain
	<-done
	consumerDone.Wait()

	readDropCount(objs.DropCount, &totalDrops)
	snap := state.Snapshot()
	display.render(snap, *interval, totalDrops.Load())
}

func readDropCount(m *ebpf.Map, dst *atomic.Uint64) {
	if m == nil {
		return
	}
	var key uint32
	var val uint64
	if err := m.Lookup(key, &val); err == nil {
		dst.Store(val)
	}
}
