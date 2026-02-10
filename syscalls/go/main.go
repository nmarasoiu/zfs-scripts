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
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/nmarasoiu/zfs-scripts/ringpoll"
	"golang.org/x/sys/unix"
)

const (
	defaultPercentiles      = "50,90,99"             // summary view (no -c)
	defaultTablePercentiles = "25,50,75,90,99,99.9"  // table view (with -c)
)

var (
	version = "dev"
	commit  = ""
)

var (
	focusProcs  = flag.String("c", "", "only trace these processes (comma-separated, empty=all)")
	topProcs    = flag.Int("n", 0, "top N rows to display (0=all)")
	batch       = flag.Bool("batch", false, "batch mode (no screen clearing)")
	colsFlag    = flag.Int("cols", 0, "override terminal width (enables panel in batch mode)")
	pollSleep   = flag.Duration("poll-sleep", 50*time.Microsecond, "ring buffer poll sleep when all rings are empty")
	maxSketches = flag.Int("max-sketches", 0, "max process×syscall sketches to keep (LRU eviction; 0=auto: 4×n)")
	showVersion = flag.Bool("version", false, "print version and exit")

	// Timing
	displayRefreshInterval = flag.Duration("display-refresh-interval", 100*time.Millisecond, "display refresh interval (e.g. 100ms, 200ms)")
	batchSize              = flag.Int("batch-size", 1024, "event batch: max events before flush to stats")
	batchInterval          = flag.Duration("batch-interval", 10*time.Millisecond, "event batch: max time before flush to stats")
	mapSampleInterval      = flag.Duration("map-sample-interval", 2*time.Second, "BPF map occupancy sample interval")

	// BPF
	ringSizeFlag   = flag.String("ring-size", "2M", "per-ring buffer size (e.g. 512K, 2M, 8M); must be power of 2, ≥4K; 4 rings total")
	mapEntriesFlag = flag.Int("map-entries", 65536, "BPF LRU hash map max entries (in-flight syscall tracking capacity)")

	// DDSketch
	alphaFlag       = flag.Float64("alpha", 0.25, "DDSketch relative accuracy — 0.25 means any reported\n\tpercentile is within ±25% of the true value; lower values\n\tuse more memory but give tighter bounds.\n\tSee: https://arxiv.org/abs/1908.10693")
	percentilesFlag = flag.String("percentiles", defaultPercentiles, "comma-separated percentile list (e.g. 50,90,99,99.9)")
)

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

// parseSize parses a human-readable byte size (e.g. "2M", "512K", "4096").
// Accepts suffixes K, M, G (case-insensitive, optional trailing B).
func parseSize(s string) (uint32, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	s = strings.ToUpper(s)
	s = strings.TrimSuffix(s, "B") // "2MB" → "2M"
	multiplier := uint64(1)
	if strings.HasSuffix(s, "K") {
		multiplier = 1024
		s = s[:len(s)-1]
	} else if strings.HasSuffix(s, "M") {
		multiplier = 1024 * 1024
		s = s[:len(s)-1]
	} else if strings.HasSuffix(s, "G") {
		multiplier = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	}
	n, err := fmt.Sscanf(s, "%d", new(uint64))
	if n != 1 || err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	var val uint64
	fmt.Sscanf(s, "%d", &val)
	result := val * multiplier
	if result > 1<<32-1 {
		return 0, fmt.Errorf("size %d exceeds uint32 max", result)
	}
	return uint32(result), nil
}

// isPowerOf2 returns true if n > 0 and n is a power of two.
func isPowerOf2(n uint32) bool {
	return n > 0 && n&(n-1) == 0
}

// parsePercentiles parses a comma-separated list of percentiles (0–100 exclusive)
// and returns sorted quantiles in 0.0–1.0 form.
func parsePercentiles(s string) ([]float64, error) {
	parts := strings.Split(s, ",")
	var quantiles []float64
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var val float64
		if _, err := fmt.Sscanf(p, "%f", &val); err != nil {
			return nil, fmt.Errorf("invalid percentile %q: %w", p, err)
		}
		if val <= 0 || val >= 100 {
			return nil, fmt.Errorf("percentile %g must be in (0, 100)", val)
		}
		quantiles = append(quantiles, val/100)
	}
	if len(quantiles) == 0 {
		return nil, fmt.Errorf("empty percentile list")
	}
	sort.Float64s(quantiles)
	return quantiles, nil
}

func allClosed(readers []*ringpoll.Reader) bool {
	for _, rd := range readers {
		if !rd.Closed() {
			return false
		}
	}
	return true
}

func commitAll(readers []*ringpoll.Reader) {
	for _, rd := range readers {
		rd.CommitAndSnap()
	}
}

// runReader busy-polls multiple ring buffers in round-robin, batches events,
// and flushes to state. Single goroutine — no new contention points.
func runReader(readers []*ringpoll.Reader, pollSleep time.Duration, batchSz int, batchIntv time.Duration, state *State) {
	var rec ringpoll.Record
	eventSize := int(unsafe.Sizeof(bpfLatencyEvent{}))
	pending := make([]pendingEvent, 0, batchSz)
	lastFlush := time.Now()

	for !allClosed(readers) {
		gotAny := false
		for _, rd := range readers {
			for rd.Poll(&rec) {
				if len(rec.RawSample) < eventSize {
					continue
				}
				event := *(*bpfLatencyEvent)(unsafe.Pointer(&rec.RawSample[0]))
				latencyUs := int64(event.LatencyNs / 1000)
				if latencyUs < 1 {
					latencyUs = 1
				}
				pending = append(pending, pendingEvent{event.Comm, event.SyscallId, latencyUs})
				gotAny = true

				if len(pending) >= batchSz {
					state.RecordBatch(pending)
					commitAll(readers)
					pending = pending[:0]
					lastFlush = time.Now()
				}
			}
		}
		if !gotAny {
			commitAll(readers)
			time.Sleep(pollSleep)
		} else if time.Since(lastFlush) >= batchIntv {
			state.RecordBatch(pending)
			commitAll(readers)
			pending = pending[:0]
			lastFlush = time.Now()
		}
	}
	// flush remainder
	if len(pending) > 0 {
		state.RecordBatch(pending)
		commitAll(readers)
	}
}

func snapshotRingStats(readers []*ringpoll.Reader, ra *ringAvg) *ringStats {
	// For capacity stats (avg/max/cap), use worst-case ring — each ring
	// drops independently, so the most-filled ring is what matters.
	var worstPending int
	var perRingCap int64
	var worstMaxPending int64
	var totalEventSum, totalNonEmpty, totalPollCount, totalLastNonEmpty int64
	var latestEmptyNano int64

	for _, rd := range readers {
		p := rd.Pending()
		if p > worstPending {
			worstPending = p
		}
		perRingCap = int64(rd.BufSize()) // all rings same size
		snap := rd.Snapshot()
		if snap != nil {
			if snap.MaxPending > worstMaxPending {
				worstMaxPending = snap.MaxPending
			}
			totalEventSum += snap.EventSum
			totalNonEmpty += snap.NonEmptyCount
			totalPollCount += snap.PollCount
			totalLastNonEmpty += snap.LastNonEmpty
			if snap.LastEmptyNano > latestEmptyNano {
				latestEmptyNano = snap.LastEmptyNano
			}
		}
	}

	ra.add(worstPending)

	var avg1, avg0 float64
	if totalNonEmpty > 0 {
		avg1 = float64(totalEventSum) / float64(totalNonEmpty)
	}
	if totalPollCount > 0 {
		avg0 = float64(totalEventSum) / float64(totalPollCount)
	}
	var last0 time.Duration
	if latestEmptyNano > 0 {
		last0 = time.Since(time.Unix(0, latestEmptyNano))
	}

	return &ringStats{
		capacityStats: capacityStats{
			avg: ra.avg(),
			max: worstMaxPending,
			cap: perRingCap,
		},
		pending: worstPending,
		avg1:    avg1,
		avg0:    avg0,
		last1:   totalLastNonEmpty,
		last0:   last0,
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "syscall-latency: %v\n", err)
		os.Exit(1)
	}
}

func printVersion() {
	if commit != "" {
		fmt.Printf("syscall-latency %s (%s)\n", version, commit)
	} else {
		fmt.Printf("syscall-latency %s\n", version)
	}
}

func run() error {
	flag.Parse()
	if *showVersion {
		printVersion()
		return nil
	}

	// Validate and parse new flags
	ringSize, err := parseSize(*ringSizeFlag)
	if err != nil {
		return fmt.Errorf("-ring-size: %w", err)
	}
	if !isPowerOf2(ringSize) || ringSize < 4096 {
		return fmt.Errorf("-ring-size: must be a power of 2 and ≥ 4K (got %d)", ringSize)
	}
	if *alphaFlag <= 0 || *alphaFlag >= 1 {
		return fmt.Errorf("-alpha: must be in (0, 1) (got %g)", *alphaFlag)
	}
	quantiles, err := parsePercentiles(*percentilesFlag)
	if err != nil {
		return fmt.Errorf("-percentiles: %w", err)
	}
	if *mapEntriesFlag <= 0 {
		return fmt.Errorf("-map-entries: must be > 0 (got %d)", *mapEntriesFlag)
	}

	if *maxSketches <= 0 {
		if *topProcs > 0 {
			*maxSketches = 4 * *topProcs // 2× what fits on screen (n×2 columns)
		} else {
			*maxSketches = 4096
		}
	}
	focusList := parseFocusList()

	// When -c is given and user didn't explicitly set -percentiles,
	// use the full table set (matches old table view defaults).
	if len(focusList) > 0 {
		percentilesExplicit := false
		flag.Visit(func(f *flag.Flag) {
			if f.Name == "percentiles" {
				percentilesExplicit = true
			}
		})
		if !percentilesExplicit {
			quantiles, _ = parsePercentiles(defaultTablePercentiles)
		}
	}

	// Remove memlock limit for eBPF
	if err := rlimit.RemoveMemlock(); err != nil {
		return fmt.Errorf("remove memlock limit: %w", err)
	}

	// Load eBPF spec and rewrite constants before loading into kernel.
	// When use_comm_filter is 0 (default), the verifier dead-code-eliminates
	// the comm filter branch — zero overhead in the hot path.
	spec, err := loadBpf()
	if err != nil {
		return fmt.Errorf("load eBPF spec: %w", err)
	}
	if len(focusList) > 0 {
		if err := spec.RewriteConstants(map[string]interface{}{
			"use_comm_filter": uint8(1),
		}); err != nil {
			return fmt.Errorf("rewrite BPF constants: %w", err)
		}
	}

	// Resize BPF maps from CLI flags before loading into kernel
	for _, name := range []string{"events0", "events1", "events2", "events3"} {
		spec.Maps[name].MaxEntries = ringSize
	}
	for _, name := range []string{"start_times", "syscall_ids"} {
		spec.Maps[name].MaxEntries = uint32(*mapEntriesFlag)
	}

	objs := bpfObjects{}
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		return fmt.Errorf("load eBPF objects: %w", err)
	}
	defer objs.Close()

	if err := configureBPFFilters(&objs, focusList); err != nil {
		return err
	}

	// Attach tracepoints
	tpEnter, err := link.Tracepoint("raw_syscalls", "sys_enter", objs.TraceSyscallEnter, nil)
	if err != nil {
		return fmt.Errorf("attach sys_enter: %w", err)
	}
	defer tpEnter.Close()

	tpExit, err := link.Tracepoint("raw_syscalls", "sys_exit", objs.TraceSyscallExit, nil)
	if err != nil {
		return fmt.Errorf("attach sys_exit: %w", err)
	}
	defer tpExit.Close()

	// Open per-CPU ring buffers (busy-poll readers — no epoll)
	ringMaps := []*ebpf.Map{objs.Events0, objs.Events1, objs.Events2, objs.Events3}
	readers := make([]*ringpoll.Reader, len(ringMaps))
	for i, m := range ringMaps {
		rd, err := ringpoll.NewReader(m, *pollSleep)
		if err != nil {
			// Clean up already-opened readers
			for j := 0; j < i; j++ {
				readers[j].Cleanup()
			}
			return fmt.Errorf("open ring buffer %d: %w", i, err)
		}
		readers[i] = rd
	}
	defer func() {
		for _, rd := range readers {
			rd.Cleanup()
		}
	}()

	state := newState(*maxSketches, *alphaFlag)
	mapCap := int64(objs.StartTimes.MaxEntries())
	interactive := !*batch && isTerminal(int(os.Stdin.Fd())) && isTerminal(int(os.Stdout.Fd()))
	display := &Display{
		batchMode:      *batch,
		focusProcesses: focusList,
		topN:           *topProcs,
		colsOverride:   *colsFlag,
		interactive:    interactive,
		quantiles:      quantiles,
	}
	metrics := &runtimeMetrics{}

	// Terminal raw mode for interactive input
	var origTermios *unix.Termios
	if interactive {
		origTermios = enableRawMode()
		fmt.Print("\033[?25l") // hide cursor
	}
	termCleanup := sync.OnceFunc(func() {
		if interactive {
			fmt.Print("\033[?25h") // restore cursor
		}
		restoreTermMode(origTermios)
	})

	// Signal handling
	done := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig
		signal.Stop(sig) // restore default handler so second Ctrl+C force-kills
		termCleanup()
		close(done)
		for _, rd := range readers {
			rd.Close()
		}
	}()
	defer termCleanup()

	// Reader goroutine: busy-polls ring buffers, batches events, flushes under single Lock
	var readerDone sync.WaitGroup
	readerDone.Add(1)
	go func() {
		defer readerDone.Done()
		runReader(readers, *pollSleep, *batchSize, *batchInterval, state)
	}()

	// Input goroutine for interactive mode
	keyCh := make(chan keyEvent, 16)
	if interactive {
		go runInput(keyCh)
	}

	// Map occupancy goroutine
	mapTicker := time.NewTicker(*mapSampleInterval)
	go func() {
		defer mapTicker.Stop()
		for {
			select {
			case <-done:
				return
			case <-mapTicker.C:
				used := countMapEntries(objs.StartTimes)
				metrics.mapSumUsed.Add(used)
				metrics.mapSamples.Add(1)
				if cur := metrics.mapMaxUsed.Load(); used > cur {
					metrics.mapMaxUsed.Store(used)
				}
			}
		}
	}()

	// Display goroutine
	displayTicker := time.NewTicker(*displayRefreshInterval)
	go func() {
		defer displayTicker.Stop()
		var ra ringAvg
		for {
			select {
			case <-done:
				return
			case <-displayTicker.C:
				readDropCount(objs.DropCount, &metrics.drops)
				display.render(state, metrics.drops.Load(), snapshotMapStats(metrics, mapCap), snapshotRingStats(readers, &ra))
			case ev := <-keyCh:
				if display.handleKey(ev) {
					// 'q' pressed — trigger shutdown via signal
					sig <- syscall.SIGINT
					return
				}
				readDropCount(objs.DropCount, &metrics.drops)
				display.render(state, metrics.drops.Load(), snapshotMapStats(metrics, mapCap), snapshotRingStats(readers, &ra))
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
	display.render(state, metrics.drops.Load(), snapshotMapStats(metrics, mapCap), snapshotRingStats(readers, &ringAvg{}))
	return nil
}
