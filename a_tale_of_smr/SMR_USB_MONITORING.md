# SMR USB Drive Monitoring - Findings & Conclusions

**Date:** 2026-01-31  
**Pool:** hddpool (dRAID1 + special vdevs)  
**Drives:** 5x Seagate Expansion HDD (USB 3.0)

---

## Executive Summary

USB-SATA bridges on Seagate Expansion drives **block all meaningful diagnostics**:
- No SMART data (temperature, reallocated sectors, power-on hours)
- No SMR zone visibility (DM-SMR hides internal state)
- No TRIM support (LBPU=0)

**Data integrity is confirmed OK** - ZFS reports 0 errors. All observed I/O errors are transient/recoverable link-level events.

---

## Hardware Inventory

| Device | Serial | Size | Firmware | Role in Pool |
|--------|--------|------|----------|--------------|
| sdc | NT17DHQR | 3.6TB | 1801 | Standalone vdev |
| sdd | NT17FC7Z | 5.5TB | 0003 | dRAID1 member |
| sde | NT17FBP5 | 5.5TB | 0003 | dRAID1 member |
| sdf | NT17FBQC | 5.5TB | 0003 | dRAID1 member |
| sdg | NT17FC6F | 5.5TB | 0003 | dRAID1 member |

**Note:** FW 1801 (4TB) exposes SCSI log pages; FW 0003 (6TB) blocks everything.

---

## What We CANNOT Monitor

| Metric | Reason |
|--------|--------|
| Temperature | SMART blocked, no SCSI temp log page (0x0D) |
| SMR zone utilization | DM-SMR - drive hides zone state completely |
| Reallocated sectors | SMART blocked |
| Pending sectors | SMART blocked |
| Power-on hours | SMART blocked |
| TRIM/UNMAP | Not supported (VPD LBPU=0) |

**Only way to get this data:** Shuck drives, connect via direct SATA.

---

## What We CAN Monitor

### 1. SCSI Error Counters (per device)
```bash
# Path: /sys/block/sdX/device/
ioerr_cnt    # I/O errors (retries, resets, link errors)
iotmo_cnt    # Command timeouts - CRITICAL for SMR cliff detection
iodone_cnt   # Completed I/O count
device_busy  # Current queue depth
state        # running/offline/blocked
```

### 2. Current Error Snapshot (2026-01-31)

| Device | Errors | Timeouts | I/Os Done | State |
|--------|--------|----------|-----------|-------|
| sdc | 20 | 0 | 4,646,944 | running |
| sdd | 14 | 0 | 1,752,763 | running |
| sde | 14 | 0 | 1,776,729 | running |
| sdf | 14 | 0 | 1,793,353 | running |
| sdg | 14 | 0 | 1,786,263 | running |

**Assessment:** All errors are transient USB link events. Zero timeouts = no SMR cliff observed yet.

### 3. Latency Monitoring (indirect SMR cliff detection)

```bash
# iostat - watch await and svctm columns
iostat -xd sdc sdd sde sdf sdg 5

# ZFS per-vdev latency (if zpool-latency module loaded)
zpool iostat -lv hddpool 5
```

### 4. ZFS-Level Indicators

```bash
# Pool status and errors
zpool status hddpool

# Fragmentation (high = more random writes = SMR pain)
zpool get fragmentation hddpool

# Slow I/O events
zpool events | grep -i slow
```

---

## SMR Cliff Warning Signs

The "cliff" is invisible until it happens. Watch for:

| Indicator | Normal | Cliff Approaching |
|-----------|--------|-------------------|
| `iotmo_cnt` | 0 | Incrementing |
| Write latency (await) | <100ms | >1000ms spikes |
| Write throughput | Steady | Collapses to <5MB/s |
| `zpool events` | Clean | `slow_io` events |

---

## Monitoring Script

```bash
#!/bin/bash
# smr_monitor.sh - Poll USB drive health

echo "=== $(date) ==="
echo "Device   Errors  Timeouts  State"
for d in sdc sdd sde sdf sdg; do
  p="/sys/block/$d/device"
  printf "%-8s %-7d %-9d %s\n" \
    "$d" \
    "$(($(cat $p/ioerr_cnt)))" \
    "$(($(cat $p/iotmo_cnt)))" \
    "$(cat $p/state)"
done

# ZFS fragmentation
echo ""
zpool get fragmentation,capacity hddpool | tail -2
```

---

## Conclusions

1. **Data is safe** - ZFS checksums confirm integrity, all I/O errors were recoverable
2. **Blind flight on SMR** - No visibility into actual shingled zone utilization
3. **Timeouts are the canary** - When `iotmo_cnt` starts climbing, evacuate data
4. **Latency histograms** - Best indirect indicator of SMR internal GC pressure
5. **46% capacity** - Still have headroom, but DM-SMR can cliff unpredictably

---

## Recommendations

1. **Poll `iotmo_cnt` regularly** - Alert if non-zero
2. **Keep pool <70% full** - SMR needs free space for internal GC
3. **Avoid small random writes** - Use special vdev for metadata (already configured)
4. **Consider shucking** - Direct SATA = full SMART visibility
5. **Log latency percentiles** - Detect degradation trend before cliff

---

## References

- VPD pages available: 0x00, 0x80, 0x83, 0xB0, 0xB2, 0xC1, 0xC2
- SCSI log pages (sdc only): 0x00, 0x10 (self-test only)
- USB bridge: Seagate RSS LLC (0bc2:2038)
