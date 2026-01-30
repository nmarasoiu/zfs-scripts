//go:build ignore

// SPDX-License-Identifier: GPL-2.0
// syscall_latency.c - eBPF program to trace syscall latencies

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>

char LICENSE[] SEC("license") = "GPL";

#define TASK_COMM_LEN 16
#define MAX_ENTRIES 10240

// Event sent to userspace
struct latency_event {
    __u64 latency_ns;
    __u32 syscall_id;
    __u32 pid;
    __u32 tid;
    __s64 ret;
    char comm[TASK_COMM_LEN];
};

// Force BTF type emission
struct latency_event *unused_event __attribute__((unused));

// Start timestamp per thread
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, __u32);  // tid
    __type(value, __u64); // start time
} start_times SEC(".maps");

// Syscall ID per thread (to match enter with exit)
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, MAX_ENTRIES);
    __type(key, __u32);  // tid
    __type(value, __u32); // syscall_id
} syscall_ids SEC(".maps");

// Ring buffer for events
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 256 * 1024);
} events SEC(".maps");

// Target process name filter (if set)
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, char[TASK_COMM_LEN]);
} target_comm SEC(".maps");

// Syscall filter: which syscalls to trace
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 64);
    __type(key, __u32);  // syscall_id
    __type(value, __u8); // enabled
} syscall_filter SEC(".maps");

static __always_inline int should_trace(__u32 syscall_id) {
    // Check if this syscall is in our filter
    __u8 *enabled = bpf_map_lookup_elem(&syscall_filter, &syscall_id);
    if (!enabled || *enabled == 0) {
        return 0;
    }

    // Check process name filter
    __u32 key = 0;
    char *target = bpf_map_lookup_elem(&target_comm, &key);
    if (target && target[0] != '\0') {
        char comm[TASK_COMM_LEN];
        bpf_get_current_comm(&comm, sizeof(comm));

        // Compare first 15 chars (comm is limited)
        #pragma unroll
        for (int i = 0; i < TASK_COMM_LEN - 1; i++) {
            if (target[i] == '\0' && comm[i] == '\0') {
                return 1;  // Match
            }
            if (target[i] != comm[i]) {
                return 0;  // No match
            }
        }
        return 1;
    }

    return 1;  // No filter, trace all
}

SEC("tracepoint/raw_syscalls/sys_enter")
int trace_syscall_enter(struct trace_event_raw_sys_enter *ctx) {
    __u32 syscall_id = ctx->id;

    if (!should_trace(syscall_id)) {
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

    struct latency_event *event = bpf_ringbuf_reserve(&events, sizeof(*event), 0);
    if (!event) {
        bpf_map_delete_elem(&start_times, &tid);
        bpf_map_delete_elem(&syscall_ids, &tid);
        return 0;
    }

    event->latency_ns = latency;
    event->syscall_id = *syscall_id;
    event->pid = bpf_get_current_pid_tgid() >> 32;
    event->tid = tid;
    event->ret = ctx->ret;
    bpf_get_current_comm(&event->comm, sizeof(event->comm));

    bpf_ringbuf_submit(event, 0);

    bpf_map_delete_elem(&start_times, &tid);
    bpf_map_delete_elem(&syscall_ids, &tid);

    return 0;
}
