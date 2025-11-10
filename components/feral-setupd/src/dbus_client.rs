use crate::dbus_utils;
use anyhow::Result;
use parking_lot::Mutex;
use std::sync::{Arc, OnceLock};

pub trait DbusClient: Send + Sync {
    fn internet_availability(&self) -> bool;
    fn get_relayer_info(&self) -> Result<String>;
    fn check_connection(&self, destination: &str, object_path: &str) -> Result<()>;
}

#[derive(Default)]
struct SystemDbusClient;

impl DbusClient for SystemDbusClient {
    fn internet_availability(&self) -> bool {
        dbus_utils::internet_availability()
    }

    fn get_relayer_info(&self) -> Result<String> {
        dbus_utils::get_relayer_info()
    }

    fn check_connection(&self, destination: &str, object_path: &str) -> Result<()> {
        dbus_utils::check_dbus_connection(destination, object_path)
    }
}

fn registry() -> &'static Mutex<Option<Arc<dyn DbusClient>>> {
    static REGISTRY: OnceLock<Mutex<Option<Arc<dyn DbusClient>>>> = OnceLock::new();
    REGISTRY.get_or_init(|| Mutex::new(None))
}

fn default_client() -> Arc<dyn DbusClient> {
    static DEFAULT: OnceLock<Arc<SystemDbusClient>> = OnceLock::new();
    let client = DEFAULT.get_or_init(|| Arc::new(SystemDbusClient::default()));
    Arc::clone(client) as Arc<dyn DbusClient>
}

pub fn set_dbus_client(client: Arc<dyn DbusClient>) {
    *registry().lock() = Some(client);
}

pub fn clear_dbus_client() {
    *registry().lock() = None;
}

pub fn get_dbus_client() -> Arc<dyn DbusClient> {
    registry()
        .lock()
        .clone()
        .unwrap_or_else(default_client)
}

pub fn internet_availability() -> bool {
    get_dbus_client().internet_availability()
}

pub fn get_relayer_info() -> Result<String> {
    get_dbus_client().get_relayer_info()
}

pub fn check_connection(destination: &str, object_path: &str) -> Result<()> {
    get_dbus_client().check_connection(destination, object_path)
}

#[cfg(test)]
mod tests {
    use super::*;

    struct TestDbusClient {
        online: bool,
        relayer: Result<String>,
        connection: Result<()>,
    }

    impl DbusClient for TestDbusClient {
        fn internet_availability(&self) -> bool {
            self.online
        }

        fn get_relayer_info(&self) -> Result<String> {
            match &self.relayer {
                Ok(value) => Ok(value.clone()),
                Err(err) => Err(anyhow::anyhow!(err.to_string())),
            }
        }

        fn check_connection(&self, _: &str, _: &str) -> Result<()> {
            match &self.connection {
                Ok(_) => Ok(()),
                Err(err) => Err(anyhow::anyhow!(err.to_string())),
            }
        }
    }

    #[test]
    fn uses_overridden_client() {
        let client = Arc::new(TestDbusClient {
            online: true,
            relayer: Ok("topic-123".to_string()),
            connection: Ok(()),
        });
        set_dbus_client(client);

        assert!(internet_availability());
        assert_eq!(get_relayer_info().unwrap(), "topic-123");
        assert!(check_connection("dest", "/obj").is_ok());

        clear_dbus_client();
    }

    #[test]
    fn reset_reverts_to_default() {
        clear_dbus_client();
        // Without a mock, the default client may fail if D-Bus is not available.
        // We only verify that the fallback path executes without panicking.
        let _ = internet_availability();
    }
}

