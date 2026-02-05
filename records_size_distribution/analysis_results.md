# ZFS Record Size Distribution: Storj Hashstore Log Files

## Setup
- Dataset: `hddpool/storj`
- Configured recordsize: **256K** (inherited from hddpool)
- Compression: lz4 (but data is Storj encrypted = incompressible)
- Files: ~4244 log files, most ~1GB each
- Storj piece avg size: ~79KB

## Results (sampled 5 files of varying sizes)

### Logical size (recordsize) distribution

| File | Size | Blocks | 256K blocks | Other |
|------|------|--------|-------------|-------|
| log-ce2 (active) | 1024 MB | 4097 | 4097 (100%) | 0 |
| log-ce1 (full) | 1024 MB | 4097 | 4097 (100%) | 0 |
| log-142 (partial) | 503 MB | 2014 | 2014 (100%) | 0 |
| log-13e (partial) | 69 MB | 277 | 277 (100%) | 0 |
| log-1da (small) | 7.8 MB | 32 | 32 (100%) | 0 |

**100% of data blocks use the full 256K recordsize across all files.**

### Physical size (after compression) distribution

- **99.98% of blocks**: 256K physical = no compression (encrypted data)
- **1 block per file**: slightly smaller (109-143K) = the tail block where
  the trailing portion is zeros and compresses

### Compression ratio
- Effectively 1.000:1 (lz4 enabled but useless on encrypted data)

## Key Findings

1. **All records are 256K** — ZFS always fills records completely for
   sequential writes. Since Storj hashstore appends pieces sequentially
   to log files, each 256K record contains ~3 average pieces (79KB each).

2. **No sub-recordsize fragmentation** — Because writes are sequential
   (append-only log files), ZFS accumulates data until a full 256K record
   is formed before flushing. There are no partial records except the
   very last block in each file.

3. **txg batching** doesn't create larger records. It batches multiple
   256K records into a single transaction group for efficient I/O, but
   each record is still capped at recordsize. The benefit of txg batching
   is reduced seek overhead, not larger records.

4. **Compression is wasted CPU** — lz4 is cheap but gains nothing on
   encrypted Storj data. Consider `zfs set compression=off hddpool/storj`
   to save a tiny amount of CPU.

## Implications for recordsize tuning

Since all blocks are at the max recordsize, the question becomes: is 256K
optimal for the Storj hashstore workload?

- **Read amplification**: When Storj reads a single 79KB piece from a log
  file, ZFS must read the entire 256K record containing it. That's 3.2x
  read amplification. With 128K recordsize it would be 1.6x. With 1M it
  would be 12.6x.

- **Write efficiency**: Larger recordsize = fewer metadata blocks, fewer
  indirect blocks, slightly better sequential throughput. But for 1GB
  files the metadata overhead is negligible regardless.

- **Recommendation**: 256K is a reasonable middle ground. If random read
  latency is critical (Storj audits), 128K would reduce amplification.
  If throughput matters more (bulk transfers), 256K or even 1M is fine.
  Use `zpool iostat -r` to check actual I/O patterns.
