use crate::constant;
use anyhow::{Context, Result};
use semver::Version;
use serde::Deserialize;
use std::sync::OnceLock;
use tokio::fs;

static CURRENT_BUILD: OnceLock<RunningBuild> = OnceLock::new();

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

pub async fn current_version() -> Result<Version> {
    let current = current_cfg().await?;
    Ok(current.version)
}

pub async fn endpoint() -> Result<String> {
    let current = current_cfg().await?;
    Ok(current.endpoint)
}

/// Optional override from `ff1-config.json` (`webapp_url`), trimmed; `None` if absent or blank.
/// Semantics match `ff1config.ResolveWebappURL` / `DefaultWebappURL` on the Go side.
pub async fn webapp_url() -> Result<Option<String>> {
    let current = current_cfg().await?;
    Ok(current.webapp_url)
}

async fn current_cfg() -> Result<RunningBuild> {
    if let Some(build) = CURRENT_BUILD.get() {
        return Ok(build.clone());
    }

    let buf = fs::read_to_string(constant::UPDATER_LOCAL_CONFIG_PATH)
        .await
        .context("reading local config")?;
    let cfg: LocalConfigJSON = serde_json::from_str(&buf).context("parsing local config JSON")?;
    let version = Version::parse(&cfg.version).context("parsing local semver")?;
    let build = RunningBuild {
        branch: cfg.branch,
        version,
        endpoint: cfg.endpoint,
        webapp_url: normalize_webapp_url(cfg.webapp_url),
    };
    CURRENT_BUILD.set(build.clone()).unwrap();
    Ok(build)
}

fn normalize_webapp_url(raw: Option<String>) -> Option<String> {
    raw.and_then(|s| {
        let t = s.trim();
        if t.is_empty() {
            None
        } else {
            Some(t.to_string())
        }
    })
}

#[cfg(test)]
mod tests {
    use super::normalize_webapp_url;

    #[test]
    fn normalize_none_stays_none() {
        assert_eq!(normalize_webapp_url(None), None);
    }

    #[test]
    fn normalize_whitespace_to_none() {
        assert_eq!(normalize_webapp_url(Some("  \t  ".to_string())), None);
    }

    #[test]
    fn normalize_trims_and_keeps() {
        assert_eq!(
            normalize_webapp_url(Some("  https://x.test/path  ".to_string())),
            Some("https://x.test/path".to_string())
        );
    }
}
