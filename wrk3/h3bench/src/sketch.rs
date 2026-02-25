use sketches_ddsketch::{Config, DDSketch};
use std::fmt;
use std::time::Duration;

/// Pair of DDSketches tracking TTFB and full-response latencies.
pub struct LatencySketches {
    pub ttfb: DDSketch,
    pub full: DDSketch,
}

impl LatencySketches {
    pub fn new(error_bound: f64) -> Self {
        let config = Config::new(error_bound, 2048, 1.0e-9);
        Self {
            ttfb: DDSketch::new(config),
            full: DDSketch::new(config),
        }
    }

    pub fn add_ttfb(&mut self, d: Duration) {
        self.ttfb.add(d.as_secs_f64() * 1000.0); // store as ms
    }

    pub fn add_full(&mut self, d: Duration) {
        self.full.add(d.as_secs_f64() * 1000.0);
    }

    /// Merge another sketch pair into this one.
    pub fn merge(&mut self, other: &LatencySketches) {
        self.ttfb.merge(&other.ttfb).expect("DDSketch merge failed");
        self.full.merge(&other.full).expect("DDSketch merge failed");
    }
}

/// Counters accumulated by a single worker.
#[derive(Default, Clone)]
pub struct Counters {
    pub requests: u64,
    pub success: u64,
    pub errors: u64,
    pub bytes_read: u64,
}

impl Counters {
    pub fn merge(&mut self, other: &Counters) {
        self.requests += other.requests;
        self.success += other.success;
        self.errors += other.errors;
        self.bytes_read += other.bytes_read;
    }
}

/// Worker result: sketches + counters.
pub struct WorkerResult {
    pub sketches: LatencySketches,
    pub counters: Counters,
}

const QUANTILES: &[(f64, &str)] = &[
    (0.50, "p50"),
    (0.75, "p75"),
    (0.90, "p90"),
    (0.95, "p95"),
    (0.99, "p99"),
    (0.999, "p99.9"),
];

fn format_ms(val: f64) -> String {
    if val < 1.0 {
        format!("{:.0}us", val * 1000.0)
    } else if val < 1000.0 {
        format!("{:.2}ms", val)
    } else {
        format!("{:.2}s", val / 1000.0)
    }
}

fn format_bytes(b: u64) -> String {
    if b < 1024 {
        format!("{} B", b)
    } else if b < 1024 * 1024 {
        format!("{:.1} KB", b as f64 / 1024.0)
    } else if b < 1024 * 1024 * 1024 {
        format!("{:.1} MB", b as f64 / (1024.0 * 1024.0))
    } else {
        format!("{:.2} GB", b as f64 / (1024.0 * 1024.0 * 1024.0))
    }
}

fn print_sketch(name: &str, sketch: &DDSketch, error_bound: f64, f: &mut fmt::Formatter<'_>) -> fmt::Result {
    writeln!(f, "  {} (DDSketch, \u{03b1}={}):", name, error_bound)?;
    for &(q, label) in QUANTILES {
        if let Some(val) = sketch.quantile(q).ok().flatten() {
            writeln!(f, "    {:<6} {}", label, format_ms(val))?;
        }
    }
    if let Some(min) = sketch.min() {
        writeln!(f, "    {:<6} {}", "min", format_ms(min))?;
    }
    if let Some(max) = sketch.max() {
        writeln!(f, "    {:<6} {}", "max", format_ms(max))?;
    }
    if sketch.count() > 0 {
        let mean = sketch.sum().unwrap_or(0.0) / sketch.count() as f64;
        writeln!(f, "    {:<6} {}", "mean", format_ms(mean))?;
    }
    Ok(())
}

/// Final report combining all worker results.
pub struct Report {
    pub sketches: LatencySketches,
    pub counters: Counters,
    pub elapsed: Duration,
    pub error_bound: f64,
    pub url: String,
    pub connections: usize,
    pub streams: usize,
}

impl fmt::Display for Report {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let secs = self.elapsed.as_secs_f64();
        let rps = if secs > 0.0 { self.counters.requests as f64 / secs } else { 0.0 };
        let bps = if secs > 0.0 { self.counters.bytes_read as f64 / secs } else { 0.0 };

        writeln!(f)?;
        writeln!(f, "Running {:.1}s test @ {}", secs, self.url)?;
        writeln!(f, "  {} connections, {} streams per connection", self.connections, self.streams)?;
        writeln!(f)?;
        writeln!(
            f,
            "  Requests:     {} total, {} ok, {} errors",
            self.counters.requests, self.counters.success, self.counters.errors,
        )?;
        writeln!(f, "  Throughput:   {:.1} req/s", rps)?;
        writeln!(
            f,
            "  Data:         {}, {}/s",
            format_bytes(self.counters.bytes_read),
            format_bytes(bps as u64),
        )?;
        writeln!(f)?;
        print_sketch("TTFB Latency", &self.sketches.ttfb, self.error_bound, f)?;
        writeln!(f)?;
        print_sketch("Full Response Latency", &self.sketches.full, self.error_bound, f)?;
        Ok(())
    }
}
