# Syscall Latency Monitor — Future Ideas

## Trend / Delta Indicators

Show rate-of-change alongside values. Track stats from the previous display
interval and emit arrows or delta percentages:

```
tor/read  24.6K/s +12%   p99: 5µs ↑
```

Could color-code rows whose p99 jumped significantly between intervals (e.g.
red if p99 doubled). Requires keeping a snapshot of previous-frame stats —
a map[sketchKey]prevFrame with avg/p99/rate from the last render cycle.

## Coefficient of Variation (CoV) Column

CoV = stddev / mean, expressible without storing all values:
stddev = sqrt(E[x²] - E[x]²). Requires tracking sum-of-squares in
simpleStats (one extra uint64 field). High CoV flags bimodal distributions —
e.g. futex (CoV ~200%: 1µs fast-path vs 20ms contention) vs read (CoV ~30%).

A `cov` column showing percentage would let users spot "spiky" syscalls at a
glance. Sort by CoV to surface the most variable ones.

## Global Max Latency in Header

`globalStats.max` is tracked but not shown. Adding it to the header gives
instant visibility into the single worst syscall event:

```
Syscall Latency Monitor - ... | worst: 35.2s (storagenode/futex)
```

Requires tracking the identity (process + syscall) of the global max, not
just the value. Could store a `globalMaxKey sketchKey` alongside globalStats.

## Total Events/s Throughput in Header

The LIFETIME(all) row already shows aggregate rate, but repeating it
prominently in the header makes it visible without scrolling:

```
Syscall Latency Monitor - ... | 56.3K events/s
```

## Drop Rate as Percentage

Show drops relative to total events processed, making it easy to assess
whether drops are significant:

```
Drops: 83 (0.004% of 2.1M) [ring:0 miss:83 short:0]
```

## Color-Coded Latency Cells

Apply ANSI color to latency values based on thresholds:
- Green: below median baseline for that syscall
- Yellow: above p90
- Red: above p99
- Bold red: at or near max

Thresholds could be per-syscall (adaptive) or configurable via flags. Would
need to detect terminal color support and provide a `--no-color` flag.

## Sparklines in Process Panel

Use Unicode block characters (▁▂▃▄▅▆▇█) to show a mini per-second rate
history in the process panel:

```
 PROCESS         RATE   TOTAL    TIME   TREND
 tor              49.8K/s    1.8M    1.8s  ▃▅▇▆▅▄▃▂
```

Requires a circular buffer of per-second samples per process (e.g. last 8
values). Adds ~8 chars to the panel width.

## Latency Distribution Shape Indicator

Flag bimodal or heavy-tailed distributions with a symbol:
- `~` unimodal (p99/p50 < 10)
- `!` heavy tail (p99/p50 > 100)
- `‼` bimodal (detectable via DDSketch bucket analysis)

Could be a 1-char column next to the latency values.

## Per-CPU Ring Buffer Breakdown

Currently ring stats are aggregated. Showing per-CPU utilization could
identify hot CPUs:

```
Ring per-CPU: [cpu0: 12% cpu1: 45% cpu2: 8% cpu3: 3%]
```
