#!/bin/bash
# Comprehensive BFQ monitoring and analysis for SMR drives

DRIVES="sdc sdd sde sdf sdg"

echo "================================================================"
echo "BFQ Configuration Analysis - $(date)"
echo "================================================================"
echo

# Current BFQ parameters
echo "Current BFQ Parameters:"
echo "----------------------------------------------------------------"
printf "%-6s %12s %10s %10s %10s %12s\n" \
    "Device" "slice_idle" "low_lat" "strict_g" "timeout_s" "max_budget"

for dev in $DRIVES; do
    idle=$(cat /sys/block/$dev/queue/iosched/slice_idle_us 2>/dev/null || echo "N/A")
    lowlat=$(cat /sys/block/$dev/queue/iosched/low_latency 2>/dev/null || echo "N/A")
    strict=$(cat /sys/block/$dev/queue/iosched/strict_guarantees 2>/dev/null || echo "N/A")
    timeout=$(cat /sys/block/$dev/queue/iosched/timeout_sync 2>/dev/null || echo "N/A")
    budget=$(cat /sys/block/$dev/queue/iosched/max_budget 2>/dev/null || echo "N/A")

    printf "%-6s %9sμs %10s %10s %10s %12s\n" \
        "$dev" "$idle" "$lowlat" "$strict" "$timeout" "$budget"
done

echo
echo "Seek Parameters:"
echo "----------------------------------------------------------------"
printf "%-6s %15s %18s\n" "Device" "back_seek_max" "back_seek_penalty"

for dev in $DRIVES; do
    seekmax=$(cat /sys/block/$dev/queue/iosched/back_seek_max 2>/dev/null || echo "N/A")
    seekpen=$(cat /sys/block/$dev/queue/iosched/back_seek_penalty 2>/dev/null || echo "N/A")

    printf "%-6s %15s %18s\n" "$dev" "$seekmax" "$seekpen"
done

echo
echo "FIFO Expiration Settings:"
echo "----------------------------------------------------------------"
printf "%-6s %15s %15s\n" "Device" "async_expire" "sync_expire"

for dev in $DRIVES; do
    async=$(cat /sys/block/$dev/queue/iosched/fifo_expire_async 2>/dev/null || echo "N/A")
    sync=$(cat /sys/block/$dev/queue/iosched/fifo_expire_sync 2>/dev/null || echo "N/A")

    printf "%-6s %15s %15s\n" "$dev" "$async" "$sync"
done

echo
echo "================================================================"
echo "Current I/O Statistics (live snapshot):"
echo "================================================================"
echo

iostat -x -d 1 1 sdc sdd sde sdf sdg | grep -E "Device|sd[cdefg]" | grep -v "sd[cdefg][0-9]"

echo
echo "================================================================"
echo "Queue Depth (Current In-Flight Requests, max 30):"
echo "================================================================"
echo

printf "%-6s %10s\n" "Device" "In-Flight"
for dev in $DRIVES; do
    inflight=$(cat /sys/block/$dev/inflight 2>/dev/null | awk '{print $1+$2}')
    printf "%-6s %10s/30\n" "$dev" "$inflight"
done

echo
echo "================================================================"
echo "Key Metrics Summary:"
echo "================================================================"
echo "Goal: Higher r_await + Lower %util = Better batching"
echo
iostat -x -d 2 3 sdc sdd sde sdf sdg | \
    grep -E "sd[cdefg]" | grep -v "sd[cdefg][0-9]" | \
    awk 'NR==1 {next}
         {dev=$1; r_await=$6; util=$NF;
          sum_await[dev]+=$6; sum_util[dev]+=$NF; count[dev]++}
         END {
           printf "%-6s %12s %12s\n", "Device", "Avg r_await", "Avg %util";
           for (d in sum_await)
             printf "%-6s %12.2f %12.2f\n", d, sum_await[d]/count[d], sum_util[d]/count[d]
         }'

echo
echo "================================================================"
echo "Run 'iostat -dxp 12' to monitor continuously"
echo "Run './usb-queue-monitor.sh' to watch queue depth real-time"
echo "================================================================"
