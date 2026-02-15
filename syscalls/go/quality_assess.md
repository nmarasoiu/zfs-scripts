# Quality Assessment

Dimensions of quality for the syscall latency monitor, roughly ordered by
impact within each category. Items marked **[FIX]** have concrete patches;
items marked **[IDEA]** are future work.

---

## 1. Measurement Accuracy

### **[FIX]** Latency includes BPF exit overhead — `syscall_latency.c:201`

The timestamp is taken *after* the start-map lookup+delete:

```c
// sys_enter: timestamp BEFORE map insert (correct)
__u64 ts = bpf_ktime_get_ns();    // line 134
INSERT_START(...);                  // line 138

// sys_exit: timestamp AFTER map lookup+delete (adds ~100-500ns bias)
TRY_LOOKUP_DELETE(...);             // lines 174-186
__u64 latency = bpf_ktime_get_ns() - start_val;  // line 201
```

Every measured latency = actual syscall + exit-side map scan overhead. For
ms-range syscalls the bias is negligible. For µs-range syscalls like `getpid`
or `clock_gettime` it's 10-50% inflation.

**Fix**: Move `bpf_ktime_get_ns()` before the map scan in `sys_exit`.
Capture `end_ts` immediately, then do the lookup, then compute
`latency = end_ts - start_val`.

### **[FIX]** Sub-µs latency floor — `main.go:168-171`

```go
latencyUs := int64(event.LatencyNs / 1000)
if latencyUs < 1 { latencyUs = 1 }
```

All sub-µs syscalls are recorded as exactly 1µs. For fast syscalls like
`clock_gettime` (~200ns) this is a 5x inflation. DDSketch requires positive
values so a floor is needed, but the floor should be 1ns not 1µs.

**Fix**: Record in nanoseconds instead of microseconds. DDSketch handles
large value ranges fine with relative accuracy (α=0.25 covers ns through
seconds). Change the conversion to happen at display time only — format.go
already has a `formatLatency(us)` function, just feed it ns and adjust
thresholds.

---

## 2. Scalability

### **[IDEA]** Hardcoded 4-way sharding — `syscall_latency.c:56,88`

Start maps and ring buffers are sharded `cpu_id % 4`. On a 128-core machine,
32 CPUs compete per slot. Making `NUM_SLOTS` configurable
(`min(num_cpus, 16)`) would reduce contention at scale, at the cost of more
fallback scanning on exit.

### **[IDEA]** Single Go mutex for all sketch updates

`State.mu` protects all sketch writes. At 180K events/sec with batch=1024,
it's ~176 locks/sec (fine today). If event rates climb 10x or batch sizes
shrink, contention rises linearly. Per-shard locking or a lock-free handoff
would help.

---

## 3. TUI / Usability

### **[IDEA]** Color-coded latency cells

ANSI green/yellow/red based on thresholds. Makes hot spots visible at a
glance without reading numbers. See `display_ideas.md` for detail.

### **[IDEA]** Trend/delta indicators

Rate change arrows or p99 deltas between intervals. Surfaces regressions
that absolute values hide. See `display_ideas.md`.

### **[IDEA]** Distribution shape indicator

Flag bimodal (`‼`), heavy-tail (`!`), or unimodal (`~`) next to latency
values. DDSketch bucket analysis can detect these. See `display_ideas.md`.

### **[IDEA]** Interactive scroll / pagination

Currently top-N truncated. With 400 sketches, most are invisible. Arrow-key
scrolling through the full sorted list would help exploration.

### **[IDEA]** Sparklines in process panel

Unicode block characters (▁▂▃▄▅▆▇█) showing per-second rate history. 8-char
mini trend per process. See `display_ideas.md`.

---

## 4. Robustness

### **[IDEA]** Comm change between enter/exit

If a process calls `prctl(PR_SET_NAME)` mid-syscall and the new name doesn't
match the `-c` filter, the start entry leaks (cleaned only by LRU eviction)
and `map_used` drifts upward. Extremely rare in practice.

### **[IDEA]** DDSketch accuracy vs memory trade-off

α=0.25 means ±25% relative error on percentiles. For latency monitoring where
10ms vs 12.5ms matters, tightening to α=0.05 (at ~2-3x memory per sketch)
could be worthwhile. Expose as a `--accuracy` flag.

### **[IDEA]** Adaptive response to drops

When ring drops climb, there's no adaptive response. Options: auto-increase
ring buffer (if kernel supports resize), sample events, or surface a
prominent warning when drop rate exceeds a threshold.

---

## 5. Operational / Packaging

### **[IDEA]** Architecture portability

`syscalls.go` is x86_64 only. arm64 has different syscall numbers. A build
tag or code generation step from kernel headers would fix this.

### **[IDEA]** Export formats

JSON, CSV, or Prometheus exposition for integration with Grafana or other
dashboards. A `--json` flag emitting periodic snapshots would be the minimal
version.

### **[IDEA]** Snapshot / bookmark

Save a point-in-time snapshot to disk for later comparison or post-mortem
analysis. Could serialize DDSketch state (protobuf support is built in).

---

## 6. Code Quality

### **[IDEA]** BPF integration tests

The test suite is solid (1464 lines) but all tests are unit-level. No tests
actually load the BPF program. A CI environment with root + BPF support could
run a smoke test: load program, generate some syscalls, verify events arrive.

### **[IDEA]** Hot-path benchmarks

`RecordBatch` and `collectEntries` sort are on the critical path but have no
benchmarks. `go test -bench` would catch regressions from refactors.
