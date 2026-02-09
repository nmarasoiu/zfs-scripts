# syscall-latency

Live per-syscall latency percentiles for Linux, powered by eBPF.

Attaches to `raw_syscalls/sys_enter` and `sys_exit` tracepoints, measures the
wall-clock time each syscall takes, and rolls everything into DDSketch
percentile summaries — grouped by process name and syscall number. The display
refreshes at 10 FPS in your terminal.

## Why

Standard tools show you *that* a process is slow, but not *which syscall* is
responsible or how its latency distributes. `strace -c` gives averages per
syscall but misses tail latency, costs ~2x overhead because it ptrace-stops
every syscall, and can only trace one process at a time.

`syscall-latency` traces system-wide from the kernel with negligible overhead
(no ptrace, no context switches), reports P25 through P99.9, and can focus on
specific processes or show all of them at once.

Useful for:
- Spotting which syscalls dominate a workload (is it `futex`? `io_uring_enter`? `epoll_wait`?)
- Seeing tail latency — the P99 of `fsync` might be 100x the median
- Comparing processes side-by-side to find the noisy neighbor
- Long-running monitoring — sketches use bounded memory regardless of sample count

## Quick start

```
# Build (requires clang, libbpf headers, Go 1.21+)
cd go/
make

# Trace all processes, show top 30 rows
sudo ./syscall-latency -n 30

# Focus on specific processes (BPF-level filter — only these hit userspace)
sudo ./syscall-latency -c "postgres,pgbench" -n 40

# Batch mode (no screen clearing, good for piping/logging)
sudo ./syscall-latency -batch -n 20

# Show version
./syscall-latency -version
```

## Usage

```
syscall-latency [flags]

Flags:
  -c string         Only trace these processes (comma-separated, empty=all)
  -n int            Top N rows to display (0=all)
  -batch            Batch mode — no screen clearing, append output
  -cols int         Override terminal width (enables process panel in batch mode)
  -poll-sleep duration   Ring buffer poll sleep when empty (default 50µs)
  -max-sketches int      Max process×syscall sketches before LRU eviction (default 4096)
  -version          Print version and exit
```

### Display modes

**Focused mode** (`-c procs`): a single table with all syscalls for the named
processes, showing the full percentile spread:

```
── postgres,pgbench (42) ──────────────────────────────────────────────────
LIFETIME          │      min      avg      p25      p50      p75      p90      p99    p99.9      max │   samples
--------------------------------------------------------------------------------------------
pgbench/futex     │     1µs     14µs      2µs      4µs     11µs     38µs    210µs   1200µs   4800µs │    125.3K
postgres/epoll_w  │     1µs    380µs      2µs     12µs    820µs   1500µs   3200µs   8100µs     22ms │     42.1K
postgres/read     │     1µs      8µs      1µs      2µs      5µs     18µs    120µs    680µs   2400µs │     38.7K
```

**Summary mode** (no `-c`): a compact dual-column layout covering all
process/syscall combinations, with a LIFETIME(all) aggregate at the bottom:

```
LIFETIME                     │      avg      p50      p90      p99      max │   samples      rate
tor/futex                    │     12µs      3µs     28µs    180µs   4200µs │    842.1K   14.0K/s
sshd/read                    │      6µs      2µs     11µs     95µs   1800µs │    210.5K    3.5K/s
```

### Interactive controls

When running in a terminal (not `--batch`):

- `/` — enter filter mode (prefix match on process or syscall name)
- `Backspace` — delete last filter character
- `/` again — cancel filter
- `q` — quit

### Side panel

When the terminal is wide enough, a right-side panel shows per-process totals
and rates — useful in summary mode to see which processes are busiest overall.

## How it works

### eBPF layer

A small C program (`bpf/syscall_latency.c`) attaches to the raw syscall
tracepoints:

1. **sys_enter**: records `bpf_ktime_get_ns()` and the syscall ID in per-TID
   LRU hash maps
2. **sys_exit**: looks up the start time, computes latency, and pushes a
   32-byte event to an 8 MB ring buffer

The BPF program skips `exit(2)` and `exit_group(2)` since those never produce a
matching `sys_exit` — without this, their map entries would leak and eventually
fill the hash map.

Process filtering uses a compile-time constant (`use_comm_filter`) rewritten
before loading. When disabled (the default), the verifier dead-code-eliminates
the filter branch entirely — zero overhead on the hot path for system-wide
tracing.

### Userspace

Events flow through a custom ring buffer reader (`ringpoll`) that busy-polls
the mmap'd producer/consumer pages directly, bypassing `epoll_wait` completely.
On a system doing 180K syscalls/sec, this eliminated 42K epoll_wait calls/sec
from the reader path alone.

Events are batched (up to 1024 or every 10ms) and flushed under a single mutex
lock into per-(process, syscall) DDSketches. DDSketch gives accurate percentiles
(1% relative error) with bounded memory (~2KB per sketch) regardless of how many
samples are recorded.

The sketch cache is LRU-bounded (default 4096 entries). When a system has many
short-lived process/syscall combinations, the least recently seen ones get
evicted. Eviction count is displayed in the header so you know if you need to
raise `--max-sketches`.

### Why `bpf_ktime_get_ns()` is nearly free

The latency measurement uses `bpf_ktime_get_ns()`, which resolves to the
kernel's `ktime_get_ns()` — the `CLOCK_MONOTONIC` clock. On x86_64 this reads
the TSC (Time Stamp Counter), a per-CPU hardware register that increments at a
fixed rate tied to the CPU's base crystal frequency. Reading it is a single
`rdtsc` instruction — no syscall, no lock, no IPI to other cores.

The raw TSC value is just a cycle count. At boot the kernel calibrates the
TSC-to-nanosecond conversion: it measures the TSC against a known reference
clock (HPET, ACPI PM timer, or PIT), computes a `mult + shift` pair, and
stores them in a per-CPU `tk_core` structure. Converting cycles→ns is then a
single multiply and right-shift — no division, no floating point. The kernel
also refines the calibration periodically via NTP/PTP adjustments, but the
fast path stays a register read plus integer math.

In BPF context this is even cheaper than userspace `clock_gettime()` via vDSO,
because we're already running in kernel context — no user/kernel transition at
all. Two `rdtsc` calls (enter + exit) per syscall add roughly 20–40ns of
overhead on modern hardware, which is negligible compared to even the fastest
syscalls (~200ns for `getpid`).

### Things done for efficiency

- **No epoll**: the ringpoll reader does direct mmap reads with
  `BPF_RB_NO_WAKEUP` on the kernel side, eliminating wakeup overhead
- **Batch flush**: events are batched before taking the lock, reducing
  contention between the reader and display goroutines
- **String interning**: process comm strings are interned to avoid ~80K
  allocations/sec from repeated byte-to-string conversions
- **LRU maps in BPF**: `start_times` and `syscall_ids` use `LRU_HASH` so the
  kernel auto-evicts stale entries — no userspace cleanup needed
- **Single-writer stats**: `ringAvg`, `commIntern`, and poll counters are
  goroutine-local, avoiding atomic operations in the hot path
- **Snapshot publishing**: ring buffer stats are published via a single
  `atomic.Pointer` swap instead of 7 separate atomic loads, giving the display
  goroutine a consistent view

## Build requirements

- Linux 5.8+ (ring buffer support)
- clang (for BPF compilation)
- libbpf headers (`/usr/include/bpf/`)
- Go 1.21+
- Root or `CAP_BPF` + `CAP_PERFMON`

```
make        # generate BPF, build binary
make check  # fmt + vet + staticcheck + race tests + govulncheck + tidy
make test   # unit tests only
```
