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
}

#[derive(Debug, Clone)]
struct RunningBuild {
    pub branch: String,
    pub version: Version,
    pub endpoint: String,
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
    };
    CURRENT_BUILD.set(build.clone()).unwrap();
    Ok(build)
}
