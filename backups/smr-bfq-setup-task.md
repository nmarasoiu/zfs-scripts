# Task: Configure BFQ I/O Scheduler for SMR Drives

## Context

This Linux system has 5 USB-attached Seagate SMR drives used for qBittorrent seeding. We want to optimize I/O scheduling to reduce seek thrashing and allow the drives to batch reads efficiently.

**Current state:**
- Drives: sdc, sdd, sde, sdf, sdg (all "Expansion HDD" model)
- Current scheduler: mq-deadline
- Connection: USB 3.0 via UAS driver, queue_depth=30
- Workload: Torrent seeding with 64MB piece sizes
- Filesystem: ZFS dRAID pool

**Goal:** Switch to BFQ scheduler with tuned parameters that prioritize throughput and request batching over latency.

---

## Files to Create

### 1. Udev Rule: `/etc/udev/rules.d/99-smr-bfq.rules`

```
# BFQ scheduler tuning for SMR drives (Seagate Expansion HDD)
# Matches by model string, applies BFQ and runs tuning script

ACTION=="add|change", SUBSYSTEM=="block", KERNEL=="sd[a-z]", ATTR{queue/rotational}=="1", ATTRS{model}=="Expansion HDD*", \
    ATTR{queue/scheduler}="bfq", \
    RUN+="/usr/local/bin/tune-smr-bfq.sh %k"
```

### 2. Tuning Script: `/usr/local/bin/tune-smr-bfq.sh`

```bash
#!/bin/bash
# Tune BFQ parameters for SMR drives
# Called by udev rule with device name (e.g., sdd) as argument

DEVICE="$1"
QUEUE_PATH="/sys/block/${DEVICE}/queue"
BFQ_PATH="${QUEUE_PATH}/iosched"

# Verify BFQ is active
if ! grep -q '\[bfq\]' "${QUEUE_PATH}/scheduler" 2>/dev/null; then
    echo "bfq" > "${QUEUE_PATH}/scheduler" 2>/dev/null || exit 1
fi

# Wait a moment for iosched parameters to appear
sleep 0.5

# Only proceed if BFQ params exist
[ -d "$BFQ_PATH" ] || exit 0

# --- BFQ Tuning for SMR / Torrent Seeding ---

# slice_idle: Time to wait for more requests before dispatching (microseconds)
# Default is 8000 (8ms). We increase to 40ms to accumulate more reads.
# This is the "wait for the shuttle to fill" setting.
echo 40000 > "${BFQ_PATH}/slice_idle_us" 2>/dev/null

# low_latency: When 0, prioritize throughput over responsiveness
# For seeding, we want throughput, not interactive latency
echo 0 > "${BFQ_PATH}/low_latency" 2>/dev/null

# strict_guarantees: When 0, allows more aggressive reordering
# Gives BFQ more freedom to batch geometrically
echo 0 > "${BFQ_PATH}/strict_guarantees" 2>/dev/null

# back_seek_max: Maximum distance for backward seeks (KB)
# Default is 16384 (16MB). For large sequential reads, we can allow more.
echo 32768 > "${BFQ_PATH}/back_seek_max" 2>/dev/null

# back_seek_penalty: Cost multiplier for backward seeks
# Default is 2. Lower = more willing to do backward seeks for batching.
echo 1 > "${BFQ_PATH}/back_seek_penalty" 2>/dev/null

# fifo_expire_async: Deadline for async requests (ms)
# Default is 250. Increase to allow more batching time.
echo 500 > "${BFQ_PATH}/fifo_expire_async" 2>/dev/null

# fifo_expire_sync: Deadline for sync requests (ms)
# Keep reasonable for any interactive use
echo 250 > "${BFQ_PATH}/fifo_expire_sync" 2>/dev/null

logger "BFQ tuned for SMR drive: ${DEVICE}"
```

---

## Installation Steps

1. Create the tuning script:
   ```bash
   sudo tee /usr/local/bin/tune-smr-bfq.sh << 'EOF'
   # (paste script content above)
   EOF
   sudo chmod +x /usr/local/bin/tune-smr-bfq.sh
   ```

2. Create the udev rule:
   ```bash
   sudo tee /etc/udev/rules.d/99-smr-bfq.rules << 'EOF'
   # (paste rule content above)
   EOF
   ```

3. Reload udev rules:
   ```bash
   sudo udevadm control --reload-rules
   ```

4. Apply to existing drives (without reboot):
   ```bash
   for dev in sdc sdd sde sdf sdg; do
       sudo udevadm trigger --action=change /dev/$dev
   done
   ```

---

## Verification

After installation, verify the configuration:

```bash
# Check schedulers are now BFQ (should show [bfq])
cat /sys/block/sd[c-g]/queue/scheduler

# Check BFQ parameters were applied
echo "=== slice_idle_us (expect 40000) ==="
cat /sys/block/sd[c-g]/queue/iosched/slice_idle_us

echo "=== low_latency (expect 0) ==="
cat /sys/block/sd[c-g]/queue/iosched/low_latency

echo "=== strict_guarantees (expect 0) ==="
cat /sys/block/sd[c-g]/queue/iosched/strict_guarantees

echo "=== back_seek_max (expect 32768) ==="
cat /sys/block/sd[c-g]/queue/iosched/back_seek_max

echo "=== fifo_expire_async (expect 500) ==="
cat /sys/block/sd[c-g]/queue/iosched/fifo_expire_async
```

Check system log for confirmation:
```bash
journalctl -t tune-smr-bfq.sh --since "5 minutes ago"
# or
grep "BFQ tuned" /var/log/syslog
```

---

## Rollback

To revert to mq-deadline:

```bash
# Remove the udev rule
sudo rm /etc/udev/rules.d/99-smr-bfq.rules

# Remove the script
sudo rm /usr/local/bin/tune-smr-bfq.sh

# Reload udev
sudo udevadm control --reload-rules

# Switch back to mq-deadline immediately
for dev in sdc sdd sde sdf sdg; do
    echo mq-deadline | sudo tee /sys/block/$dev/queue/scheduler
done
```

---

## Performance Monitoring

### Baseline Measurement (before BFQ)
Capture baseline metrics with mq-deadline for comparison:
```bash
# Overall drive utilization and I/O patterns (5 second intervals, 12 samples = 1 minute)
iostat -x 5 12 sdc sdd sde sdf sdg > baseline-iostat.txt

# Key metrics to note:
# - %util: Current 15-20%, target ~10%
# - r/s: Reads per second
# - rMB/s: Read throughput
# - await: Average I/O latency
# - r_await: Read latency specifically
```

### BFQ-Specific Statistics
After switching to BFQ, monitor batching effectiveness:

```bash
# Check BFQ dispatched request stats
for dev in sdc sdd sde sdf sdg; do
    echo "=== $dev ==="
    cat /sys/block/$dev/queue/iosched/dispatched 2>/dev/null
done

# Monitor queue depth usage (how well we're filling the 30-slot bucket)
for dev in sdc sdd sde sdf sdg; do
    echo "=== $dev queue stats ==="
    cat /sys/block/$dev/inflight
    cat /sys/block/$dev/queue/nr_requests
done

# Watch real-time BFQ behavior (Ctrl+C to stop)
watch -n 2 'for d in sdc sdd sde sdf sdg; do echo "$d: $(cat /sys/block/$d/inflight) in-flight"; done'
```

### Compare Before/After
```bash
# After BFQ is running, capture same metrics
iostat -x 5 12 sdc sdd sde sdf sdg > bfq-iostat.txt

# Compare key improvements:
# - Lower %util (less time seeking, more batching)
# - Similar or higher rMB/s (maintained throughput)
# - Higher r/s with lower %util = better efficiency
# - r_await might increase slightly (acceptable trade-off for batching)
```

### Success Indicators
- **%util drops to ~10%**: Better geometric batching, less seek time
- **rMB/s stays similar or increases**: Throughput maintained or improved
- **Inflight count averages higher**: Queue bucket filling better before dispatch
- **Fewer context switches in BFQ stats**: More requests batched per dispatch

### Continuous Monitoring
```bash
# Quick health check command
alias bfq-check='for d in sdc sdd sde sdf sdg; do echo "$d: $(cat /sys/block/$d/queue/scheduler) idle=$(cat /sys/block/$d/queue/iosched/slice_idle_us 2>/dev/null)us"; done'

# Detailed I/O stats (run during peak seeding)
iostat -x -d -m 10 sdc sdd sde sdf sdg
```

---

## Notes

- **Why BFQ over mq-deadline:** BFQ has more sophisticated request batching and can accumulate I/O before dispatching, which is beneficial for SMR drives that suffer from random seek patterns.

- **Why these parameters:**
  - `slice_idle_us=40000`: Waits 40ms for more requests from the same process before dispatching, allowing the "shuttle to fill up"
  - `low_latency=0`: Prioritizes throughput over interactive responsiveness
  - `strict_guarantees=0`: Allows more aggressive reordering across processes
  - `back_seek_max=32768`: Allows considering requests up to 32MB behind the head
  - `back_seek_penalty=1`: Treats backward seeks nearly equal to forward seeks
  - `fifo_expire_async=500`: Gives async requests more time to be batched

- **Persistence:** The udev rule ensures these settings apply automatically on boot and when drives are plugged in.
