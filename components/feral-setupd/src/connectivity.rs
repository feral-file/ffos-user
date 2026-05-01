//! connectivity.rs
//! Drop-in helper for “am I online?” logic.

use crate::dbus_utils;
use std::{
    sync::Arc,
    time::{Duration, Instant},
};
use tokio::{
    sync::{Mutex, watch},
    time,
};

/// Clone-able handle you keep in `AppState`.
#[derive(Clone, Debug)]
pub struct Connectivity {
    inner: Arc<Inner>,
}

#[derive(Debug)]
struct Inner {
    tx: watch::Sender<bool>,   // authoritative state
    rx: watch::Receiver<bool>, // everybody listens on this
    /// Protects the *forced* DBus call so we never launch two at once.
    force_lock: Mutex<()>,
}

impl Connectivity {
    // ---------------------------------------------------------------------
    // Construction
    // ---------------------------------------------------------------------
    /// Spawns the background refresher and returns a usable handle.
    pub async fn spawn() -> Self {
        let initial = dbus_utils::internet_availability(true);
        let (tx, rx) = watch::channel(initial);

        let inner = Arc::new(Inner {
            tx: tx.clone(),
            rx,
            force_lock: Mutex::new(()),
        });

        // Kick off the 60-second poller
        tokio::spawn(background_refresher(tx));

        Self { inner }
    }

    // ---------------------------------------------------------------------
    // Public API
    // ---------------------------------------------------------------------

    /// Returns the cached internet status synchronously.
    ///
    /// This is useful for contexts where async is not available (e.g., BLE callbacks).
    /// The value is updated by the background refresher every 60 seconds.
    pub fn is_online_cached(&self) -> bool {
        *self.inner.rx.borrow()
    }

    /// Returns the cached value unless `force_refresh == true`.
    ///
    /// * When `force_refresh` is **false** (typical case) it is *zero-cost*.
    /// * When `force_refresh` is **true** we synchronously call DBus,
    ///   update the cache, then return the fresh result.
    pub async fn is_online(&self, force_refresh: bool) -> bool {
        if !force_refresh {
            return *self.inner.rx.borrow();
        }

        // Serialize concurrent “force” calls.
        let _guard = self.inner.force_lock.lock().await;
        let fresh = dbus_utils::internet_availability(true);
        let _ = self.inner.tx.send_if_modified(|old| {
            if *old != fresh {
                *old = fresh;
                true
            } else {
                false
            }
        });
        fresh
    }

    /// Suspends until the state flips to *online*.
    /// Duration allows the caller to set the urgency of the wait.
    pub async fn wait_until_online(&self, interval: Duration, timeout: Option<Duration>) {
        let start = Instant::now();
        loop {
            // Force a fresh check so callers don't have to wait for
            // the 30‑second background refresher.
            if self.is_online(true).await {
                return;
            }
            // Poll again if time is not up
            if let Some(timeout) = timeout
                && start.elapsed() > timeout
            {
                return;
            }
            time::sleep(interval).await;
        }
    }
}

// -------------------------------------------------------------------------
// Background task
// -------------------------------------------------------------------------

async fn background_refresher(tx: watch::Sender<bool>) {
    let mut ticker = time::interval(Duration::from_secs(60));
    ticker.tick().await; // Discard the immediate tick; spawn() already forced a fresh value.

    loop {
        ticker.tick().await;
        let ok = dbus_utils::internet_availability(false);
        let _ = tx.send_if_modified(|old| {
            if *old != ok {
                *old = ok;
                true
            } else {
                false
            }
        });
    }
}
