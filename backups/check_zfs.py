#!/usr/bin/env python3
"""
ZFS Health Monitor - OK/WARN/NOK status for scrub monitoring
"""

import subprocess
import re

def check_zfs_health():
    try:
        output = subprocess.check_output(["zpool", "status"], stderr=subprocess.STDOUT, text=True)
    except Exception as e:
        print(f"NOK: Error running zpool status: {e}")
        return

    pools = output.split("pool: ")
    warnings = []
    criticals = []

    for pool_data in pools[1:]:
        lines = pool_data.strip().split('\n')
        pool_name = lines[0].strip()

        # Check pool state
        state_match = re.search(r"state:\s+(\w+)", pool_data)
        if state_match:
            state = state_match.group(1)
            if state in ("FAULTED", "UNAVAIL", "SUSPENDED"):
                criticals.append(f"POOL {pool_name}: State is {state}")
            elif state == "DEGRADED":
                warnings.append(f"POOL {pool_name}: State is DEGRADED (redundancy in use)")

        # Check for unrecoverable data errors
        errors_match = re.search(r"errors:\s+(.+)", pool_data)
        if errors_match:
            errors_text = errors_match.group(1).strip()
            if errors_text != "No known data errors":
                criticals.append(f"POOL {pool_name}: UNRECOVERABLE - {errors_text}")

        # Check scrub repaired bytes (WARN if > 0)
        scrub_match = re.search(r"scrub repaired (\d+[A-Z]?)", pool_data)
        if scrub_match:
            repaired = scrub_match.group(1)
            if repaired != "0" and repaired != "0B":
                warnings.append(f"POOL {pool_name}: Scrub repaired {repaired} (redundancy saved you)")

        # Check for FAULTED/UNAVAIL devices (catches devices that aren't ONLINE)
        faulted_devices = re.findall(r"^\s+(\S+)\s+(FAULTED|UNAVAIL|OFFLINE|REMOVED)", pool_data, re.MULTILINE)
        for device, dev_state in faulted_devices:
            criticals.append(f"POOL {pool_name}: Device {device} is {dev_state}")

        # Check vdev error counters (any device state, not just ONLINE)
        vdev_errors = re.findall(r"^\s+(\S+)\s+\w+\s+(\d+)\s+(\d+)\s+(\d+)", pool_data, re.MULTILINE)
        for device, r, w, c in vdev_errors:
            if int(r) > 0 or int(w) > 0 or int(c) > 0:
                warnings.append(f"POOL {pool_name}: {device} has errors (R:{r} W:{w} C:{c})")

    # Output
    print(output)
    print("-" * 40)

    if criticals:
        print("STATUS: NOK")
        for c in criticals:
            print(f"  !! {c}")
        for w in warnings:
            print(f"  ?  {w}")
    elif warnings:
        print("STATUS: WARN")
        for w in warnings:
            print(f"  ?  {w}")
    else:
        print("STATUS: OK")

if __name__ == "__main__":
    check_zfs_health()
