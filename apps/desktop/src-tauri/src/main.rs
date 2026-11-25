#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]
use ghostlink_contracts::{Action, ConnectionState, Request, Response, Status, SCHEMA_VERSION};
use std::time::Duration;
use tauri::{
    menu::{Menu, MenuItem},
    tray::{TrayIcon, TrayIconBuilder},
    Manager,
};

/// How long the app waits for the service: as long as the CLI and the MCP bridge do. The service
/// may be busy with a tunnel start or a call to a server for longer than 30 seconds (the old
/// limit), and its answer must not be reported as a timeout while the operation goes on and
/// succeeds.
const REQUEST_TIMEOUT: Duration = Duration::from_secs(ghostlink_contracts::REQUEST_TIMEOUT_SECONDS);

#[tauri::command]
async fn vpn_request(action: Action) -> Response {
    // The GUI may import WireGuard/AmneziaWG files; OpenVPN profiles stay CLI-only.
    let wireguard_file = matches!(&action, Action::ProfileImport { content, .. }
        if content.lines().any(|line| line.trim().eq_ignore_ascii_case("[interface]")));
    if !wireguard_file
        && matches!(
            action,
            Action::AgentGrant { .. } | Action::AgentRevoke { .. } | Action::ProfileImport { .. }
        )
    {
        return Response::err(
            "permission_denied",
            "Use the CLI for administrator and OpenVPN profile operations",
        );
    }
    let endpoint = std::env::var("GHOSTLINK_ENDPOINT")
        .unwrap_or_else(|_| ghostlink_contracts::default_endpoint());
    ghostlink_contracts::call(
        &endpoint,
        Request {
            schema_version: SCHEMA_VERSION,
            agent_token: None,
            action,
        },
        REQUEST_TIMEOUT,
    )
    .await
}

/// WireGuard for Windows and AmneziaWG name a tunnel after its .conf file and refuse names longer
/// than this, or with a space: a second copy is "name-1.conf" rather than "name (1).conf". (The
/// views keep .conf names to 29 characters, which leaves room for "-99".)
const CONF_NAME_MAX: usize = 32;

/// The file name for the `n`th try at saving `stem.extension` (0 is the plain name). `stem` holds
/// only ASCII letters, digits, '-' and '_'.
fn download_name(stem: &str, extension: &str, n: u32) -> String {
    let suffix = match (n, extension) {
        (0, _) => String::new(),
        (_, "conf") => format!("-{n}"),
        _ => format!(" ({n})"),
    };
    let stem = if extension == "conf" {
        &stem[..stem.len().min(CONF_NAME_MAX.saturating_sub(suffix.len()))]
    } else {
        stem
    };
    format!("{stem}{suffix}.{extension}")
}

/// Saves a connection kit page, an Apple VPN profile or a WireGuard/AmneziaWG .conf in the user's
/// Downloads folder. WebView2 cannot download blob: links from the app's custom scheme, so the
/// shell writes the file itself: a name built from safe characters with an allowlisted extension
/// (html, mobileconfig or conf), never overwriting an existing file, at most 4 MiB.
#[tauri::command]
fn save_download(
    app: tauri::AppHandle,
    name: String,
    content: String,
    extension: Option<String>,
) -> Result<String, String> {
    let extension = match extension.as_deref() {
        None | Some("html") => "html",
        Some("mobileconfig") => "mobileconfig",
        Some("conf") => "conf",
        Some(_) => return Err("This kind of file cannot be saved".into()),
    };
    if content.len() > 4 << 20 {
        return Err("The file is too large".into());
    }
    let stem: String = name
        .chars()
        .filter(|c| c.is_ascii_alphanumeric() || *c == '-' || *c == '_')
        .take(60)
        .collect();
    let stem = if stem.is_empty() {
        "ghostlink-kit".to_string()
    } else {
        stem
    };
    let dir = app
        .path()
        .download_dir()
        .map_err(|_| "Could not find your Downloads folder".to_string())?;
    save_in(&dir, &stem, extension, &content).map(|path| path.display().to_string())
}

/// Writes `content` to a NEW file in `dir`, named after `stem`: the first of the numbered names
/// that is free. The file holds keys and client configurations, so it is private to this user
/// from the moment it exists (mode 0600 on Unix; on Windows a protected ACL for SYSTEM and this
/// user), where a file made the ordinary way takes the folder's permissions; and one that cannot
/// be written completely is not left behind.
fn save_in(
    dir: &std::path::Path,
    stem: &str,
    extension: &str,
    content: &str,
) -> Result<std::path::PathBuf, String> {
    use std::io::Write;
    for n in 0..100 {
        let path = dir.join(download_name(stem, extension, n));
        match ghostlink_contracts::private::create_file(&path) {
            Ok(mut file) => {
                let written = file
                    .write_all(content.as_bytes())
                    .and_then(|_| file.sync_all());
                if written.is_err() {
                    // Closed first: Windows does not delete a file that is open.
                    drop(file);
                    let _ = std::fs::remove_file(&path);
                    return Err("Could not write the file".into());
                }
                return Ok(path);
            }
            Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(_) => return Err("Could not create the file in Downloads".into()),
        }
    }
    Err("Too many files with this name in Downloads".into())
}

/// What the tray icon's tooltip says about the connection, from the service's status; None is a
/// service that does not answer. It names the server by its ID, cut to 40 characters: the
/// platforms cut a tooltip at about 127.
fn tray_tooltip(status: Option<&Status>) -> String {
    let Some(status) = status else {
        return "Ghostlink: the service is not running".into();
    };
    let server: String = status
        .profile
        .as_deref()
        .unwrap_or_default()
        .chars()
        .take(40)
        .collect();
    let to = if server.is_empty() {
        String::new()
    } else {
        format!(" to {server}")
    };
    match status.status {
        ConnectionState::Connected => format!("Ghostlink: connected{to}"),
        ConnectionState::Connecting | ConnectionState::Reconnecting => {
            format!("Ghostlink: connecting{to}")
        }
        ConnectionState::Disconnecting => "Ghostlink: disconnecting".into(),
        ConnectionState::Failed => "Ghostlink: connection failed".into(),
        ConnectionState::Disconnected => "Ghostlink: disconnected".into(),
    }
}

/// Keeps the tray icon's tooltip in step with the connection: the service is asked every few
/// seconds, and the tooltip is set only when its text changes. The menu is untouched.
async fn follow_connection(tray: TrayIcon) {
    let endpoint = std::env::var("GHOSTLINK_ENDPOINT")
        .unwrap_or_else(|_| ghostlink_contracts::default_endpoint());
    let mut shown = String::new();
    loop {
        let request = Request {
            schema_version: SCHEMA_VERSION,
            agent_token: None,
            action: Action::Status {},
        };
        let response = ghostlink_contracts::call(&endpoint, request, Duration::from_secs(3)).await;
        let status: Option<Status> = response
            .data
            .filter(|_| response.success)
            .and_then(|data| serde_json::from_value(data).ok());
        let text = tray_tooltip(status.as_ref());
        if text != shown {
            let _ = tray.set_tooltip(Some(&text));
            shown = text;
        }
        tokio::time::sleep(Duration::from_secs(3)).await;
    }
}

fn main() {
    tauri::Builder::default()
        .invoke_handler(tauri::generate_handler![vpn_request, save_download])
        .setup(|app| {
            let show = MenuItem::with_id(app, "show", "Open Ghostlink", true, None::<&str>)?;
            let disconnect =
                MenuItem::with_id(app, "disconnect", "Disconnect", true, None::<&str>)?;
            let quit = MenuItem::with_id(app, "quit", "Quit Ghostlink", true, None::<&str>)?;
            let menu = Menu::with_items(app, &[&show, &disconnect, &quit])?;
            let mut tray = TrayIconBuilder::new()
                .tooltip("Ghostlink")
                .menu(&menu)
                .show_menu_on_left_click(true)
                .on_menu_event(|app, event| match event.id.as_ref() {
                    "show" => {
                        if let Some(window) = app.get_webview_window("main") {
                            // show() leaves a minimized window minimized, and set_focus() does
                            // nothing then: restore it first.
                            let _ = window.unminimize();
                            let _ = window.show();
                            let _ = window.set_focus();
                        }
                    }
                    "disconnect" => {
                        tauri::async_runtime::spawn(async {
                            let _ = vpn_request(Action::Disconnect {}).await;
                        });
                    }
                    "quit" => app.exit(0),
                    _ => {}
                });
            if let Some(icon) = app.default_window_icon() {
                tray = tray.icon(icon.clone());
            }
            let tray = tray.build(app)?;
            tauri::async_runtime::spawn(follow_connection(tray));
            Ok(())
        })
        .on_window_event(|window, event| {
            if let tauri::WindowEvent::CloseRequested { api, .. } = event {
                api.prevent_close();
                let _ = window.hide();
            }
        })
        .run(tauri::generate_context!())
        .expect("Ghostlink desktop could not start");
}

#[cfg(test)]
mod tests {
    use super::{download_name, save_in, tray_tooltip, ConnectionState, Status, REQUEST_TIMEOUT};

    #[test]
    fn conf_files_keep_names_that_wireguard_can_import() {
        assert_eq!(
            download_name("maya-wireguard-15", "conf", 0),
            "maya-wireguard-15.conf"
        );
        assert_eq!(
            download_name("maya-wireguard-15", "conf", 1),
            "maya-wireguard-15-1.conf"
        );
        let views = "Grandpa-s-Window-wireguard-15"; // the views send at most 29 characters
        assert_eq!(download_name(views, "conf", 99), format!("{views}-99.conf"));
        let long = "a".repeat(40);
        assert_eq!(
            download_name(&long, "conf", 0),
            format!("{}.conf", "a".repeat(32))
        );
        assert_eq!(
            download_name(&long, "conf", 12),
            format!("{}-12.conf", "a".repeat(29))
        );
    }

    #[test]
    fn other_files_keep_their_names() {
        assert_eq!(download_name("maya-vpn", "html", 0), "maya-vpn.html");
        assert_eq!(download_name("maya-vpn", "html", 2), "maya-vpn (2).html");
        let long = "a".repeat(60);
        assert_eq!(
            download_name(&long, "mobileconfig", 0),
            format!("{long}.mobileconfig")
        );
    }

    fn scratch(name: &str) -> std::path::PathBuf {
        let dir =
            std::env::temp_dir().join(format!("ghostlink-desktop-{}-{name}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    /// An export never replaces a file that is already there: it takes the next free name.
    #[test]
    fn exports_take_the_next_free_name_and_never_replace_a_file() {
        let dir = scratch("names");
        let first = save_in(&dir, "maya-vpn", "html", "one").unwrap();
        let second = save_in(&dir, "maya-vpn", "html", "two").unwrap();
        assert_eq!(first.file_name().unwrap(), "maya-vpn.html");
        assert_eq!(second.file_name().unwrap(), "maya-vpn (1).html");
        assert_eq!(std::fs::read_to_string(&first).unwrap(), "one");
        assert_eq!(std::fs::read_to_string(&second).unwrap(), "two");
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Exports hold keys and client configurations: they are private to this user from the moment
    /// they exist, not world-readable under the default umask or the folder's inherited ACL.
    #[cfg(unix)]
    #[test]
    fn exports_are_private_to_this_user() {
        use std::os::unix::fs::PermissionsExt;
        let dir = scratch("mode");
        let path = save_in(&dir, "maya-wireguard", "conf", "[Interface]\n").unwrap();
        let mode = std::fs::metadata(&path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600);
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// On Windows the ACL has SYSTEM and this user only, and takes nothing from the folder: no
    /// entry is inherited (icacls marks those `(I)`), where an ordinary file in the same folder
    /// has the folder's.
    #[cfg(windows)]
    #[test]
    fn exports_are_private_to_this_user() {
        let dir = scratch("acl");
        let acl = |path: &std::path::Path| {
            let out = std::process::Command::new("icacls")
                .arg(path)
                .output()
                .unwrap();
            assert!(out.status.success(), "icacls failed");
            String::from_utf8_lossy(&out.stdout).into_owned()
        };
        let path = save_in(&dir, "maya-wireguard", "conf", "[Interface]\n").unwrap();
        let private = acl(&path);
        assert!(
            !private.contains("(I)"),
            "inherits from the folder: {private}"
        );
        assert!(private.contains("AUTHORITY\\SYSTEM:"), "{private}");
        let entries = private.lines().filter(|l| l.contains(":(")).count();
        assert_eq!(entries, 2, "SYSTEM and this user, nobody else: {private}");
        let ordinary = dir.join("ordinary.txt");
        std::fs::write(&ordinary, "x").unwrap();
        assert!(acl(&ordinary).contains("(I)"), "the control inherits");
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// The tray's tooltip tells the state of the connection (it said "Ghostlink" whatever it was).
    #[test]
    fn the_tray_tooltip_follows_the_connection() {
        let status = |state: ConnectionState, profile: Option<&str>| Status {
            status: state,
            profile: profile.map(String::from),
            ..Default::default()
        };
        assert_eq!(
            tray_tooltip(Some(&status(ConnectionState::Connected, Some("home")))),
            "Ghostlink: connected to home"
        );
        assert_eq!(
            tray_tooltip(Some(&status(ConnectionState::Reconnecting, Some("home")))),
            "Ghostlink: connecting to home"
        );
        assert_eq!(
            tray_tooltip(Some(&status(ConnectionState::Disconnected, None))),
            "Ghostlink: disconnected"
        );
        assert_eq!(
            tray_tooltip(Some(&status(ConnectionState::Failed, Some("home")))),
            "Ghostlink: connection failed"
        );
        assert_eq!(
            tray_tooltip(Some(&status(ConnectionState::Connected, None))),
            "Ghostlink: connected"
        );
        assert_eq!(tray_tooltip(None), "Ghostlink: the service is not running");
        // A long server ID cannot push the text past what the platforms show (127 characters).
        let long = "x".repeat(200);
        let text = tray_tooltip(Some(&status(ConnectionState::Connected, Some(&long))));
        assert!(text.chars().count() <= 127, "{text}");
        assert!(text.ends_with(&"x".repeat(40)));
    }

    /// The app waits for the service as long as the CLI and the MCP bridge do; it used to give up
    /// after 30 seconds, while a tunnel start or a call to a server was still going on.
    #[test]
    fn the_app_waits_as_long_as_the_other_clients() {
        assert_eq!(ghostlink_contracts::REQUEST_TIMEOUT_SECONDS, 75);
        assert_eq!(REQUEST_TIMEOUT, std::time::Duration::from_secs(75));
    }
}
