# h3bench

An HTTP/3 load generator with DDSketch percentile reporting. Like `wrk`, but for the QUIC + TLS 1.3 world.

Built on **quinn** (QUIC) + **rustls** (TLS 1.3) + **h3** (HTTP/3). No OpenSSL, no C dependencies.

## Quick start

```
cargo build --release
./target/release/h3bench -c 4 -s 50 -d 10 https://cloudflare.com
```

## Example output

```
Running 11.0s test @ https://cloudflare.com
  4 connections, 50 streams per connection

  Requests:     40,109 total, 39,909 ok, 200 errors
  Throughput:   3,645.6 req/s
  Data:         6.4 MB, 591.6 KB/s

  Throughput (1s buckets, trimmed) (DDSketch, α=0.01):
    p50    963 req/s
    p75    9,230 req/s
    p90    9,607 req/s
    p95    9,607 req/s
    p99    9,607 req/s
    p99.9  9,607 req/s
    min    0 req/s
    max    9,686 req/s
    mean   3,482 req/s

  TTFB Latency (DDSketch, α=0.01):
    p50    18.73ms
    p75    26.31ms
    p90    33.45ms
    p95    37.72ms
    p99    47.95ms
    p99.9  64.72ms
    min    9.61ms
    max    138.73ms
    mean   21.77ms

  Full Response Latency (DDSketch, α=0.01):
    p50    18.73ms
    p75    26.31ms
    p90    33.45ms
    p95    37.72ms
    p99    47.95ms
    p99.9  64.72ms
    min    9.61ms
    max    138.74ms
    mean   21.78ms
```

## Usage

```
h3bench [OPTIONS] <URL>
```

| Flag | Default | Description |
|------|---------|-------------|
| `-c, --connections` | 1 | Number of QUIC connections |
| `-s, --streams` | 100 | HTTP/3 streams per connection |
| `-d, --duration` | 10 | Test duration in seconds |
| `-n, --requests` | — | Total requests (overrides `-d`) |
| `-m, --method` | GET | HTTP method |
| `-H, --header` | — | Add header (`"Key: Value"`), repeatable |
| `--body` | — | Request body from file path |
| `--error-bound` | 0.01 | DDSketch relative error (α) |
| `-t, --threads` | num_cpus | Tokio worker threads |
| `--connect-timeout` | 5000 | QUIC handshake timeout (ms) |
| `--insecure` | false | Skip TLS certificate verification |

## What it measures

**Three DDSketch distributions**, each with p50/p75/p90/p95/p99/p99.9/min/max/mean:

- **Throughput** — Per-second request completion rate. A global `AtomicU64` is sampled every 1s; the first and last seconds are trimmed to remove ramp-up/drain noise. Skipped for tests ≤3s.

- **TTFB Latency** — Time from request send to first response header byte.

- **Full Response Latency** — Time from request send to last body byte received.

## Why DDSketch

HDR histograms require choosing a value range and precision upfront. DDSketch provides guaranteed relative error bounds (default 1%) with automatic range adaptation and constant memory. It won't clip your p99.9, won't pre-allocate for a range you'll never hit, and merges across workers in O(buckets).

## Architecture

```
main
 ├── worker task (1 per connection)
 │    ├── QUIC connect + h3 handshake
 │    ├── h3 driver task (background)
 │    └── stream tasks (N per worker)
 │         └── loop: send request → measure TTFB/full → fetch_add(completed)
 ├── ticker task (1s interval → throughput samples)
 └── stop timer (duration-based) or atomic countdown (request-based)
```

Each connection is an independent QUIC endpoint bound to `0.0.0.0:0`. Stream tasks within a connection share multiplexed h3 streams over a single QUIC connection. All latency recording and counter aggregation happens via lock-free atomics and unbounded channels.

## Building

Requires Rust 1.70+.

```
cargo build --release
```

The release profile enables LTO and single codegen unit for maximum throughput in the benchmark binary itself.
