//! The real service (`ghostlinkd`), its real IPC and the real session code, with a stand-in for
//! the tunnel worker (code review 2026-11-25, C3 and M1). The stand-in is this test binary
//! itself, which acts as the worker when the service starts it with `version`, `check` or `run`
//! (the test has its own `main`, `harness = false`). It changes no network settings. A
//! `fake-mode` file in the service's run directory says how it ends, and every start is counted
//! in its `runs` file.
use ghostlink_contracts::{call, Action, ConnectionState, Request, Response, Status};
use ghostlink_engine::worker_session;
use std::{
    io::{Read, Write},
    net::TcpListener,
    path::{Path, PathBuf},
    process::{exit, Child, Command, Stdio},
    sync::{Arc, Condvar, Mutex},
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
    // One line for every time the service started a worker.
    if let Ok(mut runs) = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(config.with_file_name("runs"))
    {
        let _ = writeln!(runs, "run");
    }
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
    /// `key_url`: where a dynamic key is fetched from (see `KeyServer`).
    fn start(name: &str, key_url: Option<&str>) -> Self {
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
            .envs(key_url.map(|url| ("GHOSTLINK_TEST_DYNAMIC_KEY_URL", url)))
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
        self.ask_within(action, Duration::from_secs(30)).await
    }

    async fn ask_within(&self, action: Action, wait: Duration) -> Response {
        ask_at(&self.endpoint, action, wait).await
    }

    /// A request that is on its way while the test goes on.
    fn ask_later(&self, action: Action) -> tokio::task::JoinHandle<Response> {
        let endpoint = self.endpoint.clone();
        tokio::spawn(async move { ask_at(&endpoint, action, Duration::from_secs(60)).await })
    }

    /// Waits until the service has started the stand-in worker `count` times.
    async fn wait_runs(&self, count: usize) -> bool {
        for _ in 0..200 {
            if self.runs() >= count {
                return true;
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        false
    }

    /// How many times the service has started the stand-in worker.
    fn runs(&self) -> usize {
        std::fs::read_to_string(self.base.join("data").join("run").join("runs"))
            .map_or(0, |text| text.lines().count())
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

async fn ask_at(endpoint: &str, action: Action, wait: Duration) -> Response {
    let request = Request {
        schema_version: ghostlink_contracts::SCHEMA_VERSION,
        agent_token: None,
        action,
    };
    call(endpoint, request, wait).await
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
    let failures = runtime.block_on(async { barrier_cases().await + dynamic_key_cases().await });
    if failures > 0 {
        exit(1);
    }
}

async fn barrier_cases() -> usize {
    {
        let service = Service::start("one", None);
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
    }
}

/// What the key server does with the next request it gets.
#[derive(Default)]
struct Gate {
    /// Hold it until `release`.
    hold_next: bool,
    /// Answer it with an error.
    fail_next: bool,
    /// How many requests have been held so far.
    held: usize,
    released: bool,
}

/// A local HTTP server that serves a dynamic key (the service fetches it from here instead of an
/// HTTPS address, see `outline::pinned_client`) and holds an answer until the test lets it go, so
/// that what happens while the service waits for it does not depend on timing.
struct KeyServer {
    url: String,
    gate: Arc<(Mutex<Gate>, Condvar)>,
}

const SERVED_KEY: &str = "ss://aes-256-gcm:test-only-secret@203.0.113.9:8388";

impl KeyServer {
    fn start() -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let url = format!("http://{}/key", listener.local_addr().unwrap());
        let gate: Arc<(Mutex<Gate>, Condvar)> = Arc::default();
        let shared = gate.clone();
        std::thread::spawn(move || {
            for stream in listener.incoming().flatten() {
                let gate = shared.clone();
                std::thread::spawn(move || serve_key(stream, &gate));
            }
        });
        Self { url, gate }
    }

    fn hold_next(&self) {
        self.gate.0.lock().unwrap().hold_next = true;
    }

    fn fail_next(&self) {
        self.gate.0.lock().unwrap().fail_next = true;
    }

    /// Waits until `count` requests have arrived and are being held.
    fn wait_held(&self, count: usize) -> bool {
        let (lock, changed) = &*self.gate;
        let (gate, _) = changed
            .wait_timeout_while(lock.lock().unwrap(), Duration::from_secs(20), |g| {
                g.held < count
            })
            .unwrap();
        gate.held >= count
    }

    fn held(&self) -> usize {
        self.gate.0.lock().unwrap().held
    }

    /// Lets the held request go.
    fn release(&self) {
        let (lock, changed) = &*self.gate;
        lock.lock().unwrap().released = true;
        changed.notify_all();
    }
}

fn serve_key(mut stream: std::net::TcpStream, gate: &(Mutex<Gate>, Condvar)) {
    // The request is read to its end; its content does not matter.
    let mut request = Vec::new();
    let mut buffer = [0u8; 1024];
    while !request.windows(4).any(|w| w == b"\r\n\r\n") {
        match stream.read(&mut buffer) {
            Ok(0) | Err(_) => return,
            Ok(n) => request.extend_from_slice(&buffer[..n]),
        }
    }
    let (lock, changed) = gate;
    let mut state = lock.lock().unwrap();
    if std::mem::take(&mut state.hold_next) {
        state.held += 1;
        changed.notify_all();
        state = changed
            .wait_timeout_while(state, Duration::from_secs(60), |g| !g.released)
            .unwrap()
            .0;
        state.released = false;
    }
    let fail = std::mem::take(&mut state.fail_next);
    drop(state);
    let (status, body) = if fail {
        ("500 Internal Server Error", "")
    } else {
        ("200 OK", SERVED_KEY)
    };
    let _ = write!(
        stream,
        "HTTP/1.1 {status}\r\nContent-Type: text/plain\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    );
}

/// A dynamic key is fetched without the service lock (code review 2026-11-25, M1): while the
/// server holds its answer, the service answers everything else, one connection is prepared at a
/// time, a disconnect gives it up, and a server removed or replaced meanwhile is not started.
async fn dynamic_key_cases() -> usize {
    let server = KeyServer::start();
    let service = Service::start("two", Some(&server.url));
    service.wait_until_ready().await;
    let mut failures = 0;
    let mut check = |ok: bool, what: &str| {
        println!("{} {what}", if ok { "ok  " } else { "FAIL" });
        failures += usize::from(!ok);
    };
    let connect = |name: &str| Action::Connect {
        profile: Some(name.into()),
    };
    let switch = |name: &str| Action::Switch {
        profile: name.into(),
    };
    let import = |name: &str, key: &str, replace: bool| Action::AccessKeyImport {
        name: name.into(),
        access_key: key.into(),
        replace,
    };
    // Importing a dynamic key fetches it too; the server answers at once while it holds nothing.
    for (name, key) in [
        ("dyn", "ssconf://keys.example.com/first#Dynamic"),
        ("dyn2", "ssconf://keys.example.com/second#Second"),
    ] {
        let imported = service.ask(import(name, key, false)).await;
        check(
            imported.success,
            &format!("import of a dynamic key {name} ({})", code(&imported)),
        );
    }
    service.import("static", "203.0.113.10").await;
    let quick = Duration::from_secs(5);
    let held_for = |what: &str| format!("while the key is fetched: {what}");

    // 1. The service answers while the key is being fetched.
    server.hold_next();
    let first = service.ask_later(connect("dyn"));
    check(server.wait_held(1), "the service asked for the dynamic key");
    let started = Instant::now();
    let status = service.ask_within(Action::Status {}, quick).await;
    check(
        status.success && started.elapsed() < quick,
        &held_for(&format!("status answers at once ({})", code(&status))),
    );
    let listed = service.ask_within(Action::ProfileList {}, quick).await;
    check(
        listed.success,
        &held_for(&format!("the list of servers answers ({})", code(&listed))),
    );
    check(
        service.status().await.status == ConnectionState::Disconnected,
        &held_for("no session has started"),
    );

    // 2. One connection is prepared at a time.
    for (what, action) in [
        ("a connect for another server", connect("static")),
        ("a connect for the same server", connect("dyn")),
        ("a switch", switch("static")),
    ] {
        let answer = service.ask_within(action, quick).await;
        check(
            code(&answer) == "busy",
            &held_for(&format!("{what} is busy ({})", code(&answer))),
        );
    }
    check(server.held() == 1, &held_for("nothing else was fetched"));
    server.release();
    let first = first.await.unwrap();
    check(
        first.success,
        &format!("the first connect starts ({})", code(&first)),
    );
    let status = service.status().await;
    check(
        status.profile.as_deref() == Some("dyn") && service.wait_runs(1).await,
        &format!("one session runs for it ({:?})", status.profile),
    );
    check(
        service.runs() == 1,
        &format!("only one worker was started ({})", service.runs()),
    );

    // 3. A connect to the server that is running fetches nothing.
    server.hold_next();
    let again = service.ask_within(connect("dyn"), quick).await;
    check(
        again.success && server.held() == 1,
        &format!(
            "a connect to the active server asks for no key ({})",
            code(&again)
        ),
    );
    let other = service.ask_within(connect("dyn2"), quick).await;
    check(
        code(&other) == "busy" && server.held() == 1,
        &format!(
            "another server is busy before its key is fetched ({})",
            code(&other)
        ),
    );

    // 4. A switch keeps the old session until the new key is in: held, it keeps running; a key
    // that cannot be fetched changes nothing; one that can replaces it.
    let switching = service.ask_later(switch("dyn2"));
    check(server.wait_held(2), "a switch asked for the other key");
    let status = service.ask_within(Action::Status {}, quick).await;
    let profile = status
        .data
        .as_ref()
        .and_then(|d| d["profile"].as_str().map(String::from));
    check(
        profile.as_deref() == Some("dyn") && service.runs() == 1,
        &held_for(&format!("the old session still runs ({profile:?})")),
    );
    server.fail_next();
    server.release();
    let failed = switching.await.unwrap();
    check(
        code(&failed) == "invalid_profile",
        &format!(
            "a key that cannot be fetched is refused ({})",
            code(&failed)
        ),
    );
    check(
        service.status().await.profile.as_deref() == Some("dyn") && service.runs() == 1,
        "the old session was not stopped for it",
    );
    server.hold_next();
    let switching = service.ask_later(switch("dyn2"));
    check(server.wait_held(3), "the switch asked again");
    server.release();
    let switched = switching.await.unwrap();
    check(
        switched.success && service.wait_runs(2).await,
        &format!("the switch replaces the session ({})", code(&switched)),
    );
    check(
        service.status().await.profile.as_deref() == Some("dyn2"),
        "the new server is the active one",
    );
    let stopped = service.ask(Action::Disconnect {}).await;
    check(stopped.success, "disconnect after the switch");

    // 5. A disconnect while the key is fetched wins: it is answered at once, and so is the
    // connect, which starts nothing, although the server still holds its answer.
    server.hold_next();
    let given_up = service.ask_later(connect("dyn"));
    check(server.wait_held(4), "a connect asked for the key again");
    let stopped = service.ask_within(Action::Disconnect {}, quick).await;
    check(
        stopped.success,
        &held_for(&format!("disconnect answers at once ({})", code(&stopped))),
    );
    let answer = tokio::time::timeout(Duration::from_secs(10), given_up).await;
    let answer = answer.map(|joined| joined.unwrap());
    check(
        answer.as_ref().is_ok_and(|r| code(r) == "cancelled"),
        &format!(
            "the connect is given up ({})",
            answer.as_ref().map_or("no answer", |r| code(r))
        ),
    );
    server.release();
    check(
        service.status().await.status == ConnectionState::Disconnected && service.runs() == 2,
        "nothing started when the key arrived",
    );
    // The way is free for the next connection.
    let free = service.ask(connect("static")).await;
    check(
        free.success,
        &format!("a new connect is accepted ({})", code(&free)),
    );
    service.ask(Action::Disconnect {}).await;

    // 6. A server removed while its key was fetched is not started.
    server.hold_next();
    let removed = service.ask_later(connect("dyn"));
    check(server.wait_held(5), "a connect asked for the key once more");
    let gone = service
        .ask_within(Action::ProfileRemove { name: "dyn".into() }, quick)
        .await;
    check(
        gone.success,
        &held_for(&format!("the server can be removed ({})", code(&gone))),
    );
    server.release();
    let answer = removed.await.unwrap();
    check(
        code(&answer) == "not_found",
        &format!("the removed server is not started ({})", code(&answer)),
    );

    // 7. A server replaced while its key was fetched is not started either.
    let runs = service.runs();
    server.hold_next();
    let replaced = service.ask_later(connect("dyn2"));
    check(
        server.wait_held(6),
        "a connect asked for the key a last time",
    );
    let new_key = service
        .ask_within(
            import("dyn2", "ssconf://keys.example.com/third#Third", true),
            quick,
        )
        .await;
    check(
        new_key.success,
        &held_for(&format!("the server can be replaced ({})", code(&new_key))),
    );
    server.release();
    let answer = replaced.await.unwrap();
    check(
        code(&answer) == "busy",
        &format!("the replaced server is not started ({})", code(&answer)),
    );
    check(
        service.status().await.status == ConnectionState::Disconnected && service.runs() == runs,
        "no session was started for it",
    );
    failures
}
