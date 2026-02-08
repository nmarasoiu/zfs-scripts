# Rust Rewrite Build Plan

Full rewrite of the syscall-latency profiler from Go to Rust.
See also: `../RUST_SYNC_ANALYSIS.md` for synchronization diagrams.

## Honest ROI Assessment

| Category | Verdict |
|----------|---------|
| **Performance** | Marginal — kernel-side BPF is identical, userspace mutex contention is ~5% |
| **Safety** | Insurance — Go version has no races today, Rust prevents future ones at compile time |
| **Code clarity** | Moderate win — sum types, exhaustive matching, owned stats |
| **Build complexity** | Cost — libbpf-sys linkage, clang, heavier than `go generate` |
| **Lines of code** | More — Go ~1,950 → Rust ~2,500-3,000 |
| **Primary value** | Learning exercise + Rust eBPF reference implementation |

## Architecture

### Thread Topology

```
T1: READER (std::thread::spawn)
    Owns: RingPollReader (mmap, busy-poll loop)
    Sends: Vec<PendingEvent> via crossbeam channel → T2
    Publishes: ArcSwap<PollSnapshot> at ring-empty boundaries
    Updates: AtomicI64 maxPending per-event
    Exits: when AtomicBool closed is set

T2: DISPLAY (main thread)
    Owns: Stats (all DDSketches, simpleStats, topN — NO MUTEX)
    Receives: Vec<PendingEvent> from T1, KeyEvent from T3
    Timer: 100ms tick via crossbeam::channel::tick()
    Loop: crossbeam::select! { event_rx | key_rx | tick_rx }
    Reads: ArcSwap<PollSnapshot>, RuntimeMetrics atomics

T3: INPUT (std::thread::spawn)
    Reads: crossterm::event::read() blocking
    Sends: KeyEvent via crossbeam channel → T2
    Exits: on stdin close or error

T4: CLEANUP (std::thread::spawn)
    Timer: cleanup_interval tick
    Iterates: BPF start_times map, deletes stale entries
    Writes: RuntimeMetrics atomics
    Exits: when done signal received
```

### Data Flow

```
Kernel ring buffer (8MB, BPF_RB_NO_WAKEUP)
       │
       │ mmap'd prodPos/consPos (atomic u64)
       ▼
T1: RingPollReader::read_into()
       │ batch 1024 events or 10ms
       │
       │ crossbeam::channel::send(Vec<PendingEvent>)
       │ ownership transfer, zero-copy (moves heap pointer)
       ▼
T2: stats.record_batch(&batch)     ← no lock, display owns stats
       │
       │ render() reads own data
       ▼
    stdout (10 FPS)
```

### Synchronization Inventory

| Primitive | Purpose | Go equivalent |
|-----------|---------|---------------|
| `crossbeam::channel` (bounded) | Reader → Display event batches | `state.mu` Mutex (eliminated) |
| `ArcSwap<PollSnapshot>` | Reader → Display poll stats | 6 atomic.Int64 + 3 shadows (eliminated) |
| `AtomicI64` maxPending | Reader → Display ring high-water | Same |
| `AtomicBool` closed | Signal → Reader shutdown | Same |
| `Arc<RuntimeMetrics>` (4 atomics) | Cleanup → Display | Same |
| `crossbeam::channel` | Input → Display key events | `keyCh` channel |
| `crossbeam::channel::tick` | Display timer | `time.NewTicker` |

**Total: 0 mutexes, 2-3 atomics, 1 ArcSwap, 3 channels**
(vs Go: 1 mutex, 12 atomics, 3 shadows, 2 channels)

---

## File Structure

```
rust/
  Cargo.toml
  build.rs                    # libbpf-cargo compiles BPF C → skeleton
  src/
    main.rs                   # entry, clap, thread spawn, signal, shutdown
    bpf.rs                    # BPF loading, constant rewrite, map handles, cleanup
    ringpoll.rs               # mmap busy-poll reader, ArcSwap publish
    stats.rs                  # TopN, SimpleStats, SyscallStats, Stats (no mutex)
    display.rs                # Display, Mode enum, render, handleKey, panel
    format.rs                 # format_latency, format_count, etc.
    terminal.rs               # thin crossterm wrapper (raw mode, key read)
    syscalls.rs               # static [&str; 451] lookup table
    types.rs                  # Config, PendingEvent, PollSnapshot, RuntimeMetrics, Mode, KeyEvent
  bpf/
    syscall_latency.c         # IDENTICAL to Go version — copied from ../go/bpf/
    vmlinux.h                 # copied from ../go/bpf/ (or symlinked)
```

## File-by-File Mapping

### types.rs (NEW — no Go equivalent)

Shared types referenced by multiple modules:

```rust
use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU64};
use std::time::Duration;

/// CLI configuration
pub struct Config {
    pub focus_procs: Vec<String>,
    pub top_n: usize,
    pub batch_mode: bool,
    pub cols_override: usize,
    pub poll_sleep: Duration,
    pub cleanup_interval: Duration,
    pub stale_age: Duration,
    pub evict_age: Duration,
}

/// Channel payload: reader → display (ownership transfer)
pub struct PendingEvent {
    pub comm: [u8; 16],   // raw bytes, display converts to String
    pub syscall_id: u32,
    pub latency_us: i64,
}

/// Atomically published by ring reader at ring-empty boundaries
pub struct PollSnapshot {
    pub event_sum: i64,
    pub non_empty_count: i64,
    pub poll_count: i64,
    pub last_non_empty: i64,
    pub last_batch: i64,
    pub last_empty_ns: i64,
}

/// Shared between cleanup and display threads
pub struct RuntimeMetrics {
    pub drops: AtomicU64,
    pub evicted: AtomicU64,
    pub map_used: AtomicI64,
    pub map_stale: AtomicI64,
}

/// Display mode — sum type replacing Go's 3 separate fields
pub enum Mode {
    Normal,
    Filter { text: String },
    Detail { proc_name: String },
}

/// Terminal key events — sum type replacing Go's kind+ch struct
pub enum KeyInput {
    Char(u8),
    Enter,
    Backspace,
}
```

### main.rs ← main.go

```rust
fn main() -> anyhow::Result<()> {
    let config = Config::parse();  // clap derive

    // BPF setup
    let mut handles = bpf::load_and_attach(&config)?;

    // Ring reader
    let reader = RingPollReader::new(handles.events_fd(), config.poll_sleep)?;
    let poll_snap = reader.snapshot_handle();  // Arc<ArcSwap<PollSnapshot>>

    // Channels
    let (event_tx, event_rx) = crossbeam::channel::bounded(64);
    let (key_tx, key_rx) = crossbeam::channel::bounded(16);
    let tick_rx = crossbeam::channel::tick(Duration::from_millis(100));
    let closed = Arc::new(AtomicBool::new(false));

    // Terminal
    if config.interactive() {
        crossterm::terminal::enable_raw_mode()?;
        // hide cursor
    }
    // Cleanup on any exit path
    let _guard = scopeguard::guard((), |_| {
        crossterm::terminal::disable_raw_mode().ok();
        // show cursor
    });

    // T1: Reader thread
    let reader_handle = thread::spawn(move || {
        run_reader(reader, event_tx);
    });

    // T3: Input thread
    if config.interactive() {
        thread::spawn(move || terminal::run_input(key_tx));
    }

    // T4: Cleanup thread
    let metrics = Arc::new(RuntimeMetrics::default());
    thread::spawn({
        let metrics = metrics.clone();
        move || bpf::run_cleanup(&handles, &config, &metrics)
    });

    // T2: Display loop (main thread)
    let mut stats = Stats::new();
    let mut display = Display::new(&config);

    loop {
        crossbeam::select! {
            recv(event_rx) -> batch => {
                let batch = batch?;
                stats.record_batch(&batch);
            }
            recv(key_rx) -> key => {
                if display.handle_key(key?) { break; }
            }
            recv(tick_rx) -> _ => {}
        }
        display.render(&stats, &poll_snap.load(), &metrics, map_cap);
    }

    closed.store(true, Ordering::Relaxed);
    reader_handle.join().ok();
    Ok(())
}
```

### bpf.rs ← main.go (BPF parts)

Maps: `configureBPFFilters`, `cleanStaleEntries`, `readDropCount`, `ktimeNow`.

```rust
pub struct BpfHandles { /* libbpf-rs skeleton fields */ }

pub fn load_and_attach(config: &Config) -> Result<BpfHandles> {
    let mut builder = SyscallLatencySkelBuilder::default();
    let mut open_skel = builder.open()?;

    if !config.focus_procs.is_empty() {
        open_skel.rodata_mut().use_comm_filter = 1;  // pre-load constant!
    }

    let mut skel = open_skel.load()?;
    configure_filters(&mut skel, &config.focus_procs)?;
    skel.attach()?;
    Ok(BpfHandles { skel })
}

pub fn ktime_now() -> u64 {
    let mut ts = libc::timespec { tv_sec: 0, tv_nsec: 0 };
    unsafe { libc::clock_gettime(libc::CLOCK_MONOTONIC, &mut ts) };
    ts.tv_sec as u64 * 1_000_000_000 + ts.tv_nsec as u64
}
```

### ringpoll.rs ← ringpoll.go

Nearly identical algorithm. Key differences:
- `unsafe {}` blocks for mmap, pointer arithmetic
- `Drop` impl for munmap (replaces Go's `defer rd.Cleanup()`)
- `ArcSwap<PollSnapshot>` replaces 6 atomic fields + 3 shadows
- `maxPending` stays as standalone `AtomicI64`

```rust
pub struct RingPollReader {
    cons_mmap: *mut u8,
    prod_mmap: *mut u8,
    cons_pos: *mut u64,
    prod_pos: *const u64,
    ring: *const u8,
    mask: u64,
    pos: u64,
    poll_sleep: Duration,
    buf_size: usize,

    // Local accumulators (reader thread only)
    batch_count: i64,
    event_sum: i64,
    non_empty_count: i64,
    poll_count: i64,

    // Cross-thread publish
    snapshot: Arc<ArcSwap<PollSnapshot>>,
    max_pending: Arc<AtomicI64>,
    closed: Arc<AtomicBool>,
}

impl Drop for RingPollReader {
    fn drop(&mut self) {
        unsafe {
            libc::munmap(self.prod_mmap as *mut _, ...);
            libc::munmap(self.cons_mmap as *mut _, ...);
        }
    }
}
```

### stats.rs ← stats.go

Same structures, but no mutex — display thread is sole owner:

```rust
pub struct Stats {
    pub proc_syscall_stats: HashMap<String, HashMap<u32, SyscallStats>>,
    pub start_time: Instant,
    comm_intern: HashMap<[u8; 16], String>,  // owned by display thread
}

impl Stats {
    pub fn record_batch(&mut self, batch: &[PendingEvent]) {
        for e in batch {
            let comm = self.intern_comm(&e.comm);
            let ss = self.proc_syscall_stats
                .entry(comm)
                .or_default()
                .entry(e.syscall_id)
                .or_insert_with(SyscallStats::new);
            ss.record(e.latency_us);
        }
    }
}
```

### display.rs ← display.go

Largest file. Key Rust improvements:
- `Mode` enum replaces 3 fields (mode + filterText + selectedProc)
- `match` on `(&mut self.mode, key)` replaces nested switch
- `write!` macro replaces `fmt.Fprintf`
- No lock acquisition — reads `&Stats` directly

### format.rs ← format.go

Direct 1:1 translation. Pure functions, no state:
```rust
pub fn format_latency(us: i64) -> String { ... }
pub fn format_count(n: i64) -> String { ... }
pub fn format_bytes(n: i64) -> String { ... }
pub fn format_duration(d: Duration) -> String { ... }
```

### terminal.rs ← terminal.go

Thin wrapper around crossterm (replaces 79 lines of manual termios):
```rust
pub fn run_input(tx: Sender<KeyInput>) {
    loop {
        match crossterm::event::read() {
            Ok(Event::Key(KeyEvent { code, .. })) => {
                let key = match code {
                    KeyCode::Char(c) => KeyInput::Char(c as u8),
                    KeyCode::Enter => KeyInput::Enter,
                    KeyCode::Backspace => KeyInput::Backspace,
                    _ => continue,
                };
                if tx.send(key).is_err() { return; }
            }
            _ => continue,
        }
    }
}
```

### syscalls.rs ← syscalls.go

Static lookup table. Returns `Cow<'static, str>` to avoid allocation
for known syscalls:

```rust
use std::borrow::Cow;

static SYSCALL_NAMES: [&str; 451] = [
    /* 0 */ "read", /* 1 */ "write", /* 2 */ "open", ...
];

pub fn syscall_name(id: u32) -> Cow<'static, str> {
    match SYSCALL_NAMES.get(id as usize) {
        Some(&name) if !name.is_empty() => Cow::Borrowed(name),
        _ => Cow::Owned(format!("sys_{}", id)),
    }
}
```

---

## Dependencies (Cargo.toml)

```toml
[package]
name = "syscall-latency"
version = "0.1.0"
edition = "2021"

[dependencies]
anyhow = "1"                           # Error handling
arc-swap = "1"                         # Lock-free ArcSwap<PollSnapshot>
clap = { version = "4", features = ["derive"] }  # CLI args
crossbeam-channel = "0.5"             # Channels + select! + tick()
crossterm = "0.28"                     # Terminal raw mode, key events
libc = "0.2"                           # mmap, clock_gettime
libbpf-rs = "0.24"                     # BPF loading, maps, programs
log = "0.4"                            # Logging
env_logger = "0.11"                    # Logger backend
scopeguard = "1"                       # RAII cleanup guard
sketches-ddsketch = "0.2"             # DDSketch percentiles

[build-dependencies]
libbpf-cargo = "0.24"                  # Compile BPF C in build.rs
```

**System requirements**: clang, libbpf-dev, libelf-dev, pkg-config, linux-headers.

---

## Trust Assessment Per Aspect

| Aspect | Trust | Notes |
|--------|-------|-------|
| BPF C program (identical) | **VERY HIGH** | No change |
| libbpf-rs skeleton codegen | **HIGH** | Mature, type-safe rodata |
| ringpoll.rs mmap busy-poll | **HIGH for logic** | Same algorithm. Rust unsafe gives no extra safety over Go for raw pointers |
| Channel ownership (no mutex) | **HIGH** | Compiler-enforced. crossbeam battle-tested |
| Stats owned by display | **VERY HIGH** | Single owner, `&mut self`, no sync |
| Sum types (Mode, KeyInput) | **VERY HIGH** | Impossible states unrepresentable |
| crossterm terminal | **HIGH** | Replaces manual termios, widely used |
| DDSketch crate | **HIGH algo, MEDIUM numeric** | Same algorithm, float details may differ slightly |
| Cow<str> for syscall names | **HIGH** | Standard pattern |
| Build system | **MEDIUM** | libbpf-sys C linkage adds friction |

## Pareto Assessment

```
HIGH VALUE (do these regardless):
  ✓ Sum types (Mode, KeyInput)     — free wins from type system
  ✓ crossterm                      — eliminates manual terminal code
  ✓ Stats owned by display         — simpler &mut access, no lock ceremony
  ✓ BPF C stays identical          — zero risk
  ✓ ArcSwap for poll snapshot      — cleaner than 6 atomics

NECESSARY (core of the rewrite):
  ~ ringpoll.rs                    — same unsafe, different language
  ~ channel architecture           — different shape, similar total sync ops
  ~ libbpf-rs skeleton             — better constant injection, worse build

LOW VALUE (the "marketing" wins):
  ✗ Mutex elimination              — solves ~5% contention, 8MB buffer absorbs
  ✗ Compile-time race prevention   — Go version has no races today
  ✗ No GC pauses                   — Go GC <1ms, busy-poll sleeps 50µs anyway
```

---

## Implementation Sequence

Build bottom-up, test each layer before the next:

### Phase 1: Foundation (no BPF, no terminal)
1. **types.rs** — all shared types
2. **syscalls.rs** — static table + unit test
3. **format.rs** — pure functions + unit tests
4. **stats.rs** — TopN, SimpleStats, SyscallStats, Stats + unit tests

### Phase 2: BPF Pipeline (batch output only)
5. **build.rs** + **bpf/** — compile BPF C, verify skeleton generates
6. **bpf.rs** — load, attach, constant rewrite, map handles
7. **ringpoll.rs** — mmap reader, busy-poll, ArcSwap publish
8. **main.rs (v1)** — wire reader→stats→simple batch print, no TUI

Test: `sudo ./target/release/syscall-latency --batch -n 10`

### Phase 3: Interactive TUI
9. **terminal.rs** — crossterm key reading
10. **display.rs** — full render with Mode enum, panel, filter, detail
11. **main.rs (v2)** — full select! loop with key events

Test: interactive mode with `/` filter, Enter select, `q` quit

### Phase 4: Polish
12. Cleanup thread (stale entry management)
13. Signal handling (`signal-hook` + scopeguard)
14. Edge cases, error paths, `--cols` override
15. Performance comparison vs Go version

---

## Build & Run

```bash
# Prerequisites
apt install clang libbpf-dev libelf-dev pkg-config linux-headers-$(uname -r)

# Build
cd syscalls/rust
cargo build --release

# Run (needs root for BPF)
sudo ./target/release/syscall-latency --batch -n 10
sudo ./target/release/syscall-latency -c firefox,chrome -n 20
```
