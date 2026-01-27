#!/bin/bash
# txg_live.sh - Human-readable ZFS TXG monitor
# Shows ongoing transaction groups with human-friendly units

# Default pools (space-separated) or pass as arguments
POOLS="${1:-hddpool ssdpool}"
INTERVAL="${2:-2}"
TXG_COUNT="${3:-20}"  # TXGs per pool

# Colors
RED='\033[0;31m'
YELLOW='\033[1;33m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
DIM='\033[2m'
NC='\033[0m'

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
    case $1 in
        O) echo -e "${GREEN}OPEN${NC}    " ;;
        Q) echo -e "${YELLOW}QUIESCE${NC} " ;;
        S) echo -e "${RED}SYNCING${NC} " ;;
        C) echo -e "${DIM}done${NC}    " ;;
        *) echo -e "${DIM}$1${NC}      " ;;
    esac
}

print_header() {
    local pool="$1"
    echo -e "${BOLD}${CYAN}${pool}${NC}"
    echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${BOLD}TXG        STATE     DIRTY     READ      WRITTEN   R/W OPS    OPEN     QUEUE    WAIT     SYNC     MB/s${NC}"
    echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
}

format_txg() {
    local line="$1"
    local txg=$(echo "$line" | awk '{print $1}')
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
    # MB/s = (nwritten / 1048576) / (stime / 1e9) = nwritten * 953.674 / stime
    local mbps_h="-"
    if (( stime > 0 && nwritten > 0 )); then
        local mbps=$(echo "scale=1; $nwritten * 953.674 / $stime" | bc)
        mbps_h="${mbps}"
    fi

    # Highlight active TXGs
    if [[ "$state" == "O" || "$state" == "S" || "$state" == "Q" ]]; then
        printf "${CYAN}%-10s${NC} %s %-9s %-9s %-9s %4d/%-5d %-8s %-8s %-8s %-8s %s\n" \
            "$txg" "$state_str" "$dirty_h" "$read_h" "$written_h" "$reads" "$writes" \
            "$otime_h" "$qtime_h" "$wtime_h" "$stime_h" "$mbps_h"
    else
        printf "${DIM}%-10s${NC} %s %-9s %-9s %-9s %4d/%-5d %-8s %-8s %-8s %-8s %s\n" \
            "$txg" "$state_str" "$dirty_h" "$read_h" "$written_h" "$reads" "$writes" \
            "$otime_h" "$qtime_h" "$wtime_h" "$stime_h" "$mbps_h"
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

# Convert pools string to array
read -ra POOL_ARRAY <<< "$POOLS"

# Main loop
clear
echo -e "${BOLD}ZFS TXG Monitor${NC} ${DIM}(refresh: ${INTERVAL}s, ${TXG_COUNT} TXGs/pool)${NC}"
echo -e "${DIM}Columns: dirty=pending writes, otime=open duration, qtime=quiesce, wtime=wait, stime=sync, MB/s=written/sync${NC}"

while true; do
    tput cup 2 0  # Move cursor to line 3

    for pool in "${POOL_ARRAY[@]}"; do
        txg_file="/proc/spl/kstat/zfs/${pool}/txgs"

        if [[ ! -f "$txg_file" ]]; then
            echo -e "${RED}Pool '$pool' not found${NC}"
            continue
        fi

        print_header "$pool"

        # Show last N TXGs
        tail -$((TXG_COUNT + 1)) "$txg_file" | while read line; do
            format_txg "$line"
        done

        show_summary "$pool"
        echo ""
    done

    # Clear any leftover lines
    tput ed

    sleep "$INTERVAL"
done
