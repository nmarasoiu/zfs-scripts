// syscall-latency: Per-syscall latency percentile tracker using eBPF
//
// Traces syscall enter/exit to compute per-syscall latency,
// maintains HDR histograms per syscall type, emits percentiles on interval.
//
// Usage: syscall-latency [-c comm] [-s syscalls] [-i interval]
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -target bpfel -type latency_event bpf bpf/syscall_latency.c -- -I/usr/include -I.

package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

const (
	displayInterval = 100 * time.Millisecond // 10 FPS display refresh
	histMin         = 1
	histMax         = 60_000_000 // 60 seconds in µs
	histSigFig      = 3
)

var (
	interval    = flag.Duration("i", 10*time.Second, "stats reset interval")
	processName = flag.String("c", "", "filter by process name (e.g., storagenode)")
	syscallList = flag.String("s", "pread64,pwrite64,fsync,fdatasync,read,write", "comma-separated syscalls to trace")
	batch       = flag.Bool("batch", false, "batch mode (no screen clearing)")
)

// x86_64 syscall numbers
var syscallNums = map[string]uint32{
	"read":      0,
	"write":     1,
	"open":      2,
	"close":     3,
	"stat":      4,
	"fstat":     5,
	"lstat":     6,
	"poll":      7,
	"lseek":     8,
	"mmap":      9,
	"mprotect":  10,
	"munmap":    11,
	"brk":       12,
	"pread64":   17,
	"pwrite64":  18,
	"readv":     19,
	"writev":    20,
	"access":    21,
	"pipe":      22,
	"select":    23,
	"dup":       32,
	"dup2":      33,
	"socket":    41,
	"connect":   42,
	"accept":    43,
	"sendto":    44,
	"recvfrom":  45,
	"sendmsg":   46,
	"recvmsg":   47,
	"shutdown":  48,
	"bind":      49,
	"listen":    50,
	"clone":     56,
	"fork":      57,
	"vfork":     58,
	"execve":    59,
	"exit":      60,
	"wait4":     61,
	"kill":      62,
	"fcntl":     72,
	"flock":     73,
	"fsync":     74,
	"fdatasync": 75,
	"truncate":  76,
	"ftruncate": 77,
	"getdents":  78,
	"getcwd":    79,
	"chdir":     80,
	"rename":    82,
	"mkdir":     83,
	"rmdir":     84,
	"creat":     85,
	"link":      86,
	"unlink":    87,
	"symlink":   88,
	"readlink":  89,
	"chmod":     90,
	"fchmod":    91,
	"chown":     92,
	"fchown":    93,
	"lchown":    94,
	"umask":     95,
	"openat":    257,
	"mkdirat":   258,
	"fstatat":   262,
	"unlinkat":  263,
	"renameat":  264,
	"faccessat": 269,
	"splice":    275,
	"sync":      162,
	"syncfs":    306,
	"fallocate": 285,
	"epoll_wait":    232,
	"epoll_pwait":   281,
	"futex":         202,
	"nanosleep":     35,
	"accept4":       288,
	"recvmmsg":      299,
	"sendmmsg":      307,
}

// Reverse map for display
var syscallNames = make(map[uint32]string)

func init() {
	for name, num := range syscallNums {
		syscallNames[num] = name
	}
}

func formatLatency(us int64) string {
	if us < 1000 {
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

// syscallStats holds both interval and lifetime histograms for a syscall
type syscallStats struct {
	interval    *hdrhistogram.Histogram
	lifetime    *hdrhistogram.Histogram
	intervalTop *topN
	lifetimeTop *topN
}

func newSyscallStats() *syscallStats {
	return &syscallStats{
		interval:    hdrhistogram.New(histMin, histMax, histSigFig),
		lifetime:    hdrhistogram.New(histMin, histMax, histSigFig),
		intervalTop: newTopN(5),
		lifetimeTop: newTopN(5),
	}
}

func (ss *syscallStats) Record(latencyUs int64) {
	ss.interval.RecordValue(latencyUs)
	ss.lifetime.RecordValue(latencyUs)
	ss.intervalTop.Add(latencyUs)
	ss.lifetimeTop.Add(latencyUs)
}

func (ss *syscallStats) ResetInterval() {
	ss.interval.Reset()
	ss.intervalTop.Reset()
}

func (ss *syscallStats) Snapshot() *syscallStats {
	return &syscallStats{
		interval:    hdrhistogram.Import(ss.interval.Export()),
		lifetime:    hdrhistogram.Import(ss.lifetime.Export()),
		intervalTop: ss.intervalTop.Clone(),
		lifetimeTop: ss.lifetimeTop.Clone(),
	}
}

// State holds all syscall stats
type State struct {
	mu        sync.RWMutex
	stats     map[uint32]*syscallStats // syscall_id -> stats
	startTime time.Time
	lastReset time.Time
}

func newState() *State {
	now := time.Now()
	return &State{
		stats:     make(map[uint32]*syscallStats),
		startTime: now,
		lastReset: now,
	}
}

func (s *State) Record(syscallID uint32, latencyUs int64) {
	s.mu.Lock()
	ss, ok := s.stats[syscallID]
	if !ok {
		ss = newSyscallStats()
		s.stats[syscallID] = ss
	}
	ss.Record(latencyUs)
	s.mu.Unlock()
}

func (s *State) ResetIntervals() {
	s.mu.Lock()
	for _, ss := range s.stats {
		ss.ResetInterval()
	}
	s.lastReset = time.Now()
	s.mu.Unlock()
}

func (s *State) Snapshot() (map[uint32]*syscallStats, time.Time, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := make(map[uint32]*syscallStats)
	for id, ss := range s.stats {
		snap[id] = ss.Snapshot()
	}
	return snap, s.startTime, s.lastReset
}

// Display handles rendering
type Display struct {
	batchMode   bool
	processName string
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

const lineWidth = 160

func (d *Display) render(stats map[uint32]*syscallStats, startTime, lastReset time.Time, intervalDur time.Duration) {
	var buf strings.Builder
	now := time.Now()

	// Sort syscalls by name
	var syscallList []uint32
	for id := range stats {
		syscallList = append(syscallList, id)
	}
	sort.Slice(syscallList, func(i, j int) bool {
		ni, nj := syscallNames[syscallList[i]], syscallNames[syscallList[j]]
		if ni == "" {
			ni = fmt.Sprintf("syscall_%d", syscallList[i])
		}
		if nj == "" {
			nj = fmt.Sprintf("syscall_%d", syscallList[j])
		}
		return ni < nj
	})

	timestamp := now.Format("15:04:05")
	elapsed := now.Sub(startTime)
	intervalElapsed := now.Sub(lastReset)

	title := "Syscall Latency Monitor"
	if d.processName != "" {
		title = fmt.Sprintf("Syscall Latency Monitor [%s]", d.processName)
	}

	fmt.Fprintf(&buf, "%s - %s (uptime: %s, interval: %s/%s)\n",
		title, timestamp, formatDuration(elapsed), formatDuration(intervalElapsed), formatDuration(intervalDur))
	buf.WriteString(strings.Repeat("=", lineWidth))
	buf.WriteString("\n")

	// Header
	fmt.Fprintf(&buf, "%-12s │ %8s %8s %8s %8s %8s %8s │ %8s %8s %8s %8s %8s │ %9s\n",
		"INTERVAL", "avg", "p50", "p90", "p99", "p99.9", "max",
		"max-4", "max-3", "max-2", "max-1", "max", "samples")
	buf.WriteString(strings.Repeat("-", lineWidth))
	buf.WriteString("\n")

	// Interval stats
	for _, id := range syscallList {
		ss := stats[id]
		name := syscallNames[id]
		if name == "" {
			name = fmt.Sprintf("sys_%d", id)
		}
		h := ss.interval
		n := h.TotalCount()
		if n == 0 {
			fmt.Fprintf(&buf, "%-12s │ %8s %8s %8s %8s %8s %8s │ %8s %8s %8s %8s %8s │ %9s\n",
				name, "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "0")
			continue
		}
		fmt.Fprintf(&buf, "%-12s │ %s %s %s %s %s %s │ %s │ %9s\n",
			name,
			formatLatencyPadded(int64(h.Mean())),
			formatLatencyPadded(h.ValueAtQuantile(50)),
			formatLatencyPadded(h.ValueAtQuantile(90)),
			formatLatencyPadded(h.ValueAtQuantile(99)),
			formatLatencyPadded(h.ValueAtQuantile(99.9)),
			formatLatencyPadded(h.Max()),
			formatTop5(ss.intervalTop),
			formatCount(n),
		)
	}

	buf.WriteString("\n")
	fmt.Fprintf(&buf, "%-12s │ %8s %8s %8s %8s %8s %8s │ %8s %8s %8s %8s %8s │ %9s\n",
		"LIFETIME", "avg", "p50", "p90", "p99", "p99.9", "max",
		"max-4", "max-3", "max-2", "max-1", "max", "samples")
	buf.WriteString(strings.Repeat("-", lineWidth))
	buf.WriteString("\n")

	// Lifetime stats
	var totalSamples int64
	for _, id := range syscallList {
		ss := stats[id]
		name := syscallNames[id]
		if name == "" {
			name = fmt.Sprintf("sys_%d", id)
		}
		h := ss.lifetime
		n := h.TotalCount()
		totalSamples += n
		if n == 0 {
			fmt.Fprintf(&buf, "%-12s │ %8s %8s %8s %8s %8s %8s │ %8s %8s %8s %8s %8s │ %9s\n",
				name, "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "0")
			continue
		}
		fmt.Fprintf(&buf, "%-12s │ %s %s %s %s %s %s │ %s │ %9s\n",
			name,
			formatLatencyPadded(int64(h.Mean())),
			formatLatencyPadded(h.ValueAtQuantile(50)),
			formatLatencyPadded(h.ValueAtQuantile(90)),
			formatLatencyPadded(h.ValueAtQuantile(99)),
			formatLatencyPadded(h.ValueAtQuantile(99.9)),
			formatLatencyPadded(h.Max()),
			formatTop5(ss.lifetimeTop),
			formatCount(n),
		)
	}

	buf.WriteString(strings.Repeat("=", lineWidth))
	buf.WriteString("\n")

	rate := float64(0)
	if elapsed.Seconds() > 0 {
		rate = float64(totalSamples) / elapsed.Seconds()
	}
	fmt.Fprintf(&buf, "Total: %s syscalls | Rate: %s/s | HDR histograms: ~40KB/syscall\n",
		formatCount(totalSamples), formatCount(int64(rate)))

	if d.batchMode {
		buf.WriteString("\n")
	}

	d.resetCursor()
	fmt.Print(buf.String())
}

func main() {
	flag.Parse()

	// Parse syscall list
	var traceSyscalls []uint32
	for _, name := range strings.Split(*syscallList, ",") {
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
	for _, num := range traceSyscalls {
		var enabled uint8 = 1
		if err := objs.SyscallFilter.Put(num, enabled); err != nil {
			log.Fatalf("Failed to add syscall to filter: %v", err)
		}
	}

	// Set up process name filter if specified
	if *processName != "" {
		var key uint32 = 0
		comm := make([]byte, 16)
		copy(comm, *processName)
		if err := objs.TargetComm.Put(key, comm); err != nil {
			log.Fatalf("Failed to set process filter: %v", err)
		}
		log.Printf("Filtering by process: %s", *processName)
	}

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

	// Open ring buffer
	rd, err := ringbuf.NewReader(objs.Events)
	if err != nil {
		log.Fatalf("Failed to open ring buffer: %v", err)
	}
	defer rd.Close()

	state := newState()
	display := &Display{batchMode: *batch, processName: *processName}

	// Signal handling
	done := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig
		close(done)
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
				stats, startTime, lastReset := state.Snapshot()
				if len(stats) > 0 {
					display.render(stats, startTime, lastReset, *interval)
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

	syscallStr := make([]string, len(traceSyscalls))
	for i, num := range traceSyscalls {
		syscallStr[i] = syscallNames[num]
	}
	log.Printf("Tracing syscalls: %s (interval=%v, display=10fps)", strings.Join(syscallStr, ","), *interval)

	// Ring buffer consumer (main loop)
	var event bpfLatencyEvent
	for {
		select {
		case <-done:
			stats, startTime, lastReset := state.Snapshot()
			display.render(stats, startTime, lastReset, *interval)
			return
		default:
		}

		record, err := rd.Read()
		if err != nil {
			if err == ringbuf.ErrClosed {
				return
			}
			continue
		}

		if err := binary.Read(bytes.NewReader(record.RawSample), binary.LittleEndian, &event); err != nil {
			continue
		}

		latencyUs := int64(event.LatencyNs / 1000)
		if latencyUs < 1 {
			latencyUs = 1
		}
		if latencyUs > histMax {
			latencyUs = histMax
		}

		state.Record(event.SyscallId, latencyUs)
	}
}
