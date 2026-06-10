use parking_lot::Mutex;
use std::collections::HashMap;
use std::fs::{self, File};
use std::io::{BufRead, BufReader, BufWriter, Write};
use std::path::Path;
use std::sync::Mutex as StdMutex;
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
/// - `setup_phase`: Current setup phase (pairing, ready, update_failed, or empty for transient states)
/// - `topic_id`: Relayer topic identifier used by the web app
/// - `connected`: Whether the device has ever successfully connected to the internet
///
/// The `save_lock` ensures concurrent saves from BLE, D-Bus, startup, and
/// update tasks are serialized to prevent temp file overwrites and lost updates.
#[derive(Debug)]
pub struct PersistentState {
    data: Mutex<HashMap<String, String>>,
    filepath: String,
    save_lock: StdMutex<()>,
}

pub const SETUP_PHASE: &str = "setup_phase";
pub const TOPIC_ID: &str = "topic_id";
pub const CONNECTED: &str = "connected";

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
            save_lock: StdMutex::new(()),
        })
    }

    pub fn set(&self, key: &str, value: &str) {
        self.data.lock().insert(key.to_string(), value.to_string());
    }

    pub fn get(&self, key: &str) -> Option<String> {
        self.data.lock().get(key).cloned()
    }

    /// Save state atomically using write-temp-then-rename.
    ///
    /// This ensures power loss during save cannot corrupt the state file,
    /// which now includes recovery-critical data like `update_failure_latch`.
    ///
    /// The save_lock ensures concurrent saves from BLE, D-Bus, startup, and
    /// update tasks don't race on the temp file or lose updates to the
    /// update_failure_latch recovery signal.
    pub fn save(&self) -> Result<()> {
        // Serialize the entire save transaction across all callers
        let _guard = self.save_lock.lock().unwrap();

        let temp_path = format!("{}.tmp", self.filepath);

        // Write to temp file
        {
            let file = File::create(&temp_path)?;
            let mut writer = BufWriter::new(file);
            for (key, value) in self.data.lock().iter() {
                writer.write_all(format!("{key}={value}\n").as_bytes())?;
            }
            writer.flush()?;
            writer.get_ref().sync_all()?;
        }

        // Atomic rename
        fs::rename(&temp_path, &self.filepath)?;

        // Sync parent directory for durability (best-effort)
        if let Some(parent) = Path::new(&self.filepath).parent() {
            if let Ok(dir) = File::open(parent) {
                let _ = dir.sync_all();
            }
        }

        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    use std::thread;

    #[test]
    fn concurrent_saves_are_serialized() {
        let temp_dir = tempfile::tempdir().unwrap();
        let state_file = temp_dir.path().join("state.txt");
        let store = Arc::new(PersistentState::new(state_file.to_str().unwrap()).unwrap());

        // Spawn multiple threads doing concurrent saves
        let handles: Vec<_> = (0..10)
            .map(|i| {
                let store = Arc::clone(&store);
                thread::spawn(move || {
                    store.set(&format!("key{i}"), &format!("value{i}"));
                    store.save().unwrap();
                })
            })
            .collect();

        for handle in handles {
            handle.join().unwrap();
        }

        // Verify all keys made it through
        for i in 0..10 {
            assert_eq!(store.get(&format!("key{i}")), Some(format!("value{i}")));
        }
    }

    #[test]
    fn save_and_restore_setup_phase() {
        let temp_dir = tempfile::tempdir().unwrap();
        let state_file = temp_dir.path().join("state.txt");
        let store = PersistentState::new(state_file.to_str().unwrap()).unwrap();

        // Set setup phase and topic_id
        store.set(SETUP_PHASE, "pairing");
        store.set(TOPIC_ID, "test-topic");
        store.save().unwrap();

        // Reload from disk
        let reloaded = PersistentState::new(state_file.to_str().unwrap()).unwrap();
        assert_eq!(reloaded.get(SETUP_PHASE), Some("pairing".to_string()));
        assert_eq!(reloaded.get(TOPIC_ID), Some("test-topic".to_string()));
    }
}
