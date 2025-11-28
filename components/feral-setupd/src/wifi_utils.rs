use std::collections::HashSet;
use std::sync::Arc;
use std::time::{Duration, Instant};
use thiserror::Error;
use tokio::process::Command;
use tokio::sync::{Mutex, Notify};
use tokio::task;

use crate::constant;

#[derive(Debug, Error)]
pub enum Error {
    #[error(transparent)]
    Io(#[from] std::io::Error),

    #[error("join error: {0}")]
    Join(#[from] tokio::task::JoinError),

    #[error("nmcli failed: {stderr}")]
    NmcliFailure {
        /// Raw exit status & captured stderr if you want it for logs/tests
        status: std::process::ExitStatus,
        stderr: String,
    },
}

pub type Result<T> = std::result::Result<T, Error>;

#[derive(Default)]
struct State {
    cached_ssids: Vec<String>,
    last_error: Option<Error>,
    expired_at: Option<Instant>,
    refreshing: bool,
}

impl State {
    fn reset(&mut self) {
        *self = Self::default();
    }
}

pub struct SSIDsCacher {
    state: Arc<Mutex<State>>,
    notify: Arc<Notify>,
}

impl SSIDsCacher {
    pub fn new() -> Self {
        Self {
            state: Arc::new(Mutex::new(State::default())),
            notify: Arc::new(Notify::new()),
        }
    }

    /// Refresh and forget
    pub fn trigger_refresh(&self) {
        let state = Arc::clone(&self.state);
        let notify = Arc::clone(&self.notify);

        task::spawn(async move {
            {
                let mut st = state.lock().await;
                if st.refreshing {
                    // someone else is doing it, we don't need to do anything
                    return;
                }
                // We need to set the flag to avoid concurrent refreshing
                st.refreshing = true;
            }

            println!("SSIDsCacher: refreshing...");
            let res = list_ssids(true).await;
            println!("SSIDsCacher: refreshed: \n{res:?}");
            {
                let mut st = state.lock().await;
                match res {
                    Ok(ssids) => {
                        st.cached_ssids = ssids;
                        st.expired_at =
                            Some(Instant::now() + Duration::from_millis(constant::SSID_CACHE_TTL));
                        st.last_error = None;
                    }
                    Err(e) => {
                        st.reset();
                        st.last_error = Some(e);
                    }
                }
                st.refreshing = false;
            }

            // wake everyone waiting in `get()`
            notify.notify_waiters();
        });
    }

    /// Trigger a refresh and wait for it to complete
    pub async fn trigger_refresh_and_wait(&self) {
        // Subscribe to notifications BEFORE checking state
        // This ensures we won't miss any notification (lost wakeup problem)
        let notified = self.notify.notified();

        // Check if already refreshing
        {
            let st = self.state.lock().await;
            if st.refreshing {
                // Someone else is refreshing, wait for them to finish
                drop(st);
                notified.await;
                return;
            }
        }

        // Start refresh
        {
            let mut st = self.state.lock().await;
            if st.refreshing {
                // Double-check after acquiring lock
                drop(st);
                notified.await;
                return;
            }
            st.refreshing = true;
        }

        // Do the actual refresh
        println!("SSIDsCacher: refreshing...");
        let res = list_ssids(true).await;
        println!("SSIDsCacher: refreshed: \n{res:?}");

        {
            let mut st = self.state.lock().await;
            match res {
                Ok(ssids) => {
                    st.cached_ssids = ssids;
                    st.expired_at =
                        Some(Instant::now() + Duration::from_millis(constant::SSID_CACHE_TTL));
                    st.last_error = None;
                }
                Err(e) => {
                    st.reset();
                    st.last_error = Some(e);
                }
            }
            st.refreshing = false;
        }

        // wake everyone waiting in `get()`
        self.notify.notify_waiters();
    }

    /// Get SSIDs, waiting only if a refresh is currently in progress or required.
    /// If the last scan failed, or return empty list of SSIDs, the first "get" will clear the cache
    /// Thus, the next "get" will trigger a new refresh
    pub async fn get(&self) -> Result<Vec<String>> {
        loop {
            let notified = self.notify.notified();

            {
                let mut st = self.state.lock().await;

                if !st.refreshing {
                    // Fast path: fresh cache
                    if st.expired_at.is_some_and(|exp| exp > Instant::now()) {
                        println!("SSIDsCacher: returning cached SSIDs");
                        let clone = st.cached_ssids.clone();
                        // If the cache is empty, we reset it
                        // The next call to `get()` will trigger a new refresh
                        if clone.is_empty() {
                            st.reset();
                        }
                        return Ok(clone);
                    }

                    // Slow path: cache expired
                    println!("SSIDsCacher: no cached or expired SSIDs");
                    if st.last_error.is_some() {
                        println!("SSIDsCacher: returning last error");
                        // We take the error here
                        // The next call to `get()` will trigger a new refresh
                        return Err(st.last_error.take().unwrap());
                    }

                    println!("SSIDsCacher: triggering refresh");
                    drop(st); // release lock before spawning
                    self.trigger_refresh();
                }
                // else: someone else is refreshing → fall through to wait
            }

            // Wait until the background task signals completion.
            notified.await;
        }
    }
}

pub async fn connect(ssid: &str, pass: &str) -> Result<()> {
    // delete any existing connection, don't care if it fails
    // we need this because of a bug with nmcli
    // https://bbs.archlinux.org/viewtopic.php?id=300321&p=2

    if let Err(err) = delete(ssid).await {
        eprintln!("Wifi: failed to delete existing connection: {err}");
    }

    println!("Wifi: connecting to {ssid}");
    let output = Command::new("nmcli")
        .args(["device", "wifi", "connect", ssid, "password", pass])
        .output()
        .await?;

    if output.status.success() {
        Ok(())
    } else {
        Err(Error::NmcliFailure {
            status: output.status,
            stderr: String::from_utf8_lossy(&output.stderr).to_string(),
        })
    }
}

async fn delete(ssid: &str) -> Result<()> {
    let output = Command::new("nmcli")
        .args(["connection", "delete", ssid])
        .output()
        .await?;

    if output.status.success() {
        Ok(())
    } else {
        Err(Error::NmcliFailure {
            status: output.status,
            stderr: String::from_utf8_lossy(&output.stderr).to_string(),
        })
    }
}

pub async fn list_ssids(force: bool) -> Result<Vec<String>> {
    let mut args = vec!["-t", "-f", "SSID", "device", "wifi", "list"];
    if force {
        args.push("--rescan");
        args.push("yes");
    }
    let output = Command::new("nmcli").args(&args).output().await?;

    if !output.status.success() {
        return Err(Error::NmcliFailure {
            status: output.status,
            stderr: String::from_utf8_lossy(&output.stderr).to_string(),
        });
    }

    // Parse stdout lines, filtering out empty entries
    let stdout = String::from_utf8_lossy(&output.stdout);
    let mut ssids = Vec::new();

    // Keep track of seen SSIDs to avoid duplicates while preserving order
    let mut seen = HashSet::new();

    // Limit to maximum 9 SSIDs
    for line in stdout.lines() {
        if !line.is_empty() && !seen.contains(line) {
            seen.insert(line.to_string());
            ssids.push(line.to_string());

            // Stop once we have 9 SSIDs
            if ssids.len() >= constant::MAX_SSIDS {
                break;
            }
        }
    }
    Ok(ssids)
}
