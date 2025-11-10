//! connectivity.rs
//! Drop-in helper for “am I online?” logic.

use crate::dbus_client;
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
        let initial = dbus_client::internet_availability();
        let (tx, rx) = watch::channel(initial);

        let inner = Arc::new(Inner {
            tx: tx.clone(),
            rx,
            force_lock: Mutex::new(()),
        });

        // Kick off the 30-second poller
        tokio::spawn(background_refresher(tx));

        Self { inner }
    }

    // ---------------------------------------------------------------------
    // Public API
    // ---------------------------------------------------------------------

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
        let fresh = dbus_client::internet_availability();
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
            if let Some(timeout) = timeout {
                if start.elapsed() > timeout {
                    return;
                }
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

    loop {
        ticker.tick().await;
        let ok = dbus_client::internet_availability();
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::dbus_client;
    use anyhow::Result;
    use parking_lot::Mutex as ParkingMutex;
    use std::sync::Arc;
    use serial_test::serial;

    struct MockDbusClient {
        states: Arc<ParkingMutex<Vec<bool>>>,
    }

    impl MockDbusClient {
        fn new(states: Vec<bool>) -> Self {
            Self {
                states: Arc::new(ParkingMutex::new(states)),
            }
        }

        fn next_state(&self) -> bool {
            let mut guard = self.states.lock();
            if guard.len() > 1 {
                guard.remove(0)
            } else {
                guard.get(0).copied().unwrap_or(false)
            }
        }
    }

    impl dbus_client::DbusClient for MockDbusClient {
        fn internet_availability(&self) -> bool {
            self.next_state()
        }

        fn get_relayer_info(&self) -> Result<String> {
            Ok("topic".to_string())
        }

        fn check_connection(&self, _: &str, _: &str) -> Result<()> {
            Ok(())
        }
    }

    #[tokio::test]
    #[serial]
    async fn is_online_uses_cached_and_forced_values() {
        dbus_client::set_dbus_client(Arc::new(MockDbusClient::new(vec![false, true, true])));

        let connectivity = Connectivity::spawn().await;
        assert!(!connectivity.is_online(false).await);
        assert!(connectivity.is_online(true).await);

        dbus_client::clear_dbus_client();
    }

    #[tokio::test]
    #[serial]
    async fn wait_until_online_stops_when_online() {
        dbus_client::set_dbus_client(Arc::new(MockDbusClient::new(vec![
            false, false, true, true,
        ])));

        let connectivity = Connectivity::spawn().await;
        connectivity
            .wait_until_online(
                Duration::from_millis(5),
                Some(Duration::from_millis(100)),
            )
            .await;
        assert!(connectivity.is_online(false).await);

        dbus_client::clear_dbus_client();
    }

    #[tokio::test]
    #[serial]
    async fn wait_until_online_respects_timeout() {
        dbus_client::set_dbus_client(Arc::new(MockDbusClient::new(vec![false, false, false])));

        let connectivity = Connectivity::spawn().await;
        connectivity
            .wait_until_online(
                Duration::from_millis(5),
                Some(Duration::from_millis(20)),
            )
            .await;
        assert!(!connectivity.is_online(false).await);

        dbus_client::clear_dbus_client();
    }
}
