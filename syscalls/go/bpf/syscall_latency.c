//go:build ignore

// SPDX-License-Identifier: GPL-2.0
// syscall_latency.c - eBPF program to trace syscall latencies

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>

char LICENSE[] SEC("license") = "GPL";

#define TASK_COMM_LEN 16
#define MAX_ENTRIES 65536

// Compile-time constant rewritten by userspace before loading.
// When 0 (default), the verifier dead-code-eliminates the comm filter
// branch, giving zero overhead in the hot path.
const volatile __u8 use_comm_filter = 0;

// Event sent to userspace
struct latency_event {
    __u64 latency_ns;
    __u32 syscall_id;
    __u32 _pad;
    char comm[TASK_COMM_LEN];
};

// Force BTF type emission
struct latency_event *unused_event __attribute__((unused));

// Per-CPU counters — each CPU writes only its own slot, zero contention.
struct percpu_counters {
    __s64 map_used;     // +1 sys_enter, -1 sys_exit/miss (can go negative per-CPU)
    __u64 drop_ring;    // ring reserve failures
    __u64 drop_miss;    // sys_exit miss (merged evict+startup)
};

// Force BTF type emission for Go codegen
struct percpu_counters *unused_counters __attribute__((unused));

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct percpu_counters);
} counters SEC(".maps");

static __always_inline struct percpu_counters *get_counters(void) {
    __u32 zero = 0;
    return bpf_map_lookup_elem(&counters, &zero);
}

// Start timestamp per thread — split into 4 slots by cpu_id % 4.
// Each slot has MAX_ENTRIES/4 capacity (total unchanged).
#define NUM_SLOTS 4

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES / NUM_SLOTS);
    __type(key, __u32);  // tid
    __type(value, __u64); // start time
} start0 SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES / NUM_SLOTS);
    __type(key, __u32);
    __type(value, __u64);
} start1 SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES / NUM_SLOTS);
    __type(key, __u32);
    __type(value, __u64);
} start2 SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES / NUM_SLOTS);
    __type(key, __u32);
    __type(value, __u64);
} start3 SEC(".maps");

// Per-CPU ring buffers (4 × 2MB = 8MB total)
#define NUM_RINGS 4

struct { __uint(type, BPF_MAP_TYPE_RINGBUF); __uint(max_entries, 2 * 1024 * 1024); } events0 SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_RINGBUF); __uint(max_entries, 2 * 1024 * 1024); } events1 SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_RINGBUF); __uint(max_entries, 2 * 1024 * 1024); } events2 SEC(".maps");
struct { __uint(type, BPF_MAP_TYPE_RINGBUF); __uint(max_entries, 2 * 1024 * 1024); } events3 SEC(".maps");

// Target process names (hash lookup for O(1) matching)
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 16);
    __type(key, char[TASK_COMM_LEN]);
    __type(value, __u8);
} target_comms SEC(".maps");

static __always_inline int should_trace(void) {
    if (use_comm_filter) {
        char comm[TASK_COMM_LEN];
        bpf_get_current_comm(&comm, sizeof(comm));
        __u8 *match = bpf_map_lookup_elem(&target_comms, comm);
        if (!match) return 0;
    }
    return 1;
}

// Insert into slot's start map by cpu_id % 4
#define INSERT_START(MAP, tid, ts) bpf_map_update_elem(&MAP, &tid, &ts, BPF_ANY)

SEC("tracepoint/raw_syscalls/sys_enter")
int trace_syscall_enter(struct trace_event_raw_sys_enter *ctx) {
    __u32 syscall_id = ctx->id;

    // exit(60) and exit_group(231) never generate sys_exit (thread dies),
    // so their entries leak in start_times forever, eventually
    // exhausting the hash map and silently blocking all new tracking.
    if (syscall_id == 60 || syscall_id == 231)
        return 0;

    if (!should_trace()) {
        return 0;
    }

    struct percpu_counters *c = get_counters();
    if (!c)
        return 0;

    __u32 tid = bpf_get_current_pid_tgid();
    __u64 ts = bpf_ktime_get_ns();

    __u32 slot = bpf_get_smp_processor_id() % NUM_SLOTS;
    switch (slot) {
        case 0: INSERT_START(start0, tid, ts); break;
        case 1: INSERT_START(start1, tid, ts); break;
        case 2: INSERT_START(start2, tid, ts); break;
        case 3: INSERT_START(start3, tid, ts); break;
    }
    c->map_used++;

    return 0;
}

// Lookup and delete from a start map; sets *val on success.
#define TRY_LOOKUP_DELETE(MAP, tid, val, found) do { \
    __u64 *_ts = bpf_map_lookup_elem(&MAP, &tid); \
    if (_ts) { \
        val = *_ts; \
        bpf_map_delete_elem(&MAP, &tid); \
        found = 1; \
    } \
} while(0)

SEC("tracepoint/raw_syscalls/sys_exit")
int trace_syscall_exit(struct trace_event_raw_sys_exit *ctx) {
    if (!should_trace())
        return 0;

    struct percpu_counters *c = get_counters();
    if (!c)
        return 0;

    __u32 tid = bpf_get_current_pid_tgid();
    __u64 end_ts = bpf_ktime_get_ns();
    __u64 start_val = 0;
    int found = 0;

    // Check local slot first (most likely hit), then scan others
    __u32 slot = bpf_get_smp_processor_id() % NUM_SLOTS;
    switch (slot) {
        case 0: TRY_LOOKUP_DELETE(start0, tid, start_val, found); break;
        case 1: TRY_LOOKUP_DELETE(start1, tid, start_val, found); break;
        case 2: TRY_LOOKUP_DELETE(start2, tid, start_val, found); break;
        case 3: TRY_LOOKUP_DELETE(start3, tid, start_val, found); break;
    }

    // Fallback: scan other 3 slots
    if (!found) {
        if (slot != 0) { TRY_LOOKUP_DELETE(start0, tid, start_val, found); }
        if (!found && slot != 1) { TRY_LOOKUP_DELETE(start1, tid, start_val, found); }
        if (!found && slot != 2) { TRY_LOOKUP_DELETE(start2, tid, start_val, found); }
        if (!found && slot != 3) { TRY_LOOKUP_DELETE(start3, tid, start_val, found); }
    }

    if (!found) {
        // fork/clone/vfork: child returns from the parent's syscall
        // with a new tid that was never in start_times — not a real drop.
        __u32 id = ctx->id;
        if (id == 56 || id == 57 || id == 58 || id == 435)
            return 0;
        c->drop_miss++;
        c->map_used--;
        return 0;
    }

    c->map_used--;

    __u64 latency = end_ts - start_val;

    // Ring selection by CPU for better locality
    __u32 ring = bpf_get_smp_processor_id() % NUM_RINGS;
    struct latency_event *event = NULL;
    switch (ring) {
        case 0: event = bpf_ringbuf_reserve(&events0, sizeof(*event), 0); break;
        case 1: event = bpf_ringbuf_reserve(&events1, sizeof(*event), 0); break;
        case 2: event = bpf_ringbuf_reserve(&events2, sizeof(*event), 0); break;
        case 3: event = bpf_ringbuf_reserve(&events3, sizeof(*event), 0); break;
    }
    if (!event) {
        c->drop_ring++;
        return 0;
    }

    event->latency_ns = latency;
    event->syscall_id = ctx->id;
    bpf_get_current_comm(&event->comm, sizeof(event->comm));

    bpf_ringbuf_submit(event, BPF_RB_NO_WAKEUP);

    return 0;
}
