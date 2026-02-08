# Go Refactor Options

7 safe, incremental improvements to the syscall-latency profiler.
Each is independent — apply in any order, test in batch mode after each,
git commit individually.

## Summary Table

| # | Change | Files | Risk | Perf | Simplicity | Trust |
|---|--------|-------|------|------|------------|-------|
| 3 | `atomic.Pointer[pollSnapshot]` | ringpoll.go | Low | Neutral (idle path) | 13 fields → 5 | HIGH |
| 4 | Extract ringStats/mapStats | display.go | Zero | N/A (10 FPS) | 45 lines → 3×12 | HIGH |
| 5 | Shared `walkCols()` | display.go | Medium | N/A (cold path) | DRY | MEDIUM-HIGH |
| 6 | topN linear scan | stats.go | Low | Slight win for N=5 | Simpler logic | HIGH |
| 7 | commString unsafe cast | stats.go | Low | Hot path micro-opt | 4 lines → 1 | HIGH |
| 8 | `sync.OnceFunc` cleanup | main.go | Low | N/A (shutdown) | Dedup | HIGH |
| 9 | Rate calc dedup | display.go | Zero | N/A | 6 lines → 2 | HIGH |

---

## #3 — Atomic Pointer for Poll Stats

**Problem**: RingPollReader has 13 stat-related fields — 6 atomic.Int64 for
cross-goroutine publish, 3 local shadow accumulators, plus batchCount,
lastBatch, maxPending, bufSize. The reader stores to local fields, then
copies to atomics at ring-empty boundaries with 6 separate `.Store()` calls.
Display reads 7 separate `.Load()` calls.

**Current code** (ringpoll.go:39-56):
```go
// Stats (written by reader goroutine, read atomically by display)
batchCount int64
lastBatch  atomic.Int64
bufSize    int

eventSum      int64
nonEmptyCount int64
pollCount     int64

atomicEventSum      atomic.Int64
atomicNonEmptyCount atomic.Int64
atomicPollCount     atomic.Int64
lastNonEmpty        atomic.Int64
lastEmptyNano       atomic.Int64
maxPending          atomic.Int64
```

**Publish site** (ringpoll.go:181-193):
```go
r.pollCount++
if r.batchCount > 0 {
    r.nonEmptyCount++
    r.eventSum += r.batchCount
    r.lastNonEmpty.Store(r.batchCount)
    r.lastBatch.Store(r.batchCount)
    r.batchCount = 0
    r.atomicEventSum.Store(r.eventSum)
    r.atomicNonEmptyCount.Store(r.nonEmptyCount)
}
r.atomicPollCount.Store(r.pollCount)
r.lastEmptyNano.Store(time.Now().UnixNano())
```

**Proposed**: Replace 6 atomic fields + 3 shadow locals with 1
`atomic.Pointer[pollSnapshot]`:

```go
type pollSnapshot struct {
    eventSum      int64
    nonEmptyCount int64
    pollCount     int64
    lastNonEmpty  int64
    lastBatch     int64
    lastEmptyNano int64
}
```

Struct fields on RingPollReader become:
```go
// Reader-local accumulators (no sync)
batchCount    int64
eventSum      int64
nonEmptyCount int64
pollCount     int64

// Published atomically (1 pointer swap replaces 6 stores)
snapshot  atomic.Pointer[pollSnapshot]

// Per-event (stays separate — can't defer to boundary)
maxPending atomic.Int64

// Immutable after construction
bufSize int
```

Publish becomes:
```go
r.pollCount++
if r.batchCount > 0 {
    r.nonEmptyCount++
    r.eventSum += r.batchCount
    snap := &pollSnapshot{
        eventSum:      r.eventSum,
        nonEmptyCount: r.nonEmptyCount,
        pollCount:     r.pollCount,
        lastNonEmpty:  r.batchCount,
        lastBatch:     r.batchCount,
        lastEmptyNano: time.Now().UnixNano(),
    }
    r.batchCount = 0
    r.snapshot.Store(snap)
} else {
    snap := r.snapshot.Load()
    updated := *snap // copy
    updated.pollCount = r.pollCount
    updated.lastEmptyNano = time.Now().UnixNano()
    r.snapshot.Store(&updated)
}
```

`PollStats()` becomes:
```go
func (r *RingPollReader) PollStats() (avg1, avg0 float64, last1 int64, last0 time.Duration) {
    snap := r.snapshot.Load()
    if snap == nil {
        return
    }
    if snap.nonEmptyCount > 0 {
        avg1 = float64(snap.eventSum) / float64(snap.nonEmptyCount)
    }
    if snap.pollCount > 0 {
        avg0 = float64(snap.eventSum) / float64(snap.pollCount)
    }
    last1 = snap.lastNonEmpty
    if snap.lastEmptyNano > 0 {
        last0 = time.Since(time.Unix(0, snap.lastEmptyNano))
    }
    return
}
```

**Thread safety**: IMPROVES — current 6 independent stores let display see
torn state (new eventSum + old pollCount mid-update). Pointer swap is
all-or-nothing.

**Allocation concern**: 48-byte struct allocated at ring-empty boundaries
(20K/s during idle). During high load this code doesn't run. GC handles
trivially.

**Requires**: Go 1.19+ for `atomic.Pointer` (go.mod has 1.21).

**Testing**: Run `--batch` mode, verify footer stats match before/after.

---

## #4 — Extract ringStats/mapStats from renderFooter

**Problem**: `renderFooter` (display.go:301-346) is 45 lines of inline
computation mixing rate calculations, ring buffer stats formatting, and
map stats formatting into one dense `Sprintf` chain.

**Proposed**: Extract two helper functions:

```go
func ringStats(r *RingPollReader) string {
    if r == nil {
        return ""
    }
    pending := r.Pending()
    capBytes := r.BufSize()
    maxPend := r.MaxPending()
    pctFull := float64(pending) / float64(capBytes) * 100
    maxPct := float64(maxPend) / float64(capBytes) * 100
    avg1, avg0, last1, last0 := r.PollStats()
    last0Str := "-"
    if last0 > 0 {
        last0Str = formatMicro(last0)
    }
    return fmt.Sprintf(" | Ring avg: %6s/%s (%5.1f%%)  Ring max: %6s/%s (%5.1f%%)  avg1:%-6.0f avg0:%-8.1f last1:%-6s last0:%-8s",
        formatBytes(int64(pending)), formatBytes(int64(capBytes)), pctFull,
        formatBytes(maxPend), formatBytes(int64(capBytes)), maxPct,
        avg1, avg0, formatCount(last1), last0Str)
}

func mapStats(mapUsed, mapCap, mapStale, evicted int64) string {
    if mapCap <= 0 {
        return ""
    }
    pct := float64(mapUsed) / float64(mapCap) * 100
    return fmt.Sprintf(" | Map: %s/%s (%4.1f%%) stale:%s evict:%s",
        formatCount(mapUsed), formatCount(mapCap), pct,
        formatCount(mapStale), formatCount(evicted))
}
```

**Risk**: Zero — pure extract, no logic change.

**Testing**: Visual diff of `--batch` output before/after.

---

## #5 — Shared walkCols for displayWidth/padOrTrunc

**Problem**: Both `displayWidth` (display.go:553-570) and `padOrTrunc`
(display.go:573-593) implement the same UTF-8 byte-stepping loop.

**Proposed**: Extract shared primitive:

```go
// walkCols walks s counting display columns, stopping at maxCols.
// Returns (byteOffset, displayCols). If maxCols < 0, walks entire string.
func walkCols(s string, maxCols int) (byteOff, cols int) {
    for byteOff < len(s) && (maxCols < 0 || cols < maxCols) {
        if s[byteOff] < 0x80 {
            byteOff++
        } else {
            byteOff++
            for byteOff < len(s) && s[byteOff]&0xC0 == 0x80 {
                byteOff++
            }
        }
        cols++
    }
    return
}

func displayWidth(s string) int {
    _, n := walkCols(s, -1)
    return n
}

func padOrTrunc(s string, width int) string {
    off, dw := walkCols(s, width)
    if dw >= width {
        return s[:off]
    }
    return s + strings.Repeat(" ", width-dw)
}
```

**Risk**: Medium — off-by-one possible. Must verify `padOrTrunc` truncation
produces byte-identical output for all inputs. Test with box-drawing chars
(3-byte UTF-8) and ASCII.

**Testing**: Write a small test comparing old vs new output for various
inputs including "── title ─────", process names, and empty strings.

---

## #6 — topN Linear Scan Instead of sort.Search

**Problem**: `topN.Add()` (stats.go:46-61) uses `sort.Search` (binary
search with closure) for sorted insert into a 5-element slice. For N=5,
the closure allocation and function call overhead exceed the algorithmic
benefit.

**Current code**:
```go
func (t *topN) Add(v int64) {
    if len(t.values) < t.n {
        i := sort.Search(len(t.values), func(i int) bool { return t.values[i] >= v })
        t.values = append(t.values, 0)
        copy(t.values[i+1:], t.values[i:])
        t.values[i] = v
        return
    }
    if v > t.values[0] {
        i := sort.Search(len(t.values), func(i int) bool { return t.values[i] >= v })
        if i > 0 {
            copy(t.values[:i-1], t.values[1:i])
            t.values[i-1] = v
        }
    }
}
```

**Proposed**: Linear scan from end (N=5 is tiny):
```go
func (t *topN) Add(v int64) {
    if len(t.values) < t.n {
        // Insert in sorted position (ascending)
        t.values = append(t.values, v)
        for i := len(t.values) - 1; i > 0 && t.values[i] < t.values[i-1]; i-- {
            t.values[i], t.values[i-1] = t.values[i-1], t.values[i]
        }
        return
    }
    if v <= t.values[0] {
        return
    }
    // Replace minimum, bubble up
    t.values[0] = v
    for i := 0; i+1 < len(t.values) && t.values[i] > t.values[i+1]; i++ {
        t.values[i], t.values[i+1] = t.values[i+1], t.values[i]
    }
}
```

**Benefits**: No `sort` import needed (for this use). No closure allocation.
Easier to reason about correctness. Identical O(N) for N=5.

**Testing**: Unit test `topN.Add` with known sequences, verify `Get()`
returns correct top-5 in ascending order.

---

## #7 — commString Unsafe Cast

**Problem**: `commString` (stats.go:18-22) copies 16 bytes one-by-one from
`[16]int8` to `[16]byte`:
```go
var buf [16]byte
for i, c := range comm {
    buf[i] = byte(c)
}
```

This runs on every event (hot path). `int8` and `byte` have identical
memory layout (1 byte each).

**Proposed**:
```go
buf := *(*[16]byte)(unsafe.Pointer(&comm))
```

Same pattern already used in main.go:96 for the event cast. One line,
eliminates 16 iterations per event.

**Risk**: Low — Go spec guarantees int8 and byte are both 1 byte.
`unsafe.Pointer` cast between same-sized arrays is well-defined.

**Testing**: Run with `-c` filter, verify process names display correctly.

---

## #8 — sync.OnceFunc for Terminal Cleanup

**Problem**: Terminal restore runs in two places (main.go:195-210):

```go
// Signal handler goroutine
go func() {
    <-sig
    fmt.Print("\033[?25h")       // restore cursor
    restoreTermMode(origTermios) // restore termios
    close(done)
    rd.Close()
}()

// Deferred cleanup
defer func() {
    fmt.Print("\033[?25h")       // same restore (again)
    restoreTermMode(origTermios) // idempotent but redundant
}()
```

Both fire on signal exit path (signal goroutine runs, then main returns,
defer fires). `restoreTermMode` is idempotent (nil check), but the cursor
escape prints twice.

**Proposed**: `sync.OnceFunc` (Go 1.21+):
```go
cleanup := sync.OnceFunc(func() {
    if interactive {
        fmt.Print("\033[?25h")
    }
    restoreTermMode(origTermios)
})
defer cleanup()

go func() {
    <-sig
    signal.Stop(sig)
    cleanup()
    close(done)
    rd.Close()
}()
```

**Benefits**: Exactly-once guarantee. Single source of truth for cleanup
logic. Thread-safe by construction.

**Testing**: Run interactively, Ctrl+C, verify cursor restores and terminal
is clean.

---

## #9 — Rate Calculation Dedup in renderFooter

**Problem**: renderFooter (display.go:307-314) computes rate and dropRate
with identical patterns:
```go
rate := float64(0)
if elapsed.Seconds() > 0 {
    rate = float64(totalSamples) / elapsed.Seconds()
}
dropRate := float64(0)
if elapsed.Seconds() > 0 {
    dropRate = float64(drops) / elapsed.Seconds()
}
```

**Proposed**: Use a small helper (or reuse existing `formatRate` logic):
```go
safeRate := func(n uint64) float64 {
    if elapsed.Seconds() > 0 {
        return float64(n) / elapsed.Seconds()
    }
    return 0
}
rate := safeRate(totalSamples)
dropRate := safeRate(drops)
```

Or inline since `elapsed.Seconds() > 0` is always true after startup.

**Testing**: Visual diff of footer output.

---

## Implementation Order

Recommended sequence (safest first, each independently testable):

1. **#9** (rate dedup) — trivial, zero risk warmup
2. **#4** (renderFooter extract) — zero risk, improves readability
3. **#7** (commString cast) — one line, hot path
4. **#8** (sync.OnceFunc) — clean, self-contained
5. **#6** (topN linear) — small, testable with unit test
6. **#5** (walkCols) — needs careful testing
7. **#3** (pollSnapshot) — largest change, most fields touched

After each: `go build && ./syscall-latency --batch -n 10` for 5 seconds,
verify output matches expectations. Git commit.

---

## Verification Checklist

For each change:
- [ ] `go build` succeeds
- [ ] `--batch` mode output looks correct (rates, percentiles, footer)
- [ ] Interactive mode works (filter with `/`, detail with Enter, quit with `q`)
- [ ] No data races: `go build -race && ./syscall-latency -c <proc> --batch`
- [ ] Git commit with descriptive message
