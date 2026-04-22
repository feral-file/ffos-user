//! Device-local ff-player URL policy: bundled static server is expected on loopback:8080.
//! When that URL is in use but nothing accepts TCP in time, we surface a launcher error instead
//! of leaving Chromium on a dead page (no remote fallback).

use std::io;
use std::net::SocketAddr;

use tokio::net::TcpStream;
use tokio::time::{self, Duration, Instant};
use url::Url;

use crate::constant;

/// Matches the bundled static server URL (`constant::WEBAPP_URL`): HTTP on 127.0.0.1:8080.
/// Remote or custom overrides use a different host/port and skip the local readiness gate.
pub fn is_local_bundle_player_url(webapp_url: &str) -> bool {
    let Ok(u) = Url::parse(webapp_url) else {
        return false;
    };
    if !u.scheme().eq_ignore_ascii_case("http") {
        return false;
    }
    if u.host_str() != Some("127.0.0.1") {
        return false;
    }
    u.port_or_known_default() == Some(8080)
}

/// Poll until `addr` accepts TCP or `overall_timeout` elapses.
pub async fn wait_tcp_ready(
    addr: SocketAddr,
    overall_timeout: Duration,
    poll_interval: Duration,
) -> io::Result<()> {
    let deadline = Instant::now() + overall_timeout;
    loop {
        match TcpStream::connect(addr).await {
            Ok(_) => return Ok(()),
            Err(e) if Instant::now() >= deadline => {
                return Err(io::Error::new(
                    io::ErrorKind::TimedOut,
                    format!("timed out waiting for TCP {addr}: {e}"),
                ));
            }
            Err(_) => {
                time::sleep(poll_interval).await;
            }
        }
    }
}

pub async fn wait_local_bundle_player_tcp() -> io::Result<()> {
    let addr = SocketAddr::from(([127, 0, 0, 1], 8080));
    wait_tcp_ready(
        addr,
        Duration::from_millis(constant::LOCAL_PLAYER_TCP_WAIT_TIMEOUT_MS),
        Duration::from_millis(constant::LOCAL_PLAYER_TCP_POLL_INTERVAL_MS),
    )
    .await
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn is_local_bundle_player_url_default_constant() {
        assert!(is_local_bundle_player_url(constant::WEBAPP_URL));
    }

    #[test]
    fn is_local_bundle_player_url_no_trailing_slash() {
        assert!(is_local_bundle_player_url("http://127.0.0.1:8080"));
    }

    #[test]
    fn is_local_bundle_player_url_rejects_https() {
        assert!(!is_local_bundle_player_url("https://127.0.0.1:8080/"));
    }

    #[test]
    fn is_local_bundle_player_url_rejects_other_port() {
        assert!(!is_local_bundle_player_url("http://127.0.0.1:9090/"));
    }

    #[test]
    fn is_local_bundle_player_url_rejects_remote() {
        assert!(!is_local_bundle_player_url(
            "https://display.feralfile.com/"
        ));
    }

    #[tokio::test]
    async fn wait_tcp_ready_connects_when_listener_accepts() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            let _ = listener.accept().await;
        });
        wait_tcp_ready(addr, Duration::from_secs(3), Duration::from_millis(10))
            .await
            .unwrap();
    }
}
