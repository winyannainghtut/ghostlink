//! How a session ends when its tunnel worker ends, and how it supervises a worker that is running,
//! through the real session code (code review 2026-11-10, Rust finding 2; code review 2026-11-25,
//! M2). The worker is a stand-in: this test binary itself, which acts as the worker when started
//! with `version`, `check` or `run` (the test has its own `main`, `harness = false`). It changes
//! no network settings. A `fake-mode` file in each case's run directory says how the stand-in
//! behaves.
use ghostlink_engine::{access::Outbound, management::Shared, outline::AccessKey, worker_session};
use std::{
    io::{Read, Write},
    net::TcpListener,
    path::Path,
    process::exit,
    time::{Duration, Instant},
};

fn main() {
    let args: Vec<String> = std::env::args().collect();
    match args.get(1).map(String::as_str) {
        Some("version") => println!("{}", worker_session::WORKER_VERSION),
        Some("check") => {}
        Some("run") if args.len() == 4 => run(Path::new(&args[3])),
        _ => cases(),
    }
}

/// Waits for the stop command (or the end of its control pipe), then exits cleanly.
fn wait_for_stop() -> ! {
    let mut line = String::new();
    let _ = std::io::stdin().read_line(&mut line);
    exit(0)
}

/// The two loopback ports of the configuration, as the real worker would listen on them: the
/// service checks who owns them before it sends the secret. It then reports `started`.
fn start_listening(config: &Path) -> (TcpListener, TcpListener) {
    let document: serde_json::Value =
        serde_json::from_slice(&std::fs::read(config).unwrap()).unwrap();
    let proxy = document["inbounds"][0]["listen_port"].as_u64().unwrap();
    let api = document["experimental"]["clash_api"]["external_controller"]
        .as_str()
        .unwrap()
        .rsplit(':')
        .next()
        .unwrap()
        .parse::<u16>()
        .unwrap();
    let proxy = TcpListener::bind(("127.0.0.1", proxy as u16)).unwrap();
    let api = TcpListener::bind(("127.0.0.1", api)).unwrap();
    eprintln!(r#"{{"event":"started","vpn_ip":"172.28.85.1"}}"#);
    (proxy, api)
}

/// The stand-in worker's `run -c CONFIG`.
fn run(config: &Path) {
    let mode = std::fs::read_to_string(config.with_file_name("fake-mode")).unwrap_or_default();
    match mode.trim() {
        // Ends with an error before it could report `started`: a crash, or a kill.
        "exit-before-started" => exit(23),
        // Reports a failure after which it undid what it changed, then exits 1, as the real
        // worker does.
        "clean-failure" => {
            eprintln!(r#"{{"event":"failed","code":"engine_start_failed"}}"#);
            exit(1)
        }
        // Reports that it could not confirm its cleanup.
        "cleanup-failure" => {
            eprintln!(r#"{{"event":"failed","code":"engine_close_failed"}}"#);
            exit(1)
        }
        // Waits for the stop command (or the end of its control pipe), then exits cleanly: a
        // stop that comes while it is still starting.
        "stop" => wait_for_stop(),
        // Writes its process ID where the test can see it, and stays for a while without
        // watching its control pipe: a worker that was started and must not stay behind.
        "stay" => {
            std::fs::write(
                config.with_file_name("worker.pid"),
                std::process::id().to_string(),
            )
            .unwrap();
            std::thread::sleep(Duration::from_secs(12));
            exit(0)
        }
        // Started, and its health request is out when it ends with an error.
        "crash-during-health-check" => {
            let (proxy, _api) = start_listening(config);
            let _health_request = proxy.accept().unwrap();
            std::thread::sleep(Duration::from_millis(1200));
            exit(23)
        }
        // Started; refuses the health request at once, and its controller never answers.
        "stuck-controller" => {
            let (proxy, api) = start_listening(config);
            std::thread::spawn(move || {
                let mut held = vec![];
                while let Ok((stream, _)) = api.accept() {
                    held.push(stream);
                }
            });
            std::thread::spawn(move || {
                while let Ok((mut stream, _)) = proxy.accept() {
                    let _ =
                        stream.write_all(b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n");
                }
            });
            wait_for_stop()
        }
        // Started; while the health request is out it gives up its controller port (a worker
        // that crashed there, and the port goes to whoever binds it next), then refuses it.
        "lose-controller-during-health-check" => {
            let (proxy, api) = start_listening(config);
            let (mut health_request, _) = proxy.accept().unwrap();
            drop(api);
            std::fs::write(config.with_file_name("released"), "").unwrap();
            std::thread::sleep(Duration::from_millis(1500));
            let _ =
                health_request.write_all(b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n");
            wait_for_stop()
        }
        _ => exit(99),
    }
}

const BARRIERS: [&str; 2] = [
    "network_cleanup_unverified",
    "forced_stop_network_cleanup_unverified",
];

/// A session with the stand-in worker in `mode`, in a fresh run directory.
async fn start(
    run_dir: &Path,
    mode: &str,
    shared: &Shared,
) -> anyhow::Result<ghostlink_engine::management::Session> {
    std::fs::create_dir_all(run_dir).unwrap();
    std::fs::write(run_dir.join("fake-mode"), mode).unwrap();
    let (events, _received) = tokio::sync::mpsc::channel(16);
    let key = AccessKey::parse("ss://aes-256-gcm:test-only-secret@203.0.113.9:8388").unwrap();
    worker_session::start(
        &std::env::current_exe().unwrap(),
        run_dir,
        mode.into(),
        Outbound::Shadowsocks(key),
        Default::default(),
        vec![],
        shared.clone(),
        events,
        Duration::from_secs(20),
    )
    .await
}

fn report(ok: bool, what: &str) -> usize {
    println!("{} {what}", if ok { "ok  " } else { "FAIL" });
    usize::from(!ok)
}

/// A worker that ends while its health request is out is noticed at once: the request goes to
/// the worker's own proxy port, so it fails when the worker's sockets close, and the session does
/// not wait for the request's 8 seconds (code review 2026-11-25, M2 feared it did; it does not,
/// and this keeps it so).
async fn crash_during_a_health_check() -> usize {
    let dir = tempfile::tempdir().unwrap();
    let shared = Shared::default();
    let mut session = start(
        &dir.path().join("run"),
        "crash-during-health-check",
        &shared,
    )
    .await
    .unwrap();
    let began = Instant::now();
    let ended = tokio::time::timeout(Duration::from_secs(30), &mut session.task).await;
    let took = began.elapsed();
    let error = shared.read().await.status.last_error.clone();
    report(
        ended.is_ok() && took < Duration::from_secs(5) && error.is_some(),
        &format!("crash during a health check is seen after {took:?} ({error:?})"),
    )
}

/// A stop is not held up by a controller request that takes its 3 seconds.
async fn stop_during_a_controller_request() -> usize {
    let dir = tempfile::tempdir().unwrap();
    let shared = Shared::default();
    let mut session = start(&dir.path().join("run"), "stuck-controller", &shared)
        .await
        .unwrap();
    // Started, the health request refused, the controller request out and unanswered.
    tokio::time::sleep(Duration::from_millis(800)).await;
    let began = Instant::now();
    session.stop().await;
    let took = began.elapsed();
    report(
        took < Duration::from_millis(1000),
        &format!("stop during a controller request takes {took:?}"),
    )
}

/// The controller's secret goes only to the worker that owns the port: the port is checked again
/// just before the request, after the health request that may have taken seconds. The test binds
/// the port the worker gave up, and sees whether anything arrives.
async fn secret_goes_only_to_the_worker() -> usize {
    let dir = tempfile::tempdir().unwrap();
    let run_dir = dir.path().join("run");
    let shared = Shared::default();
    let mut session = start(&run_dir, "lose-controller-during-health-check", &shared)
        .await
        .unwrap();
    let released = run_dir.join("released");
    for _ in 0..200 {
        if released.exists() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    let document: serde_json::Value =
        serde_json::from_slice(&std::fs::read(run_dir.join("outline.json")).unwrap()).unwrap();
    let port: u16 = document["experimental"]["clash_api"]["external_controller"]
        .as_str()
        .unwrap()
        .rsplit(':')
        .next()
        .unwrap()
        .parse()
        .unwrap();
    let impostor = TcpListener::bind(("127.0.0.1", port)).unwrap();
    impostor.set_nonblocking(true).unwrap();
    let received = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
    let watcher = {
        let received = received.clone();
        std::thread::spawn(move || {
            let until = Instant::now() + Duration::from_secs(6);
            while Instant::now() < until {
                if let Ok((mut stream, _)) = impostor.accept() {
                    stream.set_nonblocking(false).unwrap();
                    stream
                        .set_read_timeout(Some(Duration::from_millis(500)))
                        .unwrap();
                    let mut bytes = [0u8; 2048];
                    let count = stream.read(&mut bytes).unwrap_or(0);
                    if String::from_utf8_lossy(&bytes[..count]).contains("Bearer") {
                        received.store(true, std::sync::atomic::Ordering::SeqCst);
                    }
                }
                std::thread::sleep(Duration::from_millis(20));
            }
        })
    };
    let ended = tokio::time::timeout(Duration::from_secs(30), &mut session.task).await;
    watcher.join().unwrap();
    let error = shared.read().await.status.last_error.clone();
    let leaked = received.load(std::sync::atomic::Ordering::SeqCst);
    report(
        ended.is_ok()
            && !leaked
            && error.as_deref() == Some("outline_engine_socket_owner_mismatch"),
        &format!("the controller secret stays with the worker (leaked {leaked}, {error:?})"),
    )
}

/// A start that is cancelled while it waits for the service state has not started a worker:
/// the worker is not killed when dropped, so one started before that wait would be left running.
async fn cancelled_start() -> usize {
    let dir = tempfile::tempdir().unwrap();
    let run_dir = dir.path().join("run");
    let shared = Shared::default();
    let held = shared.write().await;
    let starting = tokio::spawn({
        let (run_dir, shared) = (run_dir.clone(), shared.clone());
        async move { start(&run_dir, "stay", &shared).await.map(|_| ()) }
    });
    tokio::time::sleep(Duration::from_millis(2000)).await;
    starting.abort();
    let _ = starting.await;
    drop(held);
    tokio::time::sleep(Duration::from_millis(1000)).await;
    let pid = run_dir.join("worker.pid");
    let left = std::fs::read_to_string(&pid).ok();
    report(
        left.is_none(),
        &format!("a cancelled start leaves no worker behind ({left:?})"),
    )
}

fn cases() {
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let failures = runtime.block_on(async {
        let mut failures = 0;
        // (mode, whether the next connection must wait for an inspection)
        for (mode, barrier) in [
            ("exit-before-started", true),
            ("clean-failure", false),
            ("cleanup-failure", true),
            ("stop", false),
        ] {
            let dir = tempfile::tempdir().unwrap();
            let run_dir = dir.path().join("run");
            let shared = Shared::default();
            let mut session = start(&run_dir, mode, &shared).await.unwrap();
            if mode == "stop" {
                tokio::time::sleep(Duration::from_millis(300)).await;
                session.stop().await;
            } else if tokio::time::timeout(Duration::from_secs(30), &mut session.task)
                .await
                .is_err()
            {
                println!("FAIL {mode}: the session did not end");
                failures += 1;
                continue;
            }
            let snapshot = shared.read().await;
            let last_error = snapshot.status.last_error.clone();
            let blocked = last_error.as_deref().is_some_and(|e| BARRIERS.contains(&e));
            // A barrier comes with what the service looks for when it verifies that the cleanup
            // happened: the TUN interface and its addresses.
            let looks_for = snapshot.footprint.as_ref().is_some_and(|f| {
                f.interface.as_deref() == Some("Ghostlink") && f.addresses.len() == 2
            });
            let ok = blocked == barrier
                && (mode == "stop") == last_error.is_none()
                && looks_for == barrier;
            println!(
                "{} {mode}: last_error {last_error:?}",
                if ok { "ok  " } else { "FAIL" }
            );
            failures += usize::from(!ok);
        }
        failures += crash_during_a_health_check().await;
        failures += stop_during_a_controller_request().await;
        failures += secret_goes_only_to_the_worker().await;
        failures += cancelled_start().await;
        failures
    });
    if failures > 0 {
        exit(1);
    }
}
