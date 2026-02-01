# OpenZFS Issue: Phantom Resilver After Crash During Vdev Detach

## System Information

- **Distribution:** Ubuntu Linux
- **OpenZFS Version:** 2.2.2+
- **Kernel Version:** [your kernel version]
- **Architecture:** x86_64

## Describe the Problem

After a system crash (e.g., kernel panic, hardware hang requiring sysrq-b) that occurs during or shortly after rapid vdev detach operations, subsequent pool import triggers a phantom resilver that:

1. Shows "resilver in progress" in `zpool status`
2. Has **no device marked as "(resilvering)"**
3. Scans the entire pool (e.g., "0B / 13.8T scanned")
4. Cannot be cancelled (`zpool scrub -s` returns EBUSY)
5. Causes severe I/O load on all devices

The root cause appears to be orphaned DTL (Dirty Time Log) entries that remain on-disk pointing to vdevs that are now "holes" after the detach operation completed in the vdev tree but the DTL cleanup transaction never committed due to the crash.

## Steps to Reproduce

1. Create a pool with multiple top-level vdevs
2. Attach mirror members to several vdevs (creating mirrors)
3. Detach all mirror members rapidly (e.g., scripted loop)
4. **Before clean export:** Simulate crash (sysrq-b, power loss, or kernel panic)
5. Import the pool
6. Observe: resilver starts with no target device

## Expected Behavior

- Pool import should detect orphaned DTL entries on hole/missing vdevs
- Either: warn and offer explicit recovery option, OR
- Automatically clear orphaned DTL entries with logged warning
- No phantom resilver should be triggered

## Actual Behavior

- Pool import may fail validation (requiring `spa_load_verify_data=0`)
- Phantom resilver starts, hammering all devices
- System becomes unstable under I/O load
- Only workarounds:
  - Let resilver run for hours (brute force completion)
  - Freeze with `zfs_scan_suspend_progress=1` and live with permanent "resilver in progress"

## Evidence

### zdb Output (DTL State)
```
hddpool [DTL-required]
    draid [DTL-required]
        ...
    hole [DTL-required]    ← Orphaned DTL on hole vdev
    hole [DTL-required]    ← Orphaned DTL on hole vdev
    ...
```

### Pool History (Mass Detach)
```
2026-01-26.19:29:08 [txg:2675586] detach vdev=...part1
2026-01-26.19:29:08 [txg:2675593] detach vdev=...part2
2026-01-26.19:29:08 [txg:2675600] detach vdev=...part3
2026-01-26.19:29:08 [txg:2675607] detach vdev=...part4
2026-01-26.19:29:09 [txg:2675614] detach vdev=...part5
```
(5 detaches in ~1 second, followed by crash before clean sync)

### zpool status
```
scan: resilver in progress since Thu Jan 29 06:21:39 2026
    0B / 13.8T scanned, 0B / 13.8T issued
    0B resilvered, 0.00% done, no estimated completion time
```

## Proposed Fix

PR: https://github.com/nmarasoiu/zfs/tree/fix/orphaned-dtl-healing

The fix adds:
1. `vdev_dtl_check_orphaned()` - detect orphaned DTL on hole/missing vdevs during import
2. New import flag `ZFS_IMPORT_HEAL_ORPHANED_DTL`
3. CLI option `zpool import -o heal_orphaned_dtl=on poolname`
4. By default, import fails with helpful error directing user to healing option
5. When healing enabled, clears orphaned DTL with logged warning

This ensures administrators are explicitly aware of and consent to the recovery action rather than silent healing.

## Additional Context

- This issue has occurred multiple times on systems with USB-attached drives that are prone to disconnects
- The crash typically occurs due to I/O timeouts cascading into kernel hangs
- Previous occurrences "self-healed" by letting the phantom resilver run for hours
- The resilver completes because it eventually scans all data, but this is wasteful and stressful on hardware

## Workarounds

Current workarounds require kernel module parameters:
```bash
# Allow import despite inconsistent state
options zfs spa_load_verify_data=0
options zfs spa_load_verify_metadata=0

# Freeze the phantom resilver
options zfs zfs_scan_suspend_progress=1
options zfs zfs_vdev_scrub_max_active=0
```

---

*Issue prepared with analysis assistance from Claude Code (Anthropic)*
