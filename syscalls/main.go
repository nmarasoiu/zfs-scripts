// syscall-latency: Per-syscall latency percentile tracker using eBPF
//
// Traces syscall enter/exit to compute per-syscall latency, grouped by process.
// Uses DDSketch for percentiles (P25/P50/P75/P90/P99/P99.9) with explicit
// min/max/avg tracking (sum+count). Lifetime stats only.
//
// -c filters to specified processes (BPF-level). Without -c, traces all.
// One unified table output, sorted by sample count.
//
// Usage: syscall-latency [-c procs] [-n top_n]
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -type latency_event bpf bpf/syscall_latency.c -- -I/usr/include -I.

package main

import (
	"bytes"
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
	"unsafe"

	"github.com/DataDog/sketches-go/ddsketch"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"
)

const (
	displayInterval = 100 * time.Millisecond // 10 FPS display refresh
	maxLatencyUs    = 60_000_000             // 60 seconds in µs - clamp values above this
	flushSize       = 1024
	flushInterval   = 10 * time.Millisecond

	panelWidth    = 34
	panelSep      = " │ "
	panelMinWidth = 80
)

type interactiveMode int

const (
	modeNormal interactiveMode = iota
	modeFilter
	modeDetail
)

type keyKind int

const (
	keyChar keyKind = iota
	keyEnter
	keyEsc
	keyBackspace
)

type keyEvent struct {
	kind keyKind
	ch   byte
}

type processSummary struct {
	name  string
	count uint64
	rate  float64
}

var (
	focusProcs = flag.String("c", "", "only trace these processes (comma-separated, empty=all)")
	topProcs   = flag.Int("n", 0, "top N rows to display (0=all)")
	batch      = flag.Bool("batch", false, "batch mode (no screen clearing)")
	pollSleep       = flag.Duration("poll-sleep", 50*time.Microsecond, "ring buffer poll sleep when empty")
	cleanupInterval = flag.Duration("cleanup-interval", 5*time.Second, "how often to scan BPF hash map for stale entries")
	staleAge        = flag.Duration("stale-age", 10*time.Second, "age threshold to count an in-flight entry as stale")
	evictAge        = flag.Duration("evict-age", 60*time.Second, "age threshold to evict (delete) a stale entry")
)

// commIntern interns process comm strings to avoid 80K allocations/s.
// Only used by the reader goroutine — no sync needed.
var commIntern = make(map[string]string)

func commString(comm [16]int8) string {
	var buf [16]byte
	for i, c := range comm {
		buf[i] = byte(c)
	}
	var raw string
	if n := bytes.IndexByte(buf[:], 0); n >= 0 {
		raw = string(buf[:n])
	} else {
		raw = string(buf[:])
	}
	if s, ok := commIntern[raw]; ok {
		return s
	}
	commIntern[raw] = raw
	return raw
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

// isTerminal returns true if the given fd refers to a terminal.
func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

// enableRawMode puts stdin into raw mode (no ICANON/ECHO) but keeps ISIG
// so Ctrl+C still triggers the signal handler. Returns the original termios
// for later restore, or nil if stdin is not a terminal.
func enableRawMode() *unix.Termios {
	orig, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS)
	if err != nil {
		return nil
	}
	raw := *orig
	raw.Lflag &^= unix.ICANON | unix.ECHO
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TCSETS, &raw)
	return orig
}

// restoreTermMode restores the original terminal settings.
func restoreTermMode(orig *unix.Termios) {
	if orig != nil {
		unix.IoctlSetTermios(int(os.Stdin.Fd()), unix.TCSETS, orig)
	}
}

// termSize returns the terminal dimensions (cols, rows). Returns 0,0 if not a terminal.
func termSize() (int, int) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0
	}
	return int(ws.Col), int(ws.Row)
}

// runInput reads stdin byte-by-byte and sends parsed key events to ch.
// Exits on read error (stdin closed or program shutting down).
func runInput(ch chan<- keyEvent) {
	var buf [3]byte
	for {
		n, err := os.Stdin.Read(buf[:1])
		if err != nil || n == 0 {
			return
		}
		b := buf[0]
		switch {
		case b == 0x1b: // Escape or CSI sequence
			// Try to read more bytes to consume CSI sequences (arrow keys etc)
			os.Stdin.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			n2, _ := os.Stdin.Read(buf[1:])
			os.Stdin.SetReadDeadline(time.Time{})
			if n2 == 0 {
				// Pure Esc key
				ch <- keyEvent{kind: keyEsc}
			}
			// Otherwise discard the CSI sequence
		case b == 0x0d || b == 0x0a:
			ch <- keyEvent{kind: keyEnter}
		case b == 0x7f || b == 0x08:
			ch <- keyEvent{kind: keyBackspace}
		case b >= 0x20 && b <= 0x7e:
			ch <- keyEvent{kind: keyChar, ch: b}
		}
	}
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

func (s *simpleStats) Avg() int64 {
	if s.count == 0 {
		return 0
	}
	return int64(s.sum / s.count)
}

// syscallStats holds lifetime stats for a syscall.
// Uses DDSketch for percentiles, explicit tracking for min/max/avg.
type syscallStats struct {
	sketch *ddsketch.DDSketch
	stats  *simpleStats
	top    *topN
}

func newSyscallStats() *syscallStats {
	sketch, _ := ddsketch.NewDefaultDDSketch(0.01)
	return &syscallStats{
		sketch: sketch,
		stats:  newSimpleStats(),
		top:    newTopN(5),
	}
}

func (ss *syscallStats) Record(latencyUs int64) {
	ss.sketch.Add(float64(latencyUs))
	ss.stats.Record(latencyUs)
	ss.top.Add(latencyUs)
}

// State holds all per-process and per-syscall stats
type State struct {
	mu sync.Mutex

	// Per-(process, syscall) stats for all processes
	procSyscallStats map[string]map[uint32]*syscallStats

	startTime time.Time
}

func newState() *State {
	return &State{
		procSyscallStats: make(map[string]map[uint32]*syscallStats),
		startTime:        time.Now(),
	}
}

type pendingEvent struct {
	comm      string
	syscallID uint32
	latencyUs int64
}

func (s *State) RecordBatch(batch []pendingEvent) {
	s.mu.Lock()
	for i := range batch {
		e := &batch[i]
		fm, ok := s.procSyscallStats[e.comm]
		if !ok {
			fm = make(map[uint32]*syscallStats)
			s.procSyscallStats[e.comm] = fm
		}
		ss, ok := fm[e.syscallID]
		if !ok {
			ss = newSyscallStats()
			fm[e.syscallID] = ss
		}
		ss.Record(e.latencyUs)
	}
	s.mu.Unlock()
}

// runtimeMetrics groups atomic counters shared between goroutines.
type runtimeMetrics struct {
	drops    atomic.Uint64
	evicted  atomic.Uint64
	mapUsed  atomic.Int64
	mapStale atomic.Int64
}

// Display handles rendering
type Display struct {
	batchMode      bool
	focusProcesses []string // ordered list of focus process names
	topN           int
	ring           *RingPollReader

	// Interactive state (all owned by display goroutine — no sync needed)
	interactive   bool
	mode          interactiveMode
	filterText    string
	selectedProc  string
	lastSummaries []processSummary
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

func sectionHeader(buf *strings.Builder, title string, width int) {
	displayWidth := 3 + len(title) + 1
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

type tableEntry struct {
	label string
	ss    *syscallStats
}

// collectEntries builds a sorted list of table entries from per-process stats.
// When alwaysPrefix is true, labels are always "proc/syscall"; otherwise the
// proc prefix is omitted when there is exactly one process.
func collectEntries(procStats map[string]map[uint32]*syscallStats, alwaysPrefix bool) []tableEntry {
	singleProc := !alwaysPrefix && len(procStats) == 1

	var entries []tableEntry
	for proc, fm := range procStats {
		for id, ss := range fm {
			label := syscallName(id)
			if !singleProc {
				label = proc + "/" + label
			}
			entries = append(entries, tableEntry{label, ss})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		ci := entries[i].ss.stats.count
		cj := entries[j].ss.stats.count
		if ci != cj {
			return ci > cj
		}
		return entries[i].label < entries[j].label
	})

	return entries
}

func (d *Display) render(state *State, metrics *runtimeMetrics, mapCap int64) {
	var buf strings.Builder
	now := time.Now()

	state.mu.Lock()

	elapsed := now.Sub(state.startTime)

	fmt.Fprintf(&buf, "Syscall Latency Monitor - %s (uptime: %s)\n",
		now.Format("15:04:05"), formatDuration(elapsed))

	// Focus mode: detailed single-column table
	// Summary mode: compact two-column table
	if len(d.focusProcesses) > 0 {
		d.renderTable(&buf, state.procSyscallStats)
	} else {
		d.renderSummary(&buf, state.procSyscallStats, elapsed)
	}

	// Totals for footer
	var totalSamples uint64
	for _, fm := range state.procSyscallStats {
		for _, ss := range fm {
			totalSamples += ss.stats.count
		}
	}
	nProcs := len(state.procSyscallStats)

	state.mu.Unlock()

	d.renderFooter(&buf, elapsed, totalSamples, nProcs, metrics, mapCap)

	d.resetCursor()
	fmt.Print(buf.String())
}

func (d *Display) renderFooter(buf *strings.Builder, elapsed time.Duration, totalSamples uint64, nProcs int, metrics *runtimeMetrics, mapCap int64) {
	drops := metrics.drops.Load()
	evicted := metrics.evicted.Load()
	mapUsed := metrics.mapUsed.Load()
	mapStale := metrics.mapStale.Load()

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
		maxPend := d.ring.MaxPending()
		pctFull := float64(pending) / float64(capBytes) * 100
		maxPct := float64(maxPend) / float64(capBytes) * 100
		avg1, avg0, last1, last0 := d.ring.PollStats()
		last0Str := "-"
		if last0 > 0 {
			last0Str = formatMicro(last0)
		}
		ringInfo = fmt.Sprintf(" | Ring avg: %6s/%s (%5.1f%%)  Ring max: %6s/%s (%5.1f%%)  avg1:%-6.0f avg0:%-8.1f last1:%-6s last0:%-8s",
			formatBytes(int64(pending)), formatBytes(int64(capBytes)), pctFull,
			formatBytes(maxPend), formatBytes(int64(capBytes)), maxPct,
			avg1, avg0, formatCount(last1), last0Str)
	}
	mapInfo := ""
	if mapCap > 0 {
		pct := float64(mapUsed) / float64(mapCap) * 100
		mapInfo = fmt.Sprintf(" | Map: %s/%s (%4.1f%%) stale:%s evict:%s",
			formatCount(mapUsed), formatCount(mapCap), pct,
			formatCount(mapStale), formatCount(int64(evicted)))
	}
	fmt.Fprintf(buf, "Total: %s syscalls | Rate: %s/s | Processes: %d | Drops: %s (%s/s)%s%s\n",
		formatCount(int64(totalSamples)), formatCount(int64(rate)), nProcs,
		formatCount(int64(drops)), formatCount(int64(dropRate)), mapInfo, ringInfo)

	if d.batchMode {
		buf.WriteString("\n")
	}
}

func (d *Display) renderTable(buf *strings.Builder, procStats map[string]map[uint32]*syscallStats) {
	entries := collectEntries(procStats, false)

	if len(entries) == 0 {
		return
	}

	// Limit to top N (0 = all)
	shown := len(entries)
	if d.topN > 0 && shown > d.topN {
		shown = d.topN
	}

	// Compute label column width from visible entries
	labelWidth := 12
	for i := 0; i < shown; i++ {
		if n := len(entries[i].label); n > labelWidth {
			labelWidth = n
		}
	}
	labelWidth++ // padding

	// Section title
	var title string
	if len(d.focusProcesses) > 0 {
		title = strings.Join(d.focusProcesses, ",")
	} else {
		title = "All Processes"
	}
	lineWidth := labelWidth + 142
	sectionHeader(buf, fmt.Sprintf("%s (%d)", title, shown), lineWidth)

	// Column headers
	nameFmt := fmt.Sprintf("%%-%ds", labelWidth)
	fmt.Fprintf(buf, "%s │ %8s %8s %8s %8s %8s %8s %8s %8s %8s │ %8s %8s %8s %8s %8s │ %9s\n",
		fmt.Sprintf(nameFmt, "LIFETIME"),
		"min", "avg", "p25", "p50", "p75", "p90", "p99", "p99.9", "max",
		"max-4", "max-3", "max-2", "max-1", "max", "samples")
	buf.WriteString(strings.Repeat("-", lineWidth))
	buf.WriteString("\n")

	// Data rows
	for i := 0; i < shown; i++ {
		e := entries[i]
		name := fmt.Sprintf(nameFmt, e.label)
		renderDetailRow(buf, name, e.ss.stats, e.ss.sketch, e.ss.top)
	}

	buf.WriteString("\n")
}

func renderDetailRow(buf *strings.Builder, name string, st *simpleStats, sketch *ddsketch.DDSketch, top *topN) {
	n := st.count
	if n == 0 {
		fmt.Fprintf(buf, "%s │ %8s %8s %8s %8s %8s %8s %8s %8s %8s │ %8s %8s %8s %8s %8s │ %9s\n",
			name, "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "0")
		return
	}
	p25, _ := sketch.GetValueAtQuantile(0.25)
	p50, _ := sketch.GetValueAtQuantile(0.50)
	p75, _ := sketch.GetValueAtQuantile(0.75)
	p90, _ := sketch.GetValueAtQuantile(0.90)
	p99, _ := sketch.GetValueAtQuantile(0.99)
	p999, _ := sketch.GetValueAtQuantile(0.999)
	fmt.Fprintf(buf, "%s │ %s %s %s %s %s %s %s %s %s │ %s │ %9s\n",
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

const summaryLineWidth = 97

func (d *Display) renderSummary(buf *strings.Builder, procStats map[string]map[uint32]*syscallStats, elapsed time.Duration) {
	entries := collectEntries(procStats, true)

	totalSecs := elapsed.Seconds()
	nPerCol := d.topN // rows per column
	if nPerCol <= 0 {
		nPerCol = (len(entries) + 1) / 2
	}
	totalShown := nPerCol * 2
	if totalShown > len(entries) {
		totalShown = len(entries)
	}

	dualWidth := summaryLineWidth + 3 + summaryLineWidth

	sectionHeader(buf, fmt.Sprintf("Process × Syscall (top %d)", totalShown), dualWidth)

	hdr := fmt.Sprintf("%-28s │ %8s %8s %8s %8s %8s │ %9s %9s",
		"LIFETIME", "avg", "p50", "p90", "p99", "max", "samples", "rate")
	fmt.Fprintf(buf, "%s │ %s\n", hdr, hdr)
	buf.WriteString(strings.Repeat("-", dualWidth))
	buf.WriteString("\n")

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
	var rightSlice []tableEntry
	if rightStart < len(entries) {
		rightSlice = entries[rightStart:rightEnd]
	}

	maxRows := len(leftSlice)
	if len(rightSlice) > maxRows {
		maxRows = len(rightSlice)
	}

	for i := 0; i < maxRows; i++ {
		var leftStr, rightStr string

		if i < len(leftSlice) {
			leftStr = formatSummaryRow(leftSlice[i].label, leftSlice[i].ss.stats, leftSlice[i].ss.sketch, totalSecs)
		} else {
			leftStr = strings.Repeat(" ", summaryLineWidth)
		}

		if i < len(rightSlice) {
			rightStr = formatSummaryRow(rightSlice[i].label, rightSlice[i].ss.stats, rightSlice[i].ss.sketch, totalSecs)
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

// collectProcessSummaries aggregates per-process totals from procSyscallStats.
// Must be called while state.mu is held.
func collectProcessSummaries(procStats map[string]map[uint32]*syscallStats, elapsedSecs float64) []processSummary {
	summaries := make([]processSummary, 0, len(procStats))
	for proc, fm := range procStats {
		var total uint64
		for _, ss := range fm {
			total += ss.stats.count
		}
		rate := float64(0)
		if elapsedSecs > 0 {
			rate = float64(total) / elapsedSecs
		}
		summaries = append(summaries, processSummary{name: proc, count: total, rate: rate})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].count != summaries[j].count {
			return summaries[i].count > summaries[j].count
		}
		return summaries[i].name < summaries[j].name
	})
	return summaries
}

// padOrTrunc pads s with spaces or truncates to exactly width characters.
func padOrTrunc(s string, width int) string {
	if len(s) >= width {
		return s[:width]
	}
	return s + strings.Repeat(" ", width-len(s))
}

// renderPanel builds the right-side process panel lines.
func renderPanel(summaries []processSummary, highlightProc, filterText string) []string {
	var lines []string

	// Header
	lines = append(lines, padOrTrunc("  PROCESS         RATE    TOTAL", panelWidth))
	lines = append(lines, strings.Repeat("─", panelWidth))

	for _, ps := range summaries {
		if filterText != "" && !strings.Contains(ps.name, filterText) {
			continue
		}
		marker := " "
		if ps.name == highlightProc {
			marker = ">"
		}
		rateStr := "-"
		if ps.rate >= 1 {
			rateStr = formatCount(int64(ps.rate)) + "/s"
		} else if ps.rate > 0 {
			rateStr = fmt.Sprintf("%.1f/s", ps.rate)
		}
		line := fmt.Sprintf("%s %-15s %8s %8s", marker, padOrTrunc(ps.name, 15), rateStr, formatCount(int64(ps.count)))
		lines = append(lines, padOrTrunc(line, panelWidth))
	}
	return lines
}

// parseFocusList parses the -c flag into a deduplicated list of process names,
// truncated to 15 chars to match BPF TASK_COMM_LEN-1.
func parseFocusList() []string {
	if *focusProcs == "" {
		return nil
	}
	seen := make(map[string]bool)
	var list []string
	for _, name := range strings.Split(*focusProcs, ",") {
		name = strings.TrimSpace(name)
		if len(name) > 15 {
			name = name[:15]
		}
		if name != "" && !seen[name] {
			seen[name] = true
			list = append(list, name)
		}
	}
	return list
}

func configureBPFFilters(objs *bpfObjects, focusList []string) {
	for _, name := range focusList {
		var comm [16]byte
		copy(comm[:], name)
		var val uint8 = 1
		if err := objs.TargetComms.Put(comm, val); err != nil {
			log.Fatalf("Failed to add comm filter %q: %v", name, err)
		}
	}
}

// runReader busy-polls the ring buffer, batches events, and flushes to state.
func runReader(rd *RingPollReader, state *State) {
	var rec PollRecord
	eventSize := int(unsafe.Sizeof(bpfLatencyEvent{}))
	batch := make([]pendingEvent, 0, flushSize)
	lastFlush := time.Now()

	for rd.ReadInto(&rec) {
		if len(rec.RawSample) < eventSize {
			continue
		}
		event := *(*bpfLatencyEvent)(unsafe.Pointer(&rec.RawSample[0]))
		latencyUs := int64(event.LatencyNs / 1000)
		if latencyUs < 1 {
			latencyUs = 1
		}
		if latencyUs > maxLatencyUs {
			latencyUs = maxLatencyUs
		}
		batch = append(batch, pendingEvent{commString(event.Comm), event.SyscallId, latencyUs})

		if len(batch) >= flushSize || time.Since(lastFlush) >= flushInterval {
			state.RecordBatch(batch)
			rd.Commit()
			batch = batch[:0]
			lastFlush = time.Now()
		}
	}
	if len(batch) > 0 {
		state.RecordBatch(batch)
		rd.Commit()
	}
}

func main() {
	flag.Parse()
	focusList := parseFocusList()

	// Remove memlock limit for eBPF
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatalf("Failed to remove memlock limit: %v", err)
	}

	// Load eBPF spec and rewrite constants before loading into kernel.
	// When use_comm_filter is 0 (default), the verifier dead-code-eliminates
	// the comm filter branch — zero overhead in the hot path.
	spec, err := loadBpf()
	if err != nil {
		log.Fatalf("Failed to load eBPF spec: %v", err)
	}
	if len(focusList) > 0 {
		if err := spec.RewriteConstants(map[string]interface{}{
			"use_comm_filter": uint8(1),
		}); err != nil {
			log.Fatalf("Failed to rewrite BPF constants: %v", err)
		}
	}
	objs := bpfObjects{}
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		log.Fatalf("Failed to load eBPF objects: %v", err)
	}
	defer objs.Close()

	configureBPFFilters(&objs, focusList)

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

	state := newState()
	display := &Display{
		batchMode:      *batch,
		focusProcesses: focusList,
		topN:           *topProcs,
		ring:           rd,
	}
	metrics := &runtimeMetrics{}
	mapMaxVal := int64(objs.StartTimes.MaxEntries())

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

	// Reader goroutine: busy-polls ring buffer, batches events, flushes under single Lock
	var readerDone sync.WaitGroup
	readerDone.Add(1)
	go func() {
		defer readerDone.Done()
		runReader(rd, state)
	}()

	// Display goroutine (10 FPS)
	displayTicker := time.NewTicker(displayInterval)
	go func() {
		defer displayTicker.Stop()
		for {
			select {
			case <-done:
				return
			case <-displayTicker.C:
				readDropCount(objs.DropCount, &metrics.drops)
				display.render(state, metrics, mapMaxVal)
			}
		}
	}()

	// Stale entry cleanup goroutine
	cleanupTicker := time.NewTicker(*cleanupInterval)
	go func() {
		defer cleanupTicker.Stop()
		for {
			select {
			case <-done:
				return
			case <-cleanupTicker.C:
				total, stale, evicted := cleanStaleEntries(objs.StartTimes, objs.SyscallIds, *staleAge, *evictAge)
				metrics.mapUsed.Store(int64(total - evicted))
				metrics.mapStale.Store(int64(stale - evicted))
				if evicted > 0 {
					metrics.evicted.Add(uint64(evicted))
				}
			}
		}
	}()

	if len(focusList) > 0 {
		log.Printf("Tracing all syscalls | processes: %s (BPF-filtered) | top %d rows",
			strings.Join(focusList, ","), *topProcs)
	} else {
		log.Printf("Tracing all syscalls | all processes | top %d rows", *topProcs)
	}

	// Wait for signal, then drain
	<-done
	readerDone.Wait()

	readDropCount(objs.DropCount, &metrics.drops)
	display.render(state, metrics, mapMaxVal)
}

// ktimeNow returns the current CLOCK_MONOTONIC time in nanoseconds,
// matching bpf_ktime_get_ns() used by the BPF program.
func ktimeNow() uint64 {
	var ts unix.Timespec
	unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return uint64(ts.Sec)*1_000_000_000 + uint64(ts.Nsec)
}

// cleanStaleEntries iterates start_times, counts entries older than staleAge,
// and evicts entries older than evictAge.
// Returns (total entries, stale count, evicted count).
func cleanStaleEntries(startTimes, syscallIds *ebpf.Map, staleAge, evictAge time.Duration) (int, int, int) {
	now := ktimeNow()
	staleThresh := now - uint64(staleAge.Nanoseconds())
	evictThresh := now - uint64(evictAge.Nanoseconds())

	var tid uint32
	var startNs uint64
	var toDelete []uint32
	total := 0
	stale := 0

	iter := startTimes.Iterate()
	for iter.Next(&tid, &startNs) {
		total++
		if startNs < staleThresh {
			stale++
		}
		if startNs < evictThresh {
			toDelete = append(toDelete, tid)
		}
	}

	for _, tid := range toDelete {
		startTimes.Delete(tid)
		syscallIds.Delete(tid)
	}
	return total, stale, len(toDelete)
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
