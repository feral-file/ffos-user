use crate::constant;
use anyhow::Result;
use base64::{Engine as _, engine::general_purpose};
use serde_json::json;

const MAX_FILE_SIZE_BYTES: usize = 1_048_576; // 1MB in bytes

// Helper function to collect log files
pub async fn collect_log_files() -> Result<Vec<(String, Vec<u8>)>, std::io::Error> {
    use tokio::fs;
    use tokio::io::{AsyncReadExt, AsyncSeekExt};

    let logs_dir = constant::LOG_FILEDIR;
    let mut log_files = Vec::new();

    let mut dir = fs::read_dir(logs_dir).await?;
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
        .post(constant::LOG_UPLOAD_API)
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

    let result = json!({
        "attachments": attachments,
        "title": title,
        "message": message,
        "tags": tags
    });

    result
}
