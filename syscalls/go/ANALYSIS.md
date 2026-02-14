# Correctness & Cache Line Analysis

## Correctness Analysis

### Solid / No Issues

**Thread ID tracking**: `bpf_get_current_pid_tgid()` truncated to `__u32` gives the kernel tid — correct for per-thread enter/exit pairing.

**exit/exit_group filtering** (syscall_latency.c:122-123): These never generate `sys_exit`, so filtering them at `sys_enter` prevents permanent map leaks. Correct.

**Fork/clone child handling** (syscall_latency.c:192-193): Child returns from parent's syscall with a new tid that was never inserted. Correctly detected and ignored without perturbing counters. Covers `fork(57)`, `vfork(58)`, `clone(56)`, `clone3(435)`.

**Per-CPU counter aggregation**: `map_used++` on enter-CPU, `map_used--` on exit-CPU (possibly different due to migration). The per-CPU values can go transiently negative, but the sum across all CPUs is always correct. The `readCounters` clamping to `[0, mapCap]` handles transient negative sums from startup race (exit before corresponding enter is visible). Correct.

**TOCTOU on lookup+delete** (syscall_latency.c:150-156): Not a real race — a given tid can only have one concurrent `sys_exit` (thread is on exactly one CPU), so no duplicate events.

**DDSketch values**: Latency added in µs, sum accumulated in µs. Consistent everywhere.

### Minor Correctness Issues

**1. Latency includes BPF exit overhead** — `syscall_latency.c:201`

```c
// Enter: timestamp BEFORE map insert
__u64 ts = bpf_ktime_get_ns();    // line 134
INSERT_START(...);                  // line 138

// Exit: timestamp AFTER map lookup+delete
TRY_LOOKUP_DELETE(...);             // line 174
__u64 latency = bpf_ktime_get_ns() - start_val;  // line 201
```

The measured latency = actual syscall + exit-side map lookup/delete overhead (~100-500ns). This is a **systematic positive bias**. Moving the `bpf_ktime_get_ns()` call before the map scan in `sys_exit` would eliminate it. For µs-range syscalls (getpid, clock_gettime) the bias is measurable (~10-50%); for ms-range syscalls it's negligible.

**2. `should_trace()` comm change between enter/exit** — `syscall_latency.c:160`

If a process calls `prctl(PR_SET_NAME)` between `sys_enter` and `sys_exit`, and the new name doesn't match the `-c` filter, `sys_exit` returns early at line 160-161 without deleting the start map entry. This leaks the entry (cleaned only by LRU eviction) and skews `map_used` upward. Extremely rare in practice — most processes never change comm.

**3. Latency floor hides sub-µs resolution** — `main.go:169`

```go
if latencyUs < 1 { latencyUs = 1 }
```

All sub-µs syscalls are recorded as exactly 1µs. For fast syscalls like `clock_gettime` (~200ns), this inflates measured values by 5x. DDSketch requires positive values, so a floor is needed, but recording in nanoseconds instead of µs would preserve resolution (DDSketch handles large value ranges fine with relative accuracy).

---

## Cache Line / Mechanical Sympathy Analysis

### Per-CPU Counters — Perfect

`BPF_MAP_TYPE_PERCPU_ARRAY` gives each CPU its own `struct percpu_counters` (24 bytes). Each CPU writes only its own slot. Zero contention, zero false sharing. The 24-byte struct fits well within a 64-byte cache line.

### Start Maps (4-way sharded LRU hash) — Good, with a structural trade-off

**The good**: On a 4-core system, each `start{N}` is accessed by exactly 1 CPU — zero cross-CPU contention on the hash's internal locks. On 8 cores, 2 CPUs per map; on 16 cores, 4 CPUs per map. The kernel's LRU hash uses per-bucket striped locks, so even with sharing the actual contention is low if tids distribute well across buckets.

**The structural contention**: When a thread migrates between `sys_enter` and `sys_exit` (different CPU → different slot), the exit handler's fallback scan (lines 181-186) touches up to 3 foreign maps. This causes:
- Lock acquisition on foreign map's bucket
- Cache line pull for the hash bucket from the remote CPU's L1/L2

This is **structurally necessary** — you can't predict at enter time which CPU will run the exit. The mitigation (check local slot first) means the common case (no migration) is fast. Thread migration during a single syscall is relatively rare (scheduler typically doesn't preempt within a syscall).

**Scaling concern**: `NUM_SLOTS=4` is hardcoded. On a 128-core machine, 32 CPUs compete per map. This would be the first bottleneck to hit at scale. Making it configurable or `MIN(num_cpus, 16)` would help, at the cost of more fallback scanning on exit.

### Ring Buffers (4-way sharded) — Good, same trade-off as maps

Each `bpf_ringbuf_reserve` atomically increments the producer position (a single `__u64` on the producer page). With 4-way sharding, contention on this atomic is reduced by ~4x.

Producer page (BPF writes) and consumer page (Go reads) are on **separate pages → separate cache lines**. The producer-consumer protocol has inherently minimal cross-domain sharing: BPF writes the producer position, Go reads it; Go writes the consumer position, BPF reads it. These are on different cache lines within their respective pages. The only cross-core traffic is:
- BPF producer position → Go reader (1 cache line invalidation per Commit cycle, not per event)
- Go consumer position → BPF (1 invalidation per Commit)

This is well-designed — the `Commit()` batching means the consumer position update is amortized across many events.

### Go Side — Well-amortized single lock

**`State.mu`**: Single mutex, but contention is low because:
- Reader goroutine: acquires lock ~176 times/sec at 180K events/sec with batch=1024
- Display goroutine: acquires lock ~10 times/sec (100ms interval)

The batch approach is key — without it, the lock would be acquired 180K times/sec.

**`pendingEvent` layout** (32 bytes with padding):
```
comm [16]int8    offset 0   (16 bytes)
syscallID uint32 offset 16  (4 bytes)
latencyUs int64  offset 24  (8 bytes, aligned to 8)
                 total: 32 bytes (4 bytes padding at offset 20)
```

Two events fit per cache line in the batch slice. Sequential access pattern during `RecordBatch` → good prefetcher behavior.

**DDSketch access pattern**: During `RecordBatch`, each event does a 2Q cache lookup (hash of `sketchKey`) then `sk.Add()`. The sketch's internal buckets are a `[]float64` that gets scattered across the heap. This is the least cache-friendly part of the hot path, but it's inherent to the data structure — each `(process, syscall)` pair has its own sketch with its own bucket array. No practical way to improve this without fundamentally changing the algorithm.

### Summary: What's structural vs. incidental

| Contention Point | Type | Justification |
|---|---|---|
| Per-CPU counters | **None** | PERCPU_ARRAY, zero sharing |
| Start map cross-slot on migration | **Structural** | Can't predict exit CPU at enter time; fallback scan is necessary |
| Ring buffer producer atomic | **Structural** | Multiple CPUs producing to same ring; 4-way sharding is the mitigation |
| Ring producer/consumer pages | **None** | Separate cache lines, batched commits |
| Go State.mu | **Structural** | Reader and display need consistent view; batching makes it cheap |
| DDSketch random heap access | **Structural** | Inherent to sketch data structure |

**No incidental/accidental contention found.** Every shared cache line access is there for a good reason and is mitigated by the right technique (per-CPU, sharding, batching). The one actionable correctness item is the latency measurement bias in `sys_exit` (issue #1 above) — easy to fix by moving the timestamp earlier. The sub-µs floor (#3) is a design choice worth revisiting if you want accuracy on fast syscalls.
