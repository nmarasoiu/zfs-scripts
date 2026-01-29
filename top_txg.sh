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

Interactive Keys:
  t   Sort by TXG number
  d   Sort by Dirty bytes
  r   Sort by Read bytes
  w   Sort by Written bytes
  o   Sort by Open time
  u   Sort by Queue time
  a   Sort by Wait time
  s   Sort by Sync time
  m   Sort by MB/s
  R   Reverse sort order (toggle asc/desc)
  q   Quit

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
SORT_COL="txg"      # Default sort column
SORT_REV=0          # 0=ascending, 1=descending
SORT_FIELD=1        # awk field number for sorting

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
        txg)     SORT_FIELD=1;  SORT_NAME="TXG" ;;
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
    elif (( ns >= 1000000 )); then
        printf "%.0fms" $(echo "scale=0; $ns/1000000" | bc)
    elif (( ns >= 1000 )); then
        printf "%.0fµs" $(echo "scale=0; $ns/1000" | bc)
    else
        printf "%dns" $ns
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
    local sort_dir="▲"
    [[ $SORT_REV -eq 1 ]] && sort_dir="▼"

    # Column widths - must match data format exactly
    # DATE=11, TIME=9, TXG=10, STATE=9, DIRTY=10, READ=10, WRITTEN=10, OPS=13, OPEN=8, QUEUE=8, WAIT=8, SYNC=8, MB/s=8
    local sep="━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo -e "${BOLD}${CYAN}${pool}${NC}  ${DIM}[sorted by ${SORT_NAME} ${sort_dir}]${NC}"
    echo -e "${BOLD}${sep}${NC}"
    printf "${BOLD}%-11s %-9s %-10s %-9s %-10s %-10s %-10s %-13s %-8s %-8s %-8s %-8s %-8s %s${NC}\n" \
        "DATE" "TIME" "TXG" "STATE" "DIRTY" "READ" "WRITTEN" "R/W OPS" "OPEN" "QUEUE" "WAIT" "SYNC" "MB/s" "DURATION"
    echo -e "${BOLD}${sep}${NC}"
}

# Convert birth hrtime (ns since boot) to wall-clock timestamp
# Uses cached CURRENT_HRTIME_NS and CURRENT_EPOCH (set in main loop)
birth_to_wallclock() {
    local birth_hrtime=$1
    local seconds_ago=$(( (CURRENT_HRTIME_NS - birth_hrtime) / 1000000000 ))
    local birth_epoch=$((CURRENT_EPOCH - seconds_ago))
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
    local birth_date=${birth_dt% *}
    local birth_time=${birth_dt#* }

    # Calculate completion time for finished TXGs
    # completion_hrtime = birth + otime + qtime + wtime + stime
    local duration_str="-"
    if [[ "$state" == "C" ]]; then
        local completion_hrtime=$((birth + otime + qtime + wtime + stime))
        local completion_dt=$(birth_to_wallclock "$completion_hrtime")
        local end_time=${completion_dt#* }
        duration_str="${birth_time} -> ${end_time}"
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
    echo -e "${DIM}Keys: [t]xg [d]irty [r]ead [w]ritten [o]pen q[u]eue w[a]it [s]ync [m]b/s  [R]everse  [q]uit  [h]elp${NC}"
}

handle_key() {
    local key="$1"
    case "$key" in
        t) SORT_COL="txg" ;;
        d) SORT_COL="dirty" ;;
        r) SORT_COL="read" ;;
        w) SORT_COL="written" ;;
        o) SORT_COL="open" ;;
        u) SORT_COL="queue" ;;
        a) SORT_COL="wait" ;;
        s) SORT_COL="sync" ;;
        m) SORT_COL="mbps" ;;
        R) SORT_REV=$((1 - SORT_REV)) ;;
        q) cleanup; exit 0 ;;
        h)
            clear
            show_help
            ;;
    esac
}

sort_txg_data() {
    local txg_file="$1"
    get_sort_info

    local sort_opts="-n"
    [[ $SORT_REV -eq 1 ]] && sort_opts="-rn"

    # Sort ALL TXGs in history, then take top N for display
    # This allows finding e.g. highest MB/s across entire history, not just recent
    if [[ "$SORT_COL" == "mbps" ]]; then
        grep -v '^txg' "$txg_file" | \
            awk '{mbps=0; if($12>0 && $6>0) mbps=$6*953.674/$12; print mbps, $0}' | \
            sort $sort_opts -k1 | head -$TXG_COUNT | cut -d' ' -f2-
    else
        grep -v '^txg' "$txg_file" | \
            sort $sort_opts -k${SORT_FIELD} | head -$TXG_COUNT
    fi
}

cleanup() {
    tput cnorm  # Show cursor
    stty echo   # Restore echo
}

trap cleanup EXIT

# Convert pools string to array
read -ra POOL_ARRAY <<< "$POOLS"

# Setup terminal
tput civis  # Hide cursor
stty -echo  # Disable echo

# Main loop
clear
echo -e "${BOLD}ZFS TXG Monitor${NC} ${DIM}(refresh: ${INTERVAL}s, ${TXG_COUNT} TXGs/pool)${NC}"
print_keys

while true; do
    tput cup 2 0  # Move cursor to line 3

    # Cache current time references for birth->wallclock conversion
    read uptime_sec _ < /proc/uptime
    CURRENT_HRTIME_NS=$(echo "$uptime_sec * 1000000000" | bc | cut -d. -f1)
    CURRENT_EPOCH=$(date +%s)

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

    # Clear any leftover lines
    tput ed

    # Non-blocking read for key input
    if read -rsn1 -t "$INTERVAL" key; then
        handle_key "$key"
    fi
done
