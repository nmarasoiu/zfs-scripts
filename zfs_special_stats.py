import sys
import re

def parse_size(size_str):
    """Converts ZFS size strings (e.g., 21.8T, 336G) to bytes."""
    if size_str == '-':
        return 0
    
    units = {
        'K': 1024,
        'M': 1024**2,
        'G': 1024**3,
        'T': 1024**4,
        'P': 1024**5,
        'E': 1024**6
    }
    
    # Check for unit at the end
    unit = size_str[-1].upper()
    if unit in units:
        number = float(size_str[:-1])
        return number * units[unit]
    
    # If just a number (unlikely in ZFS output but possible)
    try:
        return float(size_str)
    except ValueError:
        return 0

def format_size(size_bytes):
    """Converts bytes back to human readable ZFS style string."""
    if size_bytes == 0:
        return "-"
    
    for unit in ['B', 'K', 'M', 'G', 'T', 'P', 'E']:
        if size_bytes < 1024 and unit != 'E':
            # Format: keep decimal precision similar to ZFS (usually 2 sig figs or 1 decimal)
            if size_bytes % 1 == 0:
                return f"{int(size_bytes)}{unit}" if unit != 'B' else f"{int(size_bytes)}"
            return f"{size_bytes:.2f}{unit}"
        size_bytes /= 1024.0
    return f"{size_bytes:.2f}E"

def main():
    # Read all lines from Stdin
    lines = sys.stdin.readlines()

    # Regex to capture columns (whitespace separated)
    # We anticipate the structure: NAME SIZE ALLOC FREE ...
    
    special_section = False
    data_rows = []
    
    # Metrics to track
    total_size = 0.0
    total_alloc = 0.0
    total_free = 0.0
    
    print(f"{'SPECIAL VDEV NAME':<40} {'SIZE':>8} {'ALLOC':>8} {'FREE':>8} {'CAP':>6} {'FRAG':>6}")
    print("-" * 80)

    for line in lines:
        parts = line.split()
        if not parts:
            continue
            
        name = parts[0]

        # Detect start of special section
        if name == 'special':
            special_section = True
            continue # The header line itself usually has no stats
        
        # Detect end of special section (start of logs, cache, spares, dedup, or another pool)
        # We assume indented lines are children; unindented keywords mark end.
        if special_section:
            if name in ['logs', 'cache', 'spares', 'dedup', 'mirror', 'raidz'] and not line.startswith(' '):
                special_section = False
                continue
            
            # If we hit a new pool name (unindented), stop
            if not line.startswith(' ') and name != 'special':
                special_section = False
                continue

            # Process rows inside special section
            # We look for rows that have valid stats (columns 1, 2, 3)
            if len(parts) >= 4:
                size_str = parts[1]
                alloc_str = parts[2]
                free_str = parts[3]
                
                # ZFS usually puts '-' for pure container rows in 'list -v'
                # We only want rows that have actual numbers
                if size_str != '-':
                    size_bytes = parse_size(size_str)
                    alloc_bytes = parse_size(alloc_str)
                    free_bytes = parse_size(free_str)
                    
                    total_size += size_bytes
                    total_alloc += alloc_bytes
                    total_free += free_bytes
                    
                    # Original Cap/Frag if available
                    cap = parts[7] if len(parts) > 7 else "-" # CAP is usually col 7 in standard, 6 in your snippet
                    # Adjusting index based on your snippet:
                    # NAME(0) SIZE(1) ALLOC(2) FREE(3) CKPOINT(4) EXPANDSZ(5) FRAG(6) CAP(7) ...
                    # Wait, your snippet:
                    # NAME SIZE ALLOC FREE CKPOINT EXPANDSZ FRAG CAP DEDUP
                    # wwn.. 41.9G 32.4G 9.11G - - 81% 78.0% ...
                    # So Frag is index 6, Cap is index 7.
                    
                    frag = parts[6]
                    cap = parts[7]

                    print(f"{name:<40} {size_str:>8} {alloc_str:>8} {free_str:>8} {cap:>6} {frag:>6}")

    # Calculate aggregate Cap/Frag
    if total_size > 0:
        total_cap_pct = (total_alloc / total_size) * 100
        total_cap_str = f"{total_cap_pct:.1f}%"
    else:
        total_cap_str = "-"

    # Print Totals
    print("-" * 80)
    print(f"{'TOTALS':<40} {format_size(total_size):>8} {format_size(total_alloc):>8} {format_size(total_free):>8} {total_cap_str:>6} {'-':>6}")

if __name__ == "__main__":
    main()
