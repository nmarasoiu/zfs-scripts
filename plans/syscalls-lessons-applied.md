# Syscalls Lessons Applied to Other eBPF Tools

## Summary

Apply the high-performance patterns from `syscalls/` to `blk-latency/`, `stats_world/blk-ddsketch/`,
and `usb-queue-monitor-during-io/`. These changes eliminate the major per-event overheads.

## Changes Per Tool

### 1. blk-latency/

| Change | Before | After |
|--------|--------|-------|
| Ring buffer reader | cilium/ebpf `ringbuf.Reader` (epoll) | Custom `ringpoll.go` (busy-poll, no syscalls) |
| Event decoding | `binary.Read(bytes.NewReader(...))` (alloc/event) | `unsafe.Pointer` cast (zero-copy) |
| Lock granularity | `State.Record()` per-event write lock | `State.RecordBatch()` single lock per 1024 events or 10ms |
| Display data access | `State.Snapshot()` deep-copies HDR histograms (~40KB/dev) | Render-under-lock (no copies) |
| eBPF submit flag | `bpf_ringbuf_submit(e, 0)` (epoll wakeup per event) | `BPF_RB_NO_WAKEUP` (no kernel wakeup) |
| Mutex type | `sync.RWMutex` | `sync.Mutex` (simpler, only 2 goroutines) |
| Ring stats | None | Poll stats in footer (avg batch, max fill, etc.) |

Files modified:
- `bpf/latency.c` — add `BPF_RB_NO_WAKEUP`
- `main.go` — batch reader, render-under-lock, remove Snapshot/Clone
- `ringpoll.go` — new file (copied from syscalls)

### 2. stats_world/blk-ddsketch/

Same pattern as blk-latency, plus:

| Change | Before | After |
|--------|--------|-------|
| DDSketch snapshot | `copySketch()` creates new sketch + MergeWith per device per frame | Render-under-lock reads sketches directly |
| `preciseStats` snapshot | `Clone()` per device | Read directly under lock |

Files modified:
- `bpf/latency.c` — add `BPF_RB_NO_WAKEUP`
- `main.go` — batch reader, render-under-lock, remove Snapshot/Clone/copySketch
- `ringpoll.go` — new file (copied from syscalls)

### 3. usb-queue-monitor-during-io/

Already uses custom `ringpoll.go` and `BPF_RB_NO_WAKEUP`, but:

| Change | Before | After |
|--------|--------|-------|
| Consumer position | `atomic.StoreUint64(r.consPos, r.pos)` per event | Deferred commits (batch boundaries only) |
| Event decoding | `binary.Read(bytes.NewReader(...))` | `unsafe.Pointer` cast |
| Lock granularity | `State.Record()` per-event write lock | `State.RecordBatch()` single lock per batch |
| Display data access | `State.Snapshot()` copies histograms | Render-under-lock |
| Mutex type | `sync.RWMutex` | `sync.Mutex` |

Files modified:
- `ringpoll.go` — replaced with syscalls version (deferred commits)
- `main.go` — batch reader, render-under-lock, remove Snapshot

## What's NOT Changed

- eBPF C tracepoint logic (block_rq_issue/complete)
- Display formatting (same columns, same FPS)
- Histogram/sketch algorithms (HDR, DDSketch, 256-bucket)
- Device filtering, name resolution, signal handling
- Batch mode support

## Expected Impact

| Metric | Before | After |
|--------|--------|-------|
| Allocs per event | 2-3 (bytes.Reader, binary.Read reflection) | 0 (unsafe.Pointer cast) |
| Lock acquisitions/s | 1 per event (100K+ at high IOPS) | 1 per batch (~100/s) |
| Snapshot allocs per frame | 40KB/dev (HDR) or 2-10KB/dev (DDSketch) | 0 (render under lock) |
| Kernel wakeups per event | 1 (epoll eventfd_signal) | 0 (BPF_RB_NO_WAKEUP) |
| Ring consumer stores/event | 1 atomic store (during-io) | ~0.001 (deferred commits) |

## Build

Each tool requires `go generate` (needs clang + kernel headers) then `go build`:
```
cd blk-latency && make
cd stats_world/blk-ddsketch && make
cd usb-queue-monitor-during-io && make
```
