# hddpool Migration Plan

## Background

The current hddpool has critical issues that cannot be safely resolved in-place:

1. **Zombie Resilver**: A resilver with no target device, state=DSS_SCANNING, scope=13.8T
   - Cannot be canceled (`zpool scrub -s` returns EBUSY)
   - When unfrozen, hammers all 9 drives causing deadlocks
   - Currently frozen via `zfs_scan_suspend_progress=1` and `zfs_vdev_scrub_max_active=0`

2. **Accidental Vdev**: `nvme0n1p1` (18.6G) was meant to be SLOG but added as normal data vdev
   - Cannot be removed (`zpool remove` blocked by draid presence in pool)
   - Cannot be replaced without interacting with zombie resilver
   - Single point of failure for entire pool

3. **Topology Issues**: 6 "hole" vdevs from previous removals, mixed ashift (9 and 12)

## Current Pool Topology

```
hddpool (27.3T total, 12.8T used)
├── draid1:3d:4c:0s-0 (21.8T) - 4x USB Seagate 6TB
│   ├── usb-Seagate_HDD_...FBP5
│   ├── usb-Seagate_HDD_...FBQC
│   ├── usb-Seagate_HDD_...FC6F
│   └── usb-Seagate_HDD_...FC7Z
├── usb-Seagate_HDD_...DHQR (3.64T) - single USB, NO redundancy
├── nvme0n1p1 (18.6G) - ACCIDENTAL, single device, NO redundancy
├── special
│   ├── wwn-...-part6 (41.9G)
│   ├── nvme1n1p1 (1.82T)
│   └── wwn-...-part5 (21G)
└── logs
    └── wwn-...-part4 (11G)
```

## Migration Plan

### Prerequisites

- [ ] 24TB external drive (non-Amazon, 30-day return policy)
- [ ] Stable power (UPS recommended)
- [ ] Low system load during transfers
- [ ] Monitoring for temperature/latency spikes

### Phase 1: Backup to Temporary Drive

```bash
# Create temporary pool on 24T drive
zpool create -o ashift=12 temppool /dev/sdX

# Transfer with throttling (adjust rate based on stability)
# At 5MB/s: ~30 days for 12.8TB
# At 20MB/s: ~7.5 days
# At 50MB/s: ~3 days
zfs snapshot -r hddpool@migrate
zfs send -Rv hddpool@migrate | pv -L 5M | zfs receive -Fvu temppool
```

### Phase 2: Destroy and Recreate hddpool

```bash
# Export old pool
zpool export hddpool

# Destroy old pool (POINT OF NO RETURN)
zpool destroy hddpool

# Recreate with CLEAN topology (see options in 02_topology_options.md)
# Example: simple draid1 with all 5 USB drives as partitions
zpool create -o ashift=12 hddpool \
    draid1:3d:4c:0s \
        /dev/disk/by-id/usb-Seagate_..._FBP5-part1 \
        /dev/disk/by-id/usb-Seagate_..._FBQC-part1 \
        /dev/disk/by-id/usb-Seagate_..._FC6F-part1 \
        /dev/disk/by-id/usb-Seagate_..._FC7Z-part1 \
        /dev/disk/by-id/usb-Seagate_..._DHQR-part1 \
    special \
        /dev/disk/by-id/nvme-WD_BLACK_SN770_..._part6 \
        /dev/disk/by-id/nvme-WD_BLACK_SN770_..._part5 \
    log \
        /dev/disk/by-id/wwn-...-part4
```

### Phase 3: Restore from Temporary Drive

```bash
# Transfer back with throttling
zfs snapshot -r temppool@restore
zfs send -Rv temppool@restore | pv -L 5M | zfs receive -Fvu hddpool

# Verify
zpool status hddpool
zpool scrub hddpool  # Now safe! No zombie!

# Cleanup
zpool export temppool
zpool destroy temppool
# Return 24T drive
```

## Time Estimates

| Throttle Rate | Per Direction | Round Trip |
|---------------|---------------|------------|
| 5 MB/s        | ~30 days      | ~60 days   |
| 10 MB/s       | ~15 days      | ~30 days   |
| 20 MB/s       | ~7.5 days     | ~15 days   |
| 50 MB/s       | ~3 days       | ~6 days    |

## Risk Mitigation

1. **During send**: Source pool is read-only, should be more stable
2. **Throttling**: Use `pv -L` to limit I/O rate, reduce thermal/latency stress
3. **Monitoring**: Watch for latency spikes, temperature, dmesg errors
4. **Incremental**: If interrupted, can resume with incremental send

## Post-Migration Cleanup

1. Remove defensive settings from `/etc/modprobe.d/zfs.conf`:
   - `zfs_scan_suspend_progress=1`
   - `zfs_vdev_scrub_max_active=0`
   - `spa_load_verify_data=0`
   - `spa_load_verify_metadata=0`

2. **nvme0 Rehabilitation** (see [03_nvme0_rehabilitation.md](03_nvme0_rehabilitation.md)):
   - After migration, nvme0 is free from pool obligations
   - Full blkdiscard to reclaim SLC cache
   - Qualification testing with sync write fio
   - If passes: mirror partner for special vdev
   - If fails: demote to L2ARC or retire

## References

- Analysis date: 2026-02-01
- Pool state: zombie resilver frozen, 12.8T used
- Investigated with: zdb, zpool status, ZFS source code analysis
