# ZFS & Block I/O Monitoring Tools

A collection of Go-based tools for monitoring ZFS pools, block device latency, queue depths, and syscall performance. Most use eBPF for accurate kernel-level tracing.

## Tools Overview

| Tool | Data Source | Stats Structure | Best For |
|------|-------------|-----------------|----------|
| `blk-latency` | eBPF block_rq_* | HDR Histogram | Per-device I/O latency with p99.999 |
| `blk-ddsketch` | eBPF block_rq_* | DDSketch | **Best long-tail accuracy** (±1% error) |
| `zpool-latency` | zpool iostat -wvv | Fixed buckets | ZFS pool latency (coarse) |
| `zpool_iostat` | zpool iostat -wv | Fixed buckets | Quick ZFS histogram percentages |
| `syscalls` | eBPF raw_syscalls | HDR Histogram | Per-syscall latency (pread, fsync, etc.) |
| `top_txg` | /proc/spl/kstat | Direct | Interactive ZFS TXG monitor |
| `usb-queue-monitor-v2` | /sys/block/*/inflight | Histogram | Block device queue depth distribution |

---

## Block I/O Latency Tools

### blk-latency

Per-device block I/O latency tracking using eBPF tracepoints on `block_rq_issue` and `block_rq_complete`.

```bash
cd blk-latency
go generate && go build
sudo ./blk-latency -i 10s -d sdc,sdd  # Filter specific devices
```

**Features:**
- HDR Histogram (~40KB/device, 3 significant figures)
- Percentiles: avg, p50, p90, p95, p99, p99.9, p99.99, p99.999
- Top-10 max and bottom-5 min tracking per interval/lifetime
- 10 FPS real-time display

**Output columns:** avg, p50, p90, p95, p99, p99.9, p99.99, p99.999, min×5, max×10, samples

---

### blk-ddsketch (stats_world/blk-ddsketch)

Same as blk-latency but uses **DDSketch** instead of HDR Histogram for provable relative error guarantees.

```bash
cd stats_world/blk-ddsketch
go generate && go build
sudo ./blk-ddsketch -i 10s -alpha 0.01  # 1% relative accuracy
```

**Why DDSketch over HDR Histogram:**
- **Guaranteed relative error**: If it reports p99=50ms, true value is within ±α% (e.g., 49.5-50.5ms at α=1%)
- Better for long-tail analysis (p99.9, p99.99, p99.999)
- Smaller memory footprint (~2-10KB vs ~40KB)
- Mergeable sketches (useful for aggregation)

**Output columns:** min, avg, p50, p90, p99, p99.9, p99.99, p99.999, max, samples

---

## ZFS Pool Monitoring

### zpool-latency

Real-time ZFS pool latency viewer parsing `zpool iostat -wvv` output.

```bash
cd zpool-latency
go build
./zpool-latency hddpool -i 10
./zpool-latency hddpool -disk  # Show disk_wait instead of total_wait
```

**Features:**
- Interval stats (streaming) + Lifetime stats (periodic poll)
- Read/Write latency separately
- total_wait (queue + disk) or disk_wait (disk only)

**Limitations:** Uses ZFS's coarse power-of-2 histogram buckets (1ns, 3ns, 7ns...1ms, 2ms, 4ms...). Percentile accuracy is limited by bucket granularity - a "4ms" bucket could contain anything from 2.1-4ms.

---

### zpool_iostat

Quick one-shot display of ZFS histogram percentages per device.

```bash
cd zpool_iostat
go build
./zpool_iostat
```

Shows percentage of I/Os in each latency bucket. Includes "LARGE" row summarizing high-latency operations (>33ms for flash, >134ms for SMR drives).

---

### top_txg

Interactive ZFS Transaction Group (TXG) monitor with sorting and pagination.

```bash
go build -o top_txg top_txg.go
./top_txg "hddpool ssdpool" 2 20  # pools, interval, count
```

**Interactive keys:**
- `t/T` - Sort by TXG number
- `d/D` - Sort by dirty bytes
- `w/W` - Sort by written bytes
- `s/S` - Sort by sync time
- `m/M` - Sort by MB/s
- `↑/↓` - Page through sorted results
- `n` - Reset to recent TXGs
- `q` - Quit

**Columns:** DATE, TIME, TXG, STATE, DIRTY, READ, WRITTEN, R/W OPS, OPEN, QUEUE, WAIT, SYNC, MB/s, DURATION

---

## Syscall Latency

### syscalls

Per-syscall latency tracking using eBPF tracepoints on `sys_enter`/`sys_exit`.

```bash
cd syscalls
go generate && go build
sudo ./syscalls -c storagenode -s pread64,pwrite64,fsync,fdatasync
```

**Options:**
- `-c <comm>` - Filter by process name
- `-s <syscalls>` - Comma-separated syscall list
- `-i <duration>` - Stats interval

**Default syscalls:** pread64, pwrite64, fsync, fdatasync, read, write

---

## Queue Depth Monitoring

### usb-queue-monitor-v2

High-frequency block device queue depth monitor using exact histograms.

```bash
go build -o usb-queue-monitor-v2 usb-queue-monitor-v2.go
./usb-queue-monitor-v2
./usb-queue-monitor-v2 -batch  # For logging/nohup
```

**Features:**
- Dedicated sampler goroutine (runs flat-out for maximum sample rate)
- Exact histogram (256 buckets, 2KB/device) - no sampling approximation
- Percentiles: P10, P20, P30...P99, P99.5, P99.9, P99.95, P99.99, P99.995, P99.999, P100
- Utilization % (time with queue > 0)
- Per-device distribution histograms (log scale)
- USB aggregate stats (combined queue depth)

---

## Statistical Structures Comparison

| Structure | Memory | Accuracy | Best For |
|-----------|--------|----------|----------|
| **DDSketch** | 2-10KB | ±α% relative error (configurable) | Long-tail percentiles, merging |
| **HDR Histogram** | ~40KB | Fixed significant figures | General percentiles, export/import |
| **Fixed Buckets** (ZFS) | Small | Coarse (bucket boundaries only) | Quick overview, limited precision |
| **Exact Histogram** | 2KB (256 buckets) | Exact for 0-255 range | Queue depths, small value ranges |
| **Reservoir Sampling** | 10KB (1000 samples) | Statistical approximation | Memory-constrained, streaming |

### Why DDSketch is Best for Latency Long-Tails

The DDSketch algorithm guarantees that for any quantile q, if the true value is v, the reported value v' satisfies:

```
v / (1 + α) ≤ v' ≤ v × (1 + α)
```

At α=0.01 (1%), this means p99.99 = 100ms is guaranteed to be within 99-101ms. HDR Histogram provides similar accuracy for most cases but DDSketch's guarantees are mathematically proven.

---

## Quick Reference — Running All Programs

All Go programs can be run directly with `go run` (no need to compile first).

### Go — Pure Stdlib (run from repo root)

```bash
go run psi/psi.go                    # PSI pressure monitor (cpu/memory/io)
go run zswap/zswap-stats.go          # Zswap compression stats
go run zpool-latency/zpool-latency.go # Zpool latency histogram
go run slow_devices/main.go          # Slow device report from zpool iostat
go run zpool_iostat/main.go          # Zpool iostat histogram percentages
go run zpool_iostat/per_dev/main.go  # Zpool iostat per-device breakdown
go run duty_cycle_limiter/main.go    # Duty cycle limiter
go run softnet/main.go               # Softnet stats (/proc/net/softnet_stat)
go run usb-queue-monitor-v2/main.go  # USB/block queue depth monitor
```

### Go — With Dependencies (run from their directory)

```bash
cd top_txg && go run .               # Interactive TXG monitor (needs x/term)
```

### Go + eBPF (run from their directory, need root)

```bash
cd blk-latency && sudo go run .               # Block I/O latency (HDR histogram)
cd stats_world/blk-ddsketch && sudo go run .   # Block I/O latency (DDSketch)
cd syscalls/go && sudo go run .                # Syscall latency tracker
```

If the generated `bpf_bpfel.o` is missing, run `go generate` first.

### Shell Scripts

```bash
# TXG monitoring
top_txg/top_txg.sh                   # Interactive TXG monitor (bash version)
top_txg/top_txgs.sh <col>            # Sort TXGs by column (otime/qtime/wtime/stime)
top_txg/times.zfs.sh                 # Run top_txgs for all time columns

# ZFS tuning
zfs-tuning/zfs_tune_report.sh       # Report vdev queue params from sysfs
zfs-tuning/zfs_tune_sync.sh         # Compare/sync zfs.conf to running kernel
zfs-tuning/zfs_parse.sh             # Parse zfs.conf into table
zfs-tuning/recordsize.sh            # Show recordsize/special_small_blocks
zfs-tuning/special_and_recordsize.sh

# USB monitor service wrapper
usb-queue-monitor-v2/monitor-usb.sh {start|stop|restart|status|errors|tail}
```

### Python

```bash
python3 zfs-health/check_zfs.py          # ZFS health check (OK/WARN/NOK)
python3 zfs-health/zfs_special_stats.py   # Special vdev stats aggregator
```

### BPFTrace

```bash
sudo bpftrace utils/disk_alert.bt        # Block I/O latency alerter
```

---

## Building

All tools require Go 1.21+. eBPF tools additionally need:
- Linux kernel 5.8+ with BTF support
- clang/llvm for BPF compilation
- Root privileges to run

```bash
# For eBPF tools (from their directory)
go generate  # Compiles BPF C code (only needed once)
go run .     # Or: go build

# For pure Go tools (from repo root)
go run <dir>/main.go
```

## Dependencies

```bash
# eBPF ring buffer reader (busy-poll, replaces cilium/ebpf's epoll-based reader)
go get github.com/nmarasoiu/zfs-scripts/ringpoll

# Histogram/sketch libraries
go get github.com/HdrHistogram/hdrhistogram-go
go get github.com/DataDog/sketches-go/ddsketch

# TUI tools
go get golang.org/x/term
```
