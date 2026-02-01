# OpenZFS Bug Report Draft: Orphaned DTL Entries After Mass Mirror Detach

## Summary

Rapid detachment of multiple mirror members leaves orphaned DTL (Dirty Time Log) entries, causing a phantom resilver on next pool import that scans the entire pool with no target device.

## Environment

- OpenZFS version: 2.2.2+
- OS: Ubuntu Linux
- Pool configuration: dRAID1 + multiple single-device vdevs + special vdevs + log vdev

## Steps to Reproduce

1. Create a pool with multiple top-level vdevs
2. Attach mirror members to several vdevs (creating mirrors)
3. Detach all mirror members rapidly (within seconds)
4. Export and re-import the pool
5. Observe: resilver starts with no device marked as "(resilvering)"

## Actual Behavior

After pool import:
- `zpool status` shows "resilver in progress"
- No device is marked with "(resilvering)" status
- Resilver shows "0B / 13.8T scanned, 0.00% done"
- Resilver hammers ALL devices in the pool
- Multiple vdev slots are now "holes" but marked `[DTL-required]`

```
scan: resilver in progress since Thu Jan 29 06:21:39 2026
    0B / 13.8T scanned, 0B / 13.8T issued
    0B resilvered, 0.00% done, no estimated completion time
```

## Expected Behavior

- DTL entries should be cleaned up when mirror members are detached
- No resilver should start if there's nothing to resilver
- Holes should not be marked as `[DTL-required]`

## Root Cause Analysis

### Timeline of Events

```
2026-01-09 to 2026-01-12: Multiple mirror attach operations
    - wwn-0x3e54314833464435-part1 → attached to usb-Seagate DHQR
    - wwn-0x3e54314833464435-part2 → attached to nvme1n1p1
    - wwn-0x3e54314833464435-part3 → attached to wwn-part6 (special)
    - wwn-0x3e54314833464435-part4 → attached to nvme-SN770 (special)
    - wwn-0x3e54314833464435-part5 → attached to sda5

2026-01-26.19:29:08-09: Mass detach (5 detaches in 1 second!)
    [txg:2675586] detach wwn-...-part1
    [txg:2675593] detach wwn-...-part2
    [txg:2675600] detach wwn-...-part3
    [txg:2675607] detach wwn-...-part4
    [txg:2675614] detach wwn-...-part5

2026-01-29.06:15:24: Pool import
2026-01-29.06:21:39: Zombie resilver starts
```

### Evidence from zdb

Pool has 6 "hole" vdevs (children 2, 5, 7, 9, 10, 11) all marked `[DTL-required]`:

```
hddpool [DTL-required]
    draid [DTL-required]
        usb-Seagate FBP5 [DTL-expendable]
        usb-Seagate FBQC [DTL-expendable]
        usb-Seagate FC6F [DTL-expendable]
        usb-Seagate FC7Z [DTL-expendable]
    wwn-...-part6 [DTL-required]
    hole [DTL-required]    ← PROBLEM
    nvme1-part1 [DTL-required]
    usb-DHQR [DTL-required]
    hole [DTL-required]    ← PROBLEM
    wwn-...-part5 [DTL-required]
    hole [DTL-required]    ← PROBLEM
    nvme0-part1 [DTL-required]
    hole [DTL-required]    ← PROBLEM
    hole [DTL-required]    ← PROBLEM
    hole [DTL-required]    ← PROBLEM
    wwn-...-part4 [DTL-required]
```

### Code Path Analysis

From `vdev.c`:
```c
boolean_t vdev_resilver_needed(vdev_t *vd, uint64_t *minp, uint64_t *maxp)
{
    if (vd->vdev_children == 0) {
        mutex_enter(&vd->vdev_dtl_lock);
        if (!zfs_range_tree_is_empty(vd->vdev_dtl[DTL_MISSING]) &&
            vdev_writeable(vd)) {
            // This returns TRUE even for holes with orphaned DTL entries
            needed = B_TRUE;
        }
        mutex_exit(&vd->vdev_dtl_lock);
    }
    ...
}
```

From `spa.c` (pool import):
```c
if (!dsl_scan_resilvering(spa->spa_dsl_pool) &&
    vdev_resilver_needed(spa->spa_root_vdev, NULL, NULL)) {
    spa_async_request(spa, SPA_ASYNC_RESILVER);
}
```

The resilver is triggered because `vdev_resilver_needed()` returns TRUE due to orphaned DTL_MISSING entries on holes.

## Impact

- Pool becomes difficult/impossible to use due to constant resilver activity
- Resilver hammers all devices causing:
  - High I/O load
  - Potential thermal issues
  - USB device disconnects (for USB-attached drives)
  - System hangs/deadlocks
- User must freeze resilver with kernel parameters:
  ```
  zfs_scan_suspend_progress=1
  zfs_vdev_scrub_max_active=0
  ```
- `zpool scrub -s` cannot cancel (returns EBUSY because resilver in progress)

## Workaround

The only known workaround is to let the resilver complete (hours of unnecessary I/O) or freeze it with module parameters and live with the "resilver in progress" status indefinitely.

## Suggested Fix

1. During mirror detach, ensure DTL entries are properly propagated/cleaned
2. `vdev_resilver_needed()` should not return TRUE for hole vdevs
3. Consider adding a mechanism to cancel orphaned resilvers
4. Add validation during pool import to detect and clear phantom resilver states

## Additional Information

### Previous Occurrence

This issue has occurred before on this system. In previous instances, the resilver eventually completed after several hours of scanning the entire pool, which "cleared" the orphaned DTL entries through brute force.

### Pool History (relevant excerpts)

```
2026-01-09.07:11:56 [txg:2658002] vdev attach attach vdev=/dev/disk/by-id/wwn-0x3e54314833464435-part2 to vdev=/dev/nvme1n1p1
2026-01-09.19:40:28 [txg:2659330] vdev attach attach vdev=/dev/disk/by-id/wwn-0x3e54314833464435-part4 to vdev=/dev/disk/by-id/nvme-WD_BLACK_SN770_2TB_24493Z401591-part1
2026-01-10.08:40:27 [txg:2661348] vdev attach attach vdev=/dev/disk/by-id/wwn-0x3e54314833464435-part5 to vdev=/dev/sda5
2026-01-11.23:17:31 [txg:2664842] vdev attach attach vdev=/dev/disk/by-id/wwn-0x3e54314833464435-part1 to vdev=/dev/disk/by-id/usb-Seagate_Expansion_HDD_00000000NT17DHQR-0:0-part1
2026-01-26.19:29:08 [txg:2675586] detach vdev=/dev/disk/by-id/wwn-0x3e54314833464435-part1
2026-01-26.19:29:08 [txg:2675593] detach vdev=/dev/disk/by-id/wwn-0x3e54314833464435-part2
2026-01-26.19:29:08 [txg:2675600] detach vdev=/dev/disk/by-id/wwn-0x3e54314833464435-part3
2026-01-26.19:29:08 [txg:2675607] detach vdev=/dev/disk/by-id/wwn-0x3e54314833464435-part4
2026-01-26.19:29:09 [txg:2675614] detach vdev=/dev/disk/by-id/wwn-0x3e54314833464435-part5
```

### Files Available

- `hddpool_history.txt` - Full pool history
- `hddpool_history_internal.txt` - Internal pool history with txg numbers
- `dtl_state.txt` - DTL state from zdb
- `log` - Session log with zdb output

## Summary
  Key Finding:
  The zombie resilver is caused by vdev_resilver_needed() in vdev.c returning TRUE for hole vdevs with orphaned DTL_MISSING entries. The code doesn't check if the vdev is actually a valid target - it just checks if DTL_MISSING is non-empty and the vdev is "writeable"
  (holes pass this check incorrectly).


---

*Draft prepared: 2026-02-01*
*System analyzed with help from Claude Code (Anthropic)*
