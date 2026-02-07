//go:build ignore

// Minimal eBPF program: tracks per-device peak queue depth.
// No ring buffer — userspace reads the peak_depth map periodically.
// Complements usb-queue-monitor-v2's sysfs polling by catching
// queue depth spikes that occur between poll intervals.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>

char LICENSE[] SEC("license") = "GPL";

// Per-device current inflight counter
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 64);
    __type(key, __u32);
    __type(value, __s64);
} queue_depth SEC(".maps");

// Per-device all-time peak queue depth
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 64);
    __type(key, __u32);
    __type(value, __s64);
} peak_depth SEC(".maps");

static __always_inline __u32 get_dev(struct request *req)
{
    struct gendisk *disk = BPF_CORE_READ(req, q, disk);
    if (!disk)
        return 0;

    __u32 major = BPF_CORE_READ(disk, major);
    __u32 minor = BPF_CORE_READ(disk, first_minor);
    return (major << 20) | minor;
}

static __always_inline __s64 *get_or_init(__s64 *dummy, void *map, __u32 *dev)
{
    __s64 *val = bpf_map_lookup_elem(map, dev);
    if (val)
        return val;

    __s64 init = 0;
    bpf_map_update_elem(map, dev, &init, BPF_NOEXIST);
    return bpf_map_lookup_elem(map, dev);
}

SEC("tp_btf/block_rq_issue")
int BPF_PROG(block_rq_issue, struct request *rq)
{
    __u32 dev = get_dev(rq);
    if (dev == 0)
        return 0;

    __s64 *depth = get_or_init(NULL, &queue_depth, &dev);
    if (!depth)
        return 0;

    __s64 new_depth = __sync_fetch_and_add(depth, 1) + 1;

    // Update peak if new depth exceeds it
    __s64 *peak = get_or_init(NULL, &peak_depth, &dev);
    if (peak && new_depth > *peak)
        __sync_val_compare_and_swap(peak, *peak, new_depth);

    return 0;
}

SEC("tp_btf/block_rq_complete")
int BPF_PROG(block_rq_complete, struct request *rq, blk_status_t error, unsigned int nr_bytes)
{
    __u32 dev = get_dev(rq);
    if (dev == 0)
        return 0;

    __s64 *depth = bpf_map_lookup_elem(&queue_depth, &dev);
    if (depth)
        __sync_fetch_and_add(depth, -1);

    return 0;
}
