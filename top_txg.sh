#!/bin/bash
# top_txg.sh - Human-readable ZFS TXG monitor with interactive sorting
# Shows ongoing transaction groups with human-friendly units

show_help() {
    cat << 'EOF'
Usage: top_txg.sh [OPTIONS] [POOLS] [INTERVAL] [TXG_COUNT]

Monitor ZFS transaction groups (TXGs) with human-readable output.

Arguments:
  POOLS       Space-separated pool names (default: "hddpool ssdpool")
  INTERVAL    Refresh interval in seconds (default: 2)
  TXG_COUNT   Number of TXGs to display per pool (default: 20)

Options:
  -h, --help  Show this help message

Interactive Keys (lowercase=ascending, UPPERCASE=descending):
  d/D   Sort by Dirty bytes
  r/R   Sort by Read bytes
  w/W   Sort by Written bytes
  o/O   Sort by Open time
  u/U   Sort by Queue time
  a/A   Sort by Wait time
  s/S   Sort by Sync time
  m/M   Sort by MB/s
  n     Reset to default (recent TXGs, no sorting)
  ↓/↑   Navigate pages (next/previous page of sorted results)
  q     Quit

Columns:
  DATE/TIME   When the TXG was born (converted from hrtime)
  TXG         Transaction group number
  STATE       OPEN=accepting writes, QUIESCE=draining, SYNCING=writing to disk, done=committed
  DIRTY       Bytes pending write in this TXG
  READ        Bytes read during sync
  WRITTEN     Bytes written to disk
  R/W OPS     Read/Write operation counts
  OPEN        Time TXG was open for writes
  QUEUE       Time waiting in quiesce
  WAIT        Time waiting to sync
  SYNC        Time spent syncing to disk
  MB/s        Write throughput (written/sync_time)
  DURATION    Time span of TXG (birth -> end), or - if still active

Examples:
  top_txg.sh                          # Monitor default pools
  top_txg.sh "tank"                   # Monitor single pool
  top_txg.sh "tank rpool" 5           # Two pools, 5s refresh
  top_txg.sh "tank" 2 30              # 30 TXGs, 2s refresh

EOF
    exit 0
}

# Parse options
case "$1" in
    -h|--help) show_help ;;
esac

# Default pools (space-separated) or pass as arguments
POOLS="${1:-hddpool ssdpool}"
INTERVAL="${2:-2}"
TXG_COUNT="${3:-20}"  # TXGs per pool

# Sorting state
SORT_COL="none"     # Default: no sorting, just tail (most recent TXGs)
SORT_REV=0          # 0=ascending, 1=descending
SORT_FIELD=1        # awk field number for sorting
LAST_KEY=""         # Last key pressed (for visual feedback)

# Pagination state
PAGE_OFFSET=0       # How many items to skip (for pagination)
TOTAL_TXGS=0        # Total TXGs available (for page indicator)

# Colors
RED='\033[0;31m'
YELLOW='\033[1;33m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
DIM='\033[2m'
NC='\033[0m'
UNDERLINE='\033[4m'

# Map sort column to awk field and display name
get_sort_info() {
    case "$SORT_COL" in
        none)    SORT_FIELD=0;  SORT_NAME="recent" ;;
        dirty)   SORT_FIELD=4;  SORT_NAME="DIRTY" ;;
        read)    SORT_FIELD=5;  SORT_NAME="READ" ;;
        written) SORT_FIELD=6;  SORT_NAME="WRITTEN" ;;
        open)    SORT_FIELD=9;  SORT_NAME="OPEN" ;;
        queue)   SORT_FIELD=10; SORT_NAME="QUEUE" ;;
        wait)    SORT_FIELD=11; SORT_NAME="WAIT" ;;
        sync)    SORT_FIELD=12; SORT_NAME="SYNC" ;;
        mbps)    SORT_FIELD=13; SORT_NAME="MB/s" ;;
    esac
}

human_bytes() {
    local bytes=$1
    if (( bytes >= 1073741824 )); then
        printf "%.1fG" $(echo "scale=1; $bytes/1073741824" | bc)
    elif (( bytes >= 1048576 )); then
        printf "%.1fM" $(echo "scale=1; $bytes/1048576" | bc)
    elif (( bytes >= 1024 )); then
        printf "%.0fK" $(echo "scale=0; $bytes/1024" | bc)
    else
        printf "%dB" $bytes
    fi
}

human_time_ns() {
    local ns=$1
    if (( ns >= 60000000000 )); then
        printf "%.1fm" $(echo "scale=1; $ns/60000000000" | bc)
    elif (( ns >= 1000000000 )); then
        printf "%.1fs" $(echo "scale=1; $ns/1000000000" | bc)
    else
        # Everything sub-second in ms (rounded)
        local ms=$(( (ns + 500000) / 1000000 ))
        printf "%dms" $ms
    fi
}

state_label() {
    # Returns 9-char padded state with colors
    case $1 in
        O) echo -e "${GREEN}OPEN${NC}     " ;;
        Q) echo -e "${YELLOW}QUIESCE${NC}  " ;;
        S) echo -e "${RED}SYNCING${NC}  " ;;
        C) echo -e "${DIM}done${NC}     " ;;
        *) echo -e "${DIM}$1${NC}       " ;;
    esac
}

print_header() {
    local pool="$1"
    get_sort_info

    # Column widths - must match data format exactly
    # DATE=11, TIME=9, TXG=10, STATE=9, DIRTY=10, READ=10, WRITTEN=10, OPS=13, OPEN=8, QUEUE=8, WAIT=8, SYNC=8, MB/s=8
    local sep="━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    local sort_indicator
    if [[ "$SORT_COL" == "none" ]]; then
        sort_indicator="[${SORT_NAME}]"
    else
        local sort_dir="▲"
        [[ $SORT_REV -eq 1 ]] && sort_dir="▼"
        sort_indicator="[sorted by ${SORT_NAME} ${sort_dir}]"
    fi
    local page_info=$(get_page_indicator)
    echo -e "${BOLD}${CYAN}${pool}${NC}  ${DIM}${sort_indicator}${page_info}${NC}"
    echo -e "${BOLD}${sep}${NC}"
    printf "${BOLD}%-11s %-9s %-10s %-9s %-10s %-10s %-10s %-13s %-8s %-8s %-8s %-8s %-8s %s${NC}\n" \
        "DATE" "TIME" "TXG" "STATE" "DIRTY" "READ" "WRITTEN" "R/W OPS" "OPEN" "QUEUE" "WAIT" "SYNC" "MB/s" "DURATION"
    echo -e "${BOLD}${sep}${NC}"
}

# Convert birth hrtime (ns since boot) to wall-clock timestamp
# Uses BOOT_EPOCH computed once at startup for stable timestamps
birth_to_wallclock() {
    local birth_hrtime=$1
    # If we don't have a valid birth time, return empty
    if [[ -z "$birth_hrtime" || "$birth_hrtime" == "0" ]]; then
        echo ""
        return
    fi
    local birth_seconds=$((birth_hrtime / 1000000000))
    local birth_epoch=$((BOOT_EPOCH + birth_seconds))
    date -d "@$birth_epoch" '+%Y-%m-%d %H:%M:%S'
}

format_txg() {
    local line="$1"
    local txg=$(echo "$line" | awk '{print $1}')
    local birth=$(echo "$line" | awk '{print $2}')
    local state=$(echo "$line" | awk '{print $3}')
    local ndirty=$(echo "$line" | awk '{print $4}')
    local nread=$(echo "$line" | awk '{print $5}')
    local nwritten=$(echo "$line" | awk '{print $6}')
    local reads=$(echo "$line" | awk '{print $7}')
    local writes=$(echo "$line" | awk '{print $8}')
    local otime=$(echo "$line" | awk '{print $9}')
    local qtime=$(echo "$line" | awk '{print $10}')
    local wtime=$(echo "$line" | awk '{print $11}')
    local stime=$(echo "$line" | awk '{print $12}')

    # Skip header line
    [[ "$txg" == "txg" ]] && return

    local state_str=$(state_label "$state")
    local dirty_h=$(human_bytes $ndirty)
    local read_h=$(human_bytes $nread)
    local written_h=$(human_bytes $nwritten)
    local otime_h=$(human_time_ns $otime)
    local qtime_h=$(human_time_ns $qtime)
    local wtime_h=$(human_time_ns $wtime)
    local stime_h=$(human_time_ns $stime)

    # Calculate MB/s (written bytes / sync time in seconds)
    local mbps_h="-"
    if (( stime > 0 && nwritten > 0 )); then
        local mbps=$(echo "scale=1; $nwritten * 953.674 / $stime" | bc)
        # Ensure leading zero (bc outputs .5 instead of 0.5)
        [[ "$mbps" == .* ]] && mbps="0$mbps"
        mbps_h="${mbps}"
    fi

    # Convert birth hrtime to wall-clock timestamp
    local birth_dt=$(birth_to_wallclock "$birth")
    local birth_date=""
    local birth_time=""
    if [[ -n "$birth_dt" ]]; then
        birth_date=${birth_dt% *}
        birth_time=${birth_dt#* }
    fi

    # Calculate completion time for finished TXGs
    # completion_hrtime = birth + otime + qtime + wtime + stime
    local duration_str="-"
    if [[ "$state" == "C" && -n "$birth_time" ]]; then
        local completion_hrtime=$((birth + otime + qtime + wtime + stime))
        local completion_dt=$(birth_to_wallclock "$completion_hrtime")
        local end_time=${completion_dt#* }
        if [[ -n "$end_time" ]]; then
            duration_str="${birth_time} -> ${end_time}"
        fi
    fi

    # Highlight active TXGs
    # Format: DATE=11, TIME=9, TXG=10, STATE=9(in state_str), DIRTY=10, READ=10, WRITTEN=10, OPS=13, OPEN=8, QUEUE=8, WAIT=8, SYNC=8, MB/s=8
    if [[ "$state" == "O" || "$state" == "S" || "$state" == "Q" ]]; then
        printf "${CYAN}%-11s %-9s %-10s${NC}%s%-10s %-10s %-10s %6d/%-6d %-8s %-8s %-8s %-8s %-8s %s\n" \
            "$birth_date" "$birth_time" "$txg" "$state_str" "$dirty_h" "$read_h" "$written_h" "$reads" "$writes" \
            "$otime_h" "$qtime_h" "$wtime_h" "$stime_h" "$mbps_h" "$duration_str"
    else
        printf "${DIM}%-11s %-9s %-10s${NC}%s%-10s %-10s %-10s %6d/%-6d %-8s %-8s %-8s %-8s %-8s %s\n" \
            "$birth_date" "$birth_time" "$txg" "$state_str" "$dirty_h" "$read_h" "$written_h" "$reads" "$writes" \
            "$otime_h" "$qtime_h" "$wtime_h" "$stime_h" "$mbps_h" "$duration_str"
    fi
}

show_summary() {
    local pool="$1"
    local txg_file="/proc/spl/kstat/zfs/${pool}/txgs"
    # Get last N committed TXGs for averages
    local recent=$(tail -20 "$txg_file" | grep ' C ' | tail -10)

    if [[ -n "$recent" ]]; then
        local avg_stime=$(echo "$recent" | awk '{sum+=$12; n++} END {if(n>0) print int(sum/n); else print 0}')
        local avg_written=$(echo "$recent" | awk '{sum+=$6; n++} END {if(n>0) print int(sum/n); else print 0}')
        local max_stime=$(echo "$recent" | awk 'BEGIN{max=0} {if($12>max)max=$12} END{print max}')
        local avg_mbps=$(echo "$recent" | awk '{if($12>0) {sum+=$6*953.674/$12; n++}} END {if(n>0) printf "%.1f", sum/n; else print "0"}')

        echo -e "${DIM}  └─ Last 10 avg: sync=$(human_time_ns $avg_stime), written=$(human_bytes $avg_written), max_sync=$(human_time_ns $max_stime), avg_MB/s=${avg_mbps}${NC}"
    fi
}

print_keys() {
    echo -e "${DIM}Keys: [d/D]irty [r/R]ead [w/W]ritten [o/O]pen q[u/U]eue w[a/A]it [s/S]ync [m/M]b/s  [n]one  [q]uit  [h]elp  [↑/↓]page  (lower=asc, UPPER=desc)${NC}"
}

get_page_indicator() {
    if [[ $TOTAL_TXGS -gt 0 && "$SORT_COL" != "none" ]]; then
        local current_page=$(( (PAGE_OFFSET / TXG_COUNT) + 1 ))
        local total_pages=$(( (TOTAL_TXGS + TXG_COUNT - 1) / TXG_COUNT ))
        local end_item=$((PAGE_OFFSET + TXG_COUNT))
        [[ $end_item -gt $TOTAL_TXGS ]] && end_item=$TOTAL_TXGS
        echo " [${PAGE_OFFSET}+${TXG_COUNT} of ${TOTAL_TXGS}]"
    fi
}

# Show immediate feedback when key is pressed
show_key_feedback() {
    local key="$1"
    local desc=""
    local dir=""

    case "$key" in
        n|N) desc="none (recent)" ;;
        d)   desc="DIRTY"; dir="▲" ;;
        D)   desc="DIRTY"; dir="▼" ;;
        r)   desc="READ"; dir="▲" ;;
        R)   desc="READ"; dir="▼" ;;
        w)   desc="WRITTEN"; dir="▲" ;;
        W)   desc="WRITTEN"; dir="▼" ;;
        o)   desc="OPEN"; dir="▲" ;;
        O)   desc="OPEN"; dir="▼" ;;
        u)   desc="QUEUE"; dir="▲" ;;
        U)   desc="QUEUE"; dir="▼" ;;
        a)   desc="WAIT"; dir="▲" ;;
        A)   desc="WAIT"; dir="▼" ;;
        s)   desc="SYNC"; dir="▲" ;;
        S)   desc="SYNC"; dir="▼" ;;
        m)   desc="MB/s"; dir="▲" ;;
        M)   desc="MB/s"; dir="▼" ;;
        q|Q) desc="quit" ;;
        h|H) desc="help" ;;
        arrow_down) desc="page ↓" ;;
        arrow_up)   desc="page ↑" ;;
        *)   return ;;  # Unknown key, no feedback
    esac

    # Show feedback at top-right area (row 1, save/restore cursor)
    # Using reverse video for visibility
    printf '\033[s'                          # Save cursor position
    printf '\033[1;60H'                      # Move to row 1, column 60
    printf '\033[7m'                         # Reverse video (highlighted)
    printf ' %s → %s %s ' "$key" "$desc" "$dir"
    printf '\033[0m'                         # Reset attributes
    printf '\033[u'                          # Restore cursor position
}

handle_key() {
    local key="$1"
    case "$key" in
        n|N) SORT_COL="none"; PAGE_OFFSET=0; LAST_KEY="$key" ;;  # Reset to default (recent TXGs)
        d) SORT_COL="dirty";   SORT_REV=0; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        D) SORT_COL="dirty";   SORT_REV=1; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        r) SORT_COL="read";    SORT_REV=0; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        R) SORT_COL="read";    SORT_REV=1; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        w) SORT_COL="written"; SORT_REV=0; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        W) SORT_COL="written"; SORT_REV=1; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        o) SORT_COL="open";    SORT_REV=0; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        O) SORT_COL="open";    SORT_REV=1; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        u) SORT_COL="queue";   SORT_REV=0; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        U) SORT_COL="queue";   SORT_REV=1; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        a) SORT_COL="wait";    SORT_REV=0; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        A) SORT_COL="wait";    SORT_REV=1; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        s) SORT_COL="sync";    SORT_REV=0; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        S) SORT_COL="sync";    SORT_REV=1; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        m) SORT_COL="mbps";    SORT_REV=0; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        M) SORT_COL="mbps";    SORT_REV=1; PAGE_OFFSET=0; LAST_KEY="$key" ;;
        arrow_down)
            # Next page (only when sorting)
            if [[ "$SORT_COL" != "none" ]]; then
                local new_offset=$((PAGE_OFFSET + TXG_COUNT))
                if [[ $new_offset -lt $TOTAL_TXGS ]]; then
                    PAGE_OFFSET=$new_offset
                fi
            fi
            LAST_KEY="$key"
            ;;
        arrow_up)
            # Previous page
            if [[ "$SORT_COL" != "none" ]]; then
                PAGE_OFFSET=$((PAGE_OFFSET - TXG_COUNT))
                [[ $PAGE_OFFSET -lt 0 ]] && PAGE_OFFSET=0
            fi
            LAST_KEY="$key"
            ;;
        q|Q) cleanup; exit 0 ;;
        h|H)
            clear
            show_help
            ;;
    esac
}

sort_txg_data() {
    local txg_file="$1"
    get_sort_info

    # Default mode: just show most recent TXGs (tail)
    if [[ "$SORT_COL" == "none" ]]; then
        tail -$((TXG_COUNT + 1)) "$txg_file" | grep -v '^txg'
        return
    fi

    local sort_opts="-n"
    [[ $SORT_REV -eq 1 ]] && sort_opts="-rn"

    # Sort ALL TXGs in history, then paginate for display
    # This allows finding e.g. highest MB/s across entire history, not just recent
    if [[ "$SORT_COL" == "mbps" ]]; then
        grep -v '^txg' "$txg_file" | \
            awk '{mbps=0; if($12>0 && $6>0) mbps=$6*953.674/$12; print mbps, $0}' | \
            sort $sort_opts -k1 | tail -n +$((PAGE_OFFSET + 1)) | head -$TXG_COUNT | cut -d' ' -f2-
    else
        grep -v '^txg' "$txg_file" | \
            sort $sort_opts -k${SORT_FIELD} | tail -n +$((PAGE_OFFSET + 1)) | head -$TXG_COUNT
    fi
}

cleanup() {
    tput cnorm  # Show cursor
    stty echo   # Restore echo
    rm -f "$TMP_FILE" 2>/dev/null
}

trap cleanup EXIT

# Create temp file for atomic display
TMP_FILE=$(mktemp /tmp/top_txg.XXXXXX)

# Convert pools string to array
read -ra POOL_ARRAY <<< "$POOLS"

# Compute boot epoch ONCE for stable timestamp conversions
# boot_epoch = current_epoch - uptime_seconds
read uptime_sec _ < /proc/uptime
BOOT_EPOCH=$(echo "$(date +%s) - $uptime_sec" | bc | cut -d. -f1)

# Setup terminal
tput civis  # Hide cursor
stty -echo  # Disable echo
clear       # Initial clear only

# Main loop
while true; do
    # Calculate TOTAL_TXGS outside the display block (to avoid subshell issues)
    # Use the first valid pool for pagination
    TOTAL_TXGS=0
    if [[ "$SORT_COL" != "none" ]]; then
        for pool in "${POOL_ARRAY[@]}"; do
            txg_file="/proc/spl/kstat/zfs/${pool}/txgs"
            if [[ -f "$txg_file" ]]; then
                TOTAL_TXGS=$(grep -cv '^txg' "$txg_file")
                break
            fi
        done
    fi

    # Build output in temp file first (atomic display, reduces flicker)
    {
        # Build sort indicator for title
        get_sort_info
        title_sort=""
        if [[ "$SORT_COL" != "none" ]]; then
            sort_dir="▲"
            [[ $SORT_REV -eq 1 ]] && sort_dir="▼"
            title_sort="                 ${SORT_NAME:0:1} → ${SORT_NAME} ${sort_dir}"
        fi
        echo -e "${BOLD}ZFS TXG Monitor${NC} ${DIM}(refresh: ${INTERVAL}s, ${TXG_COUNT} TXGs/pool)${title_sort}${NC}"
        print_keys

        for pool in "${POOL_ARRAY[@]}"; do
            txg_file="/proc/spl/kstat/zfs/${pool}/txgs"

            if [[ ! -f "$txg_file" ]]; then
                echo -e "${RED}Pool '$pool' not found${NC}"
                continue
            fi

            print_header "$pool"

            # Show sorted TXGs
            sort_txg_data "$txg_file" | while read line; do
                format_txg "$line"
            done

            show_summary "$pool"
            echo ""
        done
    } > "$TMP_FILE"

    # Display atomically: move cursor home, overwrite content, clear leftovers
    # This avoids flash (unlike clear-then-write)
    printf '\033[H'      # cursor to home (0,0)
    cat "$TMP_FILE"      # overwrite in place
    printf '\033[J'      # clear from cursor to end of screen (leftover lines)

    # Non-blocking read for key input
    if read -rsn1 -t "$INTERVAL" key; then
        # Handle escape sequences (arrow keys)
        if [[ "$key" == $'\x1b' ]]; then
            read -rsn2 -t 0.1 seq
            case "$seq" in
                '[A') key="arrow_up" ;;
                '[B') key="arrow_down" ;;
                *)    key="" ;;  # Ignore other escape sequences
            esac
        fi
        if [[ -n "$key" ]]; then
            show_key_feedback "$key"  # Immediate visual feedback
            handle_key "$key"
        fi
    fi
done
