#!/bin/bash
# memory_map.sh — Full memory layout diagram with swap universe
#
# Shows where every byte of RAM actually lives, how swap slots map to
# real storage (zswap vs disk vs SwapCached), and the true demand picture.
#
# Key accounting insight:
#   "Committed" is derived TOP-DOWN from MemAvailable, not by summing
#   components (which double-counts — e.g. ZFS slab metadata overlaps
#   with ARC size).  MemAvailable already handles page-cache / slab_r
#   reclaimability with proper watermarks.  We just add ARC shrinkability
#   that the kernel doesn't know about.
#
# Data sources: /proc/meminfo, /proc/spl/kstat/zfs/arcstats

files="/proc/meminfo"
[ -f /proc/spl/kstat/zfs/arcstats ] && files="$files /proc/spl/kstat/zfs/arcstats"

awk '
# ─── parse ───
/^MemTotal:/         { mt=$2 }
/^MemFree:/          { mf=$2 }
/^MemAvailable:/     { ma=$2 }
/^Buffers:/          { buf=$2 }
/^Cached:/           { if (!got_cached) { cached=$2; got_cached=1 } }
/^SwapCached:/       { sc=$2 }
/^Active\(file\):/   { af=$2 }
/^Inactive\(file\):/ { iff=$2 }
/^AnonPages:/        { anon=$2 }
/^Slab:/             { slab=$2 }
/^SReclaimable:/     { slab_r=$2 }
/^SUnreclaim:/       { slab_u=$2 }
/^SwapTotal:/        { st=$2 }
/^SwapFree:/         { sf=$2 }
/^Zswap:/            { zr=$2 }
/^Zswapped:/         { zd=$2 }
/^size /             { arc=$3 }
/^c /                { c=$3 }
/^c_min /            { cmin=$3 }
/^c_max /            { cmax=$3 }

# ─── helpers ───
function bar(used, total, width,    filled, i, s) {
    filled = int(used / total * width + 0.5)
    if (filled < 0) filled = 0
    if (filled > width) filled = width
    s = ""
    for (i = 0; i < filled; i++) s = s "▓"
    for (i = filled; i < width; i++) s = s "░"
    return s
}

function bar2(a, b, total, width,    fa, fb, i, s) {
    fa = int(a / total * width + 0.5)
    fb = int(b / total * width + 0.5)
    if (fa + fb > width) fb = width - fa
    s = ""
    for (i = 0; i < fa; i++) s = s "█"
    for (i = 0; i < fb; i++) s = s "▒"
    for (i = fa + fb; i < width; i++) s = s "░"
    return s
}

function fmt(kb) {
    if (kb < 0) kb = 0
    if (kb >= 1024*1024) return sprintf("%.1fG", kb/1024/1024)
    if (kb >= 1024)      return sprintf("%.0fM", kb/1024)
    return sprintf("%.0fK", kb)
}

END {
    bK = 1024                      # bytes → kB

    # ─── ARC in kB ───
    arc_kb     = arc / bK
    cmin_kb    = cmin / bK
    cmax_kb    = cmax / bK
    c_kb       = c / bK
    arc_shrink = (arc > cmin) ? (arc - cmin) / bK : 0
    arc_floor  = cmin_kb

    # ─── Swap breakdown ───
    swap_used   = st - sf
    in_zswap    = zd                       # uncompressed size in zswap pool
    on_disk     = swap_used - zd           # slots with data on swap device
    if (on_disk < 0) on_disk = 0
    swap_cached = sc                       # on disk AND in page cache
    disk_only   = on_disk - sc             # on disk, NOT in RAM
    if (disk_only < 0) disk_only = 0
    zswap_ratio = (zr > 0) ? zd / zr : 0

    # ─── Top-down committed / reclaimable / free ───
    # MemAvailable accounts for reclaimable page cache + slab_r with
    # proper watermarks.  It does NOT know ARC is shrinkable, so we add that.
    true_avail  = ma + arc_shrink
    committed   = mt - true_avail
    reclaimable = true_avail - mf
    # free = mf

    # Committed breakdown (components — may not sum exactly due to
    # watermarks and kernel internals, residual absorbs the gap)
    kernel_other = committed - anon - arc_floor - zr
    if (kernel_other < 0) kernel_other = 0

    # Reclaimable breakdown (informational — MemAvailable uses watermarks
    # so these may not sum exactly to reclaimable, but they show the pieces)
    filecache = af + iff

    # ═══════════════════════════════════════════════════════════════
    printf "═══════════════════════════════════════════════════════════════════════\n"
    printf "  MEMORY MAP — %s RAM", fmt(mt)
    if (st > 0) printf " + %s swap", fmt(st)
    printf "\n"
    printf "═══════════════════════════════════════════════════════════════════════\n"

    # ─── RAM LAYOUT ───
    printf "\n"
    printf "  ┌─ PHYSICAL RAM ────────────────────────────────────────────────┐\n"
    printf "  │                                                                 │\n"
    printf "  │  COMMITTED (not reclaimable)                   %6s  %2.0f%%    │\n",
        fmt(committed), committed*100/mt
    printf "  │  ┌───────────────────────────────────────────────────────────┐  │\n"
    printf "  │  │  Anon pages ......... %6s  (app heap, stacks, mmap)    │  │\n", fmt(anon)
    if (arc > 0)
    printf "  │  │  ARC floor (c_min) .. %6s  (ZFS keeps, incl. slab)    │  │\n", fmt(arc_floor)
    if (zr > 0)
    printf "  │  │  Zswap pool ......... %6s  (compressed swap in RAM)    │  │\n", fmt(zr)
    printf "  │  │  Kernel other ....... %6s  (slab, pagetables, stacks)  │  │\n", fmt(kernel_other)
    printf "  │  └───────────────────────────────────────────────────────────┘  │\n"
    printf "  │                                                                 │\n"
    printf "  │  RECLAIMABLE (caches the kernel can drop)      %6s  %2.0f%%    │\n",
        fmt(reclaimable), reclaimable*100/mt
    printf "  │  ┌───────────────────────────────────────────────────────────┐  │\n"
    if (arc > 0)
    printf "  │  │  ARC above c_min .... %6s  (shrinkable under pressure) │  │\n", fmt(arc_shrink)
    printf "  │  │  Page cache ......... %6s  (file data, SwapCached)     │  │\n", fmt(filecache)
    printf "  │  │  Slab reclaimable ... %6s  (dentries, inodes, znodes)  │  │\n", fmt(slab_r)
    printf "  │  │  Buffers ............ %6s  (block device metadata)     │  │\n", fmt(buf)
    printf "  │  └───────────────────────────────────────────────────────────┘  │\n"
    printf "  │                                                                 │\n"
    printf "  │  FREE .............................................%6s  %2.0f%%    │\n",
        fmt(mf), mf*100/mt
    printf "  │                                                                 │\n"
    printf "  │  %s│\n", bar2(committed, reclaimable, mt, 63)
    printf "  │  █ committed %5s    ▒ reclaimable %5s    ░ free %5s    │\n",
        fmt(committed), fmt(reclaimable), fmt(mf)
    printf "  └─────────────────────────────────────────────────────────────────┘\n"

    # ─── SWAP UNIVERSE ───
    if (st > 0) {
        printf "\n"
        printf "  ┌─ SWAP UNIVERSE ───────────────────────────────────────────────┐\n"
        printf "  │                                                                 │\n"
        printf "  │  %s slots used / %s total (%2.0f%%)                               │\n",
            fmt(swap_used), fmt(st), swap_used*100/st
        printf "  │  A slot is a claim — but where is the data REALLY?              │\n"
        printf "  │                                                                 │\n"

        if (zd > 0) {
        printf "  │    IN ZSWAP ........... %6s  (%2.0f%%)                           │\n",
            fmt(in_zswap), in_zswap*100/swap_used
        printf "  │      compressed to %s in RAM (ratio %.1f:1)                  │\n",
            fmt(zr), zswap_ratio
        printf "  │      NOT on disk — lives entirely in RAM                        │\n"
        printf "  │                                                                 │\n"
        }

        printf "  │    SWAPCACHED ......... %6s  (%2.0f%%)                           │\n",
            fmt(swap_cached), (swap_used>0) ? swap_cached*100/swap_used : 0
        printf "  │      written to disk, but clean copy already in page cache      │\n"
        printf "  │      no I/O needed — readable from RAM                          │\n"
        printf "  │                                                                 │\n"
        printf "  │    DISK-ONLY .......... %6s  (%2.0f%%)                           │\n",
            fmt(disk_only), (swap_used>0) ? disk_only*100/swap_used : 0
        printf "  │      on disk, NOT in RAM — needs I/O to read back               │\n"
        printf "  │                                                                 │\n"

        printf "  │  Flow:  anon page ─pressure─► zswap ─full?─► disk ─fault─► RAM  │\n"
        printf "  │                    (compress)  (evict)    (SwapCached, slot kept) │\n"
        printf "  │                                                                 │\n"
        printf "  │  %s│\n", bar(swap_used, st, 63)
        printf "  │  ▓ used %5s                                     ░ free %5s│\n",
            fmt(swap_used), fmt(sf)
        printf "  └─────────────────────────────────────────────────────────────────┘\n"
    }

    # ─── ZFS ARC ───
    if (arc > 0) {
        printf "\n"
        printf "  ┌─ ZFS ARC ────────────────────────────────────────────────────┐\n"
        printf "  │                                                                 │\n"
        printf "  │  c_min         current          c (target)        c_max         │\n"
        printf "  │  %-6s        %-6s           %-6s            %-6s        │\n",
            fmt(cmin_kb), fmt(arc_kb), fmt(c_kb), fmt(cmax_kb)
        printf "  │   │              │                │                 │            │\n"
        printf "  │   ├──────────────┼────────────────┼─────────────────┤            │\n"
        printf "  │   │██████████████│▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒│            │\n"
        printf "  │   │  COMMITTED   │  RECLAIMABLE                    │            │\n"
        printf "  │   │   %-6s     │   %-6s                        │            │\n",
            fmt(arc_floor), fmt(arc_shrink)
        printf "  │   └──────────────┴─────────────────────────────────┘            │\n"
        printf "  │                                                                 │\n"
        printf "  │  █ c_min floor — ZFS will NOT release, ever                     │\n"
        printf "  │  ▒ shrinkable — kernel reclaims under memory pressure           │\n"
        printf "  │  MemAvailable ignores ARC → undercounts avail by ~%-6s        │\n",
            fmt(arc_shrink)
        printf "  │                                                                 │\n"
        printf "  │  ARC memory lives in two places:                                │\n"
        printf "  │    slab (SUnreclaim) ... headers, dnodes, dmu_bufs, zio_bufs    │\n"
        printf "  │    page alloc (ABDs) ... actual cached block data               │\n"
        printf "  │    slab: %-6s  SUnrecl total: %-6s                          │\n",
            fmt(slab), fmt(slab_u)
        printf "  └─────────────────────────────────────────────────────────────────┘\n"
    }

    # ─── BOTTOM LINE ───
    printf "\n"
    printf "  ┌─ BOTTOM LINE ──────────────────────────────────────────────────┐\n"
    printf "  │                                                                 │\n"

    hard_ram   = committed
    hard_swap  = disk_only
    hard_total = hard_ram + hard_swap
    capacity   = mt + st
    headroom   = capacity - hard_total

    printf "  │  Hard demand (RAM):    %6s  (committed, not reclaimable)     │\n", fmt(hard_ram)
    printf "  │  Hard demand (swap):   %6s  (disk-only, needs I/O to read)   │\n", fmt(hard_swap)
    printf "  │                        ──────                                   │\n"
    printf "  │  Total hard demand:    %6s                                    │\n", fmt(hard_total)
    printf "  │                                                                 │\n"
    printf "  │  Capacity:             %6s  (%s RAM + %s swap)            │\n",
        fmt(capacity), fmt(mt), fmt(st)
    printf "  │  Headroom:             %6s  (%2.0f%%)                            │\n",
        fmt(headroom), headroom*100/capacity
    printf "  │                                                                 │\n"

    pct = hard_total * 100 / capacity
    if (pct < 50)       verdict = "HEALTHY — mostly cache, plenty of room"
    else if (pct < 70)  verdict = "MODERATE — watch for swap growth"
    else if (pct < 85)  verdict = "TIGHT — consider reducing workload"
    else                verdict = "CRITICAL — OOM risk, reduce load now"

    printf "  │  %-63s│\n", verdict
    printf "  │                                                                 │\n"
    printf "  │  %s│\n", bar(hard_total, capacity, 63)
    printf "  │  ▓ hard demand %5s                          ░ headroom %5s│\n",
        fmt(hard_total), fmt(headroom)
    printf "  └─────────────────────────────────────────────────────────────────┘\n"

    # ─── CHEAT SHEET ───
    printf "\n"
    printf "  ┌─ vs. free(1) ─────────────────────────────────────────────────┐\n"
    printf "  │                                                                 │\n"
    free_used = mt - mf - buf - cached - slab_r
    free_bc   = buf + cached + slab_r
    printf "  │  free(1) \"used\":     %6s (%2.0f%%)  ← includes ARC data (%s)│\n",
        fmt(free_used), free_used*100/mt, fmt(arc_kb)
    printf "  │  free(1) \"buff/cache\": %4s        ← SSD-backed, cheap to re-read│\n",
        fmt(free_bc)
    printf "  │  free(1) \"swap used\":  %4s        ← only %s needs actual I/O │\n",
        fmt(swap_used), fmt(disk_only)
    printf "  │                                                                 │\n"
    printf "  │  MemAvailable:  %6s               (kernel'\''s estimate)         │\n", fmt(ma)
    printf "  │  True available: %5s               (+%s ARC shrinkable)    │\n",
        fmt(true_avail), fmt(arc_shrink)
    printf "  └─────────────────────────────────────────────────────────────────┘\n"
}
' $files
