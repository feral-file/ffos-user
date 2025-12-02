use parking_lot::Mutex;
use std::collections::HashMap;
use std::fs::File;
use std::io::{BufRead, BufReader, BufWriter, Write};
use std::path::Path;
use thiserror::Error;

#[derive(Debug, Error)]
pub enum Error {
    #[error(transparent)]
    Io(#[from] std::io::Error),

    #[error("Invalid cache file format: {0}")]
    InvalidFormat(String),
}

pub type Result<T> = std::result::Result<T, Error>;

/// Simple on-disk key/value store for setup-related state.
///
/// This is intentionally small and text-based so it can be inspected and
/// edited easily on a device. Keys currently in use:
/// - `TOPIC_ID`: relayer topic identifier used by the web app.
/// - `CONNECTED`: whether the device has ever successfully connected to the internet.
/// - `PAIRED`: whether the mobile app has confirmed pairing (was able to
///   receive `TOPIC_ID` and switch from QR to web app).
#[derive(Debug)]
pub struct PersistentState {
    data: Mutex<HashMap<String, String>>,
    filepath: String,
}

pub const TOPIC_ID: &str = "topic_id";
pub const CONNECTED: &str = "connected"; // This key determines whether the device is used to connect to the internet
pub const PAIRED: &str = "paired"; // This key determines whether the device has successfully paired with the mobile app

impl PersistentState {
    pub fn new(filepath: &str) -> Result<Self> {
        let mut data = HashMap::new();
        let path = Path::new(filepath);
        if path.exists() {
            let file = File::open(path)?;
            let reader = BufReader::new(file);
            for line in reader.lines().map_while(std::result::Result::ok) {
                if line.trim().is_empty() {
                    continue;
                }
                let (key, value) = line
                    .split_once("=")
                    .ok_or(Error::InvalidFormat(line.clone()))?;
                data.insert(key.to_string(), value.to_string());
            }
        }
        Ok(Self {
            data: Mutex::new(data),
            filepath: filepath.to_string(),
        })
    }

    pub fn set(&self, key: &str, value: &str) {
        self.data.lock().insert(key.to_string(), value.to_string());
    }

    pub fn get(&self, key: &str) -> Option<String> {
        self.data.lock().get(key).cloned()
    }

    pub fn save(&self) -> Result<()> {
        let file = File::create(&self.filepath)?;
        let mut writer = BufWriter::new(file);
        for (key, value) in self.data.lock().iter() {
            writer.write_all(format!("{key}={value}\n").as_bytes())?;
        }
        Ok(())
    }
}
