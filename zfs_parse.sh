#!/bin/bash

# Check if file is provided
if [ -z "$1" ]; then
    echo "Usage: $0 <path_to_zfs_config>"
    exit 1
fi

echo "Parsing $1..."
echo ""

awk '
BEGIN {
    # Define the table header format
    fmt = "%-32s | %-15s | %-10s\n"
    printf fmt, "Parameter", "Raw Value", "Size (GB)"
    print "---------------------------------|-----------------|----------"
}

/^options zfs/ {
    # $3 contains "key=value"
    # We split it by the "=" sign
    split($3, pair, "=")
    key = pair[1]
    val = pair[2]

    # Define 1 GB in bytes (1024^3)
    gb_bytes = 1073741824
    
    # Logic: If value is a number and greater than 10MB, assume it is bytes 
    # and convert to GB. Otherwise, just print the raw value.
    if (val ~ /^[0-9]+$/ && val > 10485760) {
        gb_val = val / gb_bytes
        printf fmt, key, val, sprintf("%.2f GB", gb_val)
    } else {
        # Print small numbers or non-numbers without GB conversion
        printf fmt, key, val, "-"
    }
}
' "$1"
