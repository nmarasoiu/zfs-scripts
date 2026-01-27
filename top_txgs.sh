#!/bin/bash
# Usage: ./top_txgs.sh [time_column] [count]
# time_column: otime, qtime, wtime, or stime (default: wtime)
# count: how many results (default: 3)

col=${1:-wtime}
n=${2:-3}

file="/proc/spl/kstat/zfs/hddpool/txgs"

case $col in
  otime) k=9 ;;
  qtime) k=10 ;;
  wtime) k=11 ;;
  stime) k=12 ;;
  *) echo "Unknown column: $col"; exit 1 ;;
esac

awk 'NR==1 {print; next} NR>1' "$file" | sort -k$k -rn | head -n $((n+1))
