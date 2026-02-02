#!/bin/bash
# Queue depth inspection across all layers for sd[c-g]

echo "=============================================="
echo "STORAGE STACK QUEUE DEPTHS - $(date)"
echo "=============================================="

for dev in sd{c,d,e,f,g}; do
    [ -b /dev/$dev ] || continue
    
    echo ""
    echo ">>> /dev/$dev <<<"
    
    host=$(readlink -f /sys/block/$dev/device | grep -oP 'host\K[0-9]+')
    
    echo "--- Block Layer (mq-deadline) ---"
    echo "  nr_requests:      $(cat /sys/block/$dev/queue/nr_requests 2>/dev/null)"
    echo "  scheduler:        $(cat /sys/block/$dev/queue/scheduler 2>/dev/null)"
    
    if [ -d /sys/block/$dev/queue/iosched ]; then
        echo "  fifo_batch:       $(cat /sys/block/$dev/queue/iosched/fifo_batch 2>/dev/null)"
        echo "  read_expire:      $(cat /sys/block/$dev/queue/iosched/read_expire 2>/dev/null)"
        echo "  write_expire:     $(cat /sys/block/$dev/queue/iosched/write_expire 2>/dev/null)"
    fi
    
    echo "--- SCSI Device Layer ---"
    echo "  queue_depth:      $(cat /sys/block/$dev/device/queue_depth 2>/dev/null)"
    
    echo "--- SCSI Host (host$host) ---"
    echo "  can_queue:        $(cat /sys/class/scsi_host/host$host/can_queue 2>/dev/null)"
    echo "  cmd_per_lun:      $(cat /sys/class/scsi_host/host$host/cmd_per_lun 2>/dev/null)"
    echo "  proc_name:        $(cat /sys/class/scsi_host/host$host/proc_name 2>/dev/null)"
    
    echo "--- blk-mq tags ---"
    echo "  nr_tags:          $(cat /sys/block/$dev/mq/0/nr_tags 2>/dev/null)"
done

echo ""
echo "=== ZFS VDEV QUEUE SETTINGS ==="
for p in zfs_vdev_async_read_max_active zfs_vdev_async_write_max_active \
         zfs_vdev_sync_read_max_active zfs_vdev_sync_write_max_active \
         zfs_vdev_max_active; do
    printf "  %-40s %s\n" "$p:" "$(cat /sys/module/zfs/parameters/$p 2>/dev/null)"
done
