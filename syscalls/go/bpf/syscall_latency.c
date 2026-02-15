//go:build ignore

// SPDX-License-Identifier: GPL-2.0
// syscall_latency.c - eBPF program to trace syscall latencies

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>

char LICENSE[] SEC("license") = "GPL";

#define TASK_COMM_LEN 16

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
    __u64 drop_ring;    // ring reserve failures
    __u64 drop_miss;    // sys_exit miss (fork/clone child)
};

// Force BTF type emission for Go codegen
struct percpu_counters *unused_counters __attribute__((unused));

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct percpu_counters);
} counters SEC(".maps");

// Per-task start timestamp — storage attached to task_struct, no hash lookup.
// Allocated on first sys_enter (BPF_LOCAL_STORAGE_GET_F_CREATE),
// automatically freed when the task exits — no leaks from exit/exit_group.
struct {
    __uint(type, BPF_MAP_TYPE_TASK_STORAGE);
    __uint(map_flags, BPF_F_NO_PREALLOC);
    __type(key, int);
    __type(value, __u64);
} start_times SEC(".maps");

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

SEC("tracepoint/raw_syscalls/sys_enter")
int trace_syscall_enter(struct trace_event_raw_sys_enter *ctx) {
    if (!should_trace())
        return 0;

    struct task_struct *task = bpf_get_current_task_btf();
    __u64 ts = bpf_ktime_get_ns();
    __u64 *slot = bpf_task_storage_get(&start_times, task, NULL,
                                       BPF_LOCAL_STORAGE_GET_F_CREATE);
    if (slot)
        *slot = ts;

    return 0;
}

SEC("tracepoint/raw_syscalls/sys_exit")
int trace_syscall_exit(struct trace_event_raw_sys_exit *ctx) {
    if (!should_trace())
        return 0;

    struct task_struct *task = bpf_get_current_task_btf();
    __u64 end_ts = bpf_ktime_get_ns();

    __u64 *start_ts = bpf_task_storage_get(&start_times, task, NULL, 0);
    if (!start_ts) {
        // fork/clone/vfork: child returns from the parent's syscall
        // with a new task that never had a start_times entry.
        __u32 id = ctx->id;
        if (id == 56 || id == 57 || id == 58 || id == 435)
            return 0;
        __u32 zero = 0;
        struct percpu_counters *c = bpf_map_lookup_elem(&counters, &zero);
        if (c) c->drop_miss++;
        return 0;
    }

    __u64 latency = end_ts - *start_ts;
    bpf_task_storage_delete(&start_times, task);

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
        __u32 zero = 0;
        struct percpu_counters *c = bpf_map_lookup_elem(&counters, &zero);
        if (c) c->drop_ring++;
        return 0;
    }

    event->latency_ns = latency;
    event->syscall_id = ctx->id;
    bpf_get_current_comm(&event->comm, sizeof(event->comm));

    bpf_ringbuf_submit(event, BPF_RB_NO_WAKEUP);

    return 0;
}
