#!/bin/bash
# Real memory utilization — no double-counting
#
# Fixes vs naive (MemTotal-MemAvailable / SwapTotal-SwapFree):
#  - SwapCached: pages in BOTH ram and swap → count in ram only, not swap
#  - Zswapped:   swap slots stored compressed in RAM (zswap pool) → not on-disk
#  - ZFS ARC:    MemAvailable ignores shrinkable ARC → real avail is higher
#
# Matches htop's accounting (see htop/linux/LinuxMachine.c:215, Platform.c:467)

awk '
/^MemTotal:/      { mt=$2 }
/^MemFree:/       { mf=$2 }
/^MemAvailable:/  { ma=$2 }
/^Buffers:/       { buf=$2 }
/^Cached:/        { cached=$2 }
/^SwapCached:/    { sc=$2 }
/^Active\(file\):/  { af=$2 }
/^Inactive\(file\):/ { iff=$2 }
/^SwapTotal:/     { st=$2 }
/^SwapFree:/      { sf=$2 }
/^Zswap:/         { zr=$2 }
/^Zswapped:/      { zd=$2 }
/^AnonPages:/     { anon=$2 }
/^Slab:/          { slab=$2 }
/^SReclaimable:/  { slab_r=$2 }
/^SUnreclaim:/    { slab_u=$2 }
END {
    G = 1024 * 1024  # kB → GiB

    # --- Swap: on-disk only (htop formula) ---
    # htop: usedSwap = SwapTotal - SwapFree - SwapCached  (LinuxMachine.c:215)
    # then:          -= Zswapped                          (Platform.c:487)
    swap_raw  = st - sf
    swap_disk = swap_raw - sc - zd
    if (swap_disk < 0) swap_disk = 0

    # --- RAM used (naive, MemAvailable-based) ---
    ram_used_naive = mt - ma

    printf "═══ Real Memory Utilization ══════════════════════\n\n"

    printf "RAM (MemAvailable): %5.1f / %4.1f GiB used  (%2.0f%%)\n",
        ram_used_naive/G, mt/G, ram_used_naive*100/mt
    printf "  anon %-5.1fG  file-cache %-5.2fG  slab %-5.1fG (%-4.2fG unrecl)\n",
        anon/G, (af+iff)/G, slab/G, slab_u/G
    printf "  zswap-pool %-5.2fG  (swap data compressed in RAM)\n", zr/G

    swap_pct = st > 0 ? swap_disk*100/st : 0
    printf "\nSwap on-disk:       %5.1f / %4.1f GiB used  (%2.0f%%)\n",
        swap_disk/G, st/G, swap_pct
    printf "  raw slots %-4.1fG  − cached %-4.2fG (back in RAM)  − zswapped %-4.2fG (in zswap)\n",
        swap_raw/G, sc/G, zd/G

    if (zr > 0 && zd > 0)
        printf "  zswap ratio: %.1f:1  (%.0f MiB RAM → %.0f MiB data)\n",
            zd/zr, zr/1024, zd/1024
}' /proc/meminfo

# ZFS ARC: MemAvailable does NOT know ARC is shrinkable
if [ -f /proc/spl/kstat/zfs/arcstats ]; then
    awk '
    /^size /  { size=$3 }
    /^c_min / { cmin=$3 }
    END {
        G = 1024*1024*1024
        shrink = (size > cmin) ? size - cmin : 0
        printf "\nZFS ARC:            %5.1f GiB  (%.1fG shrinkable + %.1fG floor)\n",
            size/G, shrink/G, cmin/G
        printf "  MemAvailable undercounts by ~%.1fG (does not see shrinkable ARC)\n",
            shrink/G
    }' /proc/spl/kstat/zfs/arcstats

    # Bottom line: combine meminfo + ARC for real picture
    paste <(awk '
        /^MemTotal:/     { mt=$2 }
        /^MemAvailable:/ { ma=$2 }
        /^SwapTotal:/    { st=$2 }
        /^SwapFree:/     { sf=$2 }
        /^SwapCached:/   { sc=$2 }
        /^Zswapped:/     { zd=$2 }
        END { printf "%d %d %d %d %d %d", mt, ma, st, sf, sc, zd }
    ' /proc/meminfo) \
    <(awk '/^size / {s=$3} /^c_min / {c=$3} END { printf "%d %d", s, c }' \
        /proc/spl/kstat/zfs/arcstats) |
    awk '{
        G = 1024*1024
        Gb = 1024*1024*1024
        mt=$1; ma=$2; st=$3; sf=$4; sc=$5; zd=$6; arc=$7; cmin=$8

        arc_shrink = (arc > cmin) ? (arc - cmin) / 1024 : 0   # bytes → kB
        real_avail = ma + arc_shrink
        ram_hard   = mt - real_avail

        swap_disk  = st - sf - sc - zd
        if (swap_disk < 0) swap_disk = 0

        total_cap  = mt + st
        hard_total = ram_hard + swap_disk
        headroom   = total_cap - hard_total

        printf "\n─── Bottom Line ──────────────────────────────────\n"
        printf "RAM hard-used:  %5.1f / %4.1f GiB  (%2.0f%%)  ← after freeing shrinkable ARC\n",
            ram_hard/G, mt/G, ram_hard*100/mt
        swap_pct = st > 0 ? swap_disk*100/st : 0
        printf "Swap on-disk:   %5.1f / %4.1f GiB  (%2.0f%%)  ← no SwapCached/zswap double-count\n",
            swap_disk/G, st/G, swap_pct
        printf "────────────────────────────────────────\n"
        printf "Hard demand:    %5.1f GiB\n", hard_total/G
        printf "Capacity:       %5.1f GiB  (%.1f RAM + %.1f swap)\n",
            total_cap/G, mt/G, st/G
        printf "Headroom:       %5.1f GiB  (%2.0f%%)\n",
            headroom/G, headroom*100/total_cap
        printf "────────────────────────────────────────\n"
    }'
else
    # No ZFS: simpler bottom line
    awk '
    /^MemTotal:/     { mt=$2 }
    /^MemAvailable:/ { ma=$2 }
    /^SwapTotal:/    { st=$2 }
    /^SwapFree:/     { sf=$2 }
    /^SwapCached:/   { sc=$2 }
    /^Zswapped:/     { zd=$2 }
    END {
        G = 1024*1024
        ram_used  = mt - ma
        swap_disk = st - sf - sc - zd
        if (swap_disk < 0) swap_disk = 0
        total_cap = mt + st
        demand    = ram_used + swap_disk
        headroom  = total_cap - demand

        printf "\n─── Bottom Line ──────────────────────────────────\n"
        printf "Demand:    %5.1f GiB  (%.1f RAM + %.1f swap on-disk)\n",
            demand/G, ram_used/G, swap_disk/G
        printf "Capacity:  %5.1f GiB  (%.1f RAM + %.1f swap)\n",
            total_cap/G, mt/G, st/G
        printf "Headroom:  %5.1f GiB  (%2.0f%%)\n",
            headroom/G, headroom*100/total_cap
        printf "────────────────────────────────────────\n"
    }' /proc/meminfo
fi
