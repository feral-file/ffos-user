use std::collections::HashSet;
use std::sync::Arc;
use std::time::{Duration, Instant};
use thiserror::Error;
use tokio::sync::{Mutex, Notify};
use tokio::task;

use crate::{command, constant};

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

    /// Get SSIDs, waiting only if a refresh is currently in progress or required.
    /// If the last scan failed, or return empty list of SSIDs, the first "get" will clear the cache
    /// Thus, the next "get" will trigger a new refresh
    pub async fn get(&self) -> Result<Vec<String>> {
        loop {
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
            self.notify.notified().await;
        }
    }
}

pub fn connect(ssid: &str, pass: &str) -> Result<()> {
    // delete any existing connection, don't care if it fails
    // we need this because of a bug with nmcli
    // https://bbs.archlinux.org/viewtopic.php?id=300321&p=2

    if let Err(err) = delete(ssid) {
        eprintln!("Wifi: failed to delete existing connection: {err}");
    }

    println!("Wifi: connecting to {ssid}");
    let args = [
        "device",
        "wifi",
        "connect",
        ssid,
        "password",
        pass,
    ];
    let output = command::run_command("nmcli", &args)?;

    if output.status.success() {
        Ok(())
    } else {
        Err(Error::NmcliFailure {
            status: output.status,
            stderr: String::from_utf8_lossy(&output.stderr).to_string(),
        })
    }
}

fn delete(ssid: &str) -> Result<()> {
    let args = ["connection", "delete", ssid];
    let output = command::run_command("nmcli", &args)?;

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
    let output = task::spawn_blocking(move || {
        let mut args = vec!["-t", "-f", "SSID", "device", "wifi", "list"];
        if force {
            args.push("--rescan");
            args.push("yes");
        }
        command::run_command("nmcli", &args)
    })
    .await? // JoinError → Error::Join
    ?; // IoError → Error::Io

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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::command;
    use parking_lot::Mutex as ParkingMutex;
    use std::io;
    use std::os::unix::process::ExitStatusExt;
    use std::process::Output;
    use std::sync::Arc;
    use serial_test::serial;

    struct MockCommandExecutor {
        outputs: ParkingMutex<Vec<io::Result<Output>>>,
        calls: ParkingMutex<Vec<(String, Vec<String>)>>,
    }

    impl MockCommandExecutor {
        fn new(outputs: Vec<io::Result<Output>>) -> Self {
            Self {
                outputs: ParkingMutex::new(outputs),
                calls: ParkingMutex::new(Vec::new()),
            }
        }

        fn success_output(stdout: &str) -> io::Result<Output> {
            Ok(Output {
                status: ExitStatusExt::from_raw(0),
                stdout: stdout.as_bytes().to_vec(),
                stderr: Vec::new(),
            })
        }

        fn failure_output(stderr: &str) -> io::Result<Output> {
            Ok(Output {
                status: ExitStatusExt::from_raw(1),
                stdout: Vec::new(),
                stderr: stderr.as_bytes().to_vec(),
            })
        }

        fn default_success_output() -> io::Result<Output> {
            Ok(Output {
                status: ExitStatusExt::from_raw(0),
                stdout: Vec::new(),
                stderr: Vec::new(),
            })
        }
    }

    impl command::CommandExecutor for MockCommandExecutor {
        fn output(&self, program: &str, args: &[&str]) -> io::Result<Output> {
            self.calls.lock().push((
                program.to_string(),
                args.iter().map(|s| s.to_string()).collect(),
            ));
            let mut outputs = self.outputs.lock();
            if outputs.is_empty() {
                Self::default_success_output()
            } else {
                outputs.remove(0)
            }
        }
    }

    fn install_executor(executor: Arc<MockCommandExecutor>) {
        command::set_command_executor(executor);
    }

    fn uninstall_executor() {
        command::clear_command_executor();
    }

    #[test]
    #[serial]
    fn connect_successful() {
        let executor = Arc::new(MockCommandExecutor::new(vec![
            MockCommandExecutor::success_output(""),
            MockCommandExecutor::success_output(""),
        ]));
        install_executor(executor.clone());

        assert!(connect("my-ssid", "secret").is_ok());
        assert_eq!(executor.calls.lock().len(), 2);

        uninstall_executor();
    }

    #[test]
    #[serial]
    fn connect_failure_returns_error() {
        let stderr = "Error: password incorrect";
        let executor = Arc::new(MockCommandExecutor::new(vec![
            MockCommandExecutor::success_output(""),
            MockCommandExecutor::failure_output(stderr),
        ]));
        install_executor(executor.clone());

        let err = connect("ssid", "pwd").unwrap_err();
        assert!(matches!(err, Error::NmcliFailure { .. }));
        assert_eq!(executor.calls.lock().len(), 2);

        uninstall_executor();
    }

    #[tokio::test]
    #[serial]
    async fn list_ssids_parses_unique_values() {
        let executor = Arc::new(MockCommandExecutor::new(vec![
            MockCommandExecutor::success_output("Home\nOffice\nHome\n\n"),
        ]));
        install_executor(executor.clone());

        let ssids = list_ssids(false).await.unwrap();
        assert_eq!(ssids, vec!["Home".to_string(), "Office".to_string()]);
        assert_eq!(executor.calls.lock().len(), 1);

        uninstall_executor();
    }

    #[tokio::test]
    #[serial]
    async fn ssids_cacher_returns_cached_results() {
        let executor = Arc::new(MockCommandExecutor::new(vec![
            MockCommandExecutor::success_output("Cafe\nLibrary\n"),
        ]));
        install_executor(executor.clone());

        let cacher = SSIDsCacher::new();
        let first = cacher.get().await.unwrap();
        assert_eq!(first, vec!["Cafe".to_string(), "Library".to_string()]);

        let second = cacher.get().await.unwrap();
        assert_eq!(second, first);
        assert_eq!(executor.calls.lock().len(), 1);

        uninstall_executor();
    }

    #[tokio::test]
    #[serial]
    async fn ssids_cacher_propagates_errors() {
        let executor = Arc::new(MockCommandExecutor::new(vec![
            MockCommandExecutor::failure_output("nmcli failed"),
        ]));
        install_executor(executor);

        let cacher = SSIDsCacher::new();
        let err = cacher.get().await.unwrap_err();
        assert!(matches!(err, Error::NmcliFailure { .. }));

        uninstall_executor();
    }
}
