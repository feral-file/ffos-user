use crate::constant;
use anyhow::Result;
use base64::{Engine as _, engine::general_purpose};
use parking_lot::Mutex;
use serde_json::json;
use std::{path::PathBuf, sync::OnceLock};

const MAX_FILE_SIZE_BYTES: usize = 1_048_576; // 1MB in bytes

fn logs_dir_override() -> &'static Mutex<Option<PathBuf>> {
    static OVERRIDE: OnceLock<Mutex<Option<PathBuf>>> = OnceLock::new();
    OVERRIDE.get_or_init(|| Mutex::new(None))
}

fn api_url_override() -> &'static Mutex<Option<String>> {
    static OVERRIDE: OnceLock<Mutex<Option<String>>> = OnceLock::new();
    OVERRIDE.get_or_init(|| Mutex::new(None))
}

fn logs_dir() -> PathBuf {
    logs_dir_override()
        .lock()
        .clone()
        .unwrap_or_else(|| PathBuf::from(constant::LOG_FILEDIR))
}

fn log_upload_api() -> String {
    api_url_override()
        .lock()
        .clone()
        .unwrap_or_else(|| constant::LOG_UPLOAD_API.to_string())
}

#[cfg(test)]
pub(crate) fn set_logs_dir_for_tests(path: Option<PathBuf>) {
    let mut guard = logs_dir_override().lock();
    *guard = path;
}

#[cfg(test)]
pub(crate) fn set_api_url_for_tests(url: Option<String>) {
    let mut guard = api_url_override().lock();
    *guard = url;
}

// Helper function to collect log files
pub async fn collect_log_files() -> Result<Vec<(String, Vec<u8>)>, std::io::Error> {
    use tokio::fs;
    use tokio::io::{AsyncReadExt, AsyncSeekExt};

    let logs_dir = logs_dir();
    let mut log_files = Vec::new();

    let mut dir = fs::read_dir(&logs_dir).await?;
    while let Some(entry) = dir.next_entry().await? {
        let path = entry.path();
        if path.extension().and_then(|s| s.to_str()) == Some("log") {
            let file_name = path
                .file_name()
                .and_then(|s| s.to_str())
                .unwrap_or("unknown.log")
                .to_string();

            let mut file = match fs::File::open(&path).await {
                Ok(f) => f,
                Err(e) => {
                    eprintln!("BLE: Failed to open log file {file_name}: {e}");
                    continue;
                }
            };

            // Get file size
            let file_size = match file.metadata().await {
                Ok(metadata) => metadata.len() as usize,
                Err(e) => {
                    eprintln!("BLE: Failed to get metadata for {file_name}: {e}");
                    continue;
                }
            };

            let contents = if file_size > MAX_FILE_SIZE_BYTES {
                println!(
                    "BLE: Log file {file_name} exceeds 1MB ({file_size} bytes), reading last 1MB"
                );

                // Seek to the position 1MB from the end
                let seek_pos = file_size - MAX_FILE_SIZE_BYTES;
                if let Err(e) = file.seek(std::io::SeekFrom::Start(seek_pos as u64)).await {
                    eprintln!("BLE: Failed to seek in file {file_name}: {e}");
                    continue;
                }

                // Read the last 1MB
                let mut buffer = Vec::with_capacity(MAX_FILE_SIZE_BYTES);
                match file.read_to_end(&mut buffer).await {
                    Ok(_) => {
                        // Try to find the first complete line
                        if let Some(first_newline) = buffer.iter().position(|&b| b == b'\n') {
                            buffer = buffer[first_newline + 1..].to_vec();
                        }

                        // Add truncation notice
                        let truncation_notice = format!(
                            "[TRUNCATED: Original file size {file_size} bytes, showing last {} bytes]\n",
                            buffer.len()
                        );
                        let mut final_contents = truncation_notice.into_bytes();
                        final_contents.extend(buffer);

                        println!(
                            "BLE: Truncated log file: {file_name} (final size: {} bytes)",
                            final_contents.len()
                        );

                        final_contents
                    }
                    Err(e) => {
                        eprintln!("BLE: Failed to read from file {file_name}: {e}");
                        continue;
                    }
                }
            } else {
                // File is small enough, read the whole thing
                let mut contents = Vec::new();
                match file.read_to_end(&mut contents).await {
                    Ok(_) => {
                        println!(
                            "BLE: Collected log file: {file_name} ({} bytes)",
                            contents.len()
                        );
                        contents
                    }
                    Err(e) => {
                        eprintln!("BLE: Failed to read file {file_name}: {e}");
                        continue;
                    }
                }
            };

            log_files.push((file_name, contents));
        }
    }

    println!("BLE: Collected {} log files", log_files.len());
    Ok(log_files)
}

pub async fn submit_logs_to_api(
    user_id: &str,
    api_key: &str,
    body: serde_json::Value,
) -> Result<(), u8> {
    use reqwest;

    println!("API: Starting log submission to remote API");

    // Log request body size
    if let Ok(serialized_body) = serde_json::to_string(&body) {
        println!("API: Request body size: {} bytes", serialized_body.len());
    }

    let client = reqwest::Client::new();

    println!("API: Building POST request");

    let request_builder = client
        .post(log_upload_api())
        .header("x-api-key", api_key)
        .header("x-device-id", user_id)
        .header("Content-Type", "application/json")
        .json(&body);

    println!("API: Sending HTTP request...");
    let start_time = std::time::Instant::now();

    let response = request_builder.send().await;
    let elapsed = start_time.elapsed();

    println!("API: HTTP request completed in {elapsed:?}");

    match response {
        Ok(resp) => {
            let status = resp.status();
            println!("API: Received HTTP response with status: {status}");

            if status.is_success() {
                println!("API: SUCCESS - Logs submitted successfully");
                println!("API: Response headers: {:?}", resp.headers());
                Ok(())
            } else {
                println!("API: ERROR - HTTP request failed with status: {status}");

                // Try to get response body for debugging
                match resp.text().await {
                    Ok(response_text) => {
                        eprintln!("API: Error response body: {response_text}");
                        println!("API: Failed to submit logs: HTTP {status}, {response_text}");
                    }
                    Err(body_err) => {
                        eprintln!("API: Failed to read error response body: {body_err}");
                        eprintln!("API: Failed to submit logs: HTTP {status}");
                    }
                }

                println!("API: Returning network error code");
                Err(constant::BLE_ERR_CODE_NETWORK_ERROR)
            }
        }
        Err(e) => {
            eprintln!("API: ERROR - Network error occurred: {e}");

            // More detailed error information
            if e.is_timeout() {
                eprintln!("API: Error type: Request timeout");
            } else if e.is_connect() {
                eprintln!("API: Error type: Connection error");
            } else if e.is_request() {
                eprintln!("API: Error type: Request building error");
            } else {
                eprintln!("API: Error type: Other network error");
            }

            println!("API: Returning network error code");
            Err(constant::BLE_ERR_CODE_NETWORK_ERROR)
        }
    }
}

pub fn create_log_submission_body(
    title: &str,
    message: &str,
    tags: Vec<String>,
    log_files: Vec<(String, Vec<u8>)>,
) -> serde_json::Value {
    let attachments: Vec<serde_json::Value> = log_files
        .into_iter()
        .map(|(file_name, data)| {
            json!({
                "title": file_name,
                "data": general_purpose::STANDARD.encode(data)
            })
        })
        .collect();

    json!({
        "attachments": attachments,
        "title": title,
        "message": message,
        "tags": tags
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use serial_test::serial;
    use serde_json::json;
    use tempfile::tempdir;
    use tokio::fs;
    use wiremock::matchers::{header, method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    #[tokio::test]
    #[serial]
    async fn collect_log_files_reads_small_and_large_logs() {
        let dir = tempdir().unwrap();
        let dir_path = dir.path();

        fs::write(dir_path.join("ignore.txt"), b"should be ignored")
            .await
            .unwrap();
        fs::write(dir_path.join("small.log"), b"line1\nline2\n")
            .await
            .unwrap();

        let mut large = vec![b'A'; MAX_FILE_SIZE_BYTES + 128];
        large[32] = b'\n';
        fs::write(dir_path.join("large.log"), &large)
            .await
            .unwrap();

        set_logs_dir_for_tests(Some(dir_path.to_path_buf()));

        let logs = collect_log_files().await.unwrap();
        set_logs_dir_for_tests(None);

        assert_eq!(logs.len(), 2);
        let mut map = std::collections::HashMap::new();
        for (name, contents) in logs {
            map.insert(name, contents);
        }

        assert_eq!(
            map.get("small.log").unwrap(),
            &b"line1\nline2\n".to_vec()
        );

        let large_contents = String::from_utf8(map.get("large.log").unwrap().clone()).unwrap();
        assert!(large_contents.starts_with("[TRUNCATED"));
    }

    #[test]
    fn create_log_submission_body_encodes_files() {
        let body = create_log_submission_body(
            "Test Title",
            "Test Message",
            vec!["tag1".into(), "tag2".into()],
            vec![("file.log".into(), b"data".to_vec())],
        );

        assert_eq!(body["title"], "Test Title");
        assert_eq!(body["message"], "Test Message");
        assert_eq!(body["tags"][1], "tag2");

        let attachment = &body["attachments"][0];
        assert_eq!(attachment["title"], "file.log");
        assert_eq!(
            attachment["data"],
            json!(general_purpose::STANDARD.encode(b"data"))
        );
    }

    #[tokio::test]
    #[serial]
    async fn submit_logs_to_api_handles_success_and_error() {
        let server = MockServer::start().await;
        let endpoint = format!("{}/submit", server.uri());
        set_api_url_for_tests(Some(endpoint.clone()));

        Mock::given(method("POST"))
            .and(path("/submit"))
            .and(header("x-api-key", "apikey"))
            .respond_with(ResponseTemplate::new(200))
            .mount(&server)
            .await;

        let body = json!({ "key": "value" });
        submit_logs_to_api("user", "apikey", body.clone())
            .await
            .unwrap();

        server.reset().await;
        Mock::given(method("POST"))
            .and(path("/submit"))
            .respond_with(ResponseTemplate::new(500).set_body_string("fail"))
            .mount(&server)
            .await;

        let err = submit_logs_to_api("user", "apikey", body).await.unwrap_err();
        assert_eq!(err, constant::BLE_ERR_CODE_NETWORK_ERROR);

        // Trigger a connectivity error to cover the network error branch.
        set_api_url_for_tests(Some("http://127.0.0.1:9/submit".into()));
        let network_err = submit_logs_to_api("user", "apikey", json!({"key": "value"}))
            .await
            .unwrap_err();
        assert_eq!(network_err, constant::BLE_ERR_CODE_NETWORK_ERROR);

        set_api_url_for_tests(None);
    }
}
