//! How an OpenVPN session ends when OpenVPN ends, through the real session code
//! (`management::start`; code review 2026-11-25, C3). OpenVPN is a stand-in: this test binary
//! itself, which acts as OpenVPN when it is started with `--config` (the test has its own `main`,
//! `harness = false`). It speaks the management protocol the service speaks with OpenVPN and
//! changes no network settings. A `fake-mode` file next to the profile says how it ends.
use ghostlink_engine::{
    management::{self, Shared},
    profile,
};
use std::{
    io::{BufRead, BufReader, Write},
    path::{Path, PathBuf},
    process::exit,
    time::Duration,
};

fn main() {
    let args: Vec<String> = std::env::args().collect();
    if args.get(1).map(String::as_str) == Some("--config") {
        openvpn(&args);
    } else {
        cases();
    }
}

fn value(args: &[String], name: &str, offset: usize) -> String {
    let at = args.iter().position(|a| a == name).expect("argument");
    args[at + offset].clone()
}

#[cfg(windows)]
fn connect(address: &str, port: &str) -> std::net::TcpStream {
    std::net::TcpStream::connect(format!("{address}:{port}")).expect("management connection")
}

#[cfg(unix)]
fn connect(address: &str, _port: &str) -> std::os::unix::net::UnixStream {
    std::os::unix::net::UnixStream::connect(address).expect("management connection")
}

/// The stand-in's OpenVPN: `--config FILE --management ADDRESS PORT AUTHFILE --management-client
/// --management-hold ...`.
fn openvpn(args: &[String]) {
    let config = PathBuf::from(value(args, "--config", 1));
    let mode = std::fs::read_to_string(config.with_file_name("fake-mode")).unwrap_or_default();
    let mut stream = connect(
        &value(args, "--management", 1),
        &value(args, "--management", 2),
    );
    let mut lines = BufReader::new(stream.try_clone().unwrap());
    let mut send = |text: &str| {
        stream.write_all(text.as_bytes()).unwrap();
        stream.flush().unwrap();
    };
    let mut next = || {
        let mut line = String::new();
        match lines.read_line(&mut line) {
            Ok(0) | Err(_) => None,
            Ok(_) => Some(line.trim().to_string()),
        }
    };
    // The service sends the management password first.
    let _password = next();
    send("SUCCESS: password is correct\r\n");
    while let Some(line) = next() {
        if line == "hold release" {
            break;
        }
    }
    let pause = || std::thread::sleep(Duration::from_millis(400));
    let states = [
        ">STATE:1700000000,CONNECTING,,,,,,\r\n",
        ">STATE:1700000001,ASSIGN_IP,,203.0.113.77,,,,\r\n",
        ">STATE:1700000002,ADD_ROUTES,,,,,,\r\n",
        ">STATE:1700000003,CONNECTED,SUCCESS,203.0.113.77,198.51.100.9,1194,,\r\n",
    ];
    match mode.trim() {
        // Gives up before it changed anything, as when its server cannot be reached.
        "give-up-early" => {
            send(states[0]);
            pause();
            exit(1)
        }
        // Brings its interface up, then dies.
        "crash-after-assign-ip" => {
            send(states[0]);
            send(states[1]);
            pause();
            exit(23)
        }
        "crash-when-connected" => {
            states.iter().for_each(|s| send(s));
            pause();
            exit(23)
        }
        // Ends by itself, but cleanly, after it was connected.
        "clean-exit-when-connected" => {
            states.iter().for_each(|s| send(s));
            pause();
            exit(0)
        }
        // Connected; leaves cleanly when the service asks it to stop.
        "stop" | "ignore-stop" => {
            states.iter().for_each(|s| send(s));
            while let Some(line) = next() {
                if line == "signal SIGTERM" && mode.trim() == "stop" {
                    exit(0)
                }
            }
            exit(0)
        }
        _ => exit(99),
    }
}

/// A valid inline profile with a fresh self-signed certificate (nothing is stored).
fn profile_text() -> String {
    let pair = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
    let cert = rcgen::CertificateParams::new(vec!["ghostlink-test".into()])
        .unwrap()
        .self_signed(&pair)
        .unwrap()
        .pem();
    let key = pair.serialize_pem();
    format!("client\ndev tun\nremote 192.0.2.10 1194\n<ca>\n{cert}</ca>\n<cert>\n{cert}</cert>\n<key>\n{key}</key>\n")
}

async fn connected(shared: &Shared) -> bool {
    for _ in 0..100 {
        if shared.read().await.status.status == ghostlink_contracts::ConnectionState::Connected {
            return true;
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    false
}

fn cases() {
    let runtime = tokio::runtime::Runtime::new().unwrap();
    let failures = runtime.block_on(async {
        let openvpn: &Path = &std::env::current_exe().unwrap();
        let mut failures = 0;
        // (mode, whether the service must be asked to stop it, how the session ends: the barrier
        // it ends with, "failed" for an end with no barrier and "clean" for none at all, and the
        // addresses the service must look for afterwards). Which of its reasons a session that
        // OpenVPN ended gives (the process, or its closed channel, noticed first) is a race
        // that does not matter; a barrier does.
        let cases: [(&str, bool, &str, &[&str]); 6] = [
            ("give-up-early", false, "failed", &[]),
            (
                "crash-after-assign-ip",
                false,
                "network_cleanup_unverified",
                &["203.0.113.77"],
            ),
            (
                "crash-when-connected",
                false,
                "network_cleanup_unverified",
                &["203.0.113.77"],
            ),
            ("clean-exit-when-connected", false, "failed", &[]),
            ("stop", true, "clean", &[]),
            // OpenVPN that does not leave when asked: killed after 10 seconds.
            (
                "ignore-stop",
                true,
                "forced_stop_network_cleanup_unverified",
                &["203.0.113.77"],
            ),
        ];
        for (mode, stop, expected, addresses) in cases {
            let dir = tempfile::tempdir().unwrap();
            let run_dir = dir.path().join("run");
            std::fs::create_dir(&run_dir).unwrap();
            std::fs::write(run_dir.join("fake-mode"), mode).unwrap();
            let shared = Shared::default();
            let (events, _received) = tokio::sync::mpsc::channel(64);
            let validated = profile::validate("test", &profile_text()).unwrap();
            let mut session = management::start(
                openvpn,
                &run_dir,
                validated,
                shared.clone(),
                "wintun",
                events,
                Duration::from_secs(20),
            )
            .await
            .unwrap();
            if stop {
                if !connected(&shared).await {
                    println!("FAIL {mode}: the stand-in never connected");
                    failures += 1;
                    session.stop().await;
                    continue;
                }
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
            let last_error = snapshot.status.last_error.as_deref();
            let found: Vec<String> = snapshot
                .footprint
                .iter()
                .flat_map(|f| f.addresses.iter().map(ToString::to_string))
                .collect();
            let outcome_ok = match (expected, last_error) {
                ("clean", error) => error.is_none(),
                ("failed", Some(error)) => !management::is_cleanup_barrier(error),
                (barrier, error) => error == Some(barrier),
            };
            let ok = outcome_ok
                && found == addresses
                && snapshot.footprint.iter().all(|f| f.interface.is_none());
            println!(
                "{} {mode}: last_error {last_error:?}, footprint {found:?}",
                if ok { "ok  " } else { "FAIL" }
            );
            failures += usize::from(!ok);
        }
        failures
    });
    if failures > 0 {
        exit(1);
    }
}
