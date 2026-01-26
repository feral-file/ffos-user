//! Firmware / software updater support for setupd.
use crate::{cfg, constant, dbus_utils};
use anyhow::{Context, Result};
use rand::Rng;
use regex::Regex;
use semver::Version;
use std::{process::Stdio, time::Duration};
use tokio::{
    fs,
    io::{AsyncBufReadExt, AsyncSeekExt, BufReader, SeekFrom},
    process::Command,
    select,
    signal::unix::{SignalKind, signal},
    sync::mpsc,
    time,
};

// ---------- Public API ----------

/// Aggregated version check result containing all computed version information.
/// This struct is returned by `check_version()` and provides all necessary
/// information for update decisions in a single D-Bus call.
#[derive(Debug, Clone)]
pub struct VersionCheckResult {
    /// True when the running build is below the minimum supported version
    /// and an update is therefore required.
    pub is_update_required: bool,
    /// True when a newer version is available from the distributor.
    pub is_update_available: bool,
    /// True when the running build is below the minimum upgradeable version,
    /// meaning the device needs to be reflashed.
    pub is_too_old_to_upgrade: bool,
    /// The latest version available from the remote server.
    pub latest_version: String,
    /// The flashing guide URL, if the device is too old to auto-upgrade.
    pub flashing_guide_url: Option<String>,
    /// The minimum upgradeable version, if available.
    pub min_upgradeable_version: Option<String>,
}

/// Check version information in a single D-Bus call and return all computed results.
///
/// This function fetches version info from sys-monitord via D-Bus and computes
/// all version-related flags in one go, avoiding multiple D-Bus calls.
///
/// # Arguments
/// * `force_refresh` - If true, sys-monitord will fetch fresh data from the API.
///   If false, it may return cached data.
pub async fn check_version(force_refresh: bool) -> Result<VersionCheckResult> {
    let current = cfg::current_version().await?;
    let version_info = fetch_version_info(force_refresh).await?;

    // Parse versions for comparison
    let latest =
        Version::parse(&version_info.latest_version).context("parsing latest_version semver")?;
    let min_runtime = Version::parse(&version_info.min_runtime_version)
        .context("parsing min_runtime_version semver")?;

    // Check if device is too old to auto-upgrade
    let is_too_old_to_upgrade =
        if let Some(ref min_upgradeable_str) = version_info.min_upgradeable_version {
            let min_upgradeable = Version::parse(min_upgradeable_str)
                .context("parsing min_upgradeable_version semver")?;
            current < min_upgradeable
        } else {
            false
        };

    Ok(VersionCheckResult {
        is_update_required: current < min_runtime,
        is_update_available: current < latest,
        is_too_old_to_upgrade,
        latest_version: version_info.latest_version,
        flashing_guide_url: version_info.flashing_guide,
        min_upgradeable_version: version_info.min_upgradeable_version,
    })
}

/// Spawn the updater in a background task and return a channel receiver that
/// yields each `[progress] …` payload or error. The caller can `recv().await` and forward
/// the message however it likes (e.g. to CDP).
pub fn spawn_updater() -> Result<mpsc::Receiver<Result<String, anyhow::Error>>> {
    // 16‑item buffer is enough for human‑speed progress updates; adjust if needed.
    let (tx, rx) = mpsc::channel::<Result<String, anyhow::Error>>(16);

    // Detach the async task; errors are logged.
    tokio::spawn(async move {
        if let Err(e) = run_update_and_send(tx).await {
            eprintln!("updater error: {e:#?}");
        }
    });

    Ok(rx)
}

// ---------- Internal helpers ----------

/// Fetch version info from sys-monitord via D-Bus.
/// This is a blocking call that runs in a blocking task.
async fn fetch_version_info(force_refresh: bool) -> Result<dbus_utils::VersionInfo> {
    // D-Bus calls are blocking, so run them in a blocking task
    let result = tokio::task::spawn_blocking(move || dbus_utils::get_latest_version(force_refresh))
        .await
        .context("spawn_blocking failed")?;

    result.context("failed to get version info from sys-monitord")
}

/// Internal async worker: starts the systemd unit, tails the log file,
/// and forwards each `[progress] …` line or error into the provided `mpsc::Sender`.
async fn run_update_and_send(tx: mpsc::Sender<Result<String, anyhow::Error>>) -> Result<()> {
    // Compile regex patterns once
    let id = format!("setupd-{}", rand::rng().random_range(1..=u64::MAX));
    let id_regex = Regex::new(&format!("id={id}")).context("compiling id regex")?;
    let progress_regex = Regex::new(r"progress=(\d+)").context("compiling progress regex")?;
    let message_regex = Regex::new(r#"message="([^"]*)""#).context("compiling message regex")?;

    // 1. Stop the feral-watchdog service to avoid conflicts
    let _ = Command::new("systemctl")
        .args(["--user", "stop", "feral-watchdog.service"])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .await
        .context("stopping watchdog service with systemctl")?;

    // 2. Start the systemd transient service and wait for it to finish
    let status = Command::new("systemctl")
        .args(["start", &format!("feral-updater-run@{id}.service")])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .await
        .context("starting updater service with systemctl")?;

    if !status.success() {
        return Err(anyhow::anyhow!(
            "Failed to start updater service: exit code {status:?}"
        ));
    }

    // 2. Open the log file with retry mechanism (1 minute timeout, retry every 5 seconds)
    let log_path = constant::UPDATER_PROCESS_LOG_FILE;
    let retry_duration = Duration::from_secs(60); // 1 minute total
    let retry_interval = Duration::from_secs(5); // 5 seconds between retries
    let start_time = time::Instant::now();

    let mut file = loop {
        match fs::OpenOptions::new().read(true).open(log_path).await {
            Ok(f) => break f,
            Err(e) => {
                let elapsed = start_time.elapsed();
                if elapsed >= retry_duration {
                    return Err(anyhow::anyhow!(
                        "Failed to open {} after {} seconds: {}",
                        log_path,
                        elapsed.as_secs(),
                        e
                    ));
                }

                // Wait before next retry
                time::sleep(retry_interval).await;
            }
        }
    };

    file.seek(SeekFrom::End(0))
        .await
        .context("seeking to end of file")?;
    let mut reader = BufReader::new(file).lines();

    // 3. Tail the file in a loop
    let mut sigint = signal(SignalKind::interrupt())?;
    let mut sigterm = signal(SignalKind::terminate())?;

    loop {
        select! {
            maybe_line = reader.next_line() => {
                match maybe_line? {
                    Some(line) => {
                        // First check if line contains our generated id
                        if !id_regex.is_match(&line) {
                            continue;
                        }

                        // Check for [PROGRESS] lines
                        if line.contains("[PROGRESS]") {
                            let mut payload = String::new();
                            let mut progress_value = None;
                            if let Some(progress_caps) = progress_regex.captures(&line) {
                                let value = progress_caps[1].to_string();
                                payload.push_str(&format!("{value}%"));
                                progress_value = Some(value);
                            }

                            if let Some(message_caps) = message_regex.captures(&line) {
                                if progress_value.is_some() {
                                    payload.push_str(&format!(" - {}", &message_caps[1]));
                                } else {
                                    payload.push_str(&message_caps[1]);
                                }
                            }

                            // Send progress as Ok
                            let _ = tx.send(Ok(payload)).await;
                            if progress_value.as_deref() == Some("100") {
                                break; // End the process
                            }
                        }
                        // Check for [ERROR] lines
                        else if line.contains("[ERROR]") {
                            let error_message = if let Some(error_caps) = message_regex.captures(&line) {
                                error_caps[1].to_string()
                            } else {
                                "Unknown error occurred".to_string()
                            };

                            // Send error as Err
                            let _ = tx.send(Err(anyhow::anyhow!(error_message))).await;
                            break; // End the process
                        }
                    }
                    None => { time::sleep(Duration::from_millis(200)).await; }
                }
            }
            _ = sigint.recv() => break,
            _ = sigterm.recv() => break,
        }
    }
    Ok(())
}
