# Zombie Resilver with No Target: Full Analysis and Proposed Fix

## Executive Summary

After a crash during rapid mirror detach operations, orphaned DTL (Dirty Time Log) entries can remain on disk pointing to vdevs that no longer exist (now "holes"). On pool import, these orphaned entries trigger a phantom resilver that scans the entire pool with no valid target device, causing severe I/O load and potential system instability.

**Root Cause:** Incomplete transaction during vdev topology change + unclean shutdown
**Proposed Fix:** Detect orphaned DTL entries on import and offer explicit recovery option

---

## Timeline of Events (Real Case Study)

### Phase 1: Mirror Attach Operations
```
2026-01-09 to 2026-01-12: Multiple mirror attach operations
Device wwn-0x3e54314833464435 was partitioned and attached as mirrors:
  - part1 → attached to usb-Seagate DHQR (single-device vdev)
  - part2 → attached to nvme1n1p1 (special vdev)
  - part3 → attached to wwn-part6 (special vdev)
  - part4 → attached to nvme-SN770 (special vdev)
  - part5 → attached to sda5 (single-device vdev)
```

### Phase 2: Mass Detach (The Trigger)
```
2026-01-26.19:29:08-09: Mass detach (5 detaches in ~1 second)
  [txg:2675586] detach wwn-...-part1
  [txg:2675593] detach wwn-...-part2  (+7 txg)
  [txg:2675600] detach wwn-...-part3  (+7 txg)
  [txg:2675607] detach wwn-...-part4  (+7 txg)
  [txg:2675614] detach wwn-...-part5  (+7 txg)
```

### Phase 3: The Crash
Between the detach operations and the next clean pool export, the system experienced issues:
- USB drive disconnects (common with this hardware under I/O load)
- Kernel deadlock or hang
- User forced to `sysrq-b` (immediate reboot)

**Critical:** The TXG containing DTL cleanup operations never committed to disk.

### Phase 4: Pool Import Failure
```
2026-01-29: Attempted pool import
- Import fails validation (spa_load_verify_data, spa_load_verify_metadata)
- Pool state inconsistent: vdev tree shows holes, but DTL entries still reference old devices
```

### Phase 5: Workaround (Disable Validation)
User had to add to `/etc/modprobe.d/zfs.conf`:
```
# Required to import pool with inconsistent DTL state
options zfs spa_load_verify_data=0
options zfs spa_load_verify_metadata=0
```

### Phase 6: Zombie Resilver
After import with disabled validation:
```
scan: resilver in progress since Thu Jan 29 06:21:39 2026
    0B / 13.8T scanned, 0B / 13.8T issued
    0B resilvered, 0.00% done, no estimated completion time
```

**Symptoms:**
- `zpool status` shows "resilver in progress"
- NO device marked with "(resilvering)" status
- Resilver hammers ALL devices in the pool
- `zpool scrub -s` returns EBUSY (can't cancel resilver)
- System becomes unstable under the I/O load

---

## Technical Deep Dive

### How Detach Normally Works

In `spa_vdev_detach()` (spa.c:7816):

1. Validate detach is allowed
2. Remove vdev from tree, compact children
3. If mirror now has single child, collapse mirror structure
4. Mark detached vdev's DTL for cleanup:
   ```c
   vd->vdev_detached = B_TRUE;
   vdev_dirty(tvd, VDD_DTL, vd, txg);
   ```
5. In `vdev_dtl_sync()` (vdev.c:3498), when `vd->vdev_detached`:
   ```c
   space_map_free(vd->vdev_dtl_sm, tx);
   space_map_close(vd->vdev_dtl_sm);
   vd->vdev_dtl_sm = NULL;
   ```

### What Happens on Crash

If the system crashes between step 4 and the TXG commit:
- Vdev tree changes ARE persisted (holes created)
- DTL cleanup is NOT persisted (entries remain on disk)
- Result: Orphaned DTL entries pointing to non-existent vdevs

### Why Resilver Triggers

On pool import, `spa_load()` calls:
```c
if (!dsl_scan_resilvering(spa->spa_dsl_pool) &&
    vdev_resilver_needed(spa->spa_root_vdev, NULL, NULL)) {
    spa_async_request(spa, SPA_ASYNC_RESILVER);
}
```

`vdev_resilver_needed()` (vdev.c:3617) checks:
```c
if (!zfs_range_tree_is_empty(vd->vdev_dtl[DTL_MISSING]) &&
    vdev_writeable(vd)) {
    needed = B_TRUE;
}
```

**The Bug:** While `vdev_writeable()` correctly returns FALSE for holes (they're "dead" and not "concrete"), the orphaned DTL entries may affect the parent vdev's DTL aggregation in `vdev_dtl_reassess()`, or there may be edge cases where the check doesn't fully protect against holes with DTL entries.

### Evidence from zdb

Pool has 6 "hole" vdevs (children 2, 5, 7, 9, 10, 11) all showing DTL state:
```
hddpool [DTL-required]
    draid [DTL-required]
        usb-Seagate FBP5 [DTL-expendable]
        usb-Seagate FBQC [DTL-expendable]
        usb-Seagate FC6F [DTL-expendable]
        usb-Seagate FC7Z [DTL-expendable]
    wwn-...-part6 [DTL-required]
    hole [DTL-required]    ← Orphaned DTL
    nvme1-part1 [DTL-required]
    usb-DHQR [DTL-required]
    hole [DTL-required]    ← Orphaned DTL
    wwn-...-part5 [DTL-required]
    hole [DTL-required]    ← Orphaned DTL
    nvme0-part1 [DTL-required]
    hole [DTL-required]    ← Orphaned DTL
    hole [DTL-required]    ← Orphaned DTL
    hole [DTL-required]    ← Orphaned DTL
    wwn-...-part4 [DTL-required]
```

---

## Impact

### Immediate Effects
- Pool difficult/impossible to use due to constant resilver activity
- Resilver hammers all devices causing high I/O load
- Potential thermal issues on drives
- USB device disconnects (for USB-attached drives)
- System hangs/deadlocks requiring sysrq-b

### Required Workarounds
User must freeze resilver with kernel parameters:
```
zfs_scan_suspend_progress=1
zfs_vdev_scrub_max_active=0
```

And live with permanent "resilver in progress" status, or let it run for hours (it eventually completes by brute force scanning everything).

---

## Proposed Fix

### Design Principles

1. **No silent healing** - User should be aware of the recovery action
2. **Explicit opt-in** - Offer CLI option for recovery, don't assume
3. **Safe default** - Fail import with clear error message by default
4. **Logged action** - Any recovery action should be logged to pool history

### Implementation Options

#### Option A: Warn and Continue (Recommended for non-critical)
```c
// In vdev_dtl_load() or spa_load():
if ((vd->vdev_ops == &vdev_hole_ops ||
     vd->vdev_ops == &vdev_missing_ops) &&
    vd->vdev_dtl_object != 0) {

    zfs_dbgmsg("pool %s: clearing orphaned DTL on %s vdev id %llu",
        spa_name(spa), vd->vdev_ops->vdev_op_type, vd->vdev_id);

    cmn_err(CE_WARN, "pool '%s': orphaned DTL entries found on "
        "%s vdev (id=%llu), likely from crash during detach. "
        "Clearing stale entries.",
        spa_name(spa), vd->vdev_ops->vdev_op_type, vd->vdev_id);

    // Clear the orphaned DTL
    vd->vdev_dtl_object = 0;
    // Mark config dirty so cleanup persists
    vdev_config_dirty(vd->vdev_top);
}
```

#### Option B: Fail with Recovery Option (Recommended for critical)
```c
// New pool import flag: ZFS_IMPORT_HEAL_ORPHANED_DTL

// In spa_load():
if (orphaned_dtl_detected &&
    !(spa->spa_import_flags & ZFS_IMPORT_HEAL_ORPHANED_DTL)) {

    spa_load_failed(spa, "orphaned DTL entries detected on hole/missing "
        "vdevs, likely from crash during topology change. "
        "Re-import with 'zpool import -o heal_orphaned_dtl=on %s' "
        "to clear stale entries and recover.",
        spa_name(spa));
    return (SET_ERROR(EINVAL));
}
```

CLI usage:
```bash
# Normal import fails with message
$ zpool import hddpool
cannot import 'hddpool': orphaned DTL entries detected...
Re-import with: zpool import -o heal_orphaned_dtl=on hddpool

# User explicitly opts in to healing
$ zpool import -o heal_orphaned_dtl=on hddpool
pool 'hddpool': cleared orphaned DTL entries from 6 hole vdevs
```

### Recommended Approach: Hybrid

1. **During import validation:** Detect orphaned DTL entries
2. **Log warning:** Always log to dmesg/syslog
3. **If pool is read-only import:** Warn and continue (no healing possible anyway)
4. **If pool is read-write import:**
   - With `heal_orphaned_dtl=on`: Warn, heal, continue
   - Without flag: Fail with helpful error message

---

## Files to Modify

1. **`module/zfs/vdev.c`**
   - `vdev_dtl_load()`: Add orphaned DTL detection for holes/missing
   - New helper: `vdev_dtl_is_orphaned()`

2. **`module/zfs/spa.c`**
   - `spa_load()`: Orchestrate detection and recovery
   - `spa_import()`: Handle new import flag

3. **`include/sys/fs/zfs.h`**
   - New import flag: `ZFS_IMPORT_HEAL_ORPHANED_DTL`

4. **`lib/libzfs/libzfs_pool.c`**
   - `zpool_import()`: Parse new option

5. **`cmd/zpool/zpool_main.c`**
   - `zpool_do_import()`: Add `-o heal_orphaned_dtl` option

---

## Testing Plan

1. **Unit test:** Create pool, attach mirror, detach, simulate crash by killing zfs module mid-txg
2. **Verify:** Pool import fails without flag
3. **Verify:** Pool import succeeds with healing flag
4. **Verify:** No phantom resilver after healing
5. **Regression:** Normal detach operations still work

---

## References

- ZFS source: `module/zfs/vdev.c`, `module/zfs/spa.c`
- DTL documentation: ZFS on Linux wiki
- Pool import: `spa_load()` and related functions
- Resilver triggering: `vdev_resilver_needed()`, `SPA_ASYNC_RESILVER`

---

## Appendix: User's Defensive Configuration

The user's `/etc/modprobe.d/zfs.conf` showing the extreme measures needed:

```bash
# Think about 9999 times before changing this section. there is a faulty drive.
# resuming scrub/resilver scan/issue can trigger the usual spiral:
# Even 1 throttled I/O to that disk could trigger disconnect + pool hangs +
# D-state + forced reboot + Storj down.

options zfs zfs_scan_suspend_progress=1

# and this is so hddpool import works; otherwise only readonly hddpool import works.
options zfs spa_load_verify_data=0
options zfs spa_load_verify_metadata=0

# Resilver throttling (added for stable crawl state + they also dictate the
# txg timeout in this current state, so set to 60s like the txg timeout itself below)
options zfs zfs_resilver_min_time_ms=60000
options zfs zfs_scrub_min_time_ms=60000
options zfs zfs_scan_vdev_limit=2621444
options zfs zfs_no_scrub_prefetch=1

# note! currently seems that the resilver_min_time and perhaps scrub_min_time
# are actually dictating this txg timeout instead, in this resilver-paused state
options zfs zfs_txg_timeout=60
```

---

*Analysis completed: 2026-02-01*
*With assistance from Claude Code (Anthropic)*
