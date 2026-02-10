# Future: Centralized Batch Drainer for ringpoll

## Status: Design Notes (not yet implemented)

The current pattern works well: each consumer uses `Poll()` + client-side sleep
gated on ring fill level. This document captures a potential future abstraction.

## Current Pattern (what consumers do today)

```go
for !rd.Closed() {
    for rd.Poll(&rec) {
        // process event, append to pending[]
        if len(pending) >= maxBatch {
            flush(pending)
            rd.CommitAndSnap()
        }
    }
    if rd.Pending()*20 <= rd.BufSize() { // ring < 5% full
        flush(pending)
        rd.CommitAndSnap()
        time.Sleep(pollSleep)
    } else if len(pending) > 0 && time.Since(lastFlush) >= batchTimeout {
        flush(pending)
        rd.CommitAndSnap()
    }
}
```

This is duplicated across consumers. It works, but each consumer re-implements
the same flow control logic.

## Idea: Kafka-style Batch Consumer

Two layers:

### Layer 1: BatchPoller (low-level, zero-alloc)

The caller provides a pre-allocated slice (like a thread-local ring/splice).
BatchPoller fills it up to capacity, round-robin across N rings, then returns.

```go
type BatchPoller struct {
    readers []*Reader
    // ...
}

// FillBatch drains up to len(buf) events into buf, round-robin across rings.
// Returns the number of events filled. Commits consumed positions.
// Does NOT sleep — caller decides when/whether to sleep.
func (bp *BatchPoller) FillBatch(buf []Record) int
```

Key properties:
- Zero allocation: caller owns the buffer, BatchPoller writes into it
- Like Kafka's `consumer.poll(maxRecords, maxWait)` — caller says "give me
  up to N events" and gets back what's available
- Commits upstream ring positions for whatever was consumed
- Caller controls sleep timing based on how full the batch came back

### Layer 2: Drainer (high-level, callback-based)

Wraps BatchPoller, owns the buffer, manages sleep, invokes callback per batch.

```go
type Drainer struct {
    poller   *BatchPoller
    buf      []Record       // owned, pre-allocated
    opts     DrainOpts
}

type DrainOpts struct {
    PollSleep    time.Duration
    MaxBatch     int
    BatchTimeout time.Duration
}

// Run polls in a loop, invokes fn with each batch, sleeps when quiet.
// Blocks until all readers are closed.
func (d *Drainer) Run(fn func(batch []Record))
```

### Why not do this now

1. Only two consumers exist — the duplication is manageable
2. The callback model had past concerns about recursive polling / reentrancy
   (callback calling back into the drainer). Need to verify this is safe if
   the callback is strictly "process and return" with no ring interaction.
3. The zero-alloc batch fill is the interesting part but adds complexity;
   current `pending []pendingEvent` pattern with append works fine and GC
   pressure is negligible at these batch sizes (1024 events, reused slice).

### When to revisit

- If a third consumer appears
- If profiling shows GC pressure from event batching
- If the flow control logic diverges between consumers (bugs in one, not other)
