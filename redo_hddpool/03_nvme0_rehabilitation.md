# nvme0 Rehabilitation and Mirror Qualification

## Background

nvme0 (WD BLACK SN770 2TB) has behavioral issues that must be resolved before it can serve as a mirror partner for the special vdev.

**Current State:**
- 18.6G accidental data vdev (nvme0n1p4) - needs evacuation first
- 87% capacity used → SLC cache severely limited
- Thermal history: 54min at warning temp, 15min at critical temp
- 45 unsafe shutdowns
- Latency profile resembles SMR drives, not NVMe (avg 5ms, max 29s observed)
- No media errors, but firmware behavior is pathological

**The Mirror Write Problem:**
```
sync write → ZFS → mirror vdev → nvme0 + nvme1
                                      ↓
                          wait for BOTH to ack
                                      ↓
                              write complete
```
A 2-second stall on nvme0 = 2-second stall for that metadata transaction. The special vdev handles metadata - this is unacceptable.

---

## Phase 1: Data Evacuation

Before any rehabilitation, evacuate the ~18.6G from nvme0n1p4.

```bash
# Check what's on the accidental vdev
zpool status hddpool | grep -A5 nvme0

# The data must be migrated off before we can wipe the drive
# This happens as part of the main hddpool migration (see 01_migration_plan.md)
```

**Do not proceed until hddpool migration is complete and nvme0 is no longer part of any pool.**

---

## Phase 2: Full Drive Reset

### Option A: blkdiscard (Preferred)

Sends TRIM/DEALLOCATE to every LBA. The drive knows all data is gone and can reclaim SLC cache.

```bash
# Verify nvme0 is not in use
lsblk /dev/nvme0n1
zpool status  # Should not show nvme0 anywhere

# Full discard - this takes a while on 2TB
time blkdiscard -v /dev/nvme0n1

# Verify with SMART
nvme smart-log /dev/nvme0
```

### Option B: nvme format (More Aggressive)

Resets the FTL mapping table entirely. Closest to factory reset.

```bash
# Check supported format options
nvme id-ns /dev/nvme0n1 -H | grep -A10 "LBA Format"

# Format with secure erase (if supported)
# WARNING: This is destructive and may take several minutes
nvme format /dev/nvme0 -s 1 -n 1

# Or just user data erase
nvme format /dev/nvme0 -s 0 -n 1
```

### Option C: Combination

```bash
# 1. wipefs to clear signatures (cosmetic, doesn't help SLC)
wipefs -a /dev/nvme0n1

# 2. blkdiscard for SLC reclamation
blkdiscard -v /dev/nvme0n1

# 3. Let drive idle for 30+ minutes for background GC
sleep 1800

# 4. Check SMART for temperature normalization
nvme smart-log /dev/nvme0
```

---

## Phase 3: Rest Period

After discard/format, let the drive sit idle for at least 30 minutes. This allows:
- Background garbage collection to complete
- SLC cache reorganization
- Firmware state to stabilize
- Temperature to normalize

```bash
# Monitor temperature during rest
watch -n 10 'nvme smart-log /dev/nvme0 | grep -E "temperature|Thermal"'
```

---

## Phase 4: Qualification Testing

### Test 1: Sync Write Latency (Critical)

This is the test that matters for mirror duty. Simulates ZFS sync metadata writes.

```bash
# Run for 5 minutes, measure tail latencies
fio --name=sync_write_test \
    --filename=/dev/nvme0n1 \
    --ioengine=libaio \
    --direct=1 \
    --sync=1 \
    --rw=randwrite \
    --bs=4k \
    --iodepth=1 \
    --runtime=300 \
    --time_based \
    --lat_percentiles=1 \
    --percentile_list=50:90:99:99.9:99.99:99.999
```

**Pass Criteria:**
| Percentile | Target | Acceptable | Fail |
|------------|--------|------------|------|
| p50 | < 100us | < 500us | > 1ms |
| p99 | < 1ms | < 5ms | > 10ms |
| p99.9 | < 5ms | < 20ms | > 50ms |
| p99.99 | < 10ms | < 50ms | > 100ms |
| p99.999 | < 50ms | < 200ms | > 500ms |
| max | < 100ms | < 500ms | > 1s |

If p99.999 or max exceeds 500ms, the drive is **not fit for mirror duty**.

### Test 2: Sustained Write (Temperature Check)

Verify the drive doesn't thermally throttle under load.

```bash
# Sustained sequential write for 10 minutes
# Monitor temperature in another terminal
fio --name=sustained_write \
    --filename=/dev/nvme0n1 \
    --ioengine=libaio \
    --direct=1 \
    --rw=write \
    --bs=128k \
    --iodepth=32 \
    --runtime=600 \
    --time_based &

# In another terminal
watch -n 5 'nvme smart-log /dev/nvme0 | grep -E "temperature|Thermal"'
```

**Pass Criteria:**
- Temperature stays below 70C (warning threshold)
- No thermal throttle events during test
- Write speed remains consistent (no sudden drops)

### Test 3: Mixed Random I/O

Simulates more realistic workload.

```bash
fio --name=mixed_random \
    --filename=/dev/nvme0n1 \
    --ioengine=libaio \
    --direct=1 \
    --rw=randrw \
    --rwmixread=70 \
    --bs=4k \
    --iodepth=16 \
    --runtime=300 \
    --time_based \
    --lat_percentiles=1 \
    --percentile_list=50:90:99:99.9:99.99:99.999
```

### Test 4: Compare Against nvme1 (Baseline)

Run the same sync write test on nvme1 for comparison.

```bash
# Same test on the healthy drive
fio --name=sync_write_baseline \
    --filename=/dev/nvme1n1 \
    --ioengine=libaio \
    --direct=1 \
    --sync=1 \
    --rw=randwrite \
    --bs=4k \
    --iodepth=1 \
    --runtime=300 \
    --time_based \
    --lat_percentiles=1 \
    --percentile_list=50:90:99:99.9:99.99:99.999
```

nvme0 should be within 2-3x of nvme1's latencies across all percentiles. If nvme0 is 10x+ worse at p99.999, it's still damaged.

---

## Phase 5: Decision

### If Tests Pass

nvme0 is rehabilitated and can serve as mirror partner:

```bash
# Create mirrored special vdev (during new hddpool creation)
zpool create ... \
    special mirror \
        /dev/disk/by-id/nvme-WD_BLACK_SN770_2TB_244932Z481591-part1 \
        /dev/disk/by-id/nvme-WD_BLACK_SN770_2TB_245077404326-part1 \
    ...
```

### If Tests Fail

Options for a drive unfit for mirror duty:

1. **L2ARC**: Read cache only, stalls don't block writes
   ```bash
   zpool add hddpool cache /dev/nvme0n1p1
   ```

2. **Scratch/temp storage**: Non-critical data only

3. **Retirement**: The drive has been through too much

---

## SMART Metrics to Monitor

Before and after rehabilitation, record these:

```bash
nvme smart-log /dev/nvme0 | tee nvme0_smart_$(date +%Y%m%d_%H%M).txt
```

Key metrics:
- `Warning Temperature Time` - should not increase during testing
- `Critical Composite Temperature Time` - should stay at 15min (existing damage)
- `Thermal Management T1/T2 Trans Count` - throttling events
- `percentage_used` - wear indicator (currently 0%, good)
- `media_errors` - should stay 0

---

## Timeline

1. **After hddpool migration complete**: Begin Phase 1-2
2. **30-60 minutes**: Phase 3 rest period
3. **~30 minutes**: Phase 4 testing
4. **Immediate**: Phase 5 decision

Total rehabilitation time: ~2 hours (excluding migration)

---

*Document created: 2026-02-01*
*Status: Ready to execute after hddpool migration*
*Referenced by: 01_migration_plan.md, 02_topology_options.md*
