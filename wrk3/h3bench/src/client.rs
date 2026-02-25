use bytes::Buf;
use h3_quinn::quinn;
use std::net::SocketAddr;
use std::sync::Arc;
use std::time::{Duration, Instant};

/// Result of a single HTTP/3 request.
pub struct RequestResult {
    pub ttfb: Duration,
    pub full: Duration,
    pub status: u16,
    pub body_bytes: u64,
}

/// Establish a QUIC connection to the given address using the provided endpoint.
pub async fn connect(
    endpoint: &quinn::Endpoint,
    addr: SocketAddr,
    server_name: &str,
) -> Result<quinn::Connection, Box<dyn std::error::Error + Send + Sync>> {
    let conn = endpoint.connect(addr, server_name)?.await?;
    Ok(conn)
}

/// Send a single HTTP/3 request on the given h3 connection and measure latencies.
pub async fn send_request<T>(
    send_request: &mut h3::client::SendRequest<T, bytes::Bytes>,
    method: &http::Method,
    uri: &http::Uri,
    headers: &[(String, String)],
    body: &Option<Arc<bytes::Bytes>>,
) -> Result<RequestResult, Box<dyn std::error::Error + Send + Sync>>
where
    T: h3::quic::OpenStreams<bytes::Bytes>,
{
    let mut builder = http::Request::builder()
        .method(method)
        .uri(uri);

    for (k, v) in headers {
        builder = builder.header(k.as_str(), v.as_str());
    }

    let start = Instant::now();

    let req = builder.body(())?;
    let mut stream = send_request.send_request(req).await?;

    if let Some(body_data) = body {
        stream.send_data(body_data.as_ref().clone()).await?;
    }
    stream.finish().await?;

    let resp = stream.recv_response().await?;
    let ttfb = start.elapsed();
    let status = resp.status().as_u16();

    let mut body_bytes: u64 = 0;
    while let Some(chunk) = stream.recv_data().await? {
        body_bytes += chunk.remaining() as u64;
    }
    let full = start.elapsed();

    Ok(RequestResult {
        ttfb,
        full,
        status,
        body_bytes,
    })
}
