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

#[derive(Debug)]
pub struct Cache {
    data: Mutex<HashMap<String, String>>,
}

pub const TOPIC_ID: &str = "topic_id";
pub const CONNECTED: &str = "connected"; // This key determines whether the device is used to connect to the internet

impl Cache {
    pub fn new(filepath: &str) -> Result<Self> {
        let mut data = HashMap::new();
        if Path::new(filepath).exists() {
            let file = File::open(filepath)?;
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
        })
    }

    pub fn set(&self, key: &str, value: &str) {
        self.data.lock().insert(key.to_string(), value.to_string());
    }

    pub fn get(&self, key: &str) -> Option<String> {
        self.data.lock().get(key).cloned()
    }

    pub fn save(&self, filepath: &str) -> Result<()> {
        let file = File::create(filepath)?;
        let mut writer = BufWriter::new(file);
        for (key, value) in self.data.lock().iter() {
            writer.write_all(format!("{key}={value}\n").as_bytes())?;
        }
        Ok(())
    }
}
