use crate::constant;
use anyhow::{Context, Result};
use parking_lot::Mutex;
use semver::Version;
use serde::Deserialize;
use std::{env, path::PathBuf, sync::OnceLock};
use tokio::fs;

static CURRENT_BUILD: OnceLock<Mutex<Option<RunningBuild>>> = OnceLock::new();
static CONFIG_OVERRIDE: OnceLock<Mutex<Option<PathBuf>>> = OnceLock::new();

#[derive(Deserialize)]
struct LocalConfigJSON {
    branch: String,
    version: String,
    endpoint: String,
    webapp_url: Option<String>,
}

#[derive(Debug, Clone)]
struct RunningBuild {
    pub branch: String,
    pub version: Version,
    pub endpoint: String,
    pub webapp_url: Option<String>,
}

pub async fn branch() -> Result<String> {
    let current = current_cfg().await?;
    Ok(current.branch)
}

fn build_cache() -> &'static Mutex<Option<RunningBuild>> {
    CURRENT_BUILD.get_or_init(|| Mutex::new(None))
}

fn config_override() -> &'static Mutex<Option<PathBuf>> {
    CONFIG_OVERRIDE.get_or_init(|| Mutex::new(None))
}

pub async fn current_version() -> Result<Version> {
    let current = current_cfg().await?;
    Ok(current.version)
}

pub async fn endpoint() -> Result<String> {
    let current = current_cfg().await?;
    Ok(current.endpoint)
}

pub async fn webapp_url() -> Result<Option<String>> {
    let current = current_cfg().await?;
    Ok(current.webapp_url)
}

async fn current_cfg() -> Result<RunningBuild> {
    if let Some(build) = build_cache().lock().clone() {
        return Ok(build);
    }

    let path = config_path();
    let buf = fs::read_to_string(&path)
        .await
        .with_context(|| format!("reading local config from {}", path.display()))?;
    let cfg: LocalConfigJSON = serde_json::from_str(&buf).context("parsing local config JSON")?;
    let version = Version::parse(&cfg.version).context("parsing local semver")?;
    let build = RunningBuild {
        branch: cfg.branch,
        version,
        endpoint: cfg.endpoint,
        webapp_url: cfg.webapp_url,
    };
    build_cache().lock().replace(build.clone());
    Ok(build)
}

fn config_path() -> PathBuf {
    if let Some(path) = config_override().lock().clone() {
        return path;
    }
    env::var("SETUPD_LOCAL_CONFIG_PATH")
        .map(PathBuf::from)
        .unwrap_or_else(|_| PathBuf::from(constant::UPDATER_LOCAL_CONFIG_PATH))
}

#[cfg(test)]
pub(crate) fn reset_current_build() {
    if let Some(cache) = CURRENT_BUILD.get() {
        cache.lock().take();
    }
}

#[cfg(test)]
pub(crate) fn set_config_path_override(path: Option<PathBuf>) {
    let mut guard = config_override().lock();
    *guard = path;
}

#[cfg(test)]
mod tests {
    use super::*;
    use assert_matches::assert_matches;
    use tempfile::NamedTempFile;
    use serial_test::serial;

    fn write_config(file: &NamedTempFile, json: &str) {
        std::fs::write(file.path(), json).unwrap();
    }

    fn prepare_env(file: &NamedTempFile) {
        reset_current_build();
        set_config_path_override(Some(file.path().to_path_buf()));
    }

    fn cleanup_env() {
        set_config_path_override(None);
        reset_current_build();
    }

    fn sample_json() -> String {
        r#"{
            "branch": "main",
            "version": "1.2.3",
            "endpoint": "https://example.com",
            "webapp_url": "https://app.example.com"
        }"#
        .to_string()
    }

    #[tokio::test]
    #[serial]
    async fn reads_valid_config() {
        let file = NamedTempFile::new().unwrap();
        write_config(&file, &sample_json());
        prepare_env(&file);

        let branch_val = branch().await.expect("branch should parse");
        let version_val = current_version().await.expect("version parse");
        let endpoint_val = endpoint().await.expect("endpoint parse");
        let webapp_val = webapp_url().await.expect("webapp parse");

        assert_eq!(branch_val, "main");
        assert_eq!(version_val, Version::parse("1.2.3").unwrap());
        assert_eq!(endpoint_val, "https://example.com");
        assert_eq!(webapp_val.as_deref(), Some("https://app.example.com"));

        cleanup_env();
    }

    #[tokio::test]
    #[serial]
    async fn caches_config_after_first_read() {
        let file = NamedTempFile::new().unwrap();
        write_config(&file, &sample_json());
        prepare_env(&file);

        let first_branch = branch().await.expect("first call should succeed");
        assert_eq!(first_branch, "main");

        write_config(
            &file,
            r#"{
                "branch": "feature",
                "version": "1.2.3",
                "endpoint": "https://example.com",
                "webapp_url": null
            }"#,
        );

        let cached_branch = branch().await.expect("cached value");
        assert_eq!(cached_branch, "main");

        cleanup_env();
    }

    #[tokio::test]
    #[serial]
    async fn invalid_json_returns_error() {
        let file = NamedTempFile::new().unwrap();
        write_config(&file, "{ invalid json }");
        prepare_env(&file);

        let result = branch().await;
        assert_matches!(result, Err(_));

        cleanup_env();
    }

    #[tokio::test]
    #[serial]
    async fn invalid_semver_returns_error() {
        let file = NamedTempFile::new().unwrap();
        write_config(
            &file,
            r#"{
                "branch": "main",
                "version": "invalid",
                "endpoint": "https://example.com",
                "webapp_url": null
            }"#,
        );
        prepare_env(&file);

        let result = current_version().await;
        assert_matches!(result, Err(_));

        cleanup_env();
    }
}
