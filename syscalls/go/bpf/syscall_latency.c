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

// Start timestamp per thread (LRU: kernel auto-evicts oldest on full)
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, __u32);  // tid
    __type(value, __u64); // start time
} start_times SEC(".maps");

// Syscall ID per thread (LRU: kernel auto-evicts oldest on full)
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, __u32);  // tid
    __type(value, __u32); // syscall_id
} syscall_ids SEC(".maps");

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

// Drop counter: incremented when ring buffer is full
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, __u64);
} drop_count SEC(".maps");

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
    __u32 syscall_id = ctx->id;

    // exit(60) and exit_group(231) never generate sys_exit (thread dies),
    // so their entries leak in start_times/syscall_ids forever, eventually
    // exhausting the hash map and silently blocking all new tracking.
    if (syscall_id == 60 || syscall_id == 231)
        return 0;

    if (!should_trace()) {
        return 0;
    }

    __u32 tid = bpf_get_current_pid_tgid();
    __u64 ts = bpf_ktime_get_ns();

    bpf_map_update_elem(&start_times, &tid, &ts, BPF_ANY);
    bpf_map_update_elem(&syscall_ids, &tid, &syscall_id, BPF_ANY);

    return 0;
}

SEC("tracepoint/raw_syscalls/sys_exit")
int trace_syscall_exit(struct trace_event_raw_sys_exit *ctx) {
    __u32 tid = bpf_get_current_pid_tgid();

    __u64 *start_ts = bpf_map_lookup_elem(&start_times, &tid);
    if (!start_ts) {
        return 0;
    }

    __u32 *syscall_id = bpf_map_lookup_elem(&syscall_ids, &tid);
    if (!syscall_id) {
        bpf_map_delete_elem(&start_times, &tid);
        return 0;
    }

    __u64 latency = bpf_ktime_get_ns() - *start_ts;

    __u32 ring = (__u32)latency % NUM_RINGS;
    struct latency_event *event = NULL;
    switch (ring) {
        case 0: event = bpf_ringbuf_reserve(&events0, sizeof(*event), 0); break;
        case 1: event = bpf_ringbuf_reserve(&events1, sizeof(*event), 0); break;
        case 2: event = bpf_ringbuf_reserve(&events2, sizeof(*event), 0); break;
        case 3: event = bpf_ringbuf_reserve(&events3, sizeof(*event), 0); break;
    }
    if (!event) {
        __u32 zero = 0;
        __u64 *cnt = bpf_map_lookup_elem(&drop_count, &zero);
        if (cnt)
            __sync_fetch_and_add(cnt, 1);
        bpf_map_delete_elem(&start_times, &tid);
        bpf_map_delete_elem(&syscall_ids, &tid);
        return 0;
    }

    event->latency_ns = latency;
    event->syscall_id = *syscall_id;
    bpf_get_current_comm(&event->comm, sizeof(event->comm));

    bpf_ringbuf_submit(event, BPF_RB_NO_WAKEUP);

    bpf_map_delete_elem(&start_times, &tid);
    bpf_map_delete_elem(&syscall_ids, &tid);

    return 0;
}
