# ZFS + Linux Latency Collection Reference

## The Problem with Built-in Histograms

**ZFS histograms** (`zpool iostat -l`): 37 power-of-2 buckets from 1ns to 137s
- You get: "5 more hits in the 2-4s bucket"
- You DON'T get: Was it 2.1s or 3.9s? What's the true p99.99?

**Linux `/sys/block/*/inflight`**: Often shows 0 reads, only writes
- Appears to aggregate all I/O types into write counter
- Sampling rate matters - you miss short-lived queue states

**HDR Histogram** (used in blk-latency/syscalls): Good but...
- Fixed bucket boundaries
- Relative *value* error, not relative *rank* error
- Less accurate at extreme tails (p99.99+)

## Solution: eBPF + ReqSketch

### Why ReqSketch over HDR/DDSketch for latency tails?

| Aspect | HDR Histogram | DDSketch | ReqSketch |
|--------|---------------|----------|-----------|
| Error type | Relative value | Relative value | Relative **rank** |
| p99.99 accuracy | Degrades | Good | **Best** (HRA mode) |
| Memory | ~40KB fixed | ~2-10KB | ~2-50KB |
| Formal guarantees | Yes | Yes | Yes |
| Go native | Yes | Yes | **No** (C++/Java/Python) |

### Implementation Options

1. **C++ with ReqSketch** - Maximum accuracy, header-only
2. **Go + CGO + ReqSketch** - Go workflow, C++ accuracy
3. **Go + DDSketch** - Pure Go, close-enough accuracy

## Data Sources

### eBPF Tracepoints (Kernel-level, Most Accurate)

```
block:block_rq_issue    - Request submitted to device driver
block:block_rq_complete - Request completed by device
```

Delta between these = true device latency (excludes OS queue time)

### ZFS-Specific (Already Collecting)

| Source | Path | Metrics |
|--------|------|---------|
| TXG timing | `/proc/spl/kstat/zfs/<pool>/txgs` | otime, qtime, wtime, stime |
| DMU TX assign | `/proc/spl/kstat/zfs/<pool>/dmu_tx_assign` | 42-bucket histogram |
| VDEV latency | `zpool iostat -lpvv` | 37-bucket histograms per op type |

### Linux Block Layer

| Source | Path | Metrics |
|--------|------|---------|
| Block stats | `/sys/block/<dev>/stat` | ios, merges, sectors, time_ms |
| Inflight | `/sys/block/<dev>/inflight` | Current queue depth (unreliable for reads) |
| iostat | `iostat -xz 1` | await, svctm, avgqu-sz |

## Directory Structure

```
stats_world/
├── README.md           # This file
├── blk-reqsketch/      # eBPF + ReqSketch C++ collector
│   ├── main.cpp
│   ├── bpf/
│   └── Makefile
└── experiments/        # Test scripts
```

## References

- [Apache DataSketches C++](https://github.com/apache/datasketches-cpp)
- [ReqSketch paper](https://arxiv.org/abs/2004.01668)
- [OpenZFS zpool-iostat.8](https://openzfs.github.io/openzfs-docs/man/master/8/zpool-iostat.8.html)
- [Linux block layer stats](https://docs.kernel.org/block/stat.html)
