use serde::Deserialize;
use serde_json::Value;
use serde_json::json;
use std::sync::Arc;
use thiserror::Error;
use tokio::net::TcpStream;
use tokio::sync::Mutex;
use tokio::time::{Duration, timeout};

use futures_util::{SinkExt, StreamExt}; // for .send() and .next()
use std::sync::atomic::{AtomicU64, Ordering};

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
}

pub type Result<T> = std::result::Result<T, Error>;

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
    #[allow(dead_code)]
    ws_url: String,
    cdp_url: String,
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
            cdp_url: cdp_url.to_string(),
            socket: Arc::new(Mutex::new(socket)),
            current_id: AtomicU64::new(constant::CDP_ID_START),
        };

        cdp.send_cmd("Page.enable", json!({})).await?;
        cdp.send_cmd("Runtime.enable", json!({})).await?;
        Ok(cdp)
    }

    /// Asynchronously navigate the page to the given URL via CDP.
    ///
    /// For navigation we still go through `send_cmd` so that the command
    /// format and ID handling stay consistent, but we intentionally ignore
    /// any error from CDP because Chromium sometimes renders successfully
    /// without sending a matching response.
    pub async fn navigate(&self, url: &str) -> Result<()> {
        println!("CDP: Navigating to {url}");
        if let Err(e) = self.send_cmd("Page.navigate", json!({ "url": url })).await {
            eprintln!("CDP: Ignoring navigate error: {e}");
        }
        Ok(())
    }

    /// Get the current URL of the page using the CDP HTTP endpoint.
    pub async fn get_current_url(&self) -> Result<String> {
        let targets: Vec<Target> = reqwest::get(&self.cdp_url).await?.json().await?;
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
                    Err(e) => {
                        return Err(Error::Command(format!("Can't get message: {e}")));
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
            Err(Error::Chromium(
                "WebSocket closed before response".to_string(),
            ))
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
