// Bakes the build id (TRAY_BUILD, the commit in CI; "dev" for local builds) into the binary. The proxy reads the
// "TRAYBUILD:<id>:END" mark from the published .exe to tell the app when a newer build is there.
fn main() {
    let id = std::env::var("TRAY_BUILD").ok().filter(|s| !s.is_empty() && s.chars().all(|c| c.is_ascii_alphanumeric() || "._-".contains(c)) && s.len() <= 64).unwrap_or_else(|| "dev".into());
    println!("cargo:rustc-env=TRAY_BUILD_ID={id}");
    println!("cargo:rerun-if-env-changed=TRAY_BUILD");
}
