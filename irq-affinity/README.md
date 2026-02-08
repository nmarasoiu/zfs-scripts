# CPU/IRQ Affinity Tuning — eno1 (e1000e)

## Problem

CPU2 at ~51% cumulative busy vs ~41% for others (measured via /proc/stat).
Live snapshots show CPU2 at 46% busy while others at 12-20%.

Root cause: Intel e1000e NIC (eno1) has a **single RX/TX queue**.
All hardware interrupts (3.36B cumulative) land on CPU2. On top of that,
the RPS mask was misconfigured:

    rps_cpus = 7 (hex) = 0111 (binary) = CPUs 0, 1, 2
                         ^^^^
                         CPU2 INCLUDED  (hardirq handler — double duty)
                         CPU3 EXCLUDED  (idle bystander)

## Hardware

    CPU:  Intel i5-6500, 4 cores, no HT
    NIC:  e1000e (eno1), single queue, PCI 0000:00:1f.6
    SATA: ahci, PCI 0000:00:17.0
    USB:  xhci_hcd, PCI 0000:00:14.0

## Solution

Dedicate CPU2 to eno1 hardware interrupts. Move everything else off.

| Setting              | Before            | After              |
|----------------------|-------------------|--------------------|
| eno1 hardirq         | CPU2 (implicit)   | CPU2 (pinned)      |
| eno1 RPS (softirqs)  | 7 = CPUs 0,1,2    | b = CPUs 0,1,3     |
| AHCI IRQ             | any → CPU1        | CPU1 (pinned)      |
| XHCI IRQ             | any → CPU3        | CPU3 (pinned)      |
| tor relay            | any               | CPUs 0,1,3         |
| rps_sock_flow_entries| 0                 | 131072 (enables RFS)|

## Persistence — 3 layers

1. **udev rule** (`/etc/udev/rules.d/99-network-tuning.rules`)
   Sets RPS mask and RFS flow count when eno1 appears.
   Reliable — sysfs writes work fine from udev context.

2. **networkd-dispatcher** (`/etc/networkd-dispatcher/routable.d/50-irq-affinity`)
   Pins IRQ affinity when eno1 becomes routable.
   Best-effort — can race with early boot or miss events.

3. **systemd overrides** (`/etc/systemd/system/tor@*.service.d/cpu-affinity.conf`)
   Sets `CPUAffinity=0 1 3` for tor service instances.

Layer 2 is the one that can race. `post-reboot.sh` is the safety net.

## Scripts

- `setup.sh`        — apply live + install persistent configs (run once)
- `post-reboot.sh`  — verify & fix after SSH login (run each reboot)

## Quick verify

    cat /proc/irq/$(cat /sys/class/net/eno1/device/irq)/smp_affinity_list
    # → 2

    cat /sys/class/net/eno1/queues/rx-0/rps_cpus
    # → b  (or 0000000b)

    mpstat -P ALL 2 1
    # softirq% should be spread across 0,1,3 — not piled on 2

## Future: interrupt coalescing

e1000e supports hardware interrupt coalescing (rx-usecs, tx-usecs via
ethtool -C). This reduces interrupt rate at the cost of latency. Worth
tuning separately — see ethtool -c eno1 for current settings.
