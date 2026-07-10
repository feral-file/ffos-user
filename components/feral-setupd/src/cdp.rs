use serde::Deserialize;
use serde_json::Value;
use serde_json::json;
use std::sync::Arc;
use thiserror::Error;
use tokio::net::TcpStream;
use tokio::sync::Mutex;
use tokio::time::{Duration, timeout};

use futures_util::{SinkExt, StreamExt}; // for .send() and .next()
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use tokio::sync::Notify;

#[derive(Debug, Error)]
pub enum Error {
    #[error(transparent)]
    Io(#[from] std::io::Error),

    #[error(transparent)]
    WebSocket(#[from] tokio_tungstenite::tungstenite::Error),

    #[error(transparent)]
    Timeout(#[from] tokio::time::error::Elapsed),

    #[error(transparent)]
    Http(#[from] reqwest::Error),

    #[error("Chromium error: {0}")]
    Chromium(String),

    #[error("Command error: {0}")]
    Command(String),

    /// The CDP WebSocket closed underneath an in-flight command (peer went away).
    /// Distinct from a command timeout so the reconnecting handle can tell a dead
    /// transport (must reconnect) from Chromium simply not replying (still usable).
    #[error("CDP transport closed")]
    Closed,
}

pub type Result<T> = std::result::Result<T, Error>;

impl Error {
    /// Whether this error means the underlying transport is dead and the cached
    /// connection must be dropped so the reconnect loop rebuilds it against a fresh
    /// Chromium (whose ws URL changes across restarts).
    ///
    /// A `Timeout` or `Command` error is deliberately NOT transport-dead: `Cdp::navigate`
    /// often gets no matching reply even though Chromium rendered fine, and a
    /// command-level CDP error leaves the socket usable. Treating those as dead would
    /// flap the connection on every successful navigation.
    fn is_transport_dead(&self) -> bool {
        matches!(
            self,
            Error::Io(_) | Error::WebSocket(_) | Error::Http(_) | Error::Closed
        )
    }
}

use tokio_tungstenite::{
    MaybeTlsStream, // async-compatible TLS/Plain wrapper
    WebSocketStream,
    connect_async,
    tungstenite::protocol::Message,
};

use crate::constant;

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Target {
    r#type: Option<String>,
    web_socket_debugger_url: Option<String>,
    url: Option<String>,
}

pub struct Cdp {
    ws_url: String,
    socket: Arc<Mutex<WebSocketStream<MaybeTlsStream<TcpStream>>>>,
    current_id: AtomicU64,
}

impl Cdp {
    /// Asynchronously create a new CDP client by fetching the WebSocket URL and connecting.
    pub async fn connect(cdp_url: &str) -> Result<Self> {
        let ws_url = Self::get_ws_url(cdp_url).await?;
        let socket = Self::connect_ws(&ws_url).await?;

        let cdp = Self {
            ws_url,
            socket: Arc::new(Mutex::new(socket)),
            current_id: AtomicU64::new(constant::CDP_ID_START),
        };

        cdp.send_cmd("Page.enable", json!({})).await?;
        cdp.send_cmd("Runtime.enable", json!({})).await?;
        Ok(cdp)
    }

    /// The page WebSocket URL this client is bound to.
    ///
    /// Used by [`CdpHandle`] to detect a Chromium restart: a restart spawns a new page target
    /// with a different `webSocketDebuggerUrl`, so a mismatch against the live `/json` value means
    /// the cached socket is stale even if TCP has not errored yet.
    pub fn ws_url(&self) -> &str {
        &self.ws_url
    }

    /// Asynchronously navigate the page to the given URL via CDP.
    ///
    /// Unlike the public [`CdpHandle::navigate`] (which is infallible by contract), this low-level
    /// call propagates the `send_cmd` error so the handle can classify it: a dead transport must
    /// drop the cached connection, while a timeout (Chromium rendered without a matching reply) must
    /// not. Callers other than the handle should not exist.
    pub async fn navigate(&self, url: &str) -> Result<()> {
        println!("CDP: Navigating to {url}");
        self.send_cmd("Page.navigate", json!({ "url": url }))
            .await?;
        Ok(())
    }

    /// Read the current page URL straight from the CDP HTTP `/json` endpoint.
    ///
    /// Associated (not `&self`) so [`CdpHandle`] can query the browser without holding a live
    /// socket — the endpoint is served by Chromium itself, independent of any WebSocket state.
    async fn fetch_current_url(cdp_url: &str) -> Result<String> {
        let targets: Vec<Target> = reqwest::get(cdp_url).await?.json().await?;
        targets
            .into_iter()
            .find_map(|t| {
                if t.r#type.as_deref() == Some("page") {
                    Some(t.url.unwrap_or_default())
                } else {
                    None
                }
            })
            .ok_or_else(|| Error::Chromium("No current URL found".into()))
    }

    /// Send any CDP command and wait for the matching reply.
    /// Logs the full response and returns the `"result"` value (or an error).
    async fn send_cmd(&self, method: &str, params: Value) -> Result<Value> {
        // Send the command with a unique ID.
        let id = self.current_id.fetch_add(1, Ordering::Relaxed) + 1;
        let body = json!({
            "id": id,
            "method": method,
            "params": params
        });
        // Because we lock the socket, there can be only one command at a time.
        let mut sock = self.socket.lock().await;
        sock.send(Message::Text(body.to_string().into())).await?;
        println!("CDP: Sent command: {body}");

        let timeout_duration = Duration::from_secs(3);

        // Wait for the response with the same ID.
        // Or if the command is Page.navigate, wait for the response with corresponding event.
        println!(
            "CDP: Waiting for response for {} with timeout {}s",
            method,
            timeout_duration.as_secs()
        );

        timeout(timeout_duration, async {
            // Keep getting messages until we get the right response.
            while let Some(msg) = sock.next().await {
                let msg = match msg {
                    Ok(msg) => msg,
                    // A receive error means the socket itself failed: classify as WebSocket so the
                    // handle drops and reconnects, not as a benign Command error.
                    Err(e) => {
                        return Err(Error::WebSocket(e));
                    }
                };
                // Get the text of the message and parse it as JSON.
                if let Message::Text(text) = msg
                    && let Ok(resp) = serde_json::from_str::<Value>(&text)
                {
                    // If the response is for the command we sent, return the result.
                    if resp.get("id").and_then(|v| v.as_u64()) == Some(id) {
                        println!("CDP: Response for {method}: {resp}");
                        if let Some(err) = resp.get("error") {
                            return Err(Error::Command(err.to_string()));
                        }
                        return Ok(resp.get("result").cloned().unwrap_or(Value::Null));
                    }
                }
            }
            // Stream ended (peer closed the socket) before our reply arrived: a dead transport.
            Err(Error::Closed)
        })
        .await?
    }

    /// Fetch the WebSocket debug URL from the CDP HTTP endpoint.
    async fn get_ws_url(cdp_url: &str) -> Result<String> {
        let targets: Vec<Target> = reqwest::get(cdp_url).await?.json().await?;

        targets
            .into_iter()
            .find_map(|t| {
                if t.r#type.as_deref() == Some("page") {
                    t.web_socket_debugger_url
                } else {
                    None
                }
            })
            .ok_or_else(|| Error::Chromium("No WebSocket URL found".into()))
    }

    /// Establish an asynchronous WebSocket connection to the CDP.
    async fn connect_ws(ws_url: &str) -> Result<WebSocketStream<MaybeTlsStream<TcpStream>>> {
        let (socket, _response) = connect_async(ws_url).await?;
        Ok(socket)
    }
}

/// A reconnecting front for [`Cdp`].
///
/// Why this exists: `feral-setupd` now starts unconditionally at boot, before the kiosk's Chromium
/// exists, and stays alive across kiosk restarts (the units no longer gate it on
/// `chromium-ready.target`). So the CDP endpoint at [`crate::constant::CDP_URL`] may be absent at
/// startup, appear later when a monitor is plugged in, and disappear/reappear whenever the kiosk
/// restarts. BLE setup, D-Bus, and OTA must all run with zero CDP; every UI navigation is
/// best-effort.
///
/// Invariants this type upholds:
/// - It owns at most one live [`Cdp`] at a time, behind `Mutex<Option<..>>`. `None` means "no
///   browser right now" — [`Self::navigate`] silently drops (see its infallible contract) and the
///   background reconnect loop keeps trying.
/// - A transport-dead error from any call drops the cached connection (`is_transport_dead`), so the
///   loop rebuilds it. Chromium restarts mint a new page target with a new ws URL, so reconnect
///   always re-reads `/json`; never cache the ws URL across a drop.
/// - `navigate` never surfaces an error to callers (preserves the historical infallible contract
///   the whole UI layer relies on). `get_current_url` still reports its result but is HTTP-only and
///   never requires a live socket.
pub struct CdpHandle {
    cdp_url: String,
    conn: Mutex<Option<Arc<Cdp>>>,
    /// Raised when a navigate times out on a cached socket. A kiosk restart can leave the old TCP
    /// socket writable but bound to a dead page target, and a timeout is the only symptom the
    /// navigate path sees (it is deliberately not transport-dead — Chromium often renders without
    /// replying). The reconnect loop consumes this as a corroborating staleness signal: probe
    /// immediately instead of sleeping out the liveness interval, and skip the blip debounce when
    /// the probe agrees. Without it, a zombie socket silently eats navigations for up to
    /// `CDP_LIVENESS_STALE_PROBES` × `CDP_LIVENESS_CHECK_INTERVAL`.
    nav_suspect: AtomicBool,
    nav_suspect_wake: Notify,
}

impl CdpHandle {
    /// Create a handle bound to a CDP HTTP `/json` URL. Starts disconnected; call [`Self::connect`]
    /// (best-effort) and/or drive the background reconnect loop to establish a connection.
    pub fn new(cdp_url: &str) -> Self {
        Self {
            cdp_url: cdp_url.to_string(),
            conn: Mutex::new(None),
            nav_suspect: AtomicBool::new(false),
            nav_suspect_wake: Notify::new(),
        }
    }

    /// Record a navigate timeout on a live socket and wake the liveness loop.
    fn mark_navigate_suspect(&self) {
        self.nav_suspect.store(true, Ordering::Relaxed);
        self.nav_suspect_wake.notify_one();
    }

    /// Consume the suspect flag: true at most once per raise.
    pub fn take_navigate_suspect(&self) -> bool {
        self.nav_suspect.swap(false, Ordering::Relaxed)
    }

    /// Resolves when a navigate raises the suspect flag (or immediately if one already has since
    /// the last wait) — lets the liveness loop cut its sleep short rather than poll the flag.
    pub async fn navigate_suspect_raised(&self) {
        self.nav_suspect_wake.notified().await;
    }

    /// Attempt a single connect, caching the connection on success. Idempotent: a success replaces
    /// any prior connection (used by the reconnect loop after a drop).
    pub async fn connect(&self) -> Result<()> {
        let cdp = Cdp::connect(&self.cdp_url).await?;
        *self.conn.lock().await = Some(Arc::new(cdp));
        Ok(())
    }

    /// True while a connection is cached. A cached connection is not a liveness guarantee — the
    /// socket may have died silently; [`Self::connection_is_current`] probes that.
    pub async fn is_connected(&self) -> bool {
        self.conn.lock().await.is_some()
    }

    /// Drop any cached connection. Next [`Self::navigate`] is a no-op until a reconnect succeeds.
    pub async fn disconnect(&self) {
        *self.conn.lock().await = None;
    }

    /// Snapshot the current connection (cheap Arc clone) without holding the map lock across the
    /// subsequent await, so a long navigate cannot block the reconnect loop's bookkeeping.
    async fn current(&self) -> Option<Arc<Cdp>> {
        self.conn.lock().await.clone()
    }

    /// Drop the cached connection only if it is still the one that just failed. Guards against a
    /// race where the reconnect loop already swapped in a fresh connection between the failed call
    /// and this cleanup.
    async fn drop_if_current(&self, dead: &Arc<Cdp>) {
        let mut guard = self.conn.lock().await;
        if guard.as_ref().is_some_and(|cur| Arc::ptr_eq(cur, dead)) {
            *guard = None;
        }
    }

    /// Whether the cached connection still matches the live Chromium page target.
    ///
    /// HTTP-only by design: a Chromium/kiosk restart leaves our TCP socket momentarily writable but
    /// bound to a dead target, so a stale socket cannot be detected by "is the Arc present". Instead
    /// we compare our bound ws URL to the current `/json` page URL — a restart yields a new target
    /// GUID, and an unreachable endpoint means Chromium is gone. Either way the connection is not
    /// current and the loop must reconnect. Returns `false` when nothing is cached.
    pub async fn connection_is_current(&self) -> bool {
        let Some(cdp) = self.current().await else {
            return false;
        };
        // Bound the probe: reqwest has no default timeout and a wedged Chromium must not
        // stall the reconnect loop indefinitely. A timed-out or failed probe counts as
        // not-current; the caller's consecutive-failure debounce absorbs one-off blips so a
        // single transient error cannot repaint a healthy kiosk.
        match timeout(
            Duration::from_millis(constant::CDP_LIVENESS_PROBE_TIMEOUT),
            Cdp::get_ws_url(&self.cdp_url),
        )
        .await
        {
            Ok(Ok(url)) => url == cdp.ws_url(),
            _ => false,
        }
    }

    /// Navigate the browser, best-effort. Infallible by contract: the entire UI layer calls this
    /// through `show_*` helpers that historically could not fail on CDP, and no BLE/D-Bus reply may
    /// ever hinge on a CDP result. When disconnected this drops the navigation; when the transport
    /// dies mid-call it drops the cached connection (so the loop reconnects) but still returns `Ok`.
    /// A timeout is treated as success (Chromium commonly renders without a matching reply).
    pub async fn navigate(&self, url: &str) -> Result<()> {
        let Some(cdp) = self.current().await else {
            println!("CDP: navigate dropped, no browser connected: {url}");
            return Ok(());
        };
        if let Err(e) = cdp.navigate(url).await {
            if e.is_transport_dead() {
                eprintln!("CDP: navigate hit dead transport, dropping connection: {e}");
                self.drop_if_current(&cdp).await;
            } else {
                eprintln!("CDP: ignoring navigate error: {e}");
                if matches!(e, Error::Timeout(_)) {
                    self.mark_navigate_suspect();
                }
            }
        }
        Ok(())
    }

    /// Read the current page URL over HTTP. Reports its result (callers like `show_webapp` use it
    /// only as a fast-path hint and must not propagate failure), but drops the cached connection on
    /// a transport-dead error so the loop reconnects.
    pub async fn get_current_url(&self) -> Result<String> {
        // Snapshot before the fetch so a transport-dead result only drops the connection that
        // was cached when the probe started — a fresh connection the reconnect loop swapped in
        // mid-call must not be clobbered (same invariant as `navigate`'s drop_if_current).
        let snapshot = self.current().await;
        let res = Cdp::fetch_current_url(&self.cdp_url).await;
        if let Err(e) = &res
            && e.is_transport_dead()
            && let Some(cdp) = snapshot
        {
            self.drop_if_current(&cdp).await;
        }
        res
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn transport_dead_errors_trigger_reconnect() {
        // Socket-level failures mean the cached connection must be dropped and rebuilt.
        assert!(Error::Closed.is_transport_dead());
        assert!(
            Error::Io(std::io::Error::from(std::io::ErrorKind::ConnectionReset))
                .is_transport_dead()
        );
    }

    #[test]
    fn timeout_and_command_errors_do_not_reconnect() {
        // Chromium renders navigations without always replying, and a command-level error leaves the
        // socket usable: neither may flap the connection.
        assert!(!Error::Chromium("No current URL found".into()).is_transport_dead());
        assert!(!Error::Command("some CDP error".into()).is_transport_dead());
    }

    #[tokio::test]
    async fn new_handle_starts_disconnected() {
        let handle = CdpHandle::new("http://127.0.0.1:9222/json");
        assert!(!handle.is_connected().await);
        // No browser: navigate is a silent no-op, never an error — and a dropped
        // navigation is not a zombie-socket symptom, so it must not raise suspicion.
        assert!(handle.navigate("file:///whatever").await.is_ok());
        assert!(!handle.take_navigate_suspect());
        // Nothing to compare against: not current.
        assert!(!handle.connection_is_current().await);
    }

    #[tokio::test]
    async fn navigate_suspect_flag_wakes_and_is_consumed_once() {
        let handle = CdpHandle::new("http://127.0.0.1:9222/json");
        assert!(!handle.take_navigate_suspect());
        handle.mark_navigate_suspect();
        // The stored wake permit must resolve even though the wait registered after the raise
        // (the liveness loop is usually mid-probe or mid-sleep when a navigate times out).
        handle.navigate_suspect_raised().await;
        assert!(handle.take_navigate_suspect());
        assert!(!handle.take_navigate_suspect());
    }
}
