#!/bin/bash
# ZFS Resilver Tuning Report - "Efficient Sprint" Mode
# Current Date: $(date)

printf "\n========================================================================\n"
printf "   ZFS RESILVER TUNING REPORT: CURRENT STATE vs. PROPOSED (STORJ)       \n"
printf "========================================================================\n"
printf "%-42s %-15s %-15s\n" "PARAMETER" "CURRENT" "PROPOSED"
printf -- "------------------------------------------ --------------- ---------------\n"

# Define the parameters and their proposed values
declare -A proposals=(
    ["zfs_vdev_scrub_max_active"]="40"
    ["zfs_vdev_scrub_min_active"]="10"
    ["zfs_vdev_max_active"]="60"
    ["zfs_vdev_async_read_max_active"]="29"
    ["zfs_vdev_async_read_min_active"]="10"
    ["zfs_vdev_aggregation_limit"]="4194304"
    ["zfs_vdev_aggregation_limit_non_rotating"]="4194304"
    ["zfs_scan_vdev_limit"]="8388608"
    ["zfs_txg_timeout"]="30"
    ["zfs_resilver_min_time_ms"]="5000"
    ["zfs_scan_fill_weight"]="0"
    ["zfs_dirty_data_max"]="2147483648"
    ["zfs_deadman_synctime_ms"]="1200000"
    ["zfs_deadman_ziotime_ms"]="600000"
    ["zio_slow_io_ms"]="120000"
)

# Loop through and report
for param in "${!proposals[@]}"; do
    current_val=$(cat /sys/module/zfs/parameters/$param 2>/dev/null || echo "N/A")
    printf "%-42s %-15s %-15s\n" "$param" "$current_val" "${proposals[$param]}"
done

printf "\n========================================================================\n"
printf "   SCSI DISK LAYER TIMEOUTS (RELIABILITY GAP)                           \n"
printf "========================================================================\n"
printf "%-42s %-15s %-15s\n" "DEVICE" "CURRENT" "PROPOSED"
printf -- "------------------------------------------ --------------- ---------------\n"

for dev in /sys/block/sd*/device/timeout; do
    disk_name=$(echo $dev | cut -d'/' -f4)
    current_timeout=$(cat $dev)
    printf "%-42s %-15s %-15s\n" "$disk_name" "$current_timeout" "480"
done

printf "\n========================================================================\n"
printf "   ZFS POOL FAILMODE (STABILITY)                                        \n"
printf "========================================================================\n"
zpool get failmode hddpool ssdpool

printf "\n[REPORT COMPLETE]\n"
