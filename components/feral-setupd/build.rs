use std::path::Path;

fn get_build_profile_name() -> Result<String, std::env::VarError> {
    // The profile name is always the 4rd last part of the path (with 1 based indexing).
    // e.g. /code/core/target/cli/build/my-build-info-9f91ba6f99d7a061/out
    let out_dir = std::env::var("OUT_DIR")?;

    let path = Path::new(&out_dir);
    let components: Vec<_> = path.components().collect();

    if components.len() >= 4
        && let Some(profile_component) = components.get(components.len() - 4)
        && let Some(profile_name) = profile_component.as_os_str().to_str()
    {
        return Ok(profile_name.to_string());
    }

    // 如果無法解析，返回 "unknown"
    Ok("unknown".to_string())
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let build_profile_name = get_build_profile_name()?;
    println!("cargo:rustc-env=BUILD_PROFILE={build_profile_name}");
    Ok(())
}
