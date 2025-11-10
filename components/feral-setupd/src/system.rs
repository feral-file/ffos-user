use anyhow::anyhow;
use parking_lot::Mutex;
use std::{env, fs, path::PathBuf, sync::OnceLock};
use tokio::task;

use crate::{command, constant};

pub fn get_device_id() -> String {
    fs::read_to_string(hostname_path())
        .unwrap_or_else(|_| "FF1".to_string())
        .trim()
        .to_string()
}

pub async fn factory_reset() -> Result<(), anyhow::Error> {
    println!("System: Factory resetting");
    match task::spawn_blocking(move || {
        println!("System: Starting factory reset service");
        let output = command::run_command("systemctl", &["start", "set-factory-boot.service"])?;

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
        Ok(Ok(())) => Ok(()),
        Ok(Err(e)) => Err(e),
        Err(e) => Err(anyhow!("failed to start factory reset thread: {e:#?}")),
    }
}

fn hostname_path() -> String {
    if let Some(path) = hostname_override().lock().clone() {
        return path.to_string_lossy().into_owned();
    }
    env::var("SETUPD_HOSTNAME_PATH").unwrap_or_else(|_| "/etc/hostname".to_string())
}

fn hostname_override() -> &'static Mutex<Option<PathBuf>> {
    static OVERRIDE: OnceLock<Mutex<Option<PathBuf>>> = OnceLock::new();
    OVERRIDE.get_or_init(|| Mutex::new(None))
}

#[cfg(test)]
fn set_hostname_override(path: Option<PathBuf>) {
    let mut guard = hostname_override().lock();
    *guard = path;
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
    use tempfile::NamedTempFile;
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
    fn get_device_id_reads_override() {
        let file = NamedTempFile::new().unwrap();
        std::fs::write(file.path(), "device123\n").unwrap();
        set_hostname_override(Some(file.path().to_path_buf()));

        assert_eq!(get_device_id(), "device123");

        set_hostname_override(None);
    }

    #[test]
    #[serial]
    fn get_device_id_falls_back_to_default() {
        set_hostname_override(Some(PathBuf::from(
            "/nonexistent/path/for/setupd/tests",
        )));

        assert_eq!(get_device_id(), "FF1");

        set_hostname_override(None);
    }

    #[tokio::test]
    #[serial]
    async fn factory_reset_invokes_systemctl() {
        let executor = Arc::new(MockCommandExecutor::new(vec![
            MockCommandExecutor::success_output("ok"),
        ]));
        install_executor(executor.clone());

        assert!(factory_reset().await.is_ok());
        let calls = executor.calls.lock();
        assert_eq!(calls.len(), 1);
        assert_eq!(calls[0].0, "systemctl");
        assert_eq!(calls[0].1, vec!["start".to_string(), "set-factory-boot.service".to_string()]);

        uninstall_executor();
    }

    #[tokio::test]
    #[serial]
    async fn factory_reset_propagates_failure() {
        let executor = Arc::new(MockCommandExecutor::new(vec![
            MockCommandExecutor::failure_output("fail"),
        ]));
        install_executor(executor);

        assert!(factory_reset().await.is_err());

        uninstall_executor();
    }

    #[tokio::test]
    #[serial]
    async fn set_time_passes_arguments() {
        let executor = Arc::new(MockCommandExecutor::new(vec![
            MockCommandExecutor::success_output("ok"),
        ]));
        install_executor(executor.clone());

        assert!(set_time("UTC", "2024-01-01T00:00:00Z").await.is_ok());

        let calls = executor.calls.lock();
        assert_eq!(calls.len(), 1);
        assert_eq!(calls[0].0, constant::TIMEZONE_CMD);
        assert_eq!(
            calls[0].1,
            vec![
                constant::TIMEZONE_INSTRUCTION.to_string(),
                "UTC".to_string(),
                "2024-01-01T00:00:00Z".to_string()
            ]
        );

        uninstall_executor();
    }

    #[tokio::test]
    #[serial]
    async fn set_time_returns_error_on_failure() {
        let executor = Arc::new(MockCommandExecutor::new(vec![
            MockCommandExecutor::failure_output("error"),
        ]));
        install_executor(executor);

        assert!(set_time("UTC", "bad").await.is_err());

        uninstall_executor();
    }
}

pub async fn set_time(timezone: &str, time: &str) -> Result<(), anyhow::Error> {
    let timezone = timezone.to_string();
    let time = time.to_string();
    match task::spawn_blocking(move || {
        let args = vec![
            constant::TIMEZONE_INSTRUCTION.to_string(),
            timezone,
            time,
        ];
        let arg_refs: Vec<&str> = args.iter().map(|s| s.as_str()).collect();
        let output = command::run_command(constant::TIMEZONE_CMD, &arg_refs)?;

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
        Ok(Ok(())) => Ok(()),
        Ok(Err(e)) => Err(e),
        Err(e) => Err(anyhow!("failed to start time setting thread: {e:#?}")),
    }
}
