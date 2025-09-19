//! Firmware / software updater support for setupd.
use crate::{cfg, constant};
use anyhow::{Context, Result};
use rand::Rng;
use regex::Regex;
use semver::Version;
use serde::Deserialize;
use std::{process::Stdio, sync::OnceLock, time::Duration};
use tokio::{
    fs,
    io::{AsyncBufReadExt, AsyncSeekExt, BufReader, SeekFrom},
    process::Command,
    select,
    signal::unix::{SignalKind, signal},
    sync::mpsc,
    time,
};

// ---------- Cache ----------
static REMOTE_VERSIONS: OnceLock<UpstreamVersion> = OnceLock::new();

// ---------- Public API ----------

/// Return `Ok(true)` when the running build is **below** the distributor’s
/// minimum supported version and an update is therefore required.
pub async fn is_update_required() -> Result<bool> {
    let current = cfg::current_version().await?;
    let remote_versions = fetch_remote_version().await?;
    Ok(current < remote_versions.min_version)
}

/// Return the latest version from the remote server.
pub async fn latest_version() -> Result<String> {
    let remote_versions = fetch_remote_version().await?;
    Ok(remote_versions.latest_version.to_string())
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
                            let progress_caps = progress_regex.captures(&line);
                            let mut progress_value = "0";
                            if progress_caps.is_some() {
                                progress_value = &progress_caps.as_ref().unwrap()[1];
                                payload.push_str(&format!("{progress_value}%"));
                            }

                            let message_caps = message_regex.captures(&line);
                            if message_caps.is_some() {
                                if progress_caps.is_some() {
                                    payload.push_str(&format!(" - {}", &message_caps.as_ref().unwrap()[1]));
                                } else {
                                    payload.push_str(&message_caps.as_ref().unwrap()[1]);
                                }
                            }

                            // Send progress as Ok
                            let _ = tx.send(Ok(payload)).await;
                            if progress_value == "100" {
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

// ---------- Internal helpers ----------

#[derive(Deserialize)]
struct UpstreamInfo {
    min_version: String,
    latest_version: String,
}

#[derive(Debug, Clone)]
struct UpstreamVersion {
    min_version: Version,
    latest_version: Version,
}

async fn fetch_remote_version() -> Result<UpstreamVersion> {
    if let Some(versions) = REMOTE_VERSIONS.get() {
        return Ok(versions.clone());
    }

    let url = format!(
        "{}{}{}",
        cfg::endpoint().await?,
        constant::UPDATER_UPSTREAM_CONFIG_URL_SUFFIX,
        cfg::branch().await?
    );
    let resp = reqwest::Client::new()
        .get(&url)
        .send()
        .await
        .with_context(|| format!("fetching {url}"))?;

    if !resp.status().is_success() {
        let status = resp.status();
        let body = resp
            .text()
            .await
            .unwrap_or_else(|_| "Failed to read response body".to_string());
        return Err(anyhow::anyhow!(
            "HTTP {status} from distributor at {url}: {body}"
        ));
    }

    let info: UpstreamInfo = resp.json().await.context("decoding distributor JSON")?;
    let versions = UpstreamVersion {
        min_version: Version::parse(&info.min_version).context("parsing upstream semver")?,
        latest_version: Version::parse(&info.latest_version).context("parsing upstream semver")?,
    };
    REMOTE_VERSIONS.set(versions.clone()).unwrap();
    Ok(versions)
}
