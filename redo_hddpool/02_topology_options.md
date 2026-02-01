# hddpool Topology Options

## Option 1: Standard Migration (Summary)

**The safe path when current pool is too fragile to repair in-place.**

```
1. Buy 24T drive (30-day return)
2. zfs send hddpool → temppool (throttled, ~7-30 days)
3. Destroy hddpool
4. Recreate hddpool with clean topology
5. zfs send temppool → hddpool (throttled, ~7-30 days)
6. Return 24T drive
```

**Benefits:**
- Eliminates zombie resilver completely
- Removes accidental vdev cleanly
- Fresh pool with no holes/baggage
- Can redesign topology

**Costs:**
- Need temporary 24T drive (~$300-400, returnable)
- Time: 2-8 weeks depending on throttle rate
- Risk during transfer (mitigated by throttling)

---

## Option 2: Exotic Partitioned dRAID Topology

**Use partitions instead of whole disks to maximize redundancy.**

### Current Hardware

| Device | Size | Current Use |
|--------|------|-------------|
| USB Seagate FBP5 | 6TB | draid1 member |
| USB Seagate FBQC | 6TB | draid1 member |
| USB Seagate FC6F | 6TB | draid1 member |
| USB Seagate FC7Z | 6TB | draid1 member |
| USB Seagate DHQR | 4TB | Single stripe (NO redundancy!) |

**Problem with current topology:**
- The 4TB USB (DHQR) is a single-device vdev = single point of failure
- If DHQR dies, entire pool is lost

### Exotic Solution: Partitioned dRAID

Instead of giving whole disks to ZFS, partition them strategically:

```
6TB drives (4x):
├── Partition 1: 4TB (decimal) → dRAID-A
└── Partition 2: 2TB (decimal) → dRAID-B

4TB drive (1x):
└── Partition 1: 4TB (decimal) → dRAID-A
```

**Resulting topology:**

```
hddpool
├── dRAID-A: draid1 with 5x 4TB partitions (from all 5 USB drives)
│   ├── FBP5-part1 (4TB)
│   ├── FBQC-part1 (4TB)
│   ├── FC6F-part1 (4TB)
│   ├── FC7Z-part1 (4TB)
│   └── DHQR-part1 (4TB)  ← Now protected by dRAID!
│
├── dRAID-B: draid1 with 4x 2TB partitions (from 6TB drives only)
│   ├── FBP5-part2 (2TB)
│   ├── FBQC-part2 (2TB)
│   ├── FC6F-part2 (2TB)
│   └── FC7Z-part2 (2TB)
│
├── special (unchanged)
└── logs (unchanged)
```

### Capacity Analysis

**Current (with single-device DHQR):**
```
draid1 (4x 6TB): ~18TB usable (with parity overhead)
single DHQR:     ~4TB usable (NO redundancy)
Total:           ~22TB usable
Risk:            DHQR failure = pool loss
```

**Exotic (all partitioned):**
```
dRAID-A (5x 4TB): ~16TB usable (better parity distribution)
dRAID-B (4x 2TB): ~6TB usable
Total:            ~22TB usable (same!)
Risk:             Any single drive failure = survivable
```

### Implementation

```bash
# 1. Partition the 6TB drives (example for one drive)
# Create GPT, then:
#   Partition 1: 4TB (4000000000000 bytes = 3.64 TiB)
#   Partition 2: 2TB (2000000000000 bytes = 1.82 TiB)
gdisk /dev/disk/by-id/usb-Seagate_Expansion_HDD_..._FBP5-0:0

# 2. Partition the 4TB drive
# Create GPT, then:
#   Partition 1: 4TB (entire usable space)
gdisk /dev/disk/by-id/usb-Seagate_Expansion_HDD_..._DHQR-0:0

# 3. Create pool with two dRAID vdevs
zpool create -o ashift=12 hddpool \
    draid1:3d:5c:0s \
        usb-Seagate_..._FBP5-part1 \
        usb-Seagate_..._FBQC-part1 \
        usb-Seagate_..._FC6F-part1 \
        usb-Seagate_..._FC7Z-part1 \
        usb-Seagate_..._DHQR-part1 \
    draid1:2d:4c:0s \
        usb-Seagate_..._FBP5-part2 \
        usb-Seagate_..._FBQC-part2 \
        usb-Seagate_..._FC6F-part2 \
        usb-Seagate_..._FC7Z-part2 \
    special \
        wwn-...-part6 \
        nvme-...-part5 \
    log \
        wwn-...-part4
```

### Pros and Cons

**Pros:**
- ALL data protected by dRAID redundancy
- No single point of failure from DHQR
- Same total capacity
- Can lose any single drive and survive

**Cons:**
- More complex topology
- Two dRAID vdevs = data striped across both (both must be healthy)
- Partition alignment must be careful
- USB drives are still USB (latency, disconnect issues)
- dRAID with partitions is less common/tested

### Open Questions (Evaluate Before Implementing)

1. Does ZFS handle partitioned dRAID as well as whole-disk?
2. What's the performance impact of two smaller dRAIDs vs one larger?
3. Should the 4TB partition sizes be slightly smaller to account for drive variance?

**Q4 ANSWERED:** For dRAID-B with 4 children, use `draid1:2d:4c:0s`:
- groupwidth = 2 data + 1 parity = 3
- Each stripe uses 3 of 4 devices, rotates via permutations
- ngroups = LCM(3, 4) = 12 permutation groups
- Valid per ZFS source (children >= ndata + nparity + nspares → 4 >= 2+1+0 ✓)

### Alternative: Simple 5-Drive dRAID1

If the exotic option feels too risky, a simpler approach:

```bash
# Just use all 5 USB drives as whole disks in one draid1
# The 4TB drive limits all drives to 4TB contribution
# ~2TB wasted on each 6TB drive, but simpler and safer

zpool create -o ashift=12 hddpool \
    draid1:4d:5c:0s \
        usb-Seagate_..._FBP5 \
        usb-Seagate_..._FBQC \
        usb-Seagate_..._FC6F \
        usb-Seagate_..._FC7Z \
        usb-Seagate_..._DHQR \
    special ... \
    log ...
```

This wastes ~8TB but gives clean redundancy with zero partition complexity.

---

## Option 3: Exotic Pyramid - Three dRAIDs with sdb

**Maximum capacity utilization with full redundancy, incorporating the idle 1TB sdb drive.**

### Hardware Inventory

| Device | Size | Partitioning |
|--------|------|--------------|
| USB Seagate FBP5 | 6TB | 1TB + 3TB + 2TB |
| USB Seagate FBQC | 6TB | 1TB + 3TB + 2TB |
| USB Seagate FC6F | 6TB | 1TB + 3TB + 2TB |
| USB Seagate FC7Z | 6TB | 1TB + 3TB + 2TB |
| USB Seagate DHQR | 4TB | 1TB + 3TB |
| SATA sdb | 1TB | 1TB |
| **Total Raw** | **29TB** | |

### The Pyramid Topology

```
hddpool
│
├── dRAID-A: 6 children × 1TB = 6TB raw (draid1:4d:6c:0s)
│   ├── FBP5-part1 (1TB)
│   ├── FBQC-part1 (1TB)
│   ├── FC6F-part1 (1TB)
│   ├── FC7Z-part1 (1TB)
│   ├── DHQR-part1 (1TB)
│   └── sdb1 (1TB)        ← sdb joins the party!
│
├── dRAID-B: 5 children × 3TB = 15TB raw (draid1:3d:5c:0s)
│   ├── FBP5-part2 (3TB)
│   ├── FBQC-part2 (3TB)
│   ├── FC6F-part2 (3TB)
│   ├── FC7Z-part2 (3TB)
│   └── DHQR-part2 (3TB)  ← DHQR fully utilized!
│
├── dRAID-C: 4 children × 2TB = 8TB raw (draid1:2d:4c:0s)
│   ├── FBP5-part3 (2TB)
│   ├── FBQC-part3 (2TB)
│   ├── FC6F-part3 (2TB)
│   └── FC7Z-part3 (2TB)  ← Only 6TB drives have remainder
│
├── special (unchanged)
└── logs (unchanged)
```

### Partition Layout Per Drive

```
6TB USB drives (4x):
┌─────────────────────────────────────────────────────────┐
│ Part1: 1TB │    Part2: 3TB        │    Part3: 2TB      │
│  → dRAID-A │     → dRAID-B        │     → dRAID-C      │
└─────────────────────────────────────────────────────────┘
              1TB        4TB                   6TB

4TB USB drive (DHQR):
┌─────────────────────────────────────────┐
│ Part1: 1TB │       Part2: 3TB           │
│  → dRAID-A │        → dRAID-B           │
└─────────────────────────────────────────┘
              1TB                  4TB

1TB SATA drive (sdb):
┌───────────┐
│ Part1: 1TB│
│  → dRAID-A│
└───────────┘
            1TB
```

### Capacity Analysis

**With typical ndata configurations:**

| dRAID | Children | Config | Raw | Efficiency | Usable |
|-------|----------|--------|-----|------------|--------|
| A | 6 | draid1:4d:6c:0s | 6TB | 4/5 = 80% | ~4.8TB |
| B | 5 | draid1:3d:5c:0s | 15TB | 3/4 = 75% | ~11.25TB |
| C | 4 | draid1:2d:4c:0s | 8TB | 2/3 = 67% | ~5.33TB |
| **Total** | | | **29TB** | | **~21.4TB** |

**With maximum ndata (higher efficiency, larger stripes):**

| dRAID | Children | Config | Raw | Efficiency | Usable |
|-------|----------|--------|-----|------------|--------|
| A | 6 | draid1:5d:6c:0s | 6TB | 5/6 = 83% | ~5TB |
| B | 5 | draid1:4d:5c:0s | 15TB | 4/5 = 80% | ~12TB |
| C | 4 | draid1:3d:4c:0s | 8TB | 3/4 = 75% | ~6TB |
| **Total** | | | **29TB** | | **~23TB** |

### ZFS Validation (from source code analysis)

All three dRAID configurations are valid per `vdev_draid.c`:

```
Constraint: children >= ndata + nparity + nspares

dRAID-A: 6 >= 4 + 1 + 0 = 5 ✓  (or 6 >= 5 + 1 + 0 = 6 ✓)
dRAID-B: 5 >= 3 + 1 + 0 = 4 ✓  (or 5 >= 4 + 1 + 0 = 5 ✓)
dRAID-C: 4 >= 2 + 1 + 0 = 3 ✓  (or 4 >= 3 + 1 + 0 = 4 ✓)
```

Permutation distribution verified:
- dRAID-A (6c, 4d): ngroups = LCM(5, 6) = 30 → data distributed across all 6
- dRAID-B (5c, 3d): ngroups = LCM(4, 5) = 20 → data distributed across all 5
- dRAID-C (4c, 2d): ngroups = LCM(3, 4) = 12 → data distributed across all 4

### Implementation

```bash
# 1. Partition 6TB drives (repeat for FBP5, FBQC, FC6F, FC7Z)
# Sizes in bytes: 1TB=1000000000000, 3TB=3000000000000, 2TB=2000000000000
gdisk /dev/disk/by-id/usb-Seagate_..._FBP5-0:0
# Create: part1=1TB, part2=3TB, part3=2TB

# 2. Partition 4TB drive (DHQR)
gdisk /dev/disk/by-id/usb-Seagate_..._DHQR-0:0
# Create: part1=1TB, part2=3TB

# 3. Partition 1TB drive (sdb)
gdisk /dev/sdb
# Create: part1=1TB (all of it)

# 4. Create pool with pyramid of dRAIDs
zpool create -o ashift=12 hddpool \
    draid1:4d:6c:0s \
        usb-Seagate_..._FBP5-part1 \
        usb-Seagate_..._FBQC-part1 \
        usb-Seagate_..._FC6F-part1 \
        usb-Seagate_..._FC7Z-part1 \
        usb-Seagate_..._DHQR-part1 \
        /dev/disk/by-id/ata-...-sdb-part1 \
    draid1:3d:5c:0s \
        usb-Seagate_..._FBP5-part2 \
        usb-Seagate_..._FBQC-part2 \
        usb-Seagate_..._FC6F-part2 \
        usb-Seagate_..._FC7Z-part2 \
        usb-Seagate_..._DHQR-part2 \
    draid1:2d:4c:0s \
        usb-Seagate_..._FBP5-part3 \
        usb-Seagate_..._FBQC-part3 \
        usb-Seagate_..._FC6F-part3 \
        usb-Seagate_..._FC7Z-part3 \
    special \
        wwn-...-part6 \
        nvme-...-part5 \
    log \
        wwn-...-part4
```

### Pros and Cons

**Pros:**
- Maximum capacity utilization (~23TB from 29TB raw)
- ALL drives protected by dRAID (no single points of failure)
- sdb utilized (was sitting idle)
- DHQR fully protected (was a SPOF before)
- Any single drive failure is survivable

**Cons:**
- Most complex topology (3 dRAID vdevs)
- Three vdevs = data striped across all three (all must be healthy)
- Mixed drive types (USB + SATA) in dRAID-A
- sdb is faster than USB drives (potential imbalance in dRAID-A)
- More partitions = more things to get wrong
- If sdb fails, dRAID-A loses redundancy (only 5 children left, still functional but degraded)

### Risk Assessment

```
Drive failure scenarios:

1. Any single USB 6TB drive fails:
   - dRAID-A: degraded (5/6 children) ✓ survivable
   - dRAID-B: degraded (4/5 children) ✓ survivable
   - dRAID-C: degraded (3/4 children) ✓ survivable
   - Pool: DEGRADED but ONLINE

2. DHQR (4TB USB) fails:
   - dRAID-A: degraded (5/6 children) ✓ survivable
   - dRAID-B: degraded (4/5 children) ✓ survivable
   - dRAID-C: unaffected
   - Pool: DEGRADED but ONLINE

3. sdb (1TB SATA) fails:
   - dRAID-A: degraded (5/6 children) ✓ survivable
   - dRAID-B: unaffected
   - dRAID-C: unaffected
   - Pool: DEGRADED but ONLINE

4. Two drives fail (same dRAID):
   - Pool: FAULTED (data loss)
```

---

## Special Vdev: Mirrored NVMe

**Goal:** Mirror nvme0 + nvme1 for the special vdev (metadata).

**The Problem:**
- nvme1: Healthy (avg 365us, max 119ms) but has PCIe correctable errors
- nvme0: Pathological (avg 5ms, max 29s observed) due to thermal history + 87% fill

**Mirror Write Constraint:**
```
sync write → mirror → wait for BOTH nvme0 AND nvme1 to ack
```
If nvme0 stalls for 2s, metadata writes stall for 2s. Unacceptable.

**Solution:** Rehabilitate nvme0 before mirroring.

See [03_nvme0_rehabilitation.md](03_nvme0_rehabilitation.md) for:
- blkdiscard to reclaim SLC cache
- fio sync write testing with tail latency targets
- Temperature monitoring under load
- Pass/fail criteria for mirror duty

**If nvme0 passes:** Mirror special vdev
```bash
special mirror \
    nvme-WD_BLACK_SN770_2TB_244932Z481591-part1 \
    nvme-WD_BLACK_SN770_2TB_245077404326-part1
```

**If nvme0 fails:** Demote to L2ARC (read cache, stalls don't block writes)

---

## Recommendation

1. **If time/money constrained**: Go with Option 1 (standard migration) + simple 5-drive dRAID
2. **If want maximum capacity with moderate complexity**: Option 2 (two dRAIDs)
3. **If want absolute maximum utilization and have sdb available**: Option 3 (pyramid)
4. **Either way**: Eliminate the single-device DHQR as a standalone vdev
5. **Special vdev**: Mirror nvme0+nvme1 only after nvme0 rehabilitation (see 03_nvme0_rehabilitation.md)

---

*Document created: 2026-02-01*
*Updated: 2026-02-01 - Added Option 3 (Exotic Pyramid)*
*Status: Planning/Evaluation*
