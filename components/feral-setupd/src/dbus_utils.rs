use dbus::arg::Append;
use dbus::blocking::{BlockingSender, Connection};
use dbus::channel::Sender;
use dbus::message::Message;
use dbus_crossroads::Crossroads;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};
use tokio::task;

use anyhow::{Result, anyhow};

use crate::constant;

pub type ListenCallback = Box<dyn Fn(Message) + Send + Sync>;

pub trait PageStateProvider: Send + Sync {
    fn get_page_state(&self) -> (String, String, i64);
}

pub fn start_dbus_service<T: PageStateProvider + 'static>(state_provider: Arc<T>) {
    std::thread::spawn(move || {
        println!("DBUS: start_dbus_service started");

        let conn = Connection::new_session().expect("DBUS: failed to create connection");
        conn.request_name(constant::DBUS_SETUPD_DESTINATION, false, true, false)
            .expect("DBUS: failed to request name");

        let mut cr = Crossroads::new();

        let provider = state_provider.clone();
        let iface = cr.register(constant::DBUS_SETUPD_INTERFACE, move |b| {
            let p = provider.clone();
            b.method(
                constant::DBUS_GET_PAGE_STATE,
                (),
                ("id", "page", "page_changed_unix"),
                move |_, (), ()| {
                    let (id, page, timestamp) = p.get_page_state();
                    Ok((id, page, timestamp))
                },
            );
        });
        cr.insert(constant::DBUS_SETUPD_OBJECT, &[iface], ());
        println!("DBUS: Service started");

        if let Err(e) = cr.serve(&conn) {
            eprintln!("DBUS: service crashed: {e:?}");
        }
    });
}

/// Sends a signal and waits for an acknowledgement from the same object/interface
/// whose member name is the original `member` plus `_ack`.
/// If the ack is not received within `ACK_TIMEOUT`, the signal is resent.
/// The operation is attempted up to `MAX_RETRIES` times.
#[allow(dead_code)]
pub fn send_signal(object_path: &str, interface: &str, member: &str, payload: &str) -> Result<()> {
    let conn = Connection::new_session()?;

    // Listen for the expected ack
    let ack_member = format!("{member}_ack");
    let rule =
        format!("type='signal',interface='{interface}',member='{ack_member}',path='{object_path}'");
    conn.add_match_no_cb(&rule)?;

    // Send the signal up to `MAX_RETRIES` times
    let max_retries = constant::DBUS_MAX_RETRIES;
    let ack_timeout = Duration::from_millis(constant::DBUS_ACK_TIMEOUT);
    for attempt in 0..max_retries {
        // Send the signal
        let msg = Message::new_signal(object_path, interface, member)
            .map_err(anyhow::Error::msg)?
            .append1(payload);
        if conn.send(msg).is_err() {
            eprintln!(
                "DBUS: Failed to send signal (attempt {}/{})",
                attempt + 1,
                max_retries
            );
        }

        // Wait for the ack until the timeout expires
        let deadline = Instant::now() + ack_timeout;
        while Instant::now() < deadline {
            let remaining = deadline - Instant::now();
            if let Some(reply) = conn.channel().blocking_pop_message(remaining)? {
                // Confirm we received the correct ack from the intended object
                if reply.member().map(|m| m.to_string()) == Some(ack_member.clone())
                    && reply.path().map(|p| p.to_string()) == Some(object_path.to_string())
                {
                    return Ok(()); // Ack received
                }
            }
        }

        // If we didn't receive the ack, log an error
        eprintln!(
            "DBUS: Ack '{}' not received within {:?} (attempt {}/{}) – retrying…",
            ack_member,
            ack_timeout,
            attempt + 1,
            max_retries
        );
    }

    Err(anyhow!(
        "DBUS: Ack '{}' not received after {} attempts",
        ack_member,
        max_retries
    ))
}

/// Waits up to `timeout_ms` milliseconds for a signal, immediately emits
/// a `<member>_ack` signal back to the same object/interface, then returns
/// the payload of the received message.
#[allow(dead_code)]
pub fn receive_signal(
    object_path: &str,
    interface: &str,
    member: &str,
    timeout_ms: u64,
) -> Result<Message> {
    let conn = Connection::new_session()?;
    let rule =
        format!("type='signal',interface='{interface}',member='{member}',path='{object_path}'");
    println!("DBUS: Rule: {rule}");
    conn.add_match_no_cb(&rule)?;

    let end_time = Instant::now() + Duration::from_millis(timeout_ms);
    while Instant::now() < end_time {
        let time_left = end_time - Instant::now();
        if let Ok(msg) = receive_internal(&conn, object_path, interface, member, time_left) {
            return Ok(msg);
        }
    }

    Err(anyhow!(
        "Timed out after {timeout_ms} ms waiting for '{member}'"
    ))
}

/// Waits up to `duration` milliseconds for a signal, immediately emits
/// a `<member>_ack` signal back if received the right signal and returns
/// the payload of the received message.
/// If the signal doesn't match the expected object path or member, an error is returned.
fn receive_internal(
    conn: &Connection,
    object_path: &str,
    interface: &str,
    member: &str,
    duration: Duration,
) -> Result<Message> {
    let msg_opt = conn.channel().blocking_pop_message(duration)?;

    let msg = msg_opt.ok_or_else(|| anyhow!("DBUS: Did not receive any signal"))?;
    let r_object_path = msg
        .path()
        .map(|p| p.to_string())
        .ok_or_else(|| anyhow!("DBUS: Received signal with no path: {msg:?}"))?;
    let r_member = msg
        .member()
        .map(|m| m.to_string())
        .ok_or_else(|| anyhow!("DBUS: Received signal with no member: {msg:?}"))?;
    if r_object_path != object_path {
        return Err(anyhow!(
            "DBUS: Received signal from wrong object: {r_object_path} (expected {object_path})"
        ));
    }
    if r_member != member {
        return Err(anyhow!(
            "DBUS: Received signal with wrong member: {r_member} (expected {member})"
        ));
    }

    // Send acknowledgement
    println!("DBUS: Sending ack signal '{member}_ack' to {object_path}, {interface}");
    let mut ack_msg = Message::new_signal(object_path, interface, format!("{member}_ack"))
        .map_err(anyhow::Error::msg)?;
    ack_msg = ack_msg.append1("");
    if conn.send(ack_msg).is_err() {
        // Failed to send ack signal doesn't matter, just log an error
        eprintln!("DBUS: Failed to send ack signal '{member}_ack'");
    }

    Ok(msg)
}

pub fn listen_for_signal(
    object_path: &str,
    interface: &str,
    member: &str,
    stop: Arc<AtomicBool>,
    cb: ListenCallback,
) {
    let object_path = object_path.to_string();
    let interface = interface.to_string();
    let member = member.to_string();
    task::spawn_blocking(move || {
        let conn = Connection::new_session().expect("DBUS: failed to create connection");
        let rule =
            format!("type='signal',interface='{interface}',member='{member}',path='{object_path}'");
        conn.add_match_no_cb(&rule)
            .expect("DBUS: failed to add match");

        println!("DBUS: Listening for '{member}' signal in a background thread");
        while !stop.load(Ordering::Relaxed) {
            if let Ok(msg) = receive_internal(
                &conn,
                &object_path,
                &interface,
                &member,
                Duration::from_millis(constant::DBUS_LISTEN_WAKE_UP_INTERVAL),
            ) {
                cb(msg);
            }
        }
    });
}

/// Sends a blocking D‑Bus method call and returns the reply `Message`.
///
/// * `destination` – well‑known or unique bus name of the service to call.
/// * `object_path` – object path exposed by that service.
/// * `interface` – interface that defines the method.
/// * `member` – the method (member) name.
/// * `payload` – single string argument to pass to the method.
///
/// The function waits up to `constant::DBUS_ACK_TIMEOUT` milliseconds for the
/// reply and returns the raw `Message` so the caller can extract whatever
/// return values it needs.
pub fn call_method<T: Send + Sync + Append>(
    destination: &str,
    object_path: &str,
    interface: &str,
    member: &str,
    payload: Option<T>,
    timeout_ms: u64,
) -> Result<Message> {
    // Establish a connection on the session bus
    let conn = Connection::new_session()?;

    // Build the method‑call message and attach the payload
    let mut msg = Message::new_method_call(destination, object_path, interface, member)
        .map_err(anyhow::Error::msg)?;
    if let Some(payload) = payload {
        msg = msg.append1(payload);
    }

    // Send the message and block until we get the reply (or timeout)
    let reply = conn.send_with_reply_and_block(msg, Duration::from_millis(timeout_ms))?;

    Ok(reply)
}

/// Checks if the internet is available by calling the `connectivity` method
/// on the `sys-monitord` service.
pub fn internet_availability() -> bool {
    match call_method(
        constant::DBUS_SYSMONITORD_DESTINATION,
        constant::DBUS_SYSMONITORD_OBJECT,
        constant::DBUS_SYSMONITORD_INTERFACE,
        constant::DBUS_CONNECTIVITY_METHOD,
        Some(true), // payload: true means forcing the check instead of using cached value from monitord
        constant::DBUS_INTERNET_CHECK_TIMEOUT,
    ) {
        Ok(response) => response.read1::<bool>().unwrap_or(false),
        Err(_) => false,
    }
}

#[allow(dead_code)]
pub fn on_internet_available<F: FnOnce() + Send + Sync + 'static>(cb: F, stop: Arc<AtomicBool>) {
    tokio::task::spawn_blocking(move || {
        while !stop.load(Ordering::Relaxed) {
            let internet = internet_availability();
            if internet {
                cb();
                break;
            }
        }
    });
}

pub fn get_relayer_info() -> Result<String> {
    let start_time = Instant::now();

    match call_method(
        constant::DBUS_CONNECTD_DESTINATION,
        constant::DBUS_CONNECTD_OBJECT,
        constant::DBUS_CONNECTD_INTERFACE,
        constant::DBUS_RELAYER_TOPIC_ID_METHOD,
        Option::<bool>::None, // payload
        constant::DBUS_RELAYER_CHECK_TIMEOUT,
    ) {
        Ok(response) => {
            let topic_id = response.read1::<String>()?;
            println!(
                "DBUS: Relayer info received in {:?} ms",
                start_time.elapsed().as_millis()
            );
            Ok(topic_id)
        }
        Err(e) => {
            eprintln!("DBUS: Error getting relayer info: {e}");
            Err(e)
        }
    }
}

pub fn check_dbus_connection(destination: &str, object_path: &str) -> Result<()> {
    let conn = Connection::new_session()?;
    let proxy = conn.with_proxy(destination, object_path, Duration::from_millis(1000));

    let _: () = proxy.method_call("org.freedesktop.DBus.Peer", "Ping", ())?;

    println!("DBUS: Connection check successful via Peer.Ping");
    Ok(())
}
