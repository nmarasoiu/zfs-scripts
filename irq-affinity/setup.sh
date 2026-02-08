#!/usr/bin/env bash
# Apply IRQ/CPU affinity live AND install persistent configuration.
# Run once as root.
set -euo pipefail

echo "=== Applying live changes ==="

# 1. Fix RPS: steer softirqs to CPUs 0,1,3 — exclude CPU2 (hardirq handler)
echo b > /sys/class/net/eno1/queues/rx-0/rps_cpus
echo "  rps_cpus = $(cat /sys/class/net/eno1/queues/rx-0/rps_cpus)"

# 2. Pin eno1 hardirq to CPU2
ENO1_IRQ=$(cat /sys/class/net/eno1/device/irq)
echo 2 > /proc/irq/"$ENO1_IRQ"/smp_affinity_list
echo "  eno1 IRQ $ENO1_IRQ → CPU2"

# 3. Pin AHCI → CPU1, XHCI → CPU3
AHCI_IRQ=$(awk -F: '/ahci/{gsub(/ /,"",$1); print $1; exit}' /proc/interrupts)
if [ -n "$AHCI_IRQ" ]; then
    echo 1 > /proc/irq/"$AHCI_IRQ"/smp_affinity_list
    echo "  AHCI IRQ $AHCI_IRQ → CPU1"
fi

XHCI_IRQ=$(awk -F: '/xhci/{gsub(/ /,"",$1); print $1; exit}' /proc/interrupts)
if [ -n "$XHCI_IRQ" ]; then
    echo 3 > /proc/irq/"$XHCI_IRQ"/smp_affinity_list
    echo "  XHCI IRQ $XHCI_IRQ → CPU3"
fi

# 4. Enable RFS globally
echo 32768 > /proc/sys/net/core/rps_sock_flow_entries
echo "  rps_sock_flow_entries = 32768"

# 5. Pin tor to CPUs 0,1,3
for pid in $(pgrep -x tor 2>/dev/null); do
    taskset -pc 0,1,3 "$pid"
done

echo ""
echo "=== Installing persistent configuration ==="

# Layer 1: udev rule (RPS + RFS)
cat > /etc/udev/rules.d/99-network-tuning.rules << 'EOF'
# RPS: steer softirqs to CPUs 0,1,3 (exclude CPU2 = eno1 hardirq handler)
# RFS: enable receive flow steering with 32k flows
ACTION=="add", SUBSYSTEM=="net", KERNEL=="eno1", RUN+="/bin/sh -c 'echo b > /sys/class/net/eno1/queues/rx-0/rps_cpus && echo 32768 > /sys/class/net/eno1/queues/rx-0/rps_flow_cnt'"
EOF
echo "  Installed /etc/udev/rules.d/99-network-tuning.rules"

# Layer 2: networkd-dispatcher (IRQ affinity — best effort, can race)
cat > /etc/networkd-dispatcher/routable.d/50-irq-affinity << 'EOF'
#!/bin/bash
# Pin IRQ affinity when eno1 becomes routable.
# Safety net: post-reboot.sh re-checks after SSH login.
[ "$IFACE" = "eno1" ] || exit 0

log() { logger -t irq-affinity "$@"; }

# eno1 hardirq → CPU2
IRQ=$(cat /sys/class/net/eno1/device/irq 2>/dev/null) || exit 0
echo 2 > /proc/irq/"$IRQ"/smp_affinity_list 2>/dev/null

# AHCI (SATA) → CPU1
AHCI=$(awk -F: '/ahci/{gsub(/ /,"",$1); print $1; exit}' /proc/interrupts)
[ -n "$AHCI" ] && echo 1 > /proc/irq/"$AHCI"/smp_affinity_list 2>/dev/null

# XHCI (USB) → CPU3
XHCI=$(awk -F: '/xhci/{gsub(/ /,"",$1); print $1; exit}' /proc/interrupts)
[ -n "$XHCI" ] && echo 3 > /proc/irq/"$XHCI"/smp_affinity_list 2>/dev/null

# RFS global
echo 32768 > /proc/sys/net/core/rps_sock_flow_entries 2>/dev/null

log "eno1 IRQ $IRQ→CPU2 AHCI=${AHCI:-?}→CPU1 XHCI=${XHCI:-?}→CPU3"
EOF
chmod +x /etc/networkd-dispatcher/routable.d/50-irq-affinity
echo "  Installed /etc/networkd-dispatcher/routable.d/50-irq-affinity"

# Layer 2b: sysctl for RFS (guaranteed at boot, no race)
echo "net.core.rps_sock_flow_entries = 32768" > /etc/sysctl.d/90-rfs.conf
echo "  Installed /etc/sysctl.d/90-rfs.conf"

# Layer 3: systemd overrides for tor
for unit in tor@default.service tor@bridge.service; do
    dir="/etc/systemd/system/${unit}.d"
    mkdir -p "$dir"
    cat > "$dir/cpu-affinity.conf" << 'SYSD'
[Service]
CPUAffinity=0 1 3
SYSD
    echo "  Installed override for $unit"
done
systemctl daemon-reload
echo "  Reloaded systemd"

echo ""
echo "Done. Verify with: ./post-reboot.sh"
