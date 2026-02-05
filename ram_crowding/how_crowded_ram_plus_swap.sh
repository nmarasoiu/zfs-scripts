awk '/MemTotal:/ {mt=$2} /MemAvailable:/ {ma=$2} /SwapTotal:/ {st=$2} /SwapFree:/ {sf=$2} END {
    printf "RAM: %.1f/%.1f GiB (%.0f%% free)  Swap: %.1f/%.1f GiB (%.0f%% free)\n",
      (mt-ma)/1024/1024, mt/1024/1024, ma*100/mt,
      (st-sf)/1024/1024, st/1024/1024, sf*100/st
  }' /proc/meminfo
