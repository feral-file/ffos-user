use parking_lot::Mutex;
use std::io;
use std::process::{Command, Output};
use std::sync::{Arc, OnceLock};

/// Abstracts command execution so production code can remain unchanged while tests
/// inject mock executors.
pub trait CommandExecutor: Send + Sync {
    fn output(&self, program: &str, args: &[&str]) -> io::Result<Output>;
}

#[derive(Default)]
struct SystemCommandExecutor;

impl CommandExecutor for SystemCommandExecutor {
    fn output(&self, program: &str, args: &[&str]) -> io::Result<Output> {
        Command::new(program).args(args).output()
    }
}

fn registry() -> &'static Mutex<Option<Arc<dyn CommandExecutor>>> {
    static REGISTRY: OnceLock<Mutex<Option<Arc<dyn CommandExecutor>>>> = OnceLock::new();
    REGISTRY.get_or_init(|| Mutex::new(None))
}

/// Register a custom executor. Intended for tests.
pub fn set_command_executor(executor: Arc<dyn CommandExecutor>) {
    *registry().lock() = Some(executor);
}

/// Reset to the default system executor.
pub fn clear_command_executor() {
    *registry().lock() = None;
}

/// Execute a command, delegating to either the registered executor or the system default.
pub fn run_command(program: &str, args: &[&str]) -> io::Result<Output> {
    let executor = registry().lock().clone();
    match executor {
        Some(exec) => exec.output(program, args),
        None => SystemCommandExecutor::default().output(program, args),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::process::ExitStatusExt;
    use serial_test::serial;
    use std::sync::Arc;

    struct TestExecutor {
        invocations: Arc<Mutex<Vec<(String, Vec<String>)>>>,
    }

    impl TestExecutor {
        fn new(invocations: Arc<Mutex<Vec<(String, Vec<String>)>>>) -> Self {
            Self { invocations }
        }
    }

    impl CommandExecutor for TestExecutor {
        fn output(&self, program: &str, args: &[&str]) -> io::Result<Output> {
            self.invocations
                .lock()
                .push((program.to_string(), args.iter().map(|s| s.to_string()).collect()));
            Ok(Output {
                status: ExitStatusExt::from_raw(0),
                stdout: b"ok".to_vec(),
                stderr: Vec::new(),
            })
        }
    }

    #[test]
    #[serial]
    fn run_command_uses_custom_executor() {
        let invocations = Arc::new(Mutex::new(Vec::new()));
        let executor = Arc::new(TestExecutor::new(invocations.clone()));
        set_command_executor(executor);

        let output = run_command("echo", &["hello"]).expect("command should succeed");
        assert_eq!(output.stdout, b"ok");

        let calls = invocations.lock();
        assert_eq!(calls.len(), 1);
        assert_eq!(calls[0].0, "echo");
        assert_eq!(calls[0].1, vec!["hello".to_string()]);

        clear_command_executor();
    }

    #[test]
    #[serial]
    fn run_command_defaults_to_system_executor() {
        // We can't assert the real command output without side effects,
        // but we can ensure it executes without a custom executor registered.
        let output = run_command("true", &[]).expect("system true command should succeed");
        assert!(output.status.success());
    }
}

