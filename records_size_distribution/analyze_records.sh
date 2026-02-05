#!/bin/bash
# Analyze ZFS record size distribution for Storj hashstore log files
# Usage: ./analyze_records.sh [file_path ...]
# If no args, reads paths from stdin (one per line)

DATASET="hddpool/storj"
MOUNTPOINT="/hddpool/storj"

analyze_file() {
    local filepath="$1"
    local relpath="${filepath#${MOUNTPOINT}/}"
    local filesize=$(stat -c%s "$filepath" 2>/dev/null)
    local filesize_mb=$(echo "scale=1; $filesize / 1048576" | bc)

    # Get object number via zdb -O
    local objnum=$(zdb -dddddd -O "$DATASET" "$relpath" 2>&1 | awk '/ZFS plain file/ {print $1}')
    if [ -z "$objnum" ]; then
        echo "ERROR: could not find object for $relpath"
        return 1
    fi

    echo "=== $relpath ==="
    echo "    File size: ${filesize_mb} MB  Object: $objnum"

    # Dump block pointers, extract only L0 data blocks
    # Format in zdb output: "  <offset> L0 <dva> <lsize>L/<psize>P ..."
    zdb -ddddddd "$DATASET" "$objnum" 2>&1 | awk '
    /^\s+[0-9a-fA-F]+\s+L0\s/ {
        # Extract lsize/psize from the line
        for (i=1; i<=NF; i++) {
            if ($i ~ /^[0-9a-fA-F]+L\/[0-9a-fA-F]+P$/) {
                split($i, parts, /[LP\/]/)
                lsize = strtonum("0x" parts[1])
                psize = strtonum("0x" parts[3])
                lsizes[lsize]++
                psizes[psize]++
                total_l += lsize
                total_p += psize
                count++
                break
            }
        }
    }
    END {
        printf "    Data blocks: %d\n", count
        printf "    Total lsize: %.1f MB | Total psize: %.1f MB\n", total_l/1048576, total_p/1048576
        if (total_p > 0) printf "    Compression: %.3f:1\n", total_l/total_p
        printf "    Logical size distribution:\n"
        n = asorti(lsizes, sorted_l, "@ind_num_asc")
        for (i=1; i<=n; i++) {
            s = sorted_l[i]
            pct = lsizes[s] * 100.0 / count
            printf "      %6d KB (%4s): %5d blocks (%5.1f%%)\n", s/1024, \
                (s >= 1048576 ? sprintf("%.0fM", s/1048576) : sprintf("%dK", s/1024)), \
                lsizes[s], pct
        }
        printf "    Physical size distribution:\n"
        n = asorti(psizes, sorted_p, "@ind_num_asc")
        for (i=1; i<=n; i++) {
            s = sorted_p[i]
            pct = psizes[s] * 100.0 / count
            printf "      %6d KB (%4s): %5d blocks (%5.1f%%)\n", s/1024, \
                (s >= 1048576 ? sprintf("%.0fM", s/1048576) : sprintf("%dK", s/1024)), \
                psizes[s], pct
        }
    }'
    echo ""
}

if [ $# -gt 0 ]; then
    for f in "$@"; do
        analyze_file "$f"
    done
else
    while IFS= read -r line; do
        # skip comments and empty lines
        [[ "$line" =~ ^#.*$ || -z "$line" ]] && continue
        # extract path (3rd field if space-delimited, or whole line)
        path=$(echo "$line" | awk '{print $NF}')
        analyze_file "$path"
    done
fi
