#!/bin/bash

# Configuration
CONF_FILE="/etc/modprobe.d/zfs.conf"
SYS_PATH="/sys/module/zfs/parameters"

# Ensure script is run as root
if [ "$EUID" -ne 0 ]; then
  echo "Please run as root"
  exit 1
fi

report() {
    echo -e "\n--- ZFS Parameter Comparison ---"
    printf "%-35s | %-15s | %-15s | %-10s\n" "Parameter" "Config File" "Current Kernel" "Status"
    echo "---------------------------------------------------------------------------------------"

    # Extract parameters from zfs.conf (ignoring comments and empty lines)
    while read -r line; do
        [[ "$line" =~ ^options[[:space:]]+zfs[[:space:]]+([^=]+)=([[:digit:]]+) ]] || continue
        param="${BASH_REMATCH[1]}"
        config_val="${BASH_REMATCH[2]}"

        if [ -f "$SYS_PATH/$param" ]; then
            current_val=$(cat "$SYS_PATH/$param")
            if [ "$config_val" == "$current_val" ]; then
                status="OK"
            else
                status="MISMATCH"
            fi
            printf "%-35s | %-15s | %-15s | %-10s\n" "$param" "$config_val" "$current_val" "$status"
        else
            printf "%-35s | %-15s | %-15s | %-10s\n" "$param" "$config_val" "NOT FOUND" "ERROR"
        fi
    done < "$CONF_FILE"
    echo -e "---------------------------------------------------------------------------------------\n"
}

sync_values() {
    echo "Applying values from $CONF_FILE to running kernel..."
    while read -r line; do
        [[ "$line" =~ ^options[[:space:]]+zfs[[:space:]]+([^=]+)=([[:digit:]]+) ]] || continue
        param="${BASH_REMATCH[1]}"
        val="${BASH_REMATCH[2]}"

        if [ -f "$SYS_PATH/$param" ]; then
            # Check if writeable (some ZFS params are read-only after boot)
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
