# Rust Rewrite: Synchronization Analysis

How much of the current Go synchronization would naturally go away in a Rust
rewrite, and what remains?

## Current Go Goroutine Topology

```
┌─────────────────────────────────────────────────────────────────────┐
│ KERNEL                                                              │
│  sys_enter tracepoint ──► start_times[tid] = ktime                 │
│  sys_exit  tracepoint ──► ringbuf_submit(latency_event)            │
│                              │                                      │
│                    mmap'd ring buffer (8MB)                         │
└──────────────────────────────┬──────────────────────────────────────┘
                               │ prodPos (atomic u64, read-only from userspace)
                               │ consPos (atomic u64, written by reader)
                               ▼
┌──────────────────────────────────────────────────────────────────┐
│ G1: READER  (runReader → rd.ReadInto loop)                       │
│                                                                   │
│  busy-poll: load prodPos, compare to local pos                   │
│  on event: decode, batch into []pendingEvent                     │
│  on flush (1024 events or 10ms):                                 │
│      state.mu.Lock() → RecordBatch → Unlock → rd.Commit()       │
│  on ring empty:                                                   │
│      Commit() → update poll stats → sleep 50µs                   │
│                     │                        │                    │
│            ┌────────┘                        └────────┐           │
│            ▼                                          ▼           │
│     state.mu (Lock)                          6× atomic.Store     │
│     shared with G2                           read by G2          │
└──────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────┐
│ G2: DISPLAY  (100ms ticker + key events)                         │
│                                                                   │
│  select { ticker | keyCh }                                       │
│  state.mu.Lock() → render (reads procSyscallStats) → Unlock     │
│  also reads: 6× atomic.Load from RingPollReader for footer      │
└──────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────┐
│ G3: INPUT  (os.Stdin.Read, byte-by-byte)                         │
│  sends keyEvent → keyCh (buffered 16) → consumed by G2          │
└──────────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────────┐
│ G4: CLEANUP  (5s ticker)                                         │
│  iterates BPF start_times map → deletes stale entries            │
│  writes: metrics.mapUsed/mapStale/evicted (atomic)               │
│  read by G2 during render                                        │
└──────────────────────────────────────────────────────────────────┘
```

## Current Go Synchronization Map

```
G1 READER          G2 DISPLAY          G3 INPUT          G4 CLEANUP
──────────          ──────────          ────────          ──────────
    │                   │                  │                  │
    │   state.mu        │                  │                  │
    ├──── Lock ────────►│                  │                  │
    │  RecordBatch()    │  Lock            │                  │
    │  (write DDSketch, │  render()        │                  │
    │   simpleStats,    │  (read all)      │                  │
    │   topN, maps)     │                  │                  │
    │   Unlock ─────────┤  Unlock          │                  │
    │                   │                  │                  │
    │  6× atomic.Store  │                  │                  │
    ├──────────────────►│  7× atomic.Load  │                  │
    │  (poll stats)     │  (PollStats etc) │                  │
    │                   │                  │                  │
    │  maxPending.Store │                  │                  │
    ├──────────────────►│  maxPending.Load │                  │
    │  (per event!)     │                  │                  │
    │                   │                  │                  │
    │                   │◄──── keyCh ──────┤                  │
    │                   │  (buffered chan)  │                  │
    │                   │                  │                  │
    │                   │                  │    4× atomic     │
    │                   │◄─────────────────┼──────────────────┤
    │                   │  (metrics.Load)  │  (metrics.Store) │
    │                   │                  │                  │
    │  closed.Load      │                  │                  │
    │◄──────── done ────┼──────────────────┼──────────────────┘
    │  (atomic.Bool)    │
```

**Inventory:**

| Category | Count | Details |
|----------|-------|---------|
| Mutexes | 1 | `state.mu` — protects `procSyscallStats` |
| Atomic fields | 7 | RingPollReader poll stats (6 published + maxPending) |
| Atomic fields | 1 | RingPollReader `closed` |
| Atomic fields | 4 | `runtimeMetrics` (drops, evicted, mapUsed, mapStale) |
| Shadow locals | 3 | eventSum, nonEmptyCount, pollCount (copied to atomics) |
| Channels | 2 | keyCh (input→display), done (signal→all) |
| **Total** | **18** | **1 mutex, 12 atomics, 3 shadows, 2 channels** |

## The RingPollReader Atomic Explosion (Detail)

The reader goroutine accumulates stats locally, then publishes to atomic
fields at each ring-empty boundary. Display reads atomics for the footer.

```
G1 (READER goroutine)                         G2 (DISPLAY goroutine)
─────────────────────                         ──────────────────────

Local accumulators          Atomic bridge           Reads via PollStats()
(per-event, zero cost)      (Store at boundary)     + LastBatch/MaxPending
─────────────────────       ─────────────────       ──────────────────────
batchCount  ─────────┐
                     │
eventSum    ────────►├──► atomicEventSum     ──► .Load()  → avg1
                     │
nonEmptyCount ──────►├──► atomicNonEmptyCount──► .Load()  → avg1 denom
                     │
pollCount   ────────►├──► atomicPollCount    ──► .Load()  → avg0 denom
                     │
(batchCount) ───────►├──► lastNonEmpty       ──► .Load()  → last1
                     │
(batchCount) ───────►├──► lastBatch          ──► .Load()  (via LastBatch())
                     │
time.Now()  ────────►└──► lastEmptyNano      ──► .Load()  → last0

(per-event):
prod - pos  ─────────────► maxPending        ──► .Load()  (via MaxPending())
```

The publish site in `ReadInto()`:

```go
// Ring empty — 6 atomic stores happen here
r.pollCount++
if r.batchCount > 0 {
    r.nonEmptyCount++
    r.eventSum += r.batchCount
    r.lastNonEmpty.Store(r.batchCount)           // ①
    r.lastBatch.Store(r.batchCount)              // ②
    r.batchCount = 0
    r.atomicEventSum.Store(r.eventSum)           // ③
    r.atomicNonEmptyCount.Store(r.nonEmptyCount) // ④
}
r.atomicPollCount.Store(r.pollCount)             // ⑤
r.lastEmptyNano.Store(time.Now().UnixNano())     // ⑥
```

## Rust: The Key Architectural Shift

Rust's ownership model lets you **transfer data by moving it**, not by
sharing it. A `Vec<PendingEvent>` moved across a channel is zero-copy
(just moves the 3-word fat pointer: ptr + len + cap). The receiver *owns*
the data — no lock needed.

```
T1 READER              T2 DISPLAY (sole owner of all stats)
──────────              ──────────────────────────────────────
    │                       │
    │  crossbeam::channel   │
    │                       │
    │   Vec<PendingEvent>   │
    ├──── send(batch) ─────►│  recv_all() at each tick
    │   (move, not copy)    │  for event in batch {
    │                       │      // directly mutate owned DDSketch,
    │                       │      // simpleStats, topN — no lock
    │                       │  }
    │                       │  render() — reads own data, no lock
    │                       │
    │  maxPending (AtomicI64, per-event — can't defer)
    ├──────────────────────►│  .load(Relaxed)
    │                       │
    │  ArcSwap<PollSnap>    │
    ├──────────────────────►│  .load()
    │  (1 pointer swap)     │
    │                       │
    │                       │◄──── keyCh ──── T3 INPUT
    │                       │
    │  closed (AtomicBool)  │
    │◄──── cancel token ────┤
```

### Why the mutex disappears

In Go, `state.mu` exists because two goroutines share a `map`:

- G1 (reader) writes `procSyscallStats[comm][syscallID].Record(latency)`
- G2 (display) reads `procSyscallStats` to render

In Rust, the compiler would refuse to compile this — two threads can't hold
`&mut` to the same data. You'd be forced to either:

1. Wrap in `Arc<Mutex<State>>` (same as Go, just compiler-enforced)
2. **Redesign so one thread owns it** (channel approach)

Option 2 is the natural Rust idiom. The reader thread sends
`Vec<PendingEvent>` through a channel. The display thread receives it,
updates its own stats, and renders. No shared mutable state, no lock.

### Why ArcSwap replaces 6 atomics

The poll stats don't need per-event granularity — they're published at
ring-empty boundaries. In Rust, the reader builds an immutable struct and
publishes it via `ArcSwap` (lock-free atomic pointer swap):

```rust
struct PollSnapshot {
    event_sum: i64,
    non_empty_count: i64,
    poll_count: i64,
    last_non_empty: i64,
    last_batch: i64,
    last_empty_ns: i64,
}

// Reader: 1 store (replaces 6 atomic stores)
poll_snap.store(Arc::new(PollSnapshot { ... }));

// Display: 1 load (replaces 7 atomic loads)
let snap = poll_snap.load();
```

### Why maxPending stays atomic

`maxPending` is updated **per-event** inside the hot loop:

```go
prod := atomic.LoadUint64(r.prodPos)
if pending := int64(prod - r.pos); pending > r.maxPending.Load() {
    r.maxPending.Store(pending)
}
```

The peak could happen mid-batch and be gone by the time the ring empties.
Can't defer to batch boundary. Stays as a standalone `AtomicI64` in Rust too.

## What Goes Away Entirely

```
BEFORE (Go)                              AFTER (Rust)
──────────────────────────               ──────────────────────────

state.mu (Mutex)                         GONE — display owns all stats
  Reader Lock/Unlock per batch             Channel send = ownership transfer
  Display Lock/Unlock per render           Display reads its own data

atomicEventSum      ┐                    GONE — folded into
atomicNonEmptyCount │ 6 atomic           1 ArcSwap<PollSnapshot>
atomicPollCount     │ fields             (1 pointer store, 1 pointer load)
lastNonEmpty        │
lastBatch           │
lastEmptyNano       ┘

eventSum       ┐                         GONE — no shadow copies needed
nonEmptyCount  │ 3 shadow locals         local fields published directly
pollCount      ┘                         into the snapshot struct
```

## What Stays (Changes Form)

| Go | Rust | Why it can't go away |
|----|------|---------------------|
| `maxPending` (AtomicI64) | `AtomicI64` | Per-event hot path, can't batch |
| `closed` (AtomicBool) | `CancellationToken` or `AtomicBool` | Signal→reader shutdown |
| `runtimeMetrics` (4 atomics) | `AtomicU64`/`AtomicI64` or channel | Cleanup→display, low frequency |
| `keyCh` (chan keyEvent) | `crossbeam::channel` | Same concept |
| `done` (chan struct{}) | `CancellationToken` or `crossbeam::select!` | Same concept |

## Scorecard

```
                          Go              Rust           Eliminated?
                          ──              ────           ───────────
Mutexes                   1               0              ✓ ownership transfer
Atomic fields             12              2-3            ✓ ArcSwap + owned stats
Shadow locals             3               0              ✓ no publish dance
Channels                  2               2-3            same (different impl)
Data race risk            runtime only    compile-time   ✓ compiler enforced
                          (go -race)
Lock hold during render   ~5-10ms         0              ✓ no lock exists
```

## The One Thing That's NOT Free

The channel itself has internal synchronization — crossbeam uses a lock-free
bounded queue with CAS operations. So you're not eliminating synchronization,
you're **moving it inside an abstraction** where:

1. The compiler *proves* you can't bypass it
2. The implementation is battle-tested
3. The API makes data races structurally impossible

Rust doesn't remove the physics of concurrency. It removes the option to
get it wrong silently.

## Bonus: Sum Types Clean Up State Machines

Not synchronization-related, but Rust enums would also eliminate impossible
states in the display mode:

```rust
// Go: 3 separate fields, 2 only valid in certain modes
//   mode          interactiveMode
//   filterText    string       // only meaningful in modeFilter
//   selectedProc  string       // only meaningful in modeDetail

// Rust: impossible states are unrepresentable
enum Mode {
    Normal,
    Filter { text: String },
    Detail { proc: String },
}
```

And key input handling becomes exhaustive pattern matching:

```rust
match (&mut self.mode, key) {
    (Mode::Normal, Key::Char(b'/'))  => self.mode = Mode::Filter { text: String::new() },
    (Mode::Normal, Key::Char(b'q'))  => return Quit,
    (Mode::Filter { text }, Key::Char(b'/')) => self.mode = Mode::Normal,
    (Mode::Filter { text }, Key::Char(c))    => text.push(c as char),
    (Mode::Filter { text }, Key::Backspace)  => {
        text.pop();
        if text.is_empty() { self.mode = Mode::Normal; }
    },
    (Mode::Filter { text }, Key::Enter) => { /* select top match */ },
    (Mode::Detail { .. }, Key::Char(b'/')) => self.mode = Mode::Normal,
    (Mode::Detail { .. }, Key::Char(b'q')) => return Quit,
    _ => {}
}
```

The compiler won't let you forget a case.

## Summary

A Rust rewrite would reduce the synchronization surface from **18 manual
primitives** (1 mutex + 12 atomics + 3 shadows + 2 channels) down to
**~5** (2-3 atomics + 2-3 channels), with zero risk of data races —
enforced at compile time, not by convention or runtime detection.

The main architectural win is the channel-based ownership transfer that
eliminates `state.mu` entirely. The reader sends event batches by moving
ownership; the display thread is the sole owner of all stats and never
needs a lock. The secondary win is collapsing the atomic field explosion
into a single `ArcSwap<PollSnapshot>`.
