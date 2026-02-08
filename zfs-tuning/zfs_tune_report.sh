#!/bin/bash

# Configuration
CONF_FILE="/etc/modprobe.d/zfs.conf"
SYS_PATH="/sys/module/zfs/parameters"

# Ensure script is run as root
if [ "$EUID" -ne 0 ]; then
  echo "Please run as root"
  exit 1
fi

get_val() {
    local param=$1
    if [ -f "$SYS_PATH/$param" ]; then
        cat "$SYS_PATH/$param"
    else
        echo "N/A"
    fi
}

report() {
    echo -e "\n--- ZFS VDEV Queue Tuning Report ---"
    printf "%-20s | %-10s | %-10s | %-10s | %-10s | %-10s | %-10s\n" "Type" "Read Min" "Read Max" "Write Min" "Write Max" "Scrub Min" "Scrub Max"
    echo "-----------------------------------------------------------------------------------------------------------------"

    # Gather Sync Values
    sr_min=$(get_val "zfs_vdev_sync_read_min_active")
    sr_max=$(get_val "zfs_vdev_sync_read_max_active")
    sw_min=$(get_val "zfs_vdev_sync_write_min_active")
    sw_max=$(get_val "zfs_vdev_sync_write_max_active")
    
    printf "%-20s | %-10s | %-10s | %-10s | %-10s | %-10s | %-10s\n" "Sync (Priority)" "$sr_min" "$sr_max" "$sw_min" "$sw_max" "-" "-"

    # Gather Async Values
    ar_min=$(get_val "zfs_vdev_async_read_min_active")
    ar_max=$(get_val "zfs_vdev_async_read_max_active")
    aw_min=$(get_val "zfs_vdev_async_write_min_active")
    aw_max=$(get_val "zfs_vdev_async_write_max_active")

    printf "%-20s | %-10s | %-10s | %-10s | %-10s | %-10s | %-10s\n" "Async (Bulk)" "$ar_min" "$ar_max" "$aw_min" "$aw_max" "-" "-"
    
    # Gather Scrub/Trim
    sc_min=$(get_val "zfs_vdev_scrub_min_active")
    sc_max=$(get_val "zfs_vdev_scrub_max_active")
    tr_min=$(get_val "zfs_vdev_trim_min_active")
    tr_max=$(get_val "zfs_vdev_trim_max_active")

    echo "-----------------------------------------------------------------------------------------------------------------"
    printf "%-20s | %-10s | %-10s\n" "Maintenance Task" "Min" "Max"
    echo "-------------------------------------------"
    printf "%-20s | %-10s | %-10s\n" "Scrub/Resilver" "$sc_min" "$sc_max"
    printf "%-20s | %-10s | %-10s\n" "Trim/Discard" "$tr_min" "$tr_max"
    echo -e "-------------------------------------------\n"
    
    echo "--- Global Limits ---"
    printf "%-30s: %s\n" "Global Max Active (zfs_vdev_max_active)" "$(get_val zfs_vdev_max_active)"
    printf "%-30s: %s\n" "Dirty Data Max (Bytes)" "$(get_val zfs_dirty_data_max)"
    printf "%-30s: %s\n" "Dirty Data Sync %" "$(get_val zfs_dirty_data_sync_percent)"
    echo ""
}

sync_values() {
    echo "Applying values from $CONF_FILE to running kernel..."
    while read -r line; do
        [[ "$line" =~ ^options[[:space:]]+zfs[[:space:]]+([^=]+)=([[:digit:]]+) ]] || continue
        param="${BASH_REMATCH[1]}"
        val="${BASH_REMATCH[2]}"

        if [ -f "$SYS_PATH/$param" ]; then
            if [ -w "$SYS_PATH/$param" ]; then
                echo "$val" > "$SYS_PATH/$param" && echo "SET: $param -> $val"
            else
                echo "SKIP: $param is READ-ONLY (Requires Reboot)"
            fi
        fi
    done < "$CONF_FILE"
    echo "Done."
}

case "$1" in
    report)
        report
        ;;
    sync)
        sync_values
        ;;
    *)
        echo "Usage: $0 {report|sync}"
        exit 1
        ;;
esac
