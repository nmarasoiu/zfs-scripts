# psi

Real-time Linux system dashboard: PSI pressure, load averages, per-core CPU utilization with CDF histograms, and ZFS pool health.

## Usage

```
psi [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-cpu` | `40ms` | CPU utilization sample interval |
| `-psi` | `5s` | PSI / load / zpool-state refresh interval |
| `-zpool` | `15s` | `zpool status` subprocess interval |
| `-display` | `100ms` | Screen refresh interval |
| `-cpu-sort` | `le99` | Sort CPU rows: `index`, `current`, `avg`, `le10`..`le90`, `le95`, `le99`. Tiebreakers: `≤99%`, `avg`, cpu index |
| `-batch N` | `0` | Print N frames then exit (0 = loop forever) |
| `-version` | | Print version and exit |

### Examples

```bash
# defaults — fast CPU sampling, moderate PSI/zpool refresh
psi

# very fast CPU sampling (smoothed to ~1 s window automatically)
psi -cpu 0.01s

# slow, low-overhead monitoring
psi -cpu 1s -psi 30s -zpool 60s

# single snapshot for scripting (no ANSI clear-screen)
psi -batch 1

# sort CPU cores by current utilization (busiest first)
psi -cpu-sort current
```

## Display sections

### Load

```
LOAD   │ 1min    │ 5min    │ 15min   │ procs
───────┼─────────┼─────────┼─────────┼───────────
       │  0.45   │  0.38   │  0.42   │ 2/412
```

### PSI pressure (CPU, IO, Memory)

One table per resource. `some` = at least one task stalled; `full` = all tasks stalled.

```
IO     │ avg10   │ avg60   │ avg300  │ total
───────┼─────────┼─────────┼─────────┼───────────
some   │  1.20%  │  0.80%  │  0.50%  │  3.2m
full   │  0.90%  │  0.60%  │  0.30%  │  1.8m
```

### CPU utilization

Per-core and aggregate utilization with a CDF histogram.

```
CPU%   │ current │ avg     │ ≤10%  │ ≤20%  │ ... │ ≤95%  │ ≤99%
───────┼─────────┼─────────┼───────┼───────┼─────┼───────┼───────
all    │  12.3%  │  15.1%  │ 22.0% │ 41.5% │ ... │ 98.2% │ 99.8%
cpu0   │   8.7%  │  14.9%  │ 25.1% │ 44.0% │ ... │ 97.5% │ 99.1%
```

- **current** — sliding-window average of the last ~1 second of samples (see below).
- **avg** — arithmetic mean since process start.
- **≤N%** — CDF: percentage of all samples where utilization was below N%.

### ZFS pool status

Pool health, device tree, and error counts. Non-ONLINE states and
non-zero error counters are highlighted in red.

## How it works

### Architecture

Three independent loops run concurrently, protected by a single mutex:

| Loop | Default rate | What it reads |
|------|-------------|---------------|
| CPU collector | 40 ms | `/proc/stat` |
| PSI / load / pool-state collector | 5 s | `/proc/pressure/{cpu,io,memory}`, `/proc/loadavg`, `/proc/spl/kstat/zfs/*/state` |
| Display | 100 ms | Renders buffered snapshot to stdout |

The `zpool status -sv` subprocess runs at a separate, slower cadence
(`-zpool`, default 15 s) to avoid fork overhead, while live pool state
is read from `/proc` at the `-psi` rate.

All `/proc` file descriptors are opened once at startup and held open;
reads use `pread(fd, buf, 0)` to re-read from offset 0 without
open/close/seek overhead.

### CPU sliding-window smoothing

Linux reports CPU time in jiffies (10 ms with HZ=100). At fast polling
rates a single sample spans at most a few jiffies per core, so the raw
delta is quantized to coarse steps (0%, 50%, 100% for a 10 ms interval).

To produce stable readings the tool maintains a **fixed-size ring buffer
per CPU tracker** sized to cover ~1 second of samples:

```
windowSize = max(1, time.Second / cpuInterval)
```

At the default `-cpu 40ms` this is 25 slots. At `-cpu 1s` the window is
1 (no smoothing — identical to a raw single-sample delta).

Each `update()` writes the newly computed utilization percentage into the
ring buffer:

```go
t.ring[t.ringPos] = t.cur
t.ringPos = (t.ringPos + 1) % len(t.ring)
if t.ringN < len(t.ring) { t.ringN++ }
```

The **current** column displays `smoothCur()`, the arithmetic mean of
all populated ring entries. The ring fills progressively on startup
(averaging fewer samples initially), then wraps and always averages the
most recent `windowSize` values.

The CDF histogram buckets and the lifetime **avg** column are unaffected —
they accumulate every individual sample regardless of the window.

### CDF histogram buckets

Utilization samples are bucketed into 12 bins:

| Bucket | Range |
|--------|-------|
| 0–8 | 0–10%, 10–20%, ..., 80–90% |
| 9 | 90–95% |
| 10 | 95–99% |
| 11 | 99–100% |

The higher-resolution tail buckets (90–95, 95–99, 99–100) make it easy
to spot cores that are consistently saturated.
