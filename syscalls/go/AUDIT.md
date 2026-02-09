# syscall-latency Audit

Inspection of Go userspace + BPF C code, Feb 2025.

---

## A. Memory Safety & Boundedness

### A1. `unsafe.Pointer` for BPF event read (main.go:84)
**Status: OK.** Guarded by `len(rec.RawSample) < eventSize` check on line 81.

### A2. `topN` sorted insert (stats.go:48-63)
**Status: OK.** Fixed-size slice, append capped at `n`. Correct sorted-insert
with shift.

### A3. `simpleStats.sum` overflow (stats.go:78)
**Status: OK.** At 10^7 µs max latency × 10^9 events = 10^16, well within
uint64 (max 1.8×10^19).

### A4. LRU eviction callback (stats.go:172)
**Status: OK.** `s.sketchEvictions++` runs inside `RecordBatch` which holds
`s.mu`. Single writer, no race.

### A5. `latencyUs` clamp to 1 (main.go:86-88)
**Status: OK.** DDSketch requires positive values. Clamping sub-microsecond
events to 1µs is correct.

### A6. `commIntern` grows unbounded (stats.go:18)
**Status: Minor.** One entry per unique process comm ever seen, never trimmed.
Bounded in practice (few hundred to low thousands of names), but dead process
names accumulate on multi-day runs. Slow leak.

### A7. `sketchBytesEach = 400` is a guess (display.go:108)
**Status: Misleading.** DDSketch with 0.01 accuracy across a wide latency range
(1µs–10s, 7 orders of magnitude) can grow to ~2000+ bins × 8 bytes ≈ 16KB per
sketch. With 4096 max sketches that's ~64MB, not the ~1.6MB the display claims.

### A8. `mapMaxUsed` non-atomic CAS (main.go:265-266)
**Status: Latent.** `Load(); if >; Store()` is not a real CAS. Safe today
because there's only one cleanup goroutine, but a footgun if a second writer is
ever added.

---

## B. Simplicity — Dead / Always-true Code

### B9. `batchMode` field + `--batch` flag
**Status: Dead weight.** Controls: `resetCursor()` skip, extra newline in
footer, whether `interactive` can be true. If never used, all `if d.batchMode`
branches collapse.

### B10. `colsOverride` field + `--cols` flag
**Status: Dead weight.** Only useful in batch mode ("enables panel in batch
mode"). If batch mode goes, this goes.

### B11. `snapshotRingStats` nil guard (main.go:104-107)
**Status: Dead.** `rd` can never be nil at the call site — program would have
`log.Fatalf`'d on line 168.

### B12. `readDropCount` nil guard (bpf_ops.go:94)
**Status: Dead.** `objs.DropCount` is always loaded by `LoadAndAssign`.

### B13. `topN` genericity (stats.go:39-71)
**Status: Over-general.** Only ever instantiated with `n=5`. Parameterized
`newTopN(n int)` is unnecessary; could be a fixed-5 struct.

### B14. `interactive` field redundant with `!batchMode`
**Status: Redundant.** Set to `!*batch && isTerminal(stdin) && isTerminal(stdout)`.
If batch mode is removed and only run in a terminal, always true. All
`if d.interactive` guards collapse.

### B15. `ringStats` struct (stats.go:237-245)
**Status: Marginal.** Exists to "decouple display from ringpoll.Reader type."
Copy-out struct used exactly once. Adds indirection without reuse benefit.

---

## C. Best Practices — SRP, Cohesion, Encapsulation

### C16. Lock held during entire render cycle (display.go:102-135)
**Status: Architectural.** `state.mu` held from snapshot through all rendering
string building. Blocks `RecordBatch` (reader goroutine) for the full render
time every 100ms. Under high event rates, events pile up in the ring buffer and
drop.

Root cause: `snapshotStats()` copies pointers not values, so rendering must
happen under the lock. Fix: deep-value snapshot, release lock, then render.

### C17. Leaky State encapsulation (display.go:102, stats.go:182)
**Status: Fixed in this commit.** `snapshotStats()` said "Must be called while
s.mu is held" but the lock was acquired by the caller (`Display.render`).
Replaced with `State.Read(fn func(StateView))` — State acquires/releases its
own lock and calls the callback with a `StateView` (pointer-shared, no cloning).
Rendering happens inside the callback, under the lock. Callers never touch
`state.mu`. No copying of sketches — the 4K DDSketches stay in place.

### C18. `commIntern` as package-level mutable global (stats.go:18-19)
**Status: Fragile.** Comment says "Only used by the reader goroutine — no sync
needed" but this is enforced by convention, not type system. Should be a field
on a reader struct passed into `runReader`.

### C19. `Display` mixes concerns
**Status: Acceptable for tool size.** Handles configuration, UI state machine,
rendering, input handling, and terminal ops. Could split into config + state
machine + renderer, but the tool is small enough that the cohesion cost is low.

### C20. Free functions on raw map type
**Status: Minor.** `collectEntries`, `collectProcessSummaries`,
`filterStatsGeneral` all take `map[string]map[uint32]...`. A named snapshot type
would give these a natural home as methods.

### C21. Variable shadowing: `batch` (main.go:43 vs main.go:77)
**Status: Confusing.** Package-level `batch = flag.Bool(...)` (`*bool`) and
local `batch := make([]pendingEvent, ...)` in `runReader`. No bug, but
misleading when reading.

---

## D. Other Elephants

### D22. Lock contention between reader and display
**Status: Acceptable.** At 10 FPS, lock held ~10 times/sec for render duration.
If rendering takes 5ms, that's 50ms/sec where `RecordBatch` is blocked. In
practice, string generation from pre-computed sketches is fast and bounded.
The reader goroutine batches events (flushSize=1024) so brief lock holds cause
negligible ring buffer backpressure. Would only matter at extreme event rates.

### D23. No backpressure or adaptive behavior
**Status: Known.** When drops > 0, only response is a counter in the footer. No
sampling, reduced refresh rate, or warning. User sees drops climbing but system
keeps rendering at the same rate that causes them.

### D24. `cleanStaleEntries` iterates full map + sorts (bpf_ops.go:47-84)
**Status: Acceptable.** Iterates up to 65536 entries, collects into slice,
optionally sorts O(n log n). Every 5 seconds. Under normal load fine. Under
pressure the sort + delete loop does many kernel map syscalls.

### D25. Double terminal restore (main.go:198-203 + 205-210)
**Status: Harmless.** Both signal handler and defer restore cursor + terminal.
`restoreTermMode` is idempotent (nil check). Cursor escape prints twice but
invisible to user.

### D26. `hashicorp/golang-lru/v2` listed as indirect (go.mod:15)
**Status: Cosmetic.** Directly imported in stats.go but marked `// indirect` in
go.mod. Should be a direct dependency.

---

## Priority Summary

| Priority | Issue | Impact |
|----------|-------|--------|
| **Done** | C17 State encapsulation | Lock inside State via Read callback |
| Medium | B9-B14 Dead code removal | Simplicity |
| Low | A6 commIntern unbounded | Slow leak, bounded in practice |
| Low | A7 sketchBytesEach estimate | Misleading display |
| Low | C18 commIntern as global | Fragility |
| Low | D23 No backpressure | Operational |
