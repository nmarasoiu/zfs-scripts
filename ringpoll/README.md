# ringpoll

A busy-polling reader for Linux BPF ring buffers in Go. Drop-in replacement for `cilium/ebpf`'s epoll-based `ringbuf.Reader` when your ring buffer has near-continuous data flow.

## Why

The `cilium/ebpf` library uses `epoll` internally to wait for ring buffer data. This is the right default for the general case: many file descriptors, sporadic activity.

But when you have **one ring buffer that is almost always ready** — as in high-throughput eBPF tracing — epoll becomes pure overhead. On a system tracing 180K syscalls/sec, the cilium reader generated **42,000 `epoll_wait` calls/sec** just to drain the ring buffer. The tracing tool was the #1 syscall source on the system. It was tracing itself.

`ringpoll` eliminates this by mmapping the ring buffer's producer/consumer pages directly and spinning on `prod_pos != cons_pos`. When the ring is empty, it sleeps for a configurable interval (default 50us), allowing events to accumulate and drain in bursts.

### Results

| Metric | cilium/ebpf (epoll) | ringpoll (busy-poll) |
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

Suppress per-event wakeup notifications — the busy-poll reader doesn't use them:

```c
bpf_ringbuf_submit(data, BPF_RB_NO_WAKEUP);
```

### Userspace (Go)

```go
import (
    "time"
    "github.com/nmarasoiu/zfs-scripts/ringpoll"
)

// Create reader from an ebpf.Map of type RingBuf.
// pollSleep controls the sleep interval when the ring is empty.
rd, err := ringpoll.NewReader(objs.Events, 50*time.Microsecond)
if err != nil {
    log.Fatal(err)
}
defer rd.Close()
defer rd.Cleanup()

var rec ringpoll.Record
for rd.ReadInto(&rec) {
    // rec.RawSample contains the raw bytes submitted by BPF.
    // Parse your struct out of it:
    // event := (*MyEvent)(unsafe.Pointer(&rec.RawSample[0]))
}
```

### Batch commit

`ReadInto` does **not** advance the kernel-visible consumer position on every read. Instead, it advances an internal position and only publishes it to the kernel (via `Commit()`) when the ring is empty and the reader is about to sleep. This batches the consumer-position update, reducing cache-line bouncing between CPU cores.

If you need to commit manually (e.g., after processing N events), call `rd.Commit()`.

## API

```go
// NewReader creates a busy-polling reader for a BPF RingBuf map.
func NewReader(m *ebpf.Map, pollSleep time.Duration) (*Reader, error)

// ReadInto reads the next record. Returns true on success, false when closed.
func (r *Reader) ReadInto(rec *Record) bool

// Commit publishes the consumer position to the kernel, freeing ring space.
func (r *Reader) Commit()

// Close signals the reader to stop. ReadInto will return false.
func (r *Reader) Close()

// Cleanup unmaps shared memory. Call after the read loop exits.
func (r *Reader) Cleanup()

// Pending returns current ring buffer fill in bytes.
func (r *Reader) Pending() int

// BufSize returns ring buffer capacity in bytes.
func (r *Reader) BufSize() int

// MaxPending returns the high-water mark of ring fill in bytes.
func (r *Reader) MaxPending() int64

// LastBatch returns the event count of the last completed drain batch.
func (r *Reader) LastBatch() int64

// PollStats returns ring poll statistics:
//   avg1:  average batch size of non-empty polls
//   avg0:  average batch size of all polls (including empty)
//   last1: batch size of most recent non-empty poll
//   last0: time since last empty poll
func (r *Reader) PollStats() (avg1, avg0 float64, last1 int64, last0 time.Duration)
```

## When to use this

Use `ringpoll` when:

- Your ring buffer has **near-continuous data** (events arriving every few microseconds)
- You are tracing at **high throughput** (>10K events/sec) and care about observer overhead
- You want the **lowest possible syscall footprint** for your eBPF userspace reader
- You need **diagnostic stats** (batch sizes, fill high-water mark, poll statistics) for tuning

Stick with `cilium/ebpf`'s default `ringbuf.Reader` when:

- Events arrive sporadically — epoll will block efficiently and save CPU
- You have multiple ring buffer maps — epoll multiplexes well
- Observer overhead is not a concern

## How it works

1. **mmap** the ring buffer's consumer page (read-write) and producer + data pages (read-only), using the BPF map's file descriptor
2. **Spin** on `atomic.LoadUint64(prodPos) != consumerPos` — when they differ, records are available
3. **Parse** the 8-byte ring buffer header (length + flags), handle busy-bit (producer mid-write) and discard-bit
4. **Copy** the data out of the double-mapped ring (no wrap-around logic needed since the kernel maps the data region twice contiguously)
5. **Sleep** for `pollSleep` when the ring is empty, committing the consumer position first so the kernel can reuse the space

The double-mapping is a kernel feature: the data region is mapped twice back-to-back in virtual memory, so a record that wraps around the physical end of the ring appears contiguous in the virtual mapping. This eliminates the need for split-read logic.

## Requirements

- Linux kernel 5.8+ (BPF ring buffer support)
- `CAP_BPF` or root (for BPF map access and mmap)
- Go 1.21+
- `github.com/cilium/ebpf` v0.12+ (for `ebpf.Map` type)

## License

Same as the parent repository.
