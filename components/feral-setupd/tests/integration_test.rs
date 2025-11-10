use feral_setupd::command;
use feral_setupd::encoding;
use feral_setupd::wifi_utils;
use parking_lot::Mutex as ParkingMutex;
use std::io;
use std::os::unix::process::ExitStatusExt;
use std::process::Output;
use std::sync::Arc;

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

#[tokio::test]
async fn command_executor_integrates_with_wifi_utils() {
    let executor = Arc::new(MockCommandExecutor::new(vec![
        MockCommandExecutor::success_output(""),
        MockCommandExecutor::success_output(""),
        MockCommandExecutor::success_output("home\nwork\n"),
    ]));
    command::set_command_executor(executor.clone());

    wifi_utils::connect("home", "password").unwrap();
    let ssids = wifi_utils::list_ssids(false).await.unwrap();

    assert_eq!(ssids, vec!["home".to_string(), "work".to_string()]);
    assert_eq!(executor.calls.lock().len(), 3);

    command::clear_command_executor();
}

#[test]
fn encoding_payload_roundtrip() {
    let payload = encoding::encode_payload(&[b"one", b"two", b"three"]);
    let parsed = encoding::parse_payload(&payload).expect("parse should succeed");
    assert_eq!(parsed, vec!["one", "two", "three"]);
}

