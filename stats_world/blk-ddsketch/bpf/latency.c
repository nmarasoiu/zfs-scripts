//go:build ignore

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>

char LICENSE[] SEC("license") = "GPL";

// Latency sample pushed to userspace
struct latency_event {
    __u32 dev;          // major<<20 | minor
    __u32 _pad;         // alignment padding
    __u64 latency_ns;   // request latency in nanoseconds
};

// Force BTF type emission
struct latency_event *unused_event __attribute__((unused));

// Hash map: request pointer -> issue timestamp
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 65536);
    __type(key, __u64);
    __type(value, __u64);
} req_start SEC(".maps");

// Per-CPU ring buffers for latency samples (4 × 2MB = 8MB total)
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 2 * 1024 * 1024);
} events0 SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 2 * 1024 * 1024);
} events1 SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 2 * 1024 * 1024);
} events2 SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 2 * 1024 * 1024);
} events3 SEC(".maps");

// Optional device filter (0 = disabled)
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 64);
    __type(key, __u32);
    __type(value, __u8);
} dev_filter SEC(".maps");

// Config: filter_enabled
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, __u8);
} lat_config SEC(".maps");

// Drop counter: incremented when ring buffer is full
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, __u64);
} drop_count SEC(".maps");

static __always_inline __u32 get_dev(struct request *req)
{
    struct gendisk *disk = BPF_CORE_READ(req, q, disk);
    if (!disk)
        return 0;

    __u32 major = BPF_CORE_READ(disk, major);
    __u32 minor = BPF_CORE_READ(disk, first_minor);
    return (major << 20) | minor;
}

static __always_inline int should_trace(__u32 dev)
{
    __u32 key = 0;
    __u8 *filter_enabled = bpf_map_lookup_elem(&lat_config, &key);
    if (!filter_enabled || *filter_enabled == 0)
        return 1; // No filter, trace all

    __u8 *found = bpf_map_lookup_elem(&dev_filter, &dev);
    return found != NULL;
}

SEC("tp_btf/block_rq_issue")
int BPF_PROG(block_rq_issue, struct request *rq)
{
    __u32 dev = get_dev(rq);
    if (dev == 0 || !should_trace(dev))
        return 0;

    __u64 ts = bpf_ktime_get_ns();
    __u64 key = (__u64)rq;
    bpf_map_update_elem(&req_start, &key, &ts, BPF_ANY);
    return 0;
}

SEC("tp_btf/block_rq_complete")
int BPF_PROG(block_rq_complete, struct request *rq, blk_status_t error, unsigned int nr_bytes)
{
    __u64 key = (__u64)rq;
    __u64 *start_ts = bpf_map_lookup_elem(&req_start, &key);
    if (!start_ts)
        return 0; // Missed issue or filtered device

    __u64 latency_ns = bpf_ktime_get_ns() - *start_ts;
    bpf_map_delete_elem(&req_start, &key);

    __u32 dev = get_dev(rq);
    if (dev == 0)
        return 0;

    // Dispatch to per-CPU ring buffer
    void *rb;
    switch (bpf_get_smp_processor_id() % 4) {
    case 0: rb = &events0; break;
    case 1: rb = &events1; break;
    case 2: rb = &events2; break;
    default: rb = &events3; break;
    }

    struct latency_event *e = bpf_ringbuf_reserve(rb, sizeof(*e), 0);
    if (!e) {
        __u32 zero = 0;
        __u64 *cnt = bpf_map_lookup_elem(&drop_count, &zero);
        if (cnt)
            __sync_fetch_and_add(cnt, 1);
        return 0;
    }

    e->dev = dev;
    e->latency_ns = latency_ns;
    bpf_ringbuf_submit(e, BPF_RB_NO_WAKEUP);
    return 0;
}
