#!/bin/bash

# ==========================================
# CONFIGURATION
# ==========================================

# 1. POOLS
# Leave empty ("") to programmatically find ALL pools.
# Or, enter a hardcoded list separated by spaces: "hddpool ssdpool"
TARGET_POOLS=""

# 2. PROPERTIES
# The ZFS properties you want to display, comma-separated.
# Common options: recordsize, compression, special_small_blocks, atime, used
PARAMS="name,special_small_blocks,recordsize"

# ==========================================
# LOGIC
# ==========================================

# If TARGET_POOLS is empty, auto-detect all pools present on the system
if [ -z "$TARGET_POOLS" ]; then
    echo "Creating report for ALL detected pools..."
    # Get list of all pools
    TARGET_POOLS=$(zpool list -H -o name)
else
    echo "Creating report for HARDCODED pools: $TARGET_POOLS..."
fi

echo ""

# Use 'zfs list' to generate the table
# -d 0 : Only look at the root dataset of the pool (depth 0), ignoring children
# -o   : Output the specific columns we defined above
zfs list -d 0 -o "$PARAMS" $TARGET_POOLS
