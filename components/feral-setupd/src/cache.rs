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

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use tempfile::NamedTempFile;

    #[test]
    fn new_without_existing_file_creates_empty_cache() {
        let file = NamedTempFile::new().expect("create temp file");
        let path = file.path().to_str().unwrap().to_string();
        std::fs::remove_file(&path).expect("delete temp file to simulate missing file");

        let cache = Cache::new(&path).expect("cache should be created");
        assert!(cache.get("any").is_none());
    }

    #[test]
    fn new_with_existing_file_loads_entries() {
        let mut file = NamedTempFile::new().unwrap();
        writeln!(file, "foo=bar").unwrap();
        writeln!(file, "baz=qux").unwrap();

        let cache = Cache::new(file.path().to_str().unwrap()).expect("cache should load");
        assert_eq!(cache.get("foo").as_deref(), Some("bar"));
        assert_eq!(cache.get("baz").as_deref(), Some("qux"));
    }

    #[test]
    fn new_with_invalid_format_returns_error() {
        let mut file = NamedTempFile::new().unwrap();
        writeln!(file, "invalid_line_without_equal").unwrap();

        let err = Cache::new(file.path().to_str().unwrap()).unwrap_err();
        match err {
            Error::InvalidFormat(line) => {
                assert_eq!(line.trim(), "invalid_line_without_equal");
            }
            other => panic!("unexpected error: {other:?}"),
        }
    }

    #[test]
    fn set_get_and_save_roundtrip() {
        let file = NamedTempFile::new().unwrap();
        let path = file.path().to_str().unwrap();

        let cache = Cache::new(path).expect("cache should be created");
        cache.set("key", "value");
        cache.set("another", "entry");
        assert_eq!(cache.get("key").as_deref(), Some("value"));

        cache.save(path).expect("save should succeed");

        let reloaded = Cache::new(path).expect("cache should reload");
        assert_eq!(reloaded.get("key").as_deref(), Some("value"));
        assert_eq!(reloaded.get("another").as_deref(), Some("entry"));
    }

    #[test]
    fn save_preserves_empty_cache() {
        let file = NamedTempFile::new().unwrap();
        let path = file.path().to_str().unwrap();

        let cache = Cache::new(path).expect("cache should be created");
        cache.save(path).expect("save should succeed");

        let contents = std::fs::read_to_string(path).unwrap();
        assert!(contents.is_empty());
    }
}
