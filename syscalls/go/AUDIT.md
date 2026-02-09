# syscall-latency Audit

Inspection of Go userspace + BPF C code. Updated Feb 2025 after cleanup commits.

---

## A. Memory Safety & Boundedness

### A1. `unsafe.Pointer` for BPF event read (main.go:87)
**Status: OK.** Guarded by `len(rec.RawSample) < eventSize` check.

### A2. `simpleStats.sum` overflow (stats.go)
**Status: OK.** At 10^7 µs max latency × 10^9 events = 10^16, well within
uint64 (max 1.8×10^19).

### A3. LRU eviction callback (stats.go)
**Status: OK.** `s.sketchEvictions++` runs inside `RecordBatch` which holds
`s.mu`. Single writer, no race.

### A4. `latencyUs` clamp to 1 (main.go)
**Status: OK.** DDSketch requires positive values. Clamping sub-microsecond
events to 1µs is correct.

### A5. `commIntern` grows unbounded (stats.go)
**Status: Minor.** One entry per unique process comm ever seen, never trimmed.
Bounded in practice (few hundred to low thousands of names), but dead process
names accumulate on multi-day runs. Slow leak.

### A6. `sketchBytesEach = 400` is a guess (display.go)
**Status: Misleading.** DDSketch with 0.01 accuracy across a wide latency range
(1µs–10s, 7 orders of magnitude) can grow to ~2000+ bins × 8 bytes ≈ 16KB per
sketch. With 4096 max sketches that's ~64MB, not the ~1.6MB the display claims.

### A7. `commString` unsafe cast (stats.go)
**Status: OK.** Uses `*(*[16]byte)(unsafe.Pointer(&comm))` — int8 and byte have
identical memory layout. Same pattern as the BPF event cast in main.go.

---

## B. Design & Encapsulation

### B1. State encapsulation via Read callback (stats.go)
**Status: Done.** `State.Read(fn func(StateView))` acquires/releases its own
lock. Callers never touch `state.mu`. Rendering happens inside the callback.

### B2. `commIntern` as package-level mutable global (stats.go)
**Status: Fragile.** Comment says "Only used by the reader goroutine — no sync
needed" but enforced by convention, not type system. Should be a field on a
reader struct. Low risk in practice.

### B3. `Display` mixes concerns
**Status: Acceptable for tool size.** Handles configuration, UI state machine,
rendering, input handling. Could split but the tool is small enough.

### B4. Variable shadowing: `batch` (main.go)
**Status: Confusing.** Package-level `batch = flag.Bool(...)` and local
`batch := make([]pendingEvent, ...)` in `runReader`. No bug, but misleading.

---

## C. Operational

### C1. Lock contention between reader and display
**Status: Acceptable.** At 10 FPS, lock held ~10 times/sec for render duration.
String generation from pre-computed sketches is fast and bounded. Reader batches
events (flushSize=1024) so brief lock holds cause negligible backpressure.

### C2. No backpressure or adaptive behavior
**Status: Known.** When drops > 0, only response is a counter in the footer.

---

## Priority Summary

| Priority | Issue | Impact |
|----------|-------|--------|
| Low | A5 commIntern unbounded | Slow leak, bounded in practice |
| Low | A6 sketchBytesEach estimate | Misleading display |
| Low | B2 commIntern as global | Fragility |
| Low | C2 No backpressure | Operational |
