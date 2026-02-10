# Drainer Migration: Findings

## What was done

Extracted the duplicated poll/batch/sleep loop from blk-ddsketch and syscalls
into reusable `Poller` + `Drainer` types in the ringpoll package.

- `Poller`: wraps N Readers, provides FillBatch / CommitAll / Quiet / Closed
- `Drainer`: owns a pre-allocated `[]Record` buffer, runs the ingestion loop,
  invokes a callback per batch

Both tools were migrated: ~130 lines deleted, ~30 added per tool.

## Performance regression observed

blk-ddsketch appears slower after migration. Root cause is a behavioral
difference in the drain loop:

### Old code (inner poll loop)

```go
for !rd.Closed() {
    for rd.Poll(&rec) {              // drains EVERYTHING available
        decode(rec) → pending
        if len(pending) >= 1024 {
            flush(pending); commit()  // flush every 1024
        }
    }
    // only exits inner loop when ring is empty
    if quiet { flush; commit; sleep }
}
```

The inner `for rd.Poll` loop stays tight — it drains all available events
across all rings before ever checking sleep. Under sustained load this means
thousands of events processed in one uninterrupted burst.

### New code (Drainer.Run)

```go
for !poller.Closed() {
    n := poller.FillBatch(buf)   // capped at 1024
    if n > 0 { fn(buf[:n]) }
    poller.CommitAll()
    if poller.Quiet(0.05) { sleep(3ms) }
}
```

FillBatch is capped at MaxBatch (1024). After each batch the loop exits to
commit + quiet-check. When rings have more data, Quiet returns false and we
loop back — but we've broken out of the tight drain, doing a full
FillBatch → callback → CommitAll → Quiet round-trip per 1024 events.

### The real issue

The old code drained all 4 CPU rings completely in one pass before considering
sleep. The new code drains up to 1024 total, then does bookkeeping, then goes
back. Under high throughput the bookkeeping overhead per event is higher.

The fix: FillBatch → callback should loop until all rings are drained (no more
data), THEN commit + quiet-check + sleep. Something like:

```go
for !poller.Closed() {
    for {
        n := poller.FillBatch(buf)
        if n == 0 { break }
        fn(buf[:n])
    }
    poller.CommitAll()
    if poller.Quiet(0.05) { sleep(3ms) }
}
```

This matches the old behavior: drain everything available, then decide about
sleep. Not yet applied — needs testing.

## Other notes

- The 3ms sleep when quiet is correct and not a bottleneck
- Quiet() is cheap (atomic loads on each reader)
- CommitAll every round is fine (frees ring space for kernel)
- Ring stats display (avg/max/cap) still reads from Reader.Snapshot() directly;
  could move aggregation into Poller as a future enhancement
