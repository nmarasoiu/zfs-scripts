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
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/nmarasoiu/zfs-scripts/ringpoll"
	"golang.org/x/sys/unix"
)

const (
	displayInterval = 100 * time.Millisecond // 10 FPS display refresh
	flushSize       = 1024
	flushInterval   = 10 * time.Millisecond
)

var (
	focusProcs = flag.String("c", "", "only trace these processes (comma-separated, empty=all)")
	topProcs   = flag.Int("n", 0, "top N rows to display (0=all)")
	batch      = flag.Bool("batch", false, "batch mode (no screen clearing)")
	colsFlag   = flag.Int("cols", 0, "override terminal width (enables panel in batch mode)")
	pollSleep       = flag.Duration("poll-sleep", 50*time.Microsecond, "ring buffer poll sleep when empty")
	cleanupInterval = flag.Duration("cleanup-interval", 5*time.Second, "how often to scan BPF hash map for stale entries")
	staleAge        = flag.Duration("stale-age", 10*time.Second, "age threshold to count an in-flight entry as stale")
	evictAge        = flag.Duration("evict-age", 60*time.Second, "age threshold to evict (delete) a stale entry")
	maxSketches     = flag.Int("max-sketches", 4096, "max process×syscall sketches to keep (LRU eviction)")
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

// runReader busy-polls the ring buffer, batches events, and flushes to state.
func runReader(rd *ringpoll.Reader, state *State) {
	var rec ringpoll.Record
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

func snapshotRingStats(rd *ringpoll.Reader) *ringStats {
	if rd == nil {
		return nil
	}
	avg1, avg0, last1, last0 := rd.PollStats()
	return &ringStats{
		pending:    rd.Pending(),
		capBytes:   rd.BufSize(),
		maxPending: rd.MaxPending(),
		avg1:       avg1,
		avg0:       avg0,
		last1:      last1,
		last0:      last0,
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
	rd, err := ringpoll.NewReader(objs.Events, *pollSleep)
	if err != nil {
		log.Fatalf("Failed to open ring buffer: %v", err)
	}
	defer rd.Cleanup()

	state := newState(*maxSketches)
	interactive := !*batch && isTerminal(int(os.Stdin.Fd())) && isTerminal(int(os.Stdout.Fd()))
	display := &Display{
		batchMode:      *batch,
		focusProcesses: focusList,
		topN:           *topProcs,
		colsOverride:   *colsFlag,
		interactive:    interactive,
	}
	metrics := &runtimeMetrics{}
	mapMaxVal := int64(objs.StartTimes.MaxEntries())

	// Terminal raw mode for interactive input
	var origTermios *unix.Termios
	if interactive {
		origTermios = enableRawMode()
		fmt.Print("\033[?25l") // hide cursor
	}

	// Signal handling
	done := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig
		signal.Stop(sig) // restore default handler so second Ctrl+C force-kills
		if interactive {
			fmt.Print("\033[?25h") // restore cursor
		}
		restoreTermMode(origTermios)
		close(done)
		rd.Close()
	}()
	defer func() {
		if interactive {
			fmt.Print("\033[?25h")
		}
		restoreTermMode(origTermios)
	}()

	// Reader goroutine: busy-polls ring buffer, batches events, flushes under single Lock
	var readerDone sync.WaitGroup
	readerDone.Add(1)
	go func() {
		defer readerDone.Done()
		runReader(rd, state)
	}()

	// Input goroutine for interactive mode
	keyCh := make(chan keyEvent, 16)
	if interactive {
		go runInput(keyCh)
	}

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
				display.render(state, metrics, mapMaxVal, snapshotRingStats(rd))
			case ev := <-keyCh:
				if display.handleKey(ev) {
					// 'q' pressed — trigger shutdown via signal
					sig <- syscall.SIGINT
					return
				}
				readDropCount(objs.DropCount, &metrics.drops)
				display.render(state, metrics, mapMaxVal, snapshotRingStats(rd))
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
				total, stale, evicted := cleanStaleEntries(objs.StartTimes, objs.SyscallIds, *staleAge, *evictAge, mapMaxVal)
				used := int64(total - evicted)
				metrics.mapUsed.Store(used)
				metrics.mapStale.Store(int64(stale - evicted))
				// Track avg/max
				metrics.mapSumUsed.Add(used)
				metrics.mapSamples.Add(1)
				if cur := metrics.mapMaxUsed.Load(); used > cur {
					metrics.mapMaxUsed.Store(used)
				}
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
	display.render(state, metrics, mapMaxVal, snapshotRingStats(rd))
}

