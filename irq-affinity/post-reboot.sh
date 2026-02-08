#!/usr/bin/env bash
# Safety-net script: verify and fix IRQ/CPU affinity after reboot.
# Run after SSH login. Idempotent — safe to run anytime.
set -euo pipefail

OK=0
FIXED=0

check_and_fix() {
    local desc="$1" file="$2" expected="$3"
    local current
    current=$(cat "$file" 2>/dev/null | sed 's/^0*//' | tr -d '[:space:]')
    if [ "$current" = "$expected" ]; then
        printf "  OK   %-40s = %s\n" "$desc" "$current"
        OK=$((OK + 1))
    else
        echo "$expected" > "$file"
        printf "  FIX  %-40s %s → %s\n" "$desc" "${current:-?}" "$expected"
        FIXED=$((FIXED + 1))
    fi
}

check_taskset() {
    local desc="$1" pid="$2" expected="$3"
    local current
    current=$(taskset -pc "$pid" 2>/dev/null | awk -F': ' '{print $2}')
    if [ "$current" = "$expected" ]; then
        printf "  OK   %-40s = %s\n" "$desc" "$current"
        OK=$((OK + 1))
    else
        taskset -pc "$expected" "$pid" > /dev/null 2>&1
        printf "  FIX  %-40s %s → %s\n" "$desc" "${current:-?}" "$expected"
        FIXED=$((FIXED + 1))
    fi
}

echo "=== IRQ/CPU Affinity Check ==="

# RPS mask: b = CPUs 0,1,3
check_and_fix "eno1 rps_cpus" \
    /sys/class/net/eno1/queues/rx-0/rps_cpus "b"

# RFS per-queue flow count
check_and_fix "eno1 rps_flow_cnt" \
    /sys/class/net/eno1/queues/rx-0/rps_flow_cnt "32768"

# RFS global
check_and_fix "rps_sock_flow_entries" \
    /proc/sys/net/core/rps_sock_flow_entries "32768"

# eno1 hardirq → CPU2
ENO1_IRQ=$(cat /sys/class/net/eno1/device/irq 2>/dev/null || true)
if [ -n "$ENO1_IRQ" ]; then
    check_and_fix "eno1 IRQ $ENO1_IRQ → CPU2" \
        /proc/irq/"$ENO1_IRQ"/smp_affinity_list "2"
fi

# AHCI → CPU1
AHCI_IRQ=$(awk -F: '/ahci/{gsub(/ /,"",$1); print $1; exit}' /proc/interrupts)
if [ -n "$AHCI_IRQ" ]; then
    check_and_fix "AHCI IRQ $AHCI_IRQ → CPU1" \
        /proc/irq/"$AHCI_IRQ"/smp_affinity_list "1"
fi

# XHCI → CPU3
XHCI_IRQ=$(awk -F: '/xhci/{gsub(/ /,"",$1); print $1; exit}' /proc/interrupts)
if [ -n "$XHCI_IRQ" ]; then
    check_and_fix "XHCI IRQ $XHCI_IRQ → CPU3" \
        /proc/irq/"$XHCI_IRQ"/smp_affinity_list "3"
fi

# tor → CPUs 0,1,3
for pid in $(pgrep -x tor 2>/dev/null); do
    check_taskset "tor (pid $pid) → CPUs 0,1,3" "$pid" "0,1,3"
done

echo ""
echo "$OK ok, $FIXED fixed"
