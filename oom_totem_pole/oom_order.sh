#!/bin/bash
while read -r pid comm; do
  if [ -f /proc/$pid/oom_score ]; then
    printf "%d\t%d\t%s\n" "$pid" "$(cat /proc/$pid/oom_score)" "$comm"
  fi
done < <(ps -e -o pid= -o comm=) | sort -k2 -n
