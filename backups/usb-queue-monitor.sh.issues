#!/bin/bash
# USB Queue Monitor with P90 visualization
# Shows current depth + p90 (not max) for more meaningful picture

DEVICES="sdc sdd sde sdf sdg"
SAMPLE_INTERVAL=0.1
HISTORY_SIZE=100  # Keep last 100 samples per device (~10 seconds at 0.1s interval)

# Arrays for storing sample history
declare -A samples
declare -A sample_idx

for dev in $DEVICES; do
    samples[$dev]=""
    sample_idx[$dev]=0
done

# Function to get p90 from space-separated values
get_p90() {
    local data="$1"
    local count=$(echo "$data" | wc -w)
    [ "$count" -eq 0 ] && echo "0" && return
    local idx=$(( (count * 90) / 100 ))
    [ "$idx" -lt 1 ] && idx=1
    echo "$data" | tr ' ' '\n' | sort -n | sed -n "${idx}p"
}

get_current() {
    cat /sys/block/$1/inflight 2>/dev/null | awk '{print $1+$2}'
}

echo "USB Queue Monitor (P90) - Ctrl+C to stop"
echo "Building initial sample set..."
sleep 1

while true; do
    # Collect one sample for each device
    for dev in $DEVICES; do
        current=$(get_current $dev)
        # Append to samples, keep limited history
        samples[$dev]="${samples[$dev]} $current"
        count=$(echo "${samples[$dev]}" | wc -w)
        if [ "$count" -gt "$HISTORY_SIZE" ]; then
            samples[$dev]=$(echo "${samples[$dev]}" | cut -d' ' -f2-)
        fi
    done
    
    # Display
    clear
    echo "USB Queue Monitor (P90) - $(date)"
    echo "================================================"
    printf "%-6s %8s %8s %s\n" "Device" "Current" "P90" "Utilization"
    echo "------------------------------------------------"
    
    for dev in $DEVICES; do
        current=$(get_current $dev)
        p90=$(get_p90 "${samples[$dev]}")
        
        # Visual bar: current=█, up to p90=░, rest=-
        bar=""
        for i in $(seq 1 30); do
            if [ $i -le $current ]; then
                bar="${bar}█"
            elif [ $i -le $p90 ]; then
                bar="${bar}░"
            else
                bar="${bar}-"
            fi
        done
        printf "%-6s %8s %8s [%s]\n" "$dev" "$current/30" "$p90/30" "$bar"
    done
    
    echo
    echo "Legend: █ = current  ░ = p90 (last ${HISTORY_SIZE} samples)  - = unused"
    echo "Samples: $(echo "${samples[sdd]}" | wc -w) (rolling window)"
    
    sleep $SAMPLE_INTERVAL
done
