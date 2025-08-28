use std::env;

fn main() {
    let profile = env::var("PROFILE").unwrap_or("unknown".to_string());
    println!("cargo:rustc-env=BUILD_PROFILE={profile}");
}
