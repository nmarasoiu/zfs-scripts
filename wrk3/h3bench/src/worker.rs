use crate::client;
use crate::sketch::{Counters, LatencySketches, WorkerResult};
use bytes::Bytes;
use h3_quinn::quinn;
use std::net::SocketAddr;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

pub struct WorkerConfig {
    pub endpoint: quinn::Endpoint,
    pub addr: SocketAddr,
    pub server_name: String,
    pub uri: http::Uri,
    pub method: http::Method,
    pub headers: Vec<(String, String)>,
    pub body: Option<Arc<Bytes>>,
    pub streams: usize,
    pub error_bound: f64,
    pub stop: Arc<AtomicBool>,
    /// If set, global request counter for count-based mode.
    pub request_limit: Option<Arc<AtomicU64>>,
    /// Global completed-request counter for throughput sampling.
    pub completed: Arc<AtomicU64>,
}

pub async fn run_worker(cfg: WorkerConfig) -> WorkerResult {
    let mut sketches = LatencySketches::new(cfg.error_bound);
    let mut counters = Counters::default();

    // Establish QUIC connection
    let quic_conn = match client::connect(&cfg.endpoint, cfg.addr, &cfg.server_name).await {
        Ok(c) => c,
        Err(e) => {
            tracing::error!("connection failed: {}", e);
            counters.errors += 1;
            return WorkerResult { sketches, counters };
        }
    };

    let h3_conn = h3_quinn::Connection::new(quic_conn);
    let (mut driver, send_request) = match h3::client::new(h3_conn).await {
        Ok(pair) => pair,
        Err(e) => {
            tracing::error!("h3 handshake failed: {}", e);
            counters.errors += 1;
            return WorkerResult { sketches, counters };
        }
    };

    // Drive the h3 connection in the background
    let stop_clone = cfg.stop.clone();
    let driver_handle = tokio::spawn(async move {
        tokio::select! {
            err = driver.wait_idle() => {
                tracing::debug!("h3 connection closed: {}", err);
            }
            _ = async {
                loop {
                    tokio::time::sleep(Duration::from_millis(50)).await;
                    if stop_clone.load(Ordering::Relaxed) {
                        break;
                    }
                }
            } => {}
        }
    });

    // Spawn stream tasks
    let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel::<StreamResult>();

    for _ in 0..cfg.streams {
        let mut send_req = send_request.clone();
        let method = cfg.method.clone();
        let uri = cfg.uri.clone();
        let headers = cfg.headers.clone();
        let body = cfg.body.clone();
        let stop = cfg.stop.clone();
        let limit = cfg.request_limit.clone();
        let completed = cfg.completed.clone();
        let tx = tx.clone();

        tokio::spawn(async move {
            loop {
                if stop.load(Ordering::Relaxed) {
                    break;
                }
                // Check count-based limit
                if let Some(ref counter) = limit {
                    let prev = counter.fetch_sub(1, Ordering::Relaxed);
                    if prev == 0 || prev > u64::MAX / 2 {
                        counter.fetch_add(1, Ordering::Relaxed);
                        stop.store(true, Ordering::Relaxed);
                        break;
                    }
                }

                // Race the request against the stop signal so we don't hang
                let result = tokio::select! {
                    r = client::send_request(&mut send_req, &method, &uri, &headers, &body) => r,
                    _ = async {
                        loop {
                            tokio::time::sleep(Duration::from_millis(50)).await;
                            if stop.load(Ordering::Relaxed) { break; }
                        }
                    } => {
                        break;
                    }
                };

                completed.fetch_add(1, Ordering::Relaxed);
                match result {
                    Ok(result) => {
                        let _ = tx.send(StreamResult::Ok {
                            ttfb: result.ttfb,
                            full: result.full,
                            bytes: result.body_bytes,
                            status: result.status,
                        });
                    }
                    Err(e) => {
                        tracing::debug!("request error: {}", e);
                        let _ = tx.send(StreamResult::Err);
                        // Fatal connection errors — stop this stream
                        break;
                    }
                }
            }
        });
    }

    drop(tx);

    while let Some(result) = rx.recv().await {
        match result {
            StreamResult::Ok { ttfb, full, bytes, status } => {
                counters.requests += 1;
                counters.bytes_read += bytes;
                if (200..400).contains(&status) {
                    counters.success += 1;
                } else {
                    counters.errors += 1;
                }
                sketches.add_ttfb(ttfb);
                sketches.add_full(full);
            }
            StreamResult::Err => {
                counters.requests += 1;
                counters.errors += 1;
            }
        }
    }

    driver_handle.abort();
    WorkerResult { sketches, counters }
}

enum StreamResult {
    Ok {
        ttfb: Duration,
        full: Duration,
        bytes: u64,
        status: u16,
    },
    Err,
}
