#!/bin/bash
# Real memory utilization — no double-counting
#
# Fixes vs naive (MemTotal-MemAvailable / SwapTotal-SwapFree):
#  - SwapCached: pages in BOTH ram and swap → count in ram only, not swap
#  - Zswapped:   swap slots stored compressed in RAM (zswap pool) → not on-disk
#  - ZFS ARC:    MemAvailable ignores shrinkable ARC → real avail is higher
#
# Bottom line shows two scenarios side-by-side:
#  - ARC→min (optimistic): ARC can shrink to c_min, freeing memory quickly
#  - ARC→max (worst-case):  ARC treated as committing c_max — it grows toward
#    c_max and releases slowly, which in practice can crowd out other consumers
#
# Matches htop's accounting (see htop/linux/LinuxMachine.c:215, Platform.c:467)

files="/proc/meminfo"
[ -f /proc/spl/kstat/zfs/arcstats ] && files="$files /proc/spl/kstat/zfs/arcstats"

awk '
# --- parse /proc/meminfo and /proc/spl/kstat/zfs/arcstats ---
/^MemTotal:/         { mt=$2 }
/^MemFree:/          { mf=$2 }
/^MemAvailable:/     { ma=$2 }
/^SwapCached:/       { sc=$2 }
/^Active\(file\):/   { af=$2 }
/^Inactive\(file\):/ { iff=$2 }
/^SwapTotal:/        { st=$2 }
/^SwapFree:/         { sf=$2 }
/^Zswap:/            { zr=$2 }
/^Zswapped:/         { zd=$2 }
/^AnonPages:/        { anon=$2 }
/^Slab:/             { slab=$2 }
/^SReclaimable:/     { slab_r=$2 }
/^SUnreclaim:/       { slab_u=$2 }
/^size /             { arc=$3 }
/^c_min /            { cmin=$3 }
/^c_max /            { cmax=$3 }

# Compute hard-used RAM for a given ARC floor commitment.
#   arc_floor = c_min → optimistic (ARC fully shrinkable)
#   arc_floor = c_max → worst-case (ARC keeps / grows to c_max)
# MemAvailable already does NOT count ARC as reclaimable, so:
#   - if arc > floor: (arc - floor) is truly shrinkable → add to avail
#   - if floor > arc: ARC may still grow by (floor - arc) → subtract from avail
function calc(tag, arc_floor,    shrink, growth, avail) {
    if (arc > 0) {
        shrink = (arc > arc_floor) ? (arc - arc_floor) / 1024 : 0   # bytes→kB
        growth = (arc_floor > arc) ? (arc_floor - arc) / 1024 : 0   # bytes→kB
        avail  = ma + shrink - growth
    } else {
        avail = ma
    }
    r_ram[tag]  = mt - avail
    r_swap[tag] = st - sf - sc - zd
    if (r_swap[tag] < 0) r_swap[tag] = 0
    r_dem[tag]  = r_ram[tag] + r_swap[tag]
    r_cap[tag]  = mt + st
    r_head[tag] = r_cap[tag] - r_dem[tag]
}

END {
    kG = 1024 * 1024           # kB  → GiB
    bG = 1024 * 1024 * 1024    # bytes → GiB

    swap_raw  = st - sf
    swap_disk = swap_raw - sc - zd
    if (swap_disk < 0) swap_disk = 0
    ram_naive = mt - ma

    printf "═══ Real Memory Utilization ══════════════════════\n\n"

    printf "RAM (MemAvailable): %5.1f / %4.1f GiB used  (%2.0f%%)\n",
        ram_naive/kG, mt/kG, ram_naive*100/mt
    printf "  anon %-5.1fG  file-cache %-5.2fG  slab %-5.1fG (%-4.2fG unrecl)\n",
        anon/kG, (af+iff)/kG, slab/kG, slab_u/kG
    printf "  zswap-pool %-5.2fG  (swap data compressed in RAM)\n", zr/kG

    swap_pct = st > 0 ? swap_disk*100/st : 0
    printf "\nSwap on-disk:       %5.1f / %4.1f GiB used  (%2.0f%%)\n",
        swap_disk/kG, st/kG, swap_pct
    printf "  raw slots %-4.1fG  − cached %-4.2fG (back in RAM)  − zswapped %-4.2fG (in zswap)\n",
        swap_raw/kG, sc/kG, zd/kG
    if (zr > 0 && zd > 0)
        printf "  zswap ratio: %.1f:1  (%.0f MiB RAM → %.0f MiB data)\n",
            zd/zr, zr/1024, zd/1024

    if (arc > 0) {
        arc_shrink = (arc > cmin) ? arc - cmin : 0
        printf "\nZFS ARC:            %5.1f GiB  (%.1fG shrinkable + %.1fG floor)\n",
            arc/bG, arc_shrink/bG, cmin/bG
        printf "  MemAvailable undercounts by ~%.1fG (does not see shrinkable ARC)\n",
            arc_shrink/bG
        printf "  c_min=%.1fG  c_max=%.1fG\n", cmin/bG, cmax/bG
    }

    # ─── Bottom Line ───
    printf "\n─── Bottom Line ──────────────────────────────────────────────────────\n"

    if (arc > 0) {
        calc("o", cmin)
        calc("w", cmax)

        printf "%-20s  %-26s  %s\n",   "", "ARC→min (optimistic)", "ARC→max (worst-case)"
        printf "────────────────────────────────────────────────────────────────────\n"

        printf "%-20s  %5.1f / %4.1f GiB (%2.0f%%)     %5.1f / %4.1f GiB (%2.0f%%)\n",
            "RAM hard-used:",
            r_ram["o"]/kG, mt/kG, r_ram["o"]*100/mt,
            r_ram["w"]/kG, mt/kG, r_ram["w"]*100/mt

        printf "%-20s  %5.1f / %4.1f GiB (%2.0f%%)\n",
            "Swap on-disk:", r_swap["o"]/kG, st/kG, swap_pct

        printf "────────────────────────────────────────────────────────────────────\n"

        printf "%-20s  %5.1f GiB                  %5.1f GiB\n",
            "Hard demand:", r_dem["o"]/kG, r_dem["w"]/kG

        printf "%-20s  %5.1f GiB  (%.1f RAM + %.1f swap)\n",
            "Capacity:", r_cap["o"]/kG, mt/kG, st/kG

        printf "%-20s  %5.1f GiB (%2.0f%%)           %5.1f GiB (%2.0f%%)\n",
            "Headroom:",
            r_head["o"]/kG, r_head["o"]*100/r_cap["o"],
            r_head["w"]/kG, r_head["w"]*100/r_cap["w"]

        printf "────────────────────────────────────────────────────────────────────\n"
    } else {
        calc("n", 0)
        printf "%-20s  %5.1f GiB  (%.1f RAM + %.1f swap on-disk)\n",
            "Demand:", r_dem["n"]/kG, r_ram["n"]/kG, r_swap["n"]/kG
        printf "%-20s  %5.1f GiB  (%.1f RAM + %.1f swap)\n",
            "Capacity:", r_cap["n"]/kG, mt/kG, st/kG
        printf "%-20s  %5.1f GiB  (%2.0f%%)\n",
            "Headroom:", r_head["n"]/kG, r_head["n"]*100/r_cap["n"]
        printf "────────────────────────────────────────────────────────────────────\n"
    }
}
' $files
