#!/usr/bin/env python3
"""
USB Queue Monitor with P90 visualization
Shows current depth + p90 (not max) for more meaningful picture
Uses reservoir sampling to track long-term statistics with fixed memory
"""

import os
import time
import random
from datetime import datetime

DEVICES = ["sdc", "sdd", "sde", "sdf", "sdg"]
SAMPLE_INTERVAL = 0.1  # seconds
RESERVOIR_SIZE = 10000  # Keep 10k samples for accurate P90 estimation
MAX_QUEUE = 30

class ReservoirSampler:
    """Reservoir sampling for maintaining representative sample with fixed memory"""
    def __init__(self, size):
        self.size = size
        self.reservoir = []
        self.count = 0  # Total samples seen

    def add(self, value):
        self.count += 1
        if len(self.reservoir) < self.size:
            self.reservoir.append(value)
        else:
            # Randomly replace elements with decreasing probability
            j = random.randint(0, self.count - 1)
            if j < self.size:
                self.reservoir[j] = value

    def get_samples(self):
        return self.reservoir

    def get_count(self):
        return self.count

# Store sample history per device using reservoir sampling
samplers = {dev: ReservoirSampler(RESERVOIR_SIZE) for dev in DEVICES}

def get_inflight(device):
    """Read current in-flight IO count for device"""
    try:
        with open(f"/sys/block/{device}/inflight", "r") as f:
            parts = f.read().strip().split()
            return int(parts[0]) + int(parts[1])  # read + write
    except (FileNotFoundError, IndexError, ValueError):
        return 0

def calc_percentile(data, pct):
    """Calculate percentile from list of values"""
    if not data:
        return 0
    sorted_data = sorted(data)
    idx = int(len(sorted_data) * pct / 100)
    idx = max(0, min(idx, len(sorted_data) - 1))
    return sorted_data[idx]

def make_bar(current, p90, width=30):
    """Create visualization bar"""
    bar = ""
    for i in range(1, width + 1):
        if i <= current:
            bar += "█"
        elif i <= p90:
            bar += "░"
        else:
            bar += "-"
    return bar

def format_count(count):
    """Format large sample counts in human-readable format"""
    if count >= 1_000_000_000:
        return f"{count / 1_000_000_000:.1f}B"
    elif count >= 1_000_000:
        return f"{count / 1_000_000:.1f}M"
    elif count >= 1_000:
        return f"{count / 1_000:.1f}K"
    else:
        return str(count)

def main():
    print("USB Queue Monitor (P90) - Ctrl+C to stop")
    print("Building initial sample set...")
    time.sleep(0.5)

    try:
        while True:
            # Collect samples
            for dev in DEVICES:
                current = get_inflight(dev)
                samplers[dev].add(current)

            # Clear screen and display
            os.system('clear')
            print(f"USB Queue Monitor (P90) - {datetime.now().strftime('%a %b %d %H:%M:%S %Y')}")
            print("=" * 60)
            print(f"{'Device':<8} {'Current':>8} {'P90':>8}  Utilization")
            print("-" * 60)

            for dev in DEVICES:
                current = get_inflight(dev)
                p90 = calc_percentile(samplers[dev].get_samples(), 90)
                bar = make_bar(current, p90, MAX_QUEUE)
                print(f"{dev:<8} {current:>4}/{MAX_QUEUE:<3} {p90:>4}/{MAX_QUEUE:<3}  [{bar}]")

            print()
            print(f"Legend: █ = current  ░ = p90 (long-term)  - = unused")
            sample_count = samplers[DEVICES[0]].get_count()
            reservoir_size = len(samplers[DEVICES[0]].get_samples())
            print(f"Samples: {format_count(sample_count)} total ({reservoir_size} in reservoir)")

            time.sleep(SAMPLE_INTERVAL)

    except KeyboardInterrupt:
        print("\nStopped.")

if __name__ == "__main__":
    main()
