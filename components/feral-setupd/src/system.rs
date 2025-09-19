use anyhow::anyhow;
use std::fs;
use std::process::Command;
use tokio::task;

use crate::constant;

pub fn get_device_id() -> String {
    fs::read_to_string("/etc/hostname")
        .unwrap_or_else(|_| "FF1".to_string())
        .trim()
        .to_string()
}

pub async fn factory_reset() -> Result<(), anyhow::Error> {
    println!("System: Factory resetting");
    if let Err(e) = task::spawn_blocking(move || {
        println!("System: Starting factory reset service");
        let output = Command::new("systemctl")
            .args(["start", "set-factory-boot.service"])
            .output()?;

        println!("System: Factory reset service output: {output:?}");
        if !output.status.success() {
            let err_msg = String::from_utf8_lossy(&output.stderr);
            return Err(anyhow::anyhow!(
                "Failed to start factory reset service: exit code {}, error: {}",
                output.status,
                err_msg
            ));
        }

        Ok(())
    })
    .await
    {
        Err(anyhow!("failed to start factory reset thread: {e:#?}"))
    } else {
        Ok(())
    }
}

pub async fn set_time(timezone: &str, time: &str) -> Result<(), anyhow::Error> {
    let timezone = timezone.to_string();
    let time = time.to_string();
    if let Err(e) = task::spawn_blocking(move || {
        let output = Command::new(constant::TIMEZONE_CMD)
            .args([constant::TIMEZONE_INSTRUCTION, &timezone, &time])
            .output()?;

        if !output.status.success() {
            let err_msg = String::from_utf8_lossy(&output.stderr);
            return Err(anyhow::anyhow!(
                "Failed to set time: exit code {}, error: {}",
                output.status,
                err_msg
            ));
        }
        Ok(())
    })
    .await
    {
        Err(anyhow!("failed to start time setting thread: {e:#?}"))
    } else {
        Ok(())
    }
}
