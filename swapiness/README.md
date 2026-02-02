# Swap as ARC Pressure Buffer

## Problem

With 32GB RAM and ZFS, the ARC (Adaptive Replacement Cache) can consume most available memory. When memory pressure spikes, the ARC will release memory — but there's a lag. During this window, the kernel sees "no free memory" before ARC has finished shrinking.

Without swap, the OOM killer activates. It sends **SIGKILL (signal 9)** which:
- Cannot be caught, blocked, or ignored
- Gives processes no chance to clean up
- Can leave behind:
  - Partial/corrupted file writes
  - Orphaned locks (file, database, semaphores)
  - Stale temp files, PID files, socket files
  - Incomplete transactions

## Solution

A small swap file acts as a buffer to absorb the pressure spike for those few moments until ARC releases memory. We're not swapping real workloads — just surviving the race condition.

- **Swap size**: 5GB (enough buffer, not for heavy swapping)
- **Swappiness**: 10 (low — only use swap under real pressure, not eagerly)

## Commands Executed

```bash
# Create 5GB swap file
sudo fallocate -l 5G /swapfile

# Secure permissions (required for swap)
sudo chmod 600 /swapfile

# Format as swap
sudo mkswap /swapfile

# Enable immediately
sudo swapon /swapfile

# Set swappiness to 10 (runtime)
sudo sysctl vm.swappiness=10
```

## System Files Modified

### /etc/fstab
Added line for persistence across reboots:
```
/swapfile none swap sw 0 0
```

### /etc/sysctl.d/99-swappiness.conf
Created file to persist swappiness setting:
```
vm.swappiness=10
```

## Verification

```bash
# Check swap status
swapon --show
free -h

# Check swappiness
cat /proc/sys/vm/swappiness
```

## Alternative Approach

Instead of (or in addition to) swap, you can cap ARC proactively:

```bash
# Example: limit ARC to 24GB on a 32GB system
echo "options zfs zfs_arc_max=25769803776" >> /etc/modprobe.d/zfs.conf
```

This leaves guaranteed headroom so ARC never fights for the last bytes. Tradeoff: less cache benefit, but more predictability.

## Nuclear Option: Disable OOM Killer

For systems that prefer **consistency over availability**, you can make the kernel panic instead of OOM killing:

| `vm.panic_on_oom` | Behavior |
|-------------------|----------|
| 0 | OOM killer runs (default) |
| 1 | Kernel panics on OOM (except certain cgroup-constrained OOMs) |
| 2 | Kernel panics on any OOM, unconditionally |

**Why panic instead of kill?**
- A panic is **deterministic** — you know exactly what happened
- Reboot runs fsck, replays journals, starts from a clean state
- Better than silently killing a database mid-transaction and continuing as if nothing happened

**Typical use cases**:
- Database servers where corruption is worse than downtime
- Systems with watchdog timers that auto-reboot
- Clustered systems where a dead node gets fenced and workload moves elsewhere

```bash
# Runtime
sudo sysctl vm.panic_on_oom=2

# Persistent
echo 'vm.panic_on_oom=2' | sudo tee /etc/sysctl.d/99-panic-on-oom.conf
```

**Note**: With swap buffer in place, the chance of hitting OOM is already significantly reduced. Panic-on-OOM would be defense in depth.
