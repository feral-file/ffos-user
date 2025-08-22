use crate::constant;
use anyhow::Result;
use base64::{Engine as _, engine::general_purpose};
use serde_json::json;

// Helper function to collect log files
pub async fn collect_log_files() -> Result<Vec<(String, Vec<u8>)>, std::io::Error> {
    use tokio::fs;

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

            match fs::read(&path).await {
                Ok(contents) => {
                    println!("BLE: Collected log file: {file_name}");
                    log_files.push((file_name, contents));
                }
                Err(e) => {
                    eprintln!("BLE: Failed to read log file {file_name}: {e}");
                }
            }
        }
    }

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
