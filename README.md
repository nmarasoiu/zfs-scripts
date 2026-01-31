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

## Building

All tools require Go 1.21+. eBPF tools additionally need:
- Linux kernel 5.8+ with BTF support
- clang/llvm for BPF compilation
- Root privileges to run

```bash
# For eBPF tools
cd <tool-directory>
go generate  # Compiles BPF C code
go build

# For non-eBPF tools
go build <tool>.go
```

## Dependencies

```bash
# eBPF tools
go get github.com/cilium/ebpf
go get github.com/HdrHistogram/hdrhistogram-go
go get github.com/DataDog/sketches-go/ddsketch

# TUI tools
go get golang.org/x/term
```
