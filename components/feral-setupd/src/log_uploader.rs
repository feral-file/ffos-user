use crate::constant;
use serde::{Deserialize, Serialize};
use std::io::{BufWriter, Write};
use std::path::{Path, PathBuf};
use tokio::fs;
use zip::ZipWriter;
use zip::write::SimpleFileOptions;

/// Size of chunks when reading files for streaming compression (64KB).
const READ_CHUNK_SIZE: usize = 64 * 1024;

/// Creates a zip archive containing all files from the logs directory recursively.
/// Uses streaming I/O to avoid loading large files entirely into memory.
/// Writes to a temporary file with a unique timestamp-based name to prevent
/// race conditions when multiple uploads run concurrently.
/// Returns a tuple of (zip_path, temp_dir_path) for cleanup in submit_logs.
pub async fn create_logs_zip() -> Result<(PathBuf, PathBuf), std::io::Error> {
    use std::time::{SystemTime, UNIX_EPOCH};

    let logs_dir = Path::new(constant::LOG_FILEDIR);

    // Generate a unique timestamp to avoid race conditions
    // when multiple uploads run concurrently (e.g., BLE + D-Bus triggered).
    let timestamp = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);

    let temp_dir = PathBuf::from(format!("/tmp/logs_upload_{timestamp}"));
    let temp_zip_path = PathBuf::from(format!("/tmp/logs_upload_{timestamp}.zip"));

    // Create temp directory
    fs::create_dir(&temp_dir).await?;

    // Copy the entire log directory to temp directory
    copy_dir_recursive(logs_dir, &temp_dir).await?;

    // Copy updaterd log files /var/log/updaterd.log /var/log/auto-updaterd.log to temp dir
    let updaterd_log = Path::new("/var/log/updaterd.log");
    let auto_updaterd_log = Path::new("/var/log/auto-updaterd.log");
    if updaterd_log.exists() {
        fs::copy(updaterd_log, temp_dir.join("updaterd.log")).await?;
    }
    if auto_updaterd_log.exists() {
        fs::copy(auto_updaterd_log, temp_dir.join("auto-updaterd.log")).await?;
    }

    // Collect all file paths from temp directory
    let files_to_zip = collect_files_to_zip(&temp_dir).await?;

    if files_to_zip.is_empty() {
        // Clean up temp dir before returning error
        let _ = fs::remove_dir_all(&temp_dir).await;
        return Err(std::io::Error::new(
            std::io::ErrorKind::NotFound,
            "No log files found",
        ));
    }

    // Spawn blocking task for the sync zip writing with streaming file reads
    let temp_dir_owned = temp_dir.clone();
    let temp_zip_path_clone = temp_zip_path.clone();
    let file_count = tokio::task::spawn_blocking(move || {
        write_zip_streaming(&temp_zip_path_clone, &temp_dir_owned, &files_to_zip)
    })
    .await
    .map_err(std::io::Error::other)??;

    // Get the file size for logging
    let metadata = fs::metadata(&temp_zip_path).await?;
    println!(
        "LOG_UPLOADER: Created zip with {file_count} files, size: {} bytes",
        metadata.len()
    );

    Ok((temp_zip_path, temp_dir))
}

/// Recursively copies the contents of a directory to a destination directory.
/// Uses `src/.` pattern to copy only the contents, not the directory itself.
async fn copy_dir_recursive(src: &Path, dst: &Path) -> Result<(), std::io::Error> {
    // Append "/." to source path to copy contents only, not the directory itself
    let src_contents = format!("{}{}.", src.display(), std::path::MAIN_SEPARATOR);
    
    let status = tokio::process::Command::new("cp")
        .arg("-r")
        .arg(&src_contents)
        .arg(dst)
        .status()
        .await?;

    if status.success() {
        Ok(())
    } else {
        Err(std::io::Error::other(format!(
            "cp command failed with status: {status}"
        )))
    }
}

/// Recursively collects all file paths under the given directory.
async fn collect_files_to_zip(logs_dir: &Path) -> Result<Vec<PathBuf>, std::io::Error> {
    let mut files = Vec::new();
    let mut dirs_to_visit = vec![logs_dir.to_path_buf()];

    while let Some(current_dir) = dirs_to_visit.pop() {
        let mut dir = match fs::read_dir(&current_dir).await {
            Ok(d) => d,
            Err(e) => {
                eprintln!(
                    "LOG_UPLOADER: Failed to read directory {}: {e}",
                    current_dir.display()
                );
                continue;
            }
        };

        while let Some(entry) = dir.next_entry().await? {
            let path = entry.path();
            let metadata = match fs::metadata(&path).await {
                Ok(m) => m,
                Err(e) => {
                    eprintln!(
                        "LOG_UPLOADER: Failed to get metadata for {}: {e}",
                        path.display()
                    );
                    continue;
                }
            };

            if metadata.is_dir() {
                dirs_to_visit.push(path);
            } else if metadata.is_file() {
                files.push(path);
            }
        }
    }

    Ok(files)
}

/// Writes files to a zip archive using streaming reads to avoid memory bloat.
/// Runs in a blocking context since the zip crate is synchronous.
fn write_zip_streaming(
    zip_path: &Path,
    logs_dir: &Path,
    files: &[PathBuf],
) -> Result<usize, std::io::Error> {
    use std::fs::File;
    use std::io::{BufReader, Read};

    let file = File::create(zip_path)?;
    let buffered = BufWriter::new(file);
    let mut zip = ZipWriter::new(buffered);
    let options = SimpleFileOptions::default().compression_method(zip::CompressionMethod::Deflated);

    let mut file_count = 0;
    let mut chunk_buffer = vec![0u8; READ_CHUNK_SIZE];

    for path in files {
        // Calculate relative path from logs_dir for the zip entry name
        let relative_path = match path.strip_prefix(logs_dir) {
            Ok(rel) => rel.to_string_lossy().to_string(),
            Err(_) => {
                eprintln!(
                    "LOG_UPLOADER: Failed to get relative path for {}",
                    path.display()
                );
                continue;
            }
        };

        // Open file for streaming read
        let source_file = match File::open(path) {
            Ok(f) => f,
            Err(e) => {
                eprintln!("LOG_UPLOADER: Failed to open file {relative_path}: {e}");
                continue;
            }
        };
        let mut reader = BufReader::new(source_file);

        // Start the zip entry
        if let Err(e) = zip.start_file(&relative_path, options) {
            eprintln!("LOG_UPLOADER: Failed to start zip entry for {relative_path}: {e}");
            continue;
        }

        // Stream the file contents in chunks
        loop {
            let bytes_read = match reader.read(&mut chunk_buffer) {
                Ok(0) => break, // EOF
                Ok(n) => n,
                Err(e) => {
                    eprintln!("LOG_UPLOADER: Error reading {relative_path}: {e}");
                    break;
                }
            };

            if let Err(e) = zip.write_all(&chunk_buffer[..bytes_read]) {
                eprintln!("LOG_UPLOADER: Failed to write chunk for {relative_path}: {e}");
                break;
            }
        }

        file_count += 1;
    }

    zip.finish().map_err(std::io::Error::other)?;
    Ok(file_count)
}

/// Request body for the v2 log-submissions API.
#[derive(Serialize)]
struct LogSubmissionRequest {
    device_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    title: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    source: Option<String>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    tags: Vec<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    branch: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    version: Option<String>,
}

/// Pre-signed upload result from S3.
#[derive(Deserialize, Debug)]
struct PresignResult {
    url: String,
}

/// Response from the v2 log-submissions API.
#[derive(Deserialize, Debug)]
struct LogSubmissionResponse {
    #[allow(dead_code)]
    object_key: String,
    upload: PresignResult,
    #[allow(dead_code)]
    expires_in_seconds: i64,
}

/// Requests a pre-signed S3 upload URL from the v2 API.
async fn get_presigned_url(
    device_id: &str,
    api_key: &str,
    source: &str,
    branch: &str,
    version: &str,
) -> Result<String, u8> {
    println!("LOG_UPLOADER: Requesting pre-signed URL from v2 API");

    let request_body = LogSubmissionRequest {
        device_id: device_id.to_string(),
        title: None,
        source: Some(source.to_string()),
        tags: vec!["device-logs".to_string()],
        branch: Some(branch.to_string()),
        version: Some(version.to_string()),
    };

    let client = reqwest::Client::new();

    let response = client
        .post(constant::LOG_UPLOAD_API)
        .header("x-api-key", api_key)
        .header("Content-Type", "application/json")
        .json(&request_body)
        .send()
        .await;

    match response {
        Ok(resp) => {
            let status = resp.status();
            if status.is_success() {
                match resp.json::<LogSubmissionResponse>().await {
                    Ok(data) => {
                        println!(
                            "LOG_UPLOADER: Got pre-signed URL, object_key: {}",
                            data.object_key
                        );
                        Ok(data.upload.url)
                    }
                    Err(e) => {
                        eprintln!("LOG_UPLOADER: Failed to parse v2 API response: {e}");
                        Err(constant::BLE_ERR_CODE_NETWORK_ERROR)
                    }
                }
            } else {
                let body = resp.text().await.unwrap_or_default();
                eprintln!("LOG_UPLOADER: v2 API returned error: HTTP {status}, body: {body}");
                Err(constant::BLE_ERR_CODE_NETWORK_ERROR)
            }
        }
        Err(e) => {
            eprintln!("LOG_UPLOADER: Network error requesting pre-signed URL: {e}");
            Err(constant::BLE_ERR_CODE_NETWORK_ERROR)
        }
    }
}

/// Uploads the zip file to S3 using streaming to avoid loading into memory.
async fn upload_zip_to_s3(upload_url: &str, zip_path: &Path) -> Result<(), u8> {
    use tokio_util::io::ReaderStream;

    // Get file size for Content-Length header
    let metadata = match fs::metadata(zip_path).await {
        Ok(m) => m,
        Err(e) => {
            eprintln!("LOG_UPLOADER: Failed to get zip file metadata: {e}");
            return Err(constant::BLE_ERR_CODE_UNKNOWN_ERROR);
        }
    };
    let file_size = metadata.len();

    println!("LOG_UPLOADER: Uploading {file_size} bytes to S3 (streaming)");

    // Open file for async streaming read
    let file = match fs::File::open(zip_path).await {
        Ok(f) => f,
        Err(e) => {
            eprintln!("LOG_UPLOADER: Failed to open zip file for upload: {e}");
            return Err(constant::BLE_ERR_CODE_UNKNOWN_ERROR);
        }
    };

    // Create a streaming body from the file
    let stream = ReaderStream::new(file);
    let body = reqwest::Body::wrap_stream(stream);

    let client = reqwest::Client::new();
    let start_time = std::time::Instant::now();

    let response = client
        .put(upload_url)
        .header("Content-Type", "application/zip")
        .header("Content-Length", file_size)
        .body(body)
        .send()
        .await;

    let elapsed = start_time.elapsed();
    println!("LOG_UPLOADER: S3 upload completed in {elapsed:?}");

    match response {
        Ok(resp) => {
            let status = resp.status();
            if status.is_success() {
                println!("LOG_UPLOADER: S3 upload successful");
                Ok(())
            } else {
                let body = resp.text().await.unwrap_or_default();
                eprintln!("LOG_UPLOADER: S3 upload failed: HTTP {status}, body: {body}");
                Err(constant::BLE_ERR_CODE_NETWORK_ERROR)
            }
        }
        Err(e) => {
            eprintln!("LOG_UPLOADER: Network error during S3 upload: {e}");
            if e.is_timeout() {
                eprintln!("LOG_UPLOADER: Error type: Request timeout");
            } else if e.is_connect() {
                eprintln!("LOG_UPLOADER: Error type: Connection error");
            }
            Err(constant::BLE_ERR_CODE_NETWORK_ERROR)
        }
    }
}

/// Main entry point: zips the logs folder and uploads to S3 via the v2 API.
///
/// Flow:
/// 1. Copy logs to temp directory and zip (streaming to temp file)
/// 2. Request a pre-signed S3 upload URL from the v2 API
/// 3. Stream upload the zip to S3
/// 4. Clean up the temp file and temp directory
pub async fn submit_logs(
    device_id: &str,
    api_key: &str,
    source: &str,
    branch: &str,
    version: &str,
) -> Result<(), u8> {
    println!("LOG_UPLOADER: Starting log submission (v2 API, streaming)");

    // Step 1: Create zip of logs folder (streaming to temp file)
    let (zip_path, temp_dir) = match create_logs_zip().await {
        Ok(paths) => paths,
        Err(e) => {
            eprintln!("LOG_UPLOADER: Failed to create logs zip: {e}");
            return Err(constant::BLE_ERR_CODE_UNKNOWN_ERROR);
        }
    };

    // Ensure temp file and directory cleanup on all exit paths
    let result = async {
        // Step 2: Get pre-signed URL from v2 API
        let upload_url = get_presigned_url(device_id, api_key, source, branch, version).await?;

        // Step 3: Stream upload zip to S3
        upload_zip_to_s3(&upload_url, &zip_path).await?;

        Ok(())
    }
    .await;

    // Step 4: Clean up temp zip file
    if let Err(e) = fs::remove_file(&zip_path).await {
        eprintln!(
            "LOG_UPLOADER: Failed to clean up temp zip file {}: {e}",
            zip_path.display()
        );
    }

    // Step 5: Clean up temp directory
    if let Err(e) = fs::remove_dir_all(&temp_dir).await {
        eprintln!(
            "LOG_UPLOADER: Failed to clean up temp directory {}: {e}",
            temp_dir.display()
        );
    }

    if result.is_ok() {
        println!("LOG_UPLOADER: Log submission completed successfully");
    }

    result
}
