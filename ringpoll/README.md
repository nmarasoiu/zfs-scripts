# ringpoll

A direct-mmap polling reader for Linux BPF ring buffers in Go. Drop-in replacement for `cilium/ebpf`'s epoll-based `ringbuf.Reader` when your ring buffer has near-continuous data flow.

## Why

The `cilium/ebpf` library uses `epoll` internally to wait for ring buffer data. This is the right default for the general case: many file descriptors, sporadic activity.

But when you have **one or more ring buffers that are almost always ready** — as in high-throughput eBPF tracing — epoll becomes pure overhead. On a system tracing 180K syscalls/sec, the cilium reader generated **42,000 `epoll_wait` calls/sec** just to drain the ring buffer. The tracing tool was the #1 syscall source on the system. It was tracing itself.

`ringpoll` eliminates this by mmapping the ring buffer's producer/consumer pages directly and spinning on `prod_pos != cons_pos`. When rings are empty, an adaptive pacer sleeps just long enough for the worst-case ring to reach a target fill level, allowing events to accumulate and drain in bursts.

### Results

| Metric | cilium/ebpf (epoll) | ringpoll (direct mmap) |
|---|---|---|
| `epoll_wait` calls/sec | 42,000 | 0 |
| `futex` calls/sec | 58,000 | 2,900 |
| Total reader syscalls/sec | ~110,000 | ~5,000 |
| CPU usage | 100% one core | near-zero |
| Ring buffer utilization | — | 0.0–0.6% |
| Events per wake cycle | 1–2 | 20–200 |
| Dropped events | 0 | 0 |

## Install

```
go get github.com/nmarasoiu/zfs-scripts/ringpoll
```

## Usage

### Kernel side (BPF C)

Suppress per-event wakeup notifications — the direct-mmap reader doesn't use them:

```c
bpf_ringbuf_submit(data, BPF_RB_NO_WAKEUP);
```

### Userspace (Go)

The primary interface is `Group`, which manages one or more ring buffer readers and provides a single polling loop:

```go
import (
    "time"
    "github.com/cilium/ebpf"
    "github.com/nmarasoiu/zfs-scripts/ringpoll"
)

// Open ring buffers (one reader per BPF map).
ringMaps := []*ebpf.Map{objs.Events0, objs.Events1, objs.Events2, objs.Events3}
rings, err := ringpoll.NewGroup(ringMaps)
if err != nil {
    log.Fatal(err)
}
defer rings.Cleanup()

// Adaptive pacer: target 50% fill, sleep range [50µs, 50ms].
pacer := ringpoll.NewPacer(0.5, 50*time.Microsecond, 50*time.Millisecond)

var rec ringpoll.Record
for !rings.Closed() {
    pending, cap := rings.FillBytes()        // pre-drain snapshot
    for rings.Poll(&rec) {
        // rec.RawSample contains the raw bytes submitted by BPF.
        // event := (*MyEvent)(unsafe.Pointer(&rec.RawSample[0]))
    }
    rings.Commit()
    pacer.Pace(pending, cap)                 // adaptive sleep
}
```

For a single ring buffer, you can use `Reader` directly:

```go
rd, err := ringpoll.NewReader(objs.Events)
if err != nil {
    log.Fatal(err)
}
defer rd.Cleanup()

var rec ringpoll.Record
for !rd.Closed() {
    for rd.Poll(&rec) { /* process rec.RawSample */ }
    rd.Commit()
    time.Sleep(50 * time.Microsecond)
}
```

### Batch commit

`Poll` does **not** advance the kernel-visible consumer position on every read. Instead, it advances an internal position and only publishes it to the kernel when you call `Commit()` (or `CommitAndSnap()`). This batches the consumer-position update, reducing cache-line bouncing between CPU cores.

Call `Commit()` after draining all available events, or periodically after processing N events.

### Adaptive pacer

The `Pacer` replaces a fixed sleep with an adaptive one. It maintains an EMA of the produce rate (bytes/sec) and computes the sleep duration needed for the worst-case ring to reach a target fill level:

```
sleep = targetFill × capacity / produceRate_ema
```

Behavior:
- **No events (pending=0)**: sleeps `minSleep` — stays responsive without decaying the rate estimate
- **Fill >= target on wake**: skips sleep entirely (we're behind) and updates the rate estimate
- **Fill < target**: computes adaptive sleep, clamped to `[minSleep, maxSleep]`

`AvgSleep()` returns the running average sleep duration for observability.

## API

### Group (multi-ring)

```go
// NewGroup creates a Group of polling readers, one per map.
func NewGroup(maps []*ebpf.Map) (*Group, error)

// Poll scans rings 0..N-1 and returns the first record found.
// Returns false when all rings are empty in a single pass.
func (g *Group) Poll(rec *Record) bool

// Commit publishes consumer positions and snapshots stats for all readers.
func (g *Group) Commit()

// FillBytes returns max pending bytes across rings and per-ring capacity.
func (g *Group) FillBytes() (maxPending, capacity int)

// MaxFill returns the maximum fill fraction (0.0–1.0) across all rings.
func (g *Group) MaxFill() float64

// Snapshot returns aggregated stats across all ring buffer readers.
func (g *Group) Snapshot() GroupSnapshot

// Closed reports whether all readers have been closed.
func (g *Group) Closed() bool

// Close signals all readers to stop.
func (g *Group) Close()

// Cleanup unmaps shared memory for all readers.
func (g *Group) Cleanup()
```

### Reader (single ring)

```go
// NewReader creates a polling reader for a BPF RingBuf map.
func NewReader(m *ebpf.Map) (*Reader, error)

// Poll reads the next record. Returns true on success, false when empty or closed.
func (r *Reader) Poll(rec *Record) bool

// Commit publishes the consumer position to the kernel, freeing ring space.
func (r *Reader) Commit()

// CommitAndSnap publishes the consumer position and a poll statistics snapshot.
func (r *Reader) CommitAndSnap()

// Snapshot returns the latest poll statistics, or nil if none published yet.
func (r *Reader) Snapshot() *PollSnapshot

// Pending returns current ring buffer fill in bytes.
func (r *Reader) Pending() int

// BufSize returns ring buffer capacity in bytes.
func (r *Reader) BufSize() int

// MaxPending returns the high-water mark of ring fill in bytes.
func (r *Reader) MaxPending() int64

// Close signals the reader to stop. Poll will return false.
func (r *Reader) Close()

// Cleanup unmaps shared memory. Call after the read loop exits.
func (r *Reader) Cleanup()
```

### Pacer (adaptive sleep)

```go
// NewPacer creates a Pacer with target fill fraction and sleep bounds.
func NewPacer(targetFill float64, minSleep, maxSleep time.Duration) *Pacer

// Pace updates the rate estimate and sleeps adaptively.
// Returns duration slept (0 if fill >= target).
func (p *Pacer) Pace(pendingBytes, capacity int) time.Duration

// AvgSleep returns the running average sleep duration.
func (p *Pacer) AvgSleep() time.Duration
```

### Snapshot types

```go
// PollSnapshot holds per-reader poll statistics (published atomically).
type PollSnapshot struct {
    EventSum      int64 // cumulative events read
    NonEmptyCount int64 // polls that returned at least one event
    PollCount     int64 // total polls
    MaxPending    int64 // high-water mark of ring fill (bytes)
}

// GroupSnapshot holds aggregated stats across multiple readers.
type GroupSnapshot struct {
    Pending    int   // worst-case pending bytes across rings
    MaxPending int64 // worst-case high-water mark
    Cap        int64 // per-ring capacity (all same size)
    EventSum   int64 // sum across rings
    NonEmpty   int64 // sum across rings
    PollCount  int64 // sum across rings
}
```

## When to use this

Use `ringpoll` when:

- Your ring buffers have **near-continuous data** (events arriving every few microseconds)
- You are tracing at **high throughput** (>10K events/sec) and care about observer overhead
- You want the **lowest possible syscall footprint** for your eBPF userspace reader
- You need **diagnostic stats** (fill levels, high-water marks, poll statistics) for tuning

Stick with `cilium/ebpf`'s default `ringbuf.Reader` when:

- Events arrive sporadically — epoll will block efficiently and save CPU
- Observer overhead is not a concern

## How it works

1. **mmap** the ring buffer's consumer page (read-write) and producer + data pages (read-only), using the BPF map's file descriptor
2. **Spin** on `atomic.LoadUint64(prodPos) != consumerPos` — when they differ, records are available
3. **Parse** the 8-byte ring buffer header (length + flags), handle busy-bit (producer mid-write) and discard-bit
4. **Copy** the data out of the double-mapped ring (no wrap-around logic needed since the kernel maps the data region twice contiguously)
5. **Sleep** adaptively via `Pacer` when all rings are drained, committing consumer positions first so the kernel can reuse the space

The double-mapping is a kernel feature: the data region is mapped twice back-to-back in virtual memory, so a record that wraps around the physical end of the ring appears contiguous in the virtual mapping. This eliminates the need for split-read logic.

## Requirements

- Linux kernel 5.8+ (BPF ring buffer support)
- `CAP_BPF` or root (for BPF map access and mmap)
- Go 1.21+
- `github.com/cilium/ebpf` v0.12+ (for `ebpf.Map` type)

## License

Same as the parent repository.
