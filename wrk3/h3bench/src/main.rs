mod client;
mod sketch;
mod tls;
mod worker;

use clap::Parser;
use h3_quinn::quinn;
use sketch::{Counters, LatencySketches, Report};
use std::net::ToSocketAddrs;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

#[derive(Parser)]
#[command(name = "h3bench", about = "HTTP/3 load generator")]
struct Args {
    /// Target URL (https://...)
    url: String,

    /// Number of QUIC connections
    #[arg(short = 'c', long, default_value_t = 1)]
    connections: usize,

    /// Streams per connection
    #[arg(short = 's', long, default_value_t = 100)]
    streams: usize,

    /// Test duration in seconds
    #[arg(short = 'd', long, default_value_t = 10)]
    duration: u64,

    /// Total requests (overrides duration)
    #[arg(short = 'n', long)]
    requests: Option<u64>,

    /// HTTP method
    #[arg(short = 'm', long, default_value = "GET")]
    method: String,

    /// Add header (repeatable), format: "Key: Value"
    #[arg(short = 'H', long = "header", value_name = "K:V")]
    headers: Vec<String>,

    /// Request body from file
    #[arg(long)]
    body: Option<String>,

    /// DDSketch relative error bound
    #[arg(long, default_value_t = 0.01)]
    error_bound: f64,

    /// Worker threads (defaults to num_cpus)
    #[arg(short = 't', long)]
    threads: Option<usize>,

    /// QUIC handshake timeout in milliseconds
    #[arg(long, default_value_t = 5000)]
    connect_timeout: u64,

    /// Skip TLS certificate verification
    #[arg(long)]
    insecure: bool,
}

fn parse_header(s: &str) -> Option<(String, String)> {
    let pos = s.find(':')?;
    let key = s[..pos].trim().to_string();
    let val = s[pos + 1..].trim().to_string();
    Some((key, val))
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    tracing_subscriber::fmt::init();

    let args = Args::parse();

    // Parse URL
    let uri: http::Uri = args.url.parse()?;
    let scheme = uri.scheme_str().unwrap_or("");
    if scheme != "https" {
        eprintln!("error: URL must use https:// scheme (got {:?})", scheme);
        std::process::exit(1);
    }
    let authority = uri
        .authority()
        .ok_or("URL must have a host")?;
    let host = authority.host().to_string();
    let port = authority.port_u16().unwrap_or(443);

    // Resolve address
    let addr = format!("{}:{}", host, port)
        .to_socket_addrs()?
        .next()
        .ok_or("failed to resolve host")?;

    // Parse method
    let method: http::Method = args.method.parse()?;

    // Parse headers
    let mut headers = Vec::new();
    for h in &args.headers {
        match parse_header(h) {
            Some(pair) => headers.push(pair),
            None => {
                eprintln!("error: invalid header format {:?}, expected 'Key: Value'", h);
                std::process::exit(1);
            }
        }
    }

    // Read body if provided
    let body = if let Some(path) = &args.body {
        let data = tokio::fs::read(path).await?;
        Some(Arc::new(bytes::Bytes::from(data)))
    } else {
        None
    };

    // Build TLS config
    let tls_config = tls::build_tls_config(args.insecure);

    // Build quinn client config
    let mut transport = quinn::TransportConfig::default();
    transport.keep_alive_interval(Some(Duration::from_secs(5)));
    let mut client_config = quinn::ClientConfig::new(Arc::new(
        quinn::crypto::rustls::QuicClientConfig::try_from(tls_config)?,
    ));
    client_config.transport_config(Arc::new(transport));

    // Shared stop signal
    let stop = Arc::new(AtomicBool::new(false));

    // Request counter for count-based mode
    let request_limit = args.requests.map(|n| Arc::new(AtomicU64::new(n)));

    let start = Instant::now();

    // Print header
    let duration_str = if args.requests.is_some() {
        format!("{} requests", args.requests.unwrap())
    } else {
        format!("{}s", args.duration)
    };
    eprintln!(
        "Running {} test @ {}",
        duration_str, args.url
    );
    eprintln!(
        "  {} connections, {} streams per connection",
        args.connections, args.streams
    );
    eprintln!();

    // Spawn workers
    let mut handles = Vec::new();
    for _ in 0..args.connections {
        // Each worker gets its own endpoint bound to 0.0.0.0:0
        let mut endpoint = quinn::Endpoint::client("0.0.0.0:0".parse()?)?;
        endpoint.set_default_client_config(client_config.clone());

        let cfg = worker::WorkerConfig {
            endpoint,
            addr,
            server_name: host.clone(),
            uri: uri.clone(),
            method: method.clone(),
            headers: headers.clone(),
            body: body.clone(),
            streams: args.streams,
            error_bound: args.error_bound,
            stop: stop.clone(),
            request_limit: request_limit.clone(),
        };

        handles.push(tokio::spawn(worker::run_worker(cfg)));
    }

    // Duration-based stop timer
    if args.requests.is_none() {
        let stop_timer = stop.clone();
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_secs(args.duration)).await;
            stop_timer.store(true, Ordering::Relaxed);
        });
    }

    // Collect results
    let mut merged_sketches = LatencySketches::new(args.error_bound);
    let mut merged_counters = Counters::default();

    for handle in handles {
        let result = handle.await?;
        merged_sketches.merge(&result.sketches);
        merged_counters.merge(&result.counters);
    }

    let elapsed = start.elapsed();

    let report = Report {
        sketches: merged_sketches,
        counters: merged_counters,
        elapsed,
        error_bound: args.error_bound,
        url: args.url,
        connections: args.connections,
        streams: args.streams,
    };

    println!("{}", report);

    Ok(())
}
