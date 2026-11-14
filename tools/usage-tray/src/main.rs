// UsageTray: a tray icon for the CLIProxyAPI /usage dashboard.
//
// A left click opens a small popup (the dashboard's `?mini` view in a WebView2) above the tray; a right click
// shows the menu. The icon itself is a ring of the weekly capacity left, refreshed every 2 minutes from
// /usage/api/data (the proxy's cached snapshot: it never makes the proxy call Anthropic).
//
// The server address comes from, in order: %APPDATA%\UsageTray\url.txt, the `USAGE_TRAY_URL=` line the proxy
// appends to the .exe when it is downloaded, then http://127.0.0.1:8317.
//
// The first run copies the .exe to %LOCALAPPDATA%\UsageTray, turns on "start with Windows" and starts from there.
// Running any copy again replaces the one that is running (the newest start wins), so there is only ever one.
//
// Updates: the build id is baked in (build.rs, "TRAYBUILD:<id>:END"); the proxy reads it from the .exe it publishes
// and reports it as `tray_build` in /usage/api/data, which the app already polls for the icon. When it differs from
// this build, the app downloads it, swaps its own file (a running .exe can be renamed, not overwritten) and restarts.
#![windows_subsystem = "windows"]

use std::{
    f64::consts::TAU,
    io::Read,
    path::{Path, PathBuf},
    process::Command,
    sync::mpsc,
    thread,
    time::{Duration, Instant},
};

use tao::{
    dpi::LogicalSize,
    event::{Event, StartCause, WindowEvent},
    event_loop::{ControlFlow, EventLoopBuilder, EventLoopProxy},
    platform::windows::{WindowBuilderExtWindows, WindowExtWindows},
    window::{Theme, Window, WindowBuilder},
};
use tray_icon::{
    menu::{CheckMenuItem, Menu, MenuEvent, MenuItem, PredefinedMenuItem},
    Icon, MouseButton, MouseButtonState, TrayIcon, TrayIconBuilder, TrayIconEvent,
};
use windows_sys::Win32::{
    Foundation::{CloseHandle, GetLastError, ERROR_ALREADY_EXISTS, POINT},
    Graphics::{
        Dwm::{DwmSetWindowAttribute, DWMWA_WINDOW_CORNER_PREFERENCE, DWMWCP_ROUND},
        Gdi::{GetMonitorInfoW, MonitorFromPoint, MONITORINFO, MONITOR_DEFAULTTONEAREST},
    },
    System::Threading::{CreateEventW, CreateMutexW, SetEvent, WaitForSingleObject, INFINITE},
    UI::WindowsAndMessaging::{GetForegroundWindow, MessageBoxW, MB_ICONWARNING, MB_OK},
};
use wry::{WebView, WebViewBuilder};

const APP: &str = "UsageTray";
const DEFAULT_URL: &str = "http://127.0.0.1:8317";
const MARK: &str = "USAGE_TRAY_URL=";
const WIDTH: f64 = 340.0;
const POLL: Duration = Duration::from_secs(120);
// The popup's WebView is dropped after this long hidden, so the app idles at a few MB.
const DROP_AFTER: Duration = Duration::from_secs(300);

// Kept in the binary as text so the proxy can read it from the file (see build.rs).
static BUILD_MARK: &str = concat!("TRAYBUILD:", env!("TRAY_BUILD_ID"), ":END");

fn build_id() -> &'static str {
    let m = std::hint::black_box(BUILD_MARK);
    &m["TRAYBUILD:".len()..m.len() - ":END".len()]
}

/// The build id inside an .exe image, if marked.
fn parse_build(data: &[u8]) -> Option<String> {
    let pat = b"TRAYBUILD:";
    let mut off = 0;
    while let Some(i) = data[off..].windows(pat.len()).position(|w| w == pat) {
        let start = off + i + pat.len();
        let rest = &data[start..data.len().min(start + 70)];
        if let Some(end) = rest.windows(4).position(|w| w == b":END") {
            let id = &rest[..end];
            if !id.is_empty() && id.iter().all(|c| c.is_ascii_alphanumeric() || b"._-".contains(c)) {
                return Some(String::from_utf8_lossy(id).into_owned());
            }
        }
        off = start;
    }
    None
}

fn url_encode(s: &str) -> String {
    s.bytes()
        .map(|b| match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'.' | b'_' | b'~' => (b as char).to_string(),
            _ => format!("%{b:02X}"),
        })
        .collect()
}

/// Downloads and installs the proxy's build `want`, then starts it. Ok(()) means: quit now.
fn update_to(base: &str, want: &str) -> Result<(), String> {
    let resp = ureq::get(&format!("{base}/usage/download/tray?base={}", url_encode(base)))
        .timeout(Duration::from_secs(120))
        .call()
        .map_err(|e| e.to_string())?;
    let mut data = Vec::new();
    resp.into_reader().take(64 << 20).read_to_end(&mut data).map_err(|e| e.to_string())?;
    // Only a whole Windows image carrying the announced build is installed.
    if data.len() < 100_000 || !data.starts_with(b"MZ") || parse_build(&data).as_deref() != Some(want) {
        return Err("download is not the announced build".into());
    }
    let exe = std::env::current_exe().map_err(|e| e.to_string())?;
    let dir = exe.parent().ok_or("no folder")?.to_path_buf();
    let (new, old) = (dir.join(format!("{APP}.new.exe")), dir.join(format!("{APP}.old.exe")));
    std::fs::write(&new, &data).map_err(|e| e.to_string())?;
    let _ = std::fs::remove_file(&old);
    std::fs::rename(&exe, &old).map_err(|e| e.to_string())?;
    if let Err(e) = std::fs::rename(&new, &exe) {
        let _ = std::fs::rename(&old, &exe);
        return Err(e.to_string());
    }
    Command::new(&exe).spawn().map_err(|e| e.to_string())?;
    Ok(())
}

/// Files a previous update or install left behind.
fn cleanup_leftovers() {
    if let Some(dir) = std::env::current_exe().ok().and_then(|e| e.parent().map(Path::to_path_buf)) {
        for f in [format!("{APP}.old.exe"), format!("{APP}.new.exe")] {
            let _ = std::fs::remove_file(dir.join(f));
        }
    }
}

fn signal_quit() {
    unsafe {
        let name = wide(&format!("Local\\{APP}.Quit"));
        let h = CreateEventW(std::ptr::null(), 0, 0, name.as_ptr());
        if !h.is_null() {
            SetEvent(h);
            CloseHandle(h);
        }
    }
}

enum UserEvent {
    Tray(TrayIconEvent),
    Menu(MenuEvent),
    Ipc(String),
    Stats(Option<Stats>),
    Quit,
}

struct Stats {
    left: f64,
    cap: f64,
    active: Option<String>,
    // The tray app build the proxy publishes (`tray_build`), when it has one.
    tray_build: Option<String>,
}

fn wide(s: &str) -> Vec<u16> {
    s.encode_utf16().chain(Some(0)).collect()
}

fn alert(text: &str) {
    let (t, c) = (wide(text), wide("Uso Claude"));
    unsafe { MessageBoxW(std::ptr::null_mut(), t.as_ptr(), c.as_ptr(), MB_OK | MB_ICONWARNING) };
}

fn install_dir() -> Option<PathBuf> {
    Some(PathBuf::from(std::env::var_os("LOCALAPPDATA")?).join(APP))
}

fn same_file(a: &Path, b: &Path) -> bool {
    match (a.canonicalize(), b.canonicalize()) {
        (Ok(x), Ok(y)) => x.to_string_lossy().eq_ignore_ascii_case(&y.to_string_lossy()),
        _ => false,
    }
}

/// The URL the proxy appended to this .exe on download, if any.
fn overlay_url(exe: &Path) -> Option<String> {
    let data = std::fs::read(exe).ok()?;
    let tail = String::from_utf8_lossy(&data[data.len().saturating_sub(512)..]).into_owned();
    let rest = &tail[tail.rfind(MARK)? + MARK.len()..];
    let url = rest[..rest.find('\n').unwrap_or(rest.len())].trim();
    (url.starts_with("http://") || url.starts_with("https://")).then(|| url.trim_end_matches('/').to_string())
}

fn base_url() -> String {
    if let Some(dir) = std::env::var_os("APPDATA") {
        if let Ok(s) = std::fs::read_to_string(PathBuf::from(dir).join(APP).join("url.txt")) {
            let s = s.trim();
            if s.starts_with("http") {
                return s.trim_end_matches('/').to_string();
            }
        }
    }
    std::env::current_exe().ok().and_then(|e| overlay_url(&e)).unwrap_or_else(|| DEFAULT_URL.to_string())
}

const RUN_KEY: &str = r"Software\Microsoft\Windows\CurrentVersion\Run";

fn autostart_get() -> bool {
    use winreg::{enums::HKEY_CURRENT_USER, RegKey};
    RegKey::predef(HKEY_CURRENT_USER)
        .open_subkey(RUN_KEY)
        .and_then(|k| k.get_value::<String, _>(APP))
        .is_ok()
}

fn autostart_set(on: bool, exe: &Path) {
    use winreg::{enums::HKEY_CURRENT_USER, RegKey};
    if let Ok((k, _)) = RegKey::predef(HKEY_CURRENT_USER).create_subkey(RUN_KEY) {
        let _ = if on { k.set_value(APP, &format!("\"{}\"", exe.display())) } else { k.delete_value(APP) };
    }
}

/// Runs from the install folder; otherwise installs (replacing a running copy) and returns false.
fn ensure_installed() -> bool {
    let (Ok(exe), Some(dir)) = (std::env::current_exe(), install_dir()) else { return true };
    let target = dir.join(format!("{APP}.exe"));
    if same_file(&exe, &target) {
        return true;
    }
    // Ask an installed copy that is running to quit, so its file can be replaced.
    signal_quit();
    if std::fs::create_dir_all(&dir).is_err() {
        alert("Não consegui criar a pasta do app em %LOCALAPPDATA%.");
        return false;
    }
    let first_install = !target.exists();
    let mut copied = false;
    for _ in 0..12 {
        thread::sleep(Duration::from_millis(400));
        if std::fs::copy(&exe, &target).is_ok() {
            copied = true;
            break;
        }
    }
    if !copied {
        alert("Não consegui atualizar o app: feche o Uso Claude pelo menu da bandeja (Sair) e abra de novo.");
        return false;
    }
    // Autostart is turned on once, at the first install; updates keep the user's choice.
    if first_install {
        autostart_set(true, &target);
    }
    if Command::new(&target).spawn().is_err() {
        alert("Instalei o app, mas não consegui abri-lo. Abra o Uso Claude pelo menu Iniciar ou pela pasta %LOCALAPPDATA%\\UsageTray.");
    }
    false
}

fn fetch_stats(base: &str) -> Option<Stats> {
    let v: serde_json::Value =
        ureq::get(&format!("{base}/usage/api/data")).timeout(Duration::from_secs(10)).call().ok()?.into_json().ok()?;
    let (mut left, mut cap, mut active) = (0.0, 0.0, None);
    for a in v["accounts"].as_array()? {
        if a["provider"] != "claude" || a["needs_login"].as_bool() == Some(true) {
            continue;
        }
        let wk = a["windows"].as_array().and_then(|ws| ws.iter().find(|w| w["kind"] == "weekly" || w["kind"] == "monthly"));
        if let Some(w) = wk {
            left += 100.0 - w["used_percent"].as_f64().unwrap_or(0.0).clamp(0.0, 100.0);
            cap += 100.0;
        }
        if a["active"].as_bool() == Some(true) {
            active = a["name"].as_str().filter(|s| !s.is_empty()).or(a["email"].as_str()).map(String::from);
        }
    }
    let tray_build = v["tray_build"].as_str().filter(|s| !s.is_empty()).map(String::from);
    Some(Stats { left, cap, active, tray_build })
}

/// 32×32 ring: the share of the weekly capacity left, green / amber / red; grey without data.
fn ring_icon(frac: Option<f64>) -> Icon {
    const N: u32 = 32;
    let (c, r_out, r_in) = ((N as f64 - 1.0) / 2.0, 15.0, 9.5);
    let col = match frac {
        Some(f) if f > 0.5 => [34, 197, 94],
        Some(f) if f > 0.2 => [250, 178, 25],
        Some(_) => [232, 72, 72],
        None => [150, 150, 150],
    };
    let mut rgba = vec![0u8; (N * N * 4) as usize];
    for y in 0..N {
        for x in 0..N {
            let (dx, dy) = (x as f64 - c, y as f64 - c);
            let d = (dx * dx + dy * dy).sqrt();
            let cover = (r_out + 0.5 - d).min(d - (r_in - 0.5)).clamp(0.0, 1.0);
            if cover <= 0.0 {
                continue;
            }
            // Clockwise from 12 o'clock, 0..1.
            let ang = (dx.atan2(-dy) + TAU) % TAU / TAU;
            let on = frac.is_some_and(|f| ang <= f.clamp(0.0, 1.0));
            let (rgb, a) = if on { (col, 255.0) } else { ([140, 140, 140], 120.0) };
            let i = ((y * N + x) * 4) as usize;
            rgba[i..i + 3].copy_from_slice(&rgb);
            rgba[i + 3] = (a * cover) as u8;
        }
    }
    Icon::from_rgba(rgba, N, N).expect("icon")
}

fn work_area(x: i32, y: i32) -> (i32, i32, i32, i32) {
    unsafe {
        let m = MonitorFromPoint(POINT { x, y }, MONITOR_DEFAULTTONEAREST);
        let mut mi: MONITORINFO = std::mem::zeroed();
        mi.cbSize = std::mem::size_of::<MONITORINFO>() as u32;
        if GetMonitorInfoW(m, &mut mi) != 0 {
            let r = mi.rcWork;
            (r.left, r.top, r.right, r.bottom)
        } else {
            (0, 0, 1920, 1040)
        }
    }
}

/// Puts the popup next to the tray icon at (ax, ay), inside the monitor's work area.
fn place(window: &Window, anchor: (i32, i32)) {
    let (l, t, r, b) = work_area(anchor.0, anchor.1);
    let size = window.outer_size();
    let (w, h, gap) = (size.width as i32, size.height as i32, 10);
    let x = (anchor.0 - w / 2).clamp(l + gap, (r - w - gap).max(l + gap));
    let y = if anchor.1 > (t + b) / 2 { (b - h - gap).max(t + gap) } else { t + gap };
    window.set_outer_position(tao::dpi::PhysicalPosition::new(x, y));
}

fn open_dashboard(base: &str) {
    let _ = Command::new("explorer").arg(format!("{base}/usage/")).spawn();
}

fn make_webview(window: &Window, url: &str, proxy: EventLoopProxy<UserEvent>) -> Option<WebView> {
    let bg = if window.theme() == Theme::Dark { (10, 10, 10, 255) } else { (255, 255, 255, 255) };
    WebViewBuilder::new()
        .with_url(url)
        .with_background_color(bg)
        .with_ipc_handler(move |req| {
            let _ = proxy.send_event(UserEvent::Ipc(req.body().clone()));
        })
        .build(window)
        .ok()
}

fn main() {
    if !ensure_installed() {
        return;
    }
    // One copy at a time, the newest: a running one is asked to quit and this one waits for it (up to 10 s).
    let mutex_name = wide(&format!("Local\\{APP}.Single"));
    let mut _mutex = unsafe { CreateMutexW(std::ptr::null(), 0, mutex_name.as_ptr()) };
    let mut tries = 0;
    while unsafe { GetLastError() } == ERROR_ALREADY_EXISTS {
        if tries == 50 {
            return;
        }
        tries += 1;
        signal_quit();
        unsafe { CloseHandle(_mutex) };
        thread::sleep(Duration::from_millis(200));
        _mutex = unsafe { CreateMutexW(std::ptr::null(), 0, mutex_name.as_ptr()) };
    }
    cleanup_leftovers();
    if let Some(dir) = install_dir() {
        std::env::set_var("WEBVIEW2_USER_DATA_FOLDER", dir.join("WebView2"));
    }
    let exe = std::env::current_exe().unwrap_or_default();
    // "Start with Windows" always points at this (installed) copy, not at an old download.
    if autostart_get() {
        autostart_set(true, &exe);
    }
    let base = base_url();
    let page = format!("{base}/usage/?mini");

    let event_loop = EventLoopBuilder::<UserEvent>::with_user_event().build();
    let proxy = event_loop.create_proxy();
    {
        let p = proxy.clone();
        TrayIconEvent::set_event_handler(Some(move |e| {
            let _ = p.send_event(UserEvent::Tray(e));
        }));
        let p = proxy.clone();
        MenuEvent::set_event_handler(Some(move |e| {
            let _ = p.send_event(UserEvent::Menu(e));
        }));
    }
    // A newer download asks this copy to quit (see ensure_installed).
    {
        let p = proxy.clone();
        let name = wide(&format!("Local\\{APP}.Quit"));
        let h = unsafe { CreateEventW(std::ptr::null(), 0, 0, name.as_ptr()) } as usize;
        if h != 0 {
            thread::spawn(move || {
                unsafe { WaitForSingleObject(h as _, INFINITE) };
                let _ = p.send_event(UserEvent::Quit);
            });
        }
    }
    {
        let (p, b) = (proxy.clone(), base.clone());
        thread::spawn(move || loop {
            if p.send_event(UserEvent::Stats(fetch_stats(&b))).is_err() {
                break;
            }
            thread::sleep(POLL);
        });
    }
    // Updater: gets the build the proxy publishes whenever the stats poll sees it differ from this one (a failed
    // attempt is retried on a later poll, at most every 10 minutes). Local "dev" builds never update.
    let (upd_tx, upd_rx) = mpsc::channel::<String>();
    {
        let (p, b) = (proxy.clone(), base.clone());
        thread::spawn(move || {
            let mut last_try: Option<Instant> = None;
            while let Ok(mut want) = upd_rx.recv() {
                while let Ok(w) = upd_rx.try_recv() {
                    want = w;
                }
                if last_try.is_some_and(|t| t.elapsed() < Duration::from_secs(600)) {
                    continue;
                }
                last_try = Some(Instant::now());
                if update_to(&b, &want).is_ok() {
                    let _ = p.send_event(UserEvent::Quit);
                    break;
                }
                cleanup_leftovers();
            }
        });
    }

    let window = WindowBuilder::new()
        .with_title("Uso Claude")
        .with_decorations(false)
        .with_undecorated_shadow(true)
        .with_resizable(false)
        .with_visible(false)
        .with_always_on_top(true)
        .with_skip_taskbar(true)
        .with_inner_size(LogicalSize::new(WIDTH, 400.0))
        .build(&event_loop)
        .expect("window");
    let hwnd = window.hwnd();
    unsafe {
        let pref = DWMWCP_ROUND;
        DwmSetWindowAttribute(hwnd as _, DWMWA_WINDOW_CORNER_PREFERENCE as _, &pref as *const _ as _, 4);
    }

    let m_open = MenuItem::new("Abrir painel no navegador", true, None);
    let m_auto = CheckMenuItem::new("Iniciar com o Windows", true, autostart_get(), None);
    let m_quit = MenuItem::new("Sair", true, None);
    let menu = Menu::new();
    let _ = menu.append_items(&[&m_open, &PredefinedMenuItem::separator(), &m_auto, &m_quit]);
    let mut menu = Some(menu);

    let mut tray: Option<TrayIcon> = None;
    let mut webview: Option<WebView> = None;
    let mut visible = false;
    let mut anchor = (0, 0);
    let mut hidden_at = Instant::now();
    // Whether the popup has held the foreground since it was shown: it hides when another window takes it over.
    // Polled while visible, since focus moving between the window and its WebView child sends no reliable event.
    let mut had_fg = false;
    // Whether the popup was open when the tray button went down: that click closes it and must not reopen it.
    let mut open_at_press = false;

    event_loop.run(move |event, _, control_flow| {
        // Visible: poll the foreground every 200 ms. Hidden: sleep until the WebView is due to be dropped, or for good.
        *control_flow = if visible {
            ControlFlow::WaitUntil(Instant::now() + Duration::from_millis(200))
        } else if webview.is_some() {
            ControlFlow::WaitUntil(hidden_at + DROP_AFTER + Duration::from_secs(1))
        } else {
            ControlFlow::Wait
        };

        let mut hide = |visible: &mut bool, hidden_at: &mut Instant| {
            if *visible {
                window.set_visible(false);
                *visible = false;
                *hidden_at = Instant::now();
            }
        };

        match event {
            Event::NewEvents(StartCause::Init) => {
                tray = TrayIconBuilder::new()
                    .with_menu(Box::new(menu.take().expect("menu")))
                    .with_menu_on_left_click(false)
                    .with_tooltip("Uso Claude")
                    .with_icon(ring_icon(None))
                    .build()
                    .ok();
            }
            Event::UserEvent(UserEvent::Tray(TrayIconEvent::Click {
                button: MouseButton::Left,
                button_state: MouseButtonState::Down,
                ..
            })) => open_at_press = visible,
            Event::UserEvent(UserEvent::Tray(TrayIconEvent::Click {
                button: MouseButton::Left,
                button_state: MouseButtonState::Up,
                rect,
                ..
            })) => {
                if visible || open_at_press {
                    // The press may already have hidden it (the taskbar took the foreground).
                    hide(&mut visible, &mut hidden_at);
                    open_at_press = false;
                } else {
                    anchor = (
                        rect.position.x as i32 + rect.size.width as i32 / 2,
                        rect.position.y as i32 + rect.size.height as i32 / 2,
                    );
                    if webview.is_none() {
                        webview = make_webview(&window, &page, proxy.clone());
                    }
                    place(&window, anchor);
                    window.set_visible(true);
                    window.set_focus();
                    visible = true;
                    had_fg = false;
                }
            }
            Event::UserEvent(UserEvent::Ipc(msg)) => {
                if let Some(h) = msg.strip_prefix("h:").and_then(|h| h.parse::<f64>().ok()).filter(|h| h.is_finite()) {
                    window.set_inner_size(LogicalSize::new(WIDTH, h.clamp(120.0, 900.0)));
                    if visible {
                        place(&window, anchor);
                    }
                } else if msg == "open" {
                    hide(&mut visible, &mut hidden_at);
                    open_dashboard(&base);
                } else if msg == "hide" {
                    hide(&mut visible, &mut hidden_at);
                }
            }
            Event::UserEvent(UserEvent::Menu(e)) => {
                if e.id == *m_open.id() {
                    open_dashboard(&base);
                } else if e.id == *m_auto.id() {
                    autostart_set(m_auto.is_checked(), &exe);
                } else if e.id == *m_quit.id() {
                    *control_flow = ControlFlow::Exit;
                }
            }
            Event::UserEvent(UserEvent::Stats(st)) => {
                if let Some(want) = st.as_ref().and_then(|s| s.tray_build.as_deref()) {
                    if build_id() != "dev" && want != build_id() {
                        let _ = upd_tx.send(want.to_string());
                    }
                }
                if let Some(t) = &tray {
                    let (frac, tip) = match &st {
                        Some(s) if s.cap > 0.0 => (
                            Some(s.left / s.cap),
                            format!(
                                "Claude · {:.0}% de {:.0}% semanal{}",
                                s.left,
                                s.cap,
                                s.active.as_deref().map(|a| format!("\nEm uso: {a}")).unwrap_or_default()
                            ),
                        ),
                        Some(_) => (None, "Claude · nenhuma conta com limite semanal".to_string()),
                        None => (None, format!("Claude · sem conexão com {base}")),
                    };
                    let _ = t.set_icon(Some(ring_icon(frac)));
                    let _ = t.set_tooltip(Some(tip.chars().take(120).collect::<String>()));
                }
            }
            Event::UserEvent(UserEvent::Quit) => *control_flow = ControlFlow::Exit,
            Event::WindowEvent { event: WindowEvent::CloseRequested, .. } => hide(&mut visible, &mut hidden_at),
            _ => {}
        }

        if visible {
            let ours = unsafe { GetForegroundWindow() } as isize == hwnd;
            if ours {
                had_fg = true;
            } else if had_fg {
                hide(&mut visible, &mut hidden_at);
            }
        }
        if !visible && webview.is_some() && hidden_at.elapsed() > DROP_AFTER {
            webview = None;
        }
        if *control_flow == ControlFlow::Exit {
            webview = None;
            tray = None;
        }
    });
}
