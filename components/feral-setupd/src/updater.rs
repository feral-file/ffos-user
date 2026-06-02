//! Firmware / software updater support for setupd.
use crate::{cfg, constant};
use anyhow::{Context, Result};
use rand::Rng;
use regex::Regex;
use semver::Version;
use serde::Deserialize;
use std::fmt;
use std::{process::Stdio, sync::RwLock, time::Duration};
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
static REMOTE_VERSIONS: RwLock<Option<UpstreamVersion>> = RwLock::new(None);

// ---------- Version fetch errors / progress ----------

/// Notifies `(current_attempt, max_attempts)` **before** each HTTP attempt (1-based),
/// so setup UI can refresh a generic “checking for updates” line while `fetch_remote_version` retries.
pub type FetchProgressTx = mpsc::Sender<(u32, u32)>;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum VersionFetchFailureKind {
    Network,
    Server,
    Client,
    Parse,
    Unknown,
}

impl VersionFetchFailureKind {
    #[must_use]
    pub fn tv_message(self) -> &'static str {
        match self {
            Self::Network => constant::VERSION_CHECK_FAILED_NETWORK_MSG,
            Self::Server => constant::VERSION_CHECK_FAILED_SERVER_MSG,
            Self::Client => constant::VERSION_CHECK_FAILED_CLIENT_MSG,
            Self::Parse => constant::VERSION_CHECK_FAILED_PARSE_MSG,
            Self::Unknown => constant::VERSION_CHECK_FAILED_UNKNOWN_MSG,
        }
    }
}

#[derive(Debug)]
pub struct VersionFetchError {
    kind: VersionFetchFailureKind,
    source: anyhow::Error,
}

impl VersionFetchError {
    pub fn new(kind: VersionFetchFailureKind, source: anyhow::Error) -> Self {
        Self { kind, source }
    }

    #[must_use]
    pub fn kind(&self) -> VersionFetchFailureKind {
        self.kind
    }
}

impl fmt::Display for VersionFetchError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{:?}: {}", self.kind, self.source)
    }
}

impl std::error::Error for VersionFetchError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        Some(self.source.as_ref())
    }
}

fn classify_version_fetch_error(err: &anyhow::Error) -> VersionFetchFailureKind {
    let full = err.to_string();
    const HTTP_PREFIX: &str = "HTTP ";
    if full.starts_with(HTTP_PREFIX) {
        if let Some(rest) = full.strip_prefix(HTTP_PREFIX) {
            if let Some(c) = rest.chars().next() {
                if c == '4' {
                    return VersionFetchFailureKind::Client;
                }
                if c == '5' {
                    return VersionFetchFailureKind::Server;
                }
            }
        }
    }

    for cause in err.chain() {
        if let Some(re) = cause.downcast_ref::<reqwest::Error>() {
            if re.is_decode() {
                return VersionFetchFailureKind::Parse;
            }
            if re.is_timeout() || re.is_connect() {
                return VersionFetchFailureKind::Network;
            }
            if let Some(status) = re.status() {
                if status.is_server_error() {
                    return VersionFetchFailureKind::Server;
                }
                if status.is_client_error() {
                    return VersionFetchFailureKind::Client;
                }
            } else {
                // A reqwest error carrying no HTTP status is a transport-layer failure
                // (DNS, TLS, connection/body transport). Classify as network so the most
                // common onboarding failure — flaky Wi-Fi — gets the actionable Wi-Fi copy
                // instead of the umbrella "contact support" message.
                return VersionFetchFailureKind::Network;
            }
        }
    }

    if full.contains("decoding distributor JSON")
        || full.contains("parsing upstream semver")
        || full.contains("parsing min_upgradeable_version semver")
    {
        return VersionFetchFailureKind::Parse;
    }

    VersionFetchFailureKind::Unknown
}

// ---------- Public API ----------

/// Force refresh the cached remote version information.
///
/// Returns the classified error when the live fetch fails so callers on a
/// user-triggered/blocking path can surface actionable copy instead of silently
/// falling back to (possibly stale) cached metadata. `progress` is used during setup
/// to drive TV copy between HTTP attempts; periodic refresh passes `None` so we never
/// spam the UI from the background task.
pub async fn refresh_remote_version(
    progress: Option<FetchProgressTx>,
) -> std::result::Result<(), VersionFetchError> {
    fetch_remote_version(true, progress).await.map(|_| ())
}

/// Spawn a background task that periodically refreshes the cached remote version.
/// The refresh happens every hour (configured via `UPDATER_REMOTE_VERSION_REFRESH_INTERVAL`).
pub fn spawn_remote_version_refresher() {
    tokio::spawn(async {
        let interval = Duration::from_millis(constant::UPDATER_REMOTE_VERSION_REFRESH_INTERVAL);
        loop {
            time::sleep(interval).await;
            println!("UPDATER: Periodic remote version refresh triggered");
            // Background refresh tolerates failures: keep serving the last-known cache.
            let _ = refresh_remote_version(None).await;
        }
    });
}

/// Return `Ok(true)` when the running build is **below** the distributor's
/// minimum supported version and an update is therefore required.
pub async fn is_update_required(
    progress: Option<FetchProgressTx>,
) -> std::result::Result<bool, VersionFetchError> {
    let current = cfg::current_version()
        .await
        .map_err(|e| VersionFetchError::new(VersionFetchFailureKind::Unknown, e))?;
    let remote_versions = fetch_remote_version(false, progress).await?;
    Ok(current < remote_versions.min_runtime_version)
}

/// Return `Ok(true)` when a newer version is available from the distributor.
pub async fn is_update_available(
    progress: Option<FetchProgressTx>,
) -> std::result::Result<bool, VersionFetchError> {
    let current = cfg::current_version()
        .await
        .map_err(|e| VersionFetchError::new(VersionFetchFailureKind::Unknown, e))?;
    let remote_versions = fetch_remote_version(false, progress).await?;
    Ok(current < remote_versions.latest_version)
}

/// Return the latest version from the remote server.
pub async fn latest_version() -> std::result::Result<String, VersionFetchError> {
    let remote_versions = fetch_remote_version(false, None).await?;
    Ok(remote_versions.latest_version.to_string())
}

/// Return `Ok(true)` when the running build is **below** the distributor's
/// minimum upgradeable version, meaning the device needs to be reflashed.
pub async fn is_too_old_to_upgrade(
    progress: Option<FetchProgressTx>,
) -> std::result::Result<bool, VersionFetchError> {
    let current = cfg::current_version()
        .await
        .map_err(|e| VersionFetchError::new(VersionFetchFailureKind::Unknown, e))?;
    let remote_versions = fetch_remote_version(false, progress).await?;

    if let Some(min_upgradeable) = &remote_versions.min_upgradeable_version {
        Ok(current < *min_upgradeable)
    } else {
        Ok(false)
    }
}

/// Return the flashing guide URL from the remote server, if available.
pub async fn flashing_guide_url() -> std::result::Result<Option<String>, VersionFetchError> {
    let remote_versions = fetch_remote_version(false, None).await?;
    Ok(remote_versions.flashing_guide.clone())
}

/// Return the minimum upgradeable version from the remote server, if available.
pub async fn min_upgradeable_version() -> std::result::Result<Option<String>, VersionFetchError> {
    let remote_versions = fetch_remote_version(false, None).await?;
    Ok(remote_versions
        .min_upgradeable_version
        .as_ref()
        .map(|v| v.to_string()))
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

// ---------- Internal helpers ----------

#[derive(Deserialize)]
struct UpstreamInfo {
    min_runtime_version: String,
    min_upgradeable_version: Option<String>,
    flashing_guide: Option<String>,
    latest_version: String,
}

#[derive(Debug, Clone)]
struct UpstreamVersion {
    min_runtime_version: Version,
    min_upgradeable_version: Option<Version>,
    flashing_guide: Option<String>,
    latest_version: Version,
}

async fn fetch_remote_version(
    refresh: bool,
    progress: Option<FetchProgressTx>,
) -> std::result::Result<UpstreamVersion, VersionFetchError> {
    // Check if we have a cached version
    if !refresh {
        let cache = REMOTE_VERSIONS.read().unwrap();
        if let Some(versions) = cache.as_ref() {
            return Ok(versions.clone());
        }
    }

    let endpoint = cfg::endpoint()
        .await
        .map_err(|e| VersionFetchError::new(VersionFetchFailureKind::Unknown, e))?;
    let branch = cfg::branch()
        .await
        .map_err(|e| VersionFetchError::new(VersionFetchFailureKind::Unknown, e))?;
    let url = format!(
        "{}{}{}",
        endpoint,
        constant::UPDATER_UPSTREAM_CONFIG_URL_SUFFIX,
        branch
    );

    // Retry logic: attempt up to UPDATER_VERSION_CHECK_RETRIES times
    let max_retries = constant::UPDATER_VERSION_CHECK_RETRIES;
    let retry_delay = Duration::from_millis(constant::UPDATER_VERSION_CHECK_RETRY_DELAY);
    let mut last_error: Option<VersionFetchError> = None;

    for attempt in 1..=max_retries {
        if let Some(tx) = &progress {
            if tx.send((attempt, max_retries)).await.is_err() {
                // Receiver dropped (setup finished); keep fetching without UI updates.
                eprintln!(
                    "UPDATER: progress channel closed; continuing version fetch without TV updates"
                );
            }
        }

        println!("UPDATER: Fetching version info from {url} (attempt {attempt}/{max_retries})");

        match fetch_remote_version_once(&url).await {
            Ok(versions) => {
                // Store in cache
                {
                    let mut cache = REMOTE_VERSIONS.write().unwrap();
                    *cache = Some(versions.clone());
                }
                return Ok(versions);
            }
            Err(e) => {
                eprintln!("UPDATER: Version check attempt {attempt}/{max_retries} failed: {e:#}");
                let kind = classify_version_fetch_error(&e);
                last_error = Some(VersionFetchError::new(kind, e));

                // Don't sleep after the last attempt
                if attempt < max_retries {
                    time::sleep(retry_delay).await;
                }
            }
        }
    }

    Err(last_error.unwrap_or_else(|| {
        VersionFetchError::new(
            VersionFetchFailureKind::Unknown,
            anyhow::anyhow!("Version check failed after {max_retries} attempts"),
        )
    }))
}

/// Single attempt to fetch remote version (no retry logic).
async fn fetch_remote_version_once(url: &str) -> Result<UpstreamVersion> {
    let resp = reqwest::Client::new()
        .get(url)
        .timeout(Duration::from_millis(
            constant::UPDATER_VERSION_CHECK_REQUEST_TIMEOUT,
        ))
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
        min_runtime_version: Version::parse(&info.min_runtime_version)
            .context("parsing upstream semver")?,
        min_upgradeable_version: info
            .min_upgradeable_version
            .as_ref()
            .map(|v| Version::parse(v).context("parsing min_upgradeable_version semver"))
            .transpose()?,
        flashing_guide: info.flashing_guide,
        latest_version: Version::parse(&info.latest_version).context("parsing upstream semver")?,
    };

    Ok(versions)
}

#[cfg(test)]
mod classify_version_fetch_error_tests {
    use super::{VersionFetchFailureKind, classify_version_fetch_error};
    use anyhow::Context;

    #[test]
    fn http_status_line_5xx_is_server() {
        let e = anyhow::anyhow!(
            "HTTP 503 Service Unavailable from distributor at https://example.invalid: body"
        );
        assert_eq!(
            classify_version_fetch_error(&e),
            VersionFetchFailureKind::Server
        );
    }

    #[test]
    fn http_status_line_4xx_is_client() {
        let e =
            anyhow::anyhow!("HTTP 404 Not Found from distributor at https://example.invalid: body");
        assert_eq!(
            classify_version_fetch_error(&e),
            VersionFetchFailureKind::Client
        );
    }

    #[test]
    fn decoding_distributor_json_context_is_parse() {
        let e = Err::<(), _>(anyhow::anyhow!("invalid JSON"))
            .context("decoding distributor JSON")
            .unwrap_err();
        assert_eq!(
            classify_version_fetch_error(&e),
            VersionFetchFailureKind::Parse
        );
    }

    #[test]
    fn parsing_upstream_semver_context_is_parse() {
        let e = Err::<(), _>(anyhow::anyhow!("not a semver"))
            .context("parsing upstream semver")
            .unwrap_err();
        assert_eq!(
            classify_version_fetch_error(&e),
            VersionFetchFailureKind::Parse
        );
    }

    #[test]
    fn parsing_min_upgradeable_semver_context_is_parse() {
        let e = Err::<(), _>(anyhow::anyhow!("bad"))
            .context("parsing min_upgradeable_version semver")
            .unwrap_err();
        assert_eq!(
            classify_version_fetch_error(&e),
            VersionFetchFailureKind::Parse
        );
    }

    #[test]
    fn unrelated_message_is_unknown() {
        let e = anyhow::anyhow!("something else entirely");
        assert_eq!(
            classify_version_fetch_error(&e),
            VersionFetchFailureKind::Unknown
        );
    }

    /// Exercises the `reqwest::Error` downcast arms with a REAL transport error (no HTTP
    /// status), wrapped exactly like `fetch_remote_version_once` wraps it. Connecting to a
    /// closed local port is deterministic and offline, and must classify as network so flaky
    /// Wi-Fi / DNS / TLS failures get the actionable copy rather than the umbrella message.
    #[tokio::test]
    async fn transport_error_without_status_is_network() {
        let reqwest_err = reqwest::Client::new()
            .get("http://127.0.0.1:1/")
            .timeout(std::time::Duration::from_secs(2))
            .send()
            .await
            .expect_err("connecting to a closed local port should fail");
        assert!(
            reqwest_err.status().is_none(),
            "a connection failure should not carry an HTTP status"
        );
        let wrapped = anyhow::Error::new(reqwest_err).context("fetching http://127.0.0.1:1/");
        assert_eq!(
            classify_version_fetch_error(&wrapped),
            VersionFetchFailureKind::Network
        );
    }
}
