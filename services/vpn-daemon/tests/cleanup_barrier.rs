//! The real service (`ghostlinkd`), its real IPC and the real session code, with a stand-in for
//! the tunnel worker (code review 2026-11-25, C3 and M1). The stand-in is this test binary
//! itself, which acts as the worker when the service starts it with `version`, `check` or `run`
//! (the test has its own `main`, `harness = false`). It changes no network settings. A
//! `fake-mode` file in the service's run directory says how it ends.
use ghostlink_contracts::{call, Action, ConnectionState, Request, Response, Status};
use ghostlink_engine::worker_session;
use std::{
    path::{Path, PathBuf},
    process::{exit, Child, Command, Stdio},
    time::{Duration, Instant},
};

fn main() {
    let args: Vec<String> = std::env::args().collect();
    match args.get(1).map(String::as_str) {
        Some("version") => {
            // A file named by the environment makes the worker "unavailable" for the service.
            let broken = std::env::var_os("GHOSTLINK_STAND_IN_BROKEN").map(PathBuf::from);
            if broken.is_some_and(|file| file.exists()) {
                exit(3)
            }
            println!("{}", worker_session::WORKER_VERSION)
        }
        Some("check") => {}
        Some("run") if args.len() == 4 => worker(Path::new(&args[3])),
        _ => cases(),
    }
}

/// The stand-in's `run -c CONFIG`.
fn worker(config: &Path) {
    let mode = std::fs::read_to_string(config.with_file_name("fake-mode")).unwrap_or_default();
    match mode.trim() {
        // Ends with an error before it could report `started`: a crash, or a kill.
        "exit-before-started" => exit(23),
        // Waits for the stop command (or the end of its control pipe), then exits cleanly.
        _ => {
            let mut line = String::new();
            let _ = std::io::stdin().read_line(&mut line);
            exit(0)
        }
    }
}

struct Service {
    child: Child,
    endpoint: String,
    base: PathBuf,
}

impl Service {
    fn start(name: &str) -> Self {
        let base =
            std::env::temp_dir().join(format!("ghostlink-barrier-{}-{name}", std::process::id()));
        let _ = std::fs::remove_dir_all(&base);
        std::fs::create_dir_all(&base).unwrap();
        // The service refuses a socket directory that others can write to, whatever the umask.
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&base, std::fs::Permissions::from_mode(0o700)).unwrap();
        }
        let endpoint = if cfg!(windows) {
            format!(r"\\.\pipe\ghostlink-barrier-{}-{name}", std::process::id())
        } else {
            base.join("control.sock").display().to_string()
        };
        let worker = std::env::current_exe().unwrap();
        let mut command = Command::new(env!("CARGO_BIN_EXE_ghostlinkd"));
        command
            .args(["--endpoint", &endpoint, "--data-dir"])
            .arg(base.join("data"))
            .arg("--outline-engine")
            .arg(&worker)
            .args(["--connection-timeout-seconds", "60"])
            .env("GHOSTLINK_STAND_IN_BROKEN", base.join("broken"))
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        // Linux asks for a full tunnel to be authorized; the stand-in changes nothing.
        #[cfg(unix)]
        command.arg("--allow-full-tunnel");
        Self {
            child: command.spawn().expect("the service starts"),
            endpoint,
            base,
        }
    }

    async fn ask(&self, action: Action) -> Response {
        let request = Request {
            schema_version: ghostlink_contracts::SCHEMA_VERSION,
            agent_token: None,
            action,
        };
        call(&self.endpoint, request, Duration::from_secs(30)).await
    }

    async fn status(&self) -> Status {
        let response = self.ask(Action::Status {}).await;
        serde_json::from_value(response.data.expect("a status")).unwrap()
    }

    async fn wait_until_ready(&self) {
        for _ in 0..200 {
            if self.ask(Action::Status {}).await.success {
                return;
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        panic!("the service did not start");
    }

    /// Polls the status until `done` holds.
    async fn wait_for(&self, what: &str, done: impl Fn(&Status) -> bool) -> Status {
        let deadline = Instant::now() + Duration::from_secs(30);
        loop {
            let status = self.status().await;
            if done(&status) {
                return status;
            }
            assert!(Instant::now() < deadline, "waiting for {what}: {status:?}");
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    }

    /// How the stand-in worker ends from now on.
    fn worker_mode(&self, mode: &str) {
        let run = self.base.join("data").join("run");
        std::fs::create_dir_all(&run).unwrap();
        std::fs::write(run.join("fake-mode"), mode).unwrap();
    }

    async fn import(&self, name: &str, host: &str) {
        let key = format!("ss://aes-256-gcm:test-only-secret@{host}:8388");
        let response = self
            .ask(Action::AccessKeyImport {
                name: name.into(),
                access_key: key,
                replace: false,
            })
            .await;
        assert!(
            response.success,
            "import {name}: {:?}",
            response.error.map(|e| e.message)
        );
    }
}

impl Drop for Service {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
        let _ = std::fs::remove_dir_all(&self.base);
    }
}

fn code(response: &Response) -> &str {
    response
        .error
        .as_ref()
        .map_or("success", |e| e.code.as_str())
}

const BARRIER: &str = "network_cleanup_unverified";

fn cases() {
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let failures = runtime.block_on(async {
        let service = Service::start("one");
        service.wait_until_ready().await;
        service.import("home", "203.0.113.9").await;
        service.import("office", "203.0.113.10").await;
        let mut failures = 0;
        let mut check = |ok: bool, what: &str| {
            println!("{} {what}", if ok { "ok  " } else { "FAIL" });
            failures += usize::from(!ok);
        };
        let connect = |name: &str| Action::Connect {
            profile: Some(name.into()),
        };

        // A worker that ends with an error before it says `started` leaves the cleanup
        // unverified: the barrier is set.
        service.worker_mode("exit-before-started");
        let accepted = service.ask(connect("home")).await;
        check(
            accepted.success,
            "a connection with a failing worker is accepted",
        );
        let status = service
            .wait_for("the barrier", |s| s.last_error.is_some())
            .await;
        check(
            status.status == ConnectionState::Failed
                && status.last_error.as_deref() == Some(BARRIER),
            &format!(
                "the failed worker sets the barrier ({:?})",
                status.last_error
            ),
        );

        // Disconnect looks again: this machine has no `Ghostlink` interface and no address of
        // the tunnel, so the cleanup is verified and the service is not blocked for good (it
        // used to answer cleanup_unverified here until it was restarted).
        let disconnected = service.ask(Action::Disconnect {}).await;
        check(
            disconnected.success,
            &format!(
                "disconnect verifies the cleanup and lifts the barrier ({})",
                code(&disconnected)
            ),
        );
        let status = service.status().await;
        check(
            status.status == ConnectionState::Disconnected && status.last_error.is_none(),
            &format!(
                "no barrier and no failure remain ({:?} {:?})",
                status.status, status.last_error
            ),
        );

        // A new connection verifies it too, so a client that only offers Connect (the desktop
        // app does, in the failed state) gets out as well.
        service.worker_mode("exit-before-started");
        service.ask(connect("home")).await;
        service
            .wait_for("the barrier again", |s| {
                s.last_error.as_deref() == Some(BARRIER)
            })
            .await;
        service.worker_mode("stop");
        let again = service.ask(connect("home")).await;
        check(
            again.success,
            &format!("connect verifies the cleanup and starts ({})", code(&again)),
        );
        let status = service.status().await;
        check(
            status.status == ConnectionState::Connecting && status.last_error.is_none(),
            &format!(
                "the new session runs ({:?} {:?})",
                status.status, status.last_error
            ),
        );

        // Another server is active and this is not a switch: busy, whatever else is wrong with
        // the request. The worker is made unavailable for the service here; the old order
        // checked that first and answered engine_unavailable.
        std::fs::write(service.base.join("broken"), "x").unwrap();
        let busy = service.ask(connect("office")).await;
        check(
            code(&busy) == "busy",
            &format!(
                "a second server while one is active is busy ({})",
                code(&busy)
            ),
        );
        std::fs::remove_file(service.base.join("broken")).unwrap();

        let stopped = service.ask(Action::Disconnect {}).await;
        check(
            stopped.success && service.status().await.last_error.is_none(),
            "a stopped session ends clean",
        );
        failures
    });
    if failures > 0 {
        exit(1);
    }
}
