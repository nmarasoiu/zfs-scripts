# Test Case Outline: Orphaned DTL Detection and Healing

## Test Location
`tests/zfs-tests/tests/functional/resilver/resilver_orphaned_dtl.ksh`

## Test Prerequisites
- Pool with at least 2 vdevs (one can be mirrored)
- Ability to simulate crash (zinject or raw manipulation)

## Test Cases

### 1. `resilver_orphaned_dtl_001_pos`
**Description:** Verify orphaned DTL detection blocks import by default

**Steps:**
1. Create pool with mirror vdev
2. Export pool
3. Manually corrupt on-disk state to simulate orphaned DTL on hole vdev
   (or use zinject to fail mid-detach sync)
4. Attempt import without healing flag
5. **Verify:** Import fails with error message mentioning "orphaned DTL"

### 2. `resilver_orphaned_dtl_002_pos`
**Description:** Verify healing option clears orphaned DTL

**Steps:**
1. Create pool with mirror vdev
2. Export pool
3. Manually corrupt on-disk state to simulate orphaned DTL
4. Import with `-o heal_orphaned_dtl=on`
5. **Verify:** Import succeeds
6. **Verify:** Warning logged about cleared DTL entries
7. **Verify:** No resilver triggered
8. **Verify:** Pool history contains "heal orphaned dtl" entry

### 3. `resilver_orphaned_dtl_003_neg`
**Description:** Verify normal detach doesn't leave orphaned DTL

**Steps:**
1. Create pool with mirror vdev
2. Detach mirror member (clean detach)
3. Export pool
4. Import pool
5. **Verify:** Import succeeds without healing flag
6. **Verify:** No warning about orphaned DTL
7. **Verify:** No resilver triggered

### 4. `resilver_orphaned_dtl_004_pos`
**Description:** Verify multiple orphaned DTL entries handled correctly

**Steps:**
1. Create pool with multiple mirror vdevs
2. Export pool
3. Simulate crash leaving multiple holes with orphaned DTL
4. Import with healing flag
5. **Verify:** All orphaned DTL entries cleared
6. **Verify:** Count in log message matches number of affected vdevs

## Implementation Notes

### Simulating Orphaned DTL State

Option A: Use `zdb` to identify DTL object numbers, then manipulate pool with `ztest` facilities

Option B: Create wrapper that:
1. Starts detach operation
2. Kills ZFS module mid-transaction (requires root and may be flaky)
3. Reimport pool

Option C: Direct nvlist manipulation of exported pool config (most reliable for testing)

### Test Tags
```
tags = ['functional', 'resilver', 'import']
```

### Cleanup
Ensure test destroys pool and cleans up any corrupted state even on failure.

---

*Note: Actual test implementation requires understanding of ZFS test suite patterns.
See existing tests in `tests/zfs-tests/tests/functional/resilver/` for examples.*
