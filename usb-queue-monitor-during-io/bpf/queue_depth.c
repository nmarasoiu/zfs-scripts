//go:build ignore

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>

char LICENSE[] SEC("license") = "GPL";

// Queue depth event pushed to userspace (on issue only)
struct queue_event {
    __u32 dev;      // major<<20 | minor
    __u32 depth;    // inflight count AFTER this issue (post-increment)
};

// Force BTF type emission
struct queue_event *unused_event __attribute__((unused));

// Per-device inflight counter: incremented on issue, decremented on complete
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 64);
    __type(key, __u32);
    __type(value, __s64);
} queue_depth SEC(".maps");

// Ring buffer for queue depth events (issue only)
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 8 * 1024 * 1024); // 8MB
} events SEC(".maps");

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
} queue_config SEC(".maps");

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
    __u8 *filter_enabled = bpf_map_lookup_elem(&queue_config, &key);
    if (!filter_enabled || *filter_enabled == 0)
        return 1; // No filter, trace all

    __u8 *found = bpf_map_lookup_elem(&dev_filter, &dev);
    return found != NULL;
}

// Lookup or create the per-device depth counter
static __always_inline __s64 *get_depth(__u32 dev)
{
    __s64 *val = bpf_map_lookup_elem(&queue_depth, &dev);
    if (val)
        return val;

    __s64 init = 0;
    bpf_map_update_elem(&queue_depth, &dev, &init, BPF_NOEXIST);
    return bpf_map_lookup_elem(&queue_depth, &dev);
}

SEC("tp_btf/block_rq_issue")
int BPF_PROG(block_rq_issue, struct request *rq)
{
    __u32 dev = get_dev(rq);
    if (dev == 0 || !should_trace(dev))
        return 0;

    __s64 *depth = get_depth(dev);
    if (!depth)
        return 0;

    __s64 new_depth = __sync_fetch_and_add(depth, 1) + 1;

    struct queue_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
    if (!e) {
        __u32 zero = 0;
        __u64 *cnt = bpf_map_lookup_elem(&drop_count, &zero);
        if (cnt)
            __sync_fetch_and_add(cnt, 1);
        return 0;
    }

    e->dev = dev;
    e->depth = (__u32)(new_depth > 0 ? new_depth : 0);
    bpf_ringbuf_submit(e, BPF_RB_NO_WAKEUP);
    return 0;
}

SEC("tp_btf/block_rq_complete")
int BPF_PROG(block_rq_complete, struct request *rq, blk_status_t error, unsigned int nr_bytes)
{
    __u32 dev = get_dev(rq);
    if (dev == 0 || !should_trace(dev))
        return 0;

    __s64 *depth = get_depth(dev);
    if (!depth)
        return 0;

    __sync_fetch_and_add(depth, -1);
    return 0;
}
