//! Administration of managed Ghostlink servers. Management keys are sealed in service state and
//! never returned. Calls go to the network without holding the service lock, so a slow server
//! cannot stall status, connect or disconnect requests.

use crate::{Daemon, ManagedServer};
use anyhow::Result;
use ghostlink_contracts::{Action, Request, Response, ServerOp};
use ghostlink_engine::{
    profile,
    server_admin::{self, AdminError, ManagementKey},
    storage,
};
use serde_json::{json, Value};
use std::time::{SystemTime, UNIX_EPOCH};
use tokio::sync::Mutex;

const MAX_MANAGED_SERVERS: usize = 32;

/// Actions that reach a remote server and are served by [`handle`].
pub(crate) fn is_remote(action: &Action) -> bool {
    matches!(
        action,
        Action::ManagedServerAdd { .. } | Action::ServerCall { .. }
    )
}

enum Plan {
    Add {
        label: Option<String>,
        name: String,
        key: ManagementKey,
        replace: bool,
    },
    Call {
        server: String,
        key: ManagementKey,
        op: ServerOp,
        client: server_admin::AdminClient,
    },
}

fn outcome(response: &Response) -> &str {
    response
        .error
        .as_ref()
        .map(|e| e.code.as_str())
        .unwrap_or("success")
}

fn admin_error(error: AdminError) -> Response {
    let code = match &error {
        AdminError::Unreachable => "server_unreachable",
        AdminError::IdentityMismatch => "server_identity_mismatch",
        AdminError::NotGhostlink { .. } => "unsupported",
        AdminError::Rejected { status: 404, .. } => "not_found",
        AdminError::Rejected { status, .. } if *status < 500 => "invalid_request",
        AdminError::Rejected { .. } | AdminError::InvalidResponse => "server_error",
        AdminError::Invalid(_) => "invalid_request",
    };
    Response::err(code, &error.to_string())
}

/// The name an owner gave a server on this device: no control characters, trimmed, at most 64
/// characters. Any script is fine (the ID it is stored under is the one with limits).
fn clean_label(label: &str) -> String {
    label
        .chars()
        .filter(|c| !c.is_control())
        .take(64)
        .collect::<String>()
        .trim()
        .to_string()
}

fn text(value: &Value, limit: usize, fallback: &str) -> String {
    let text: String = value
        .as_str()
        .unwrap_or(fallback)
        .chars()
        .filter(|c| !c.is_control())
        .take(limit)
        .collect();
    if text.trim().is_empty() {
        fallback.into()
    } else {
        text
    }
}

pub(crate) async fn handle(daemon: &Mutex<Daemon>, request: Request) -> Response {
    let operation = request.action.name();
    // 1. Authorize, audit and read the sealed key while holding the lock only briefly. The audit
    // log has a lock of its own, so what is recorded at the end needs no hold on the service.
    let (actor, plan, audit_log) = {
        let guard = daemon.lock().await;
        let actor = match guard.authorize(&request) {
            Ok(actor) => actor,
            Err(response) => {
                let _ = guard.audit("unauthorized", operation, "denied");
                return response;
            }
        };
        if guard.audit(&actor, operation, "accepted").is_err() {
            return Response::err("internal", "Audit storage is unavailable");
        }
        match guard.plan(request.action) {
            Ok(plan) => (actor, plan, guard.audit_log.clone()),
            Err(response) => {
                let _ = guard.audit(&actor, operation, outcome(&response));
                return response;
            }
        }
    };
    // 2. The network call, unlocked.
    let response = match plan {
        Plan::Add {
            name,
            key,
            replace,
            label,
        } => match server_admin::probe(&key).await {
            Err(error) => admin_error(error),
            Ok(info) => daemon
                .lock()
                .await
                .finish_add(name, key, replace, label, &info)
                .unwrap_or_else(|_| Response::err("internal", "Service storage failed")),
        },
        Plan::Call {
            server,
            key,
            op,
            client,
        } => match client.call(&key, &op).await {
            Err(error) => admin_error(error),
            Ok(value) => match op {
                // 3. Saving a new key on this device needs the lock again.
                ServerOp::KeyCreate {
                    import_as: Some(local),
                    ..
                } => daemon.lock().await.import_issued(local, value).await,
                // The new key is saved here and never returned: management keys are write-only.
                ServerOp::RotateKey {} => {
                    daemon.lock().await.finish_rotation(&server, &key, &value)
                }
                _ => Response::ok(value),
            },
        },
    };
    // Not through the service lock: a connect that holds it for seconds would otherwise hold this
    // answer back, although the remote call is long done.
    let _ = audit_log.record(&actor, operation, outcome(&response));
    response
}

impl ManagedServer {
    fn key(&self) -> Result<ManagementKey> {
        Ok(serde_json::from_slice(&storage::unseal(&self.sealed_key)?)?)
    }
}

impl Daemon {
    fn plan(&self, action: Action) -> Result<Plan, Response> {
        match action {
            Action::ManagedServerAdd {
                name,
                management_key,
                replace,
                label,
            } => {
                if !profile::valid_name(&name) {
                    return Err(Response::err(
                        "invalid_request",
                        "Server ID must contain 1-64 letters, digits, hyphens or underscores",
                    ));
                }
                let exists = self.store.managed_servers.contains_key(&name);
                if exists && !replace {
                    return Err(Response::err("invalid_request", "A managed server with this ID already exists; choose another ID or explicitly replace it"));
                }
                if !exists && self.store.managed_servers.len() >= MAX_MANAGED_SERVERS {
                    return Err(Response::err(
                        "invalid_request",
                        "At most 32 managed servers may be saved",
                    ));
                }
                // Parse errors are fixed messages; they never echo the key.
                let key = ManagementKey::parse(&management_key)
                    .map_err(|e| Response::err("invalid_request", &e.to_string()))?;
                Ok(Plan::Add {
                    name,
                    key,
                    replace,
                    label: label.as_deref().map(clean_label),
                })
            }
            Action::ServerCall { server, op } => {
                let managed =
                    self.store.managed_servers.get(&server).ok_or_else(|| {
                        Response::err("not_found", "No managed server has this ID")
                    })?;
                let key = managed.key().map_err(|_| {
                    Response::err("internal", "The saved management key could not be read")
                })?;
                if let ServerOp::KeyCreate {
                    import_as: Some(local),
                    ..
                } = &op
                {
                    // Check before the server creates a key we could not save.
                    if !profile::valid_name(local) {
                        return Err(Response::err("invalid_request", "The local server ID must contain 1-64 letters, digits, hyphens or underscores"));
                    }
                    if self.store.profiles.contains_key(local) {
                        return Err(Response::err("invalid_request", "A VPN server with this ID already exists on this device; choose another ID"));
                    }
                    if self.store.profiles.len() >= 32 {
                        return Err(Response::err(
                            "invalid_request",
                            "At most 32 servers may be saved",
                        ));
                    }
                }
                // Reuse this server's client while its key is unchanged.
                let identity = format!("{}|{}", key.api_url, key.cert_sha256);
                let mut clients = self
                    .admin_clients
                    .lock()
                    .map_err(|_| Response::err("internal", "Service state is unavailable"))?;
                let client = match clients.get(&server) {
                    Some((known, client)) if *known == identity => client.clone(),
                    _ => {
                        let client = server_admin::AdminClient::new(&key).map_err(admin_error)?;
                        clients.insert(server.clone(), (identity, client.clone()));
                        client
                    }
                };
                Ok(Plan::Call {
                    server,
                    key,
                    op,
                    client,
                })
            }
            _ => Err(Response::err(
                "internal",
                "Not a server administration action",
            )),
        }
    }

    fn finish_add(
        &mut self,
        name: String,
        key: ManagementKey,
        replace: bool,
        label: Option<String>,
        info: &Value,
    ) -> Result<Response> {
        // Another request may have saved this ID, or other servers, while the server was being
        // checked.
        let exists = self.store.managed_servers.contains_key(&name);
        if exists && !replace {
            return Ok(Response::err(
                "invalid_request",
                "A managed server with this ID already exists; choose another ID or explicitly replace it",
            ));
        }
        if !exists && self.store.managed_servers.len() >= MAX_MANAGED_SERVERS {
            return Ok(Response::err(
                "invalid_request",
                "At most 32 managed servers may be saved",
            ));
        }
        let (host, port) = key.host_port();
        // Replacing a server's key without a new label keeps the name it already had here.
        let label = label
            .filter(|label| !label.is_empty())
            .or_else(|| {
                self.store
                    .managed_servers
                    .get(&name)
                    .map(|old| old.label.clone())
            })
            .unwrap_or_default();
        let managed = ManagedServer {
            sealed_key: storage::seal(&serde_json::to_vec(&key)?)?,
            host,
            port,
            label,
            server_name: text(&info["name"], 80, "Ghostlink server"),
            version: text(&info["version"], 40, ""),
            added_unix: SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs(),
        };
        let summary = managed.summary(&name);
        let mut next = self.store.clone();
        next.managed_servers.insert(name, managed);
        self.commit(next)?;
        let mut summary = summary;
        summary["protocols"] = info["protocols"].clone();
        Ok(Response::ok(summary))
    }

    /// Saves the management key a server just issued in place of the one it replaced, and
    /// answers without it. The server has changed its key by now, so whatever goes wrong here
    /// leaves this device with a key that no longer works: the answer says so, and how to add the
    /// new one.
    fn finish_rotation(&mut self, server: &str, old: &ManagementKey, answer: &Value) -> Response {
        // The saved key's client is for the old key, which no longer works.
        if let Ok(mut clients) = self.admin_clients.lock() {
            clients.remove(server);
        }
        match self.replace_key(server, old, answer) {
            Ok(summary) => Response::ok(json!({"rotated": true, "server": summary})),
            Err(why) => Response::err(
                "server_error",
                &format!("The server replaced its management key, but {why}. The key saved here no longer works. Run `sudo ghostlink-server show-key` on the server to print the new key, then add it again here with Update key."),
            ),
        }
    }

    /// Seals `answer`'s key into the entry of `server`, which keeps its name, label and address.
    /// Only the secret may differ from `old`: a key for another address or certificate is not
    /// the same server. The error is a reason that is safe to show (it never holds a key).
    fn replace_key(
        &mut self,
        server: &str,
        old: &ManagementKey,
        answer: &Value,
    ) -> Result<Value, &'static str> {
        let new = answer
            .get("management_key")
            .and_then(|key| serde_json::to_string(key).ok())
            .and_then(|text| ManagementKey::parse(&text).ok())
            .ok_or("its answer did not carry a usable new key")?;
        if new.host_port() != old.host_port() || new.cert_sha256 != old.cert_sha256 {
            return Err("its new key is for another address or certificate than the saved one");
        }
        let mut next = self.store.clone();
        let entry = next
            .managed_servers
            .get_mut(server)
            .ok_or("this server was removed from this device meanwhile")?;
        entry.sealed_key = serde_json::to_vec(&new)
            .map_err(anyhow::Error::from)
            .and_then(|bytes| storage::seal(&bytes))
            .map_err(|_| "the new key could not be saved on this device")?;
        let summary = entry.summary(server);
        self.commit(next)
            .map_err(|_| "the new key could not be saved on this device")?;
        Ok(summary)
    }

    /// Saves a key the server just issued as a VPN server on this device.
    async fn import_issued(&mut self, local: String, mut issued: Value) -> Response {
        let Some(access_key) = issued
            .get("access_key")
            .and_then(Value::as_str)
            .map(str::to_owned)
        else {
            return Response::err("server_error", "The server returned no access key");
        };
        match self.import_credential(local, access_key, false).await {
            Ok(response) if response.success => issued["imported"] = json!(response.data),
            // The key exists on the server either way; report it so it is not lost.
            Ok(response) => issued["import_error"] = json!(response.error.map(|e| e.message)),
            Err(_) => issued["import_error"] = json!("Service storage failed"),
        }
        Response::ok(issued)
    }
}

impl ManagedServer {
    pub(crate) fn summary(&self, name: &str) -> Value {
        let endpoint = format!("{}:{}", self.host, self.port);
        json!({
            "name": name,
            "endpoint": endpoint,
            "server_name": self.server_name,
            "label": self.label,
            "version": self.version,
            "added_unix": self.added_unix,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_key() -> ManagementKey {
        ManagementKey::parse(&format!(
            r#"{{"apiUrl":"https://192.0.2.1:8443/abcdefghijklmnop0123","certSha256":"{}"}}"#,
            "ab".repeat(32)
        ))
        .unwrap()
    }

    /// The name an owner types is kept as it is (any script, spaces, capitals), cleaned and cut to
    /// 64 characters; the ID it is saved under is a separate, limited thing.
    #[test]
    fn labels_are_cleaned() {
        assert_eq!(clean_label("  My home server  "), "My home server");
        assert_eq!(clean_label("ကျွန်ုပ်၏ ဆာဗာ"), "ကျွန်ုပ်၏ ဆာဗာ");
        assert_eq!(clean_label("a\u{0}b\nc\u{7}"), "abc");
        assert_eq!(clean_label("   "), "");
        assert_eq!(clean_label(&"é".repeat(100)).chars().count(), 64);
    }

    /// The label is saved with the server and shown in its summary; a server added without one
    /// shows none; replacing its key without a label keeps the one it had; a new label replaces it.
    #[test]
    fn the_name_given_on_this_device_is_kept() {
        let mut daemon = crate::test_daemon("server-label");
        let info = json!({"name": "Ghostlink server"});
        daemon
            .finish_add(
                "home".into(),
                test_key(),
                false,
                Some("My home".into()),
                &info,
            )
            .unwrap();
        daemon
            .finish_add("plain".into(), test_key(), false, None, &info)
            .unwrap();
        let summary =
            |daemon: &crate::Daemon, name: &str| daemon.store.managed_servers[name].summary(name);
        assert_eq!(summary(&daemon, "home")["label"], "My home");
        assert_eq!(summary(&daemon, "home")["server_name"], "Ghostlink server");
        assert_eq!(summary(&daemon, "plain")["label"], "");
        daemon
            .finish_add("home".into(), test_key(), true, None, &info)
            .unwrap();
        assert_eq!(
            summary(&daemon, "home")["label"],
            "My home",
            "a replaced key keeps its name"
        );
        daemon
            .finish_add(
                "home".into(),
                test_key(),
                true,
                Some("Home, Singapore".into()),
                &info,
            )
            .unwrap();
        assert_eq!(summary(&daemon, "home")["label"], "Home, Singapore");
        // An entry saved by an older version has no label field at all.
        let old: ManagedServer = serde_json::from_value(json!({
            "sealed_key": [], "host": "192.0.2.1", "port": 8443, "server_name": "Old", "added_unix": 1
        }))
        .unwrap();
        assert_eq!(old.summary("old")["label"], "");
        let _ = std::fs::remove_dir_all(&daemon.args.data_dir);
    }

    /// The outcome of a call to a server is recorded through the audit log's own lock: the answer
    /// does not wait for the service, which a connect may hold for many seconds, once the call is
    /// done (it used to take the service lock again just to write that line).
    #[tokio::test]
    async fn the_answer_of_a_remote_call_does_not_wait_for_the_service_lock() {
        use std::{sync::Arc, time::Duration};
        // A "server" that takes the connection and drops it a moment later: the call fails.
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        std::thread::spawn(move || {
            if let Ok((stream, _)) = listener.accept() {
                std::thread::sleep(Duration::from_millis(800));
                drop(stream);
            }
        });
        let key = format!(
            r#"{{"apiUrl":"https://127.0.0.1:{port}/abcdefghijklmnop0123","certSha256":"{}"}}"#,
            "ab".repeat(32)
        );
        let daemon = Arc::new(Mutex::new(crate::test_daemon("remote-lock")));
        let request = Request {
            schema_version: ghostlink_contracts::SCHEMA_VERSION,
            agent_token: None,
            action: Action::ManagedServerAdd {
                name: "home".into(),
                management_key: key,
                replace: false,
                label: None,
            },
        };
        let call = tokio::spawn({
            let daemon = daemon.clone();
            async move { handle(&daemon, request).await }
        });
        // The call is on its way: the service lock was needed only to plan it. Now something
        // else (a connect, say) holds the service for longer than the call takes.
        tokio::time::sleep(Duration::from_millis(250)).await;
        let held = daemon.lock().await;
        let response = tokio::time::timeout(Duration::from_secs(15), call)
            .await
            .expect("the answer waited for the service lock")
            .unwrap();
        assert!(!response.success);
        // The outcome is in the audit log all the same.
        let audit = std::fs::read_to_string(held.args.data_dir.join("audit.jsonl")).unwrap();
        assert!(
            audit.contains(r#""operation":"managed_server_add","outcome":"server_unreachable""#),
            "{audit}"
        );
        let dir = held.args.data_dir.clone();
        drop(held);
        let _ = std::fs::remove_dir_all(dir);
    }

    const OLD_SECRET: &str = "abcdefghijklmnop0123";
    const NEW_SECRET: &str = "zyxwvutsrqponmlk9876";

    /// What a server answers to `POST /server/rotate-key`.
    fn rotated(host: &str, port: u16, secret: &str, pin: &str) -> Value {
        json!({"management_key": {"apiUrl": format!("https://{host}:{port}/{secret}"), "certSha256": pin}})
    }

    fn saved_key(daemon: &crate::Daemon, name: &str) -> ManagementKey {
        daemon.store.managed_servers[name].key().unwrap()
    }

    fn with_server(name: &str) -> crate::Daemon {
        let mut daemon = crate::test_daemon(name);
        daemon
            .finish_add(
                "home".into(),
                test_key(),
                false,
                Some("My home".into()),
                &json!({"name": "Ghostlink server", "version": "0.9.1"}),
            )
            .unwrap();
        daemon
    }

    /// The key the server issued replaces the saved one, in the same entry (name, label, address
    /// kept), and the answer says that it happened without carrying any key: neither the new
    /// secret, nor the old one, nor the pin.
    #[test]
    fn a_rotated_key_replaces_the_saved_one_and_is_never_returned() {
        let mut daemon = with_server("rotate-ok");
        let old = saved_key(&daemon, "home");
        // The client of the old key is cached, as after any call to the server.
        let client = server_admin::AdminClient::new(&old).unwrap();
        let identity = format!("{}|{}", old.api_url, old.cert_sha256);
        daemon
            .admin_clients
            .lock()
            .unwrap()
            .insert("home".into(), (identity, client));
        let answer = rotated("192.0.2.1", 8443, NEW_SECRET, &"ab".repeat(32));
        let response = daemon.finish_rotation("home", &old, &answer);
        assert!(response.success, "{:?}", response.error);
        let text = serde_json::to_string(&response).unwrap();
        for secret in [NEW_SECRET, OLD_SECRET, &"ab".repeat(32), &"AB".repeat(32)] {
            assert!(!text.contains(secret), "the answer holds a secret: {text}");
        }
        let data = response.data.unwrap();
        assert_eq!(data["rotated"], true);
        assert_eq!(data["server"]["name"], "home");
        assert_eq!(data["server"]["label"], "My home");
        assert_eq!(data["server"]["endpoint"], "192.0.2.1:8443");
        // Saved, sealed, in the same entry.
        let saved = saved_key(&daemon, "home");
        assert!(
            saved.api_url.ends_with(NEW_SECRET),
            "the new secret is saved"
        );
        assert_eq!(saved.cert_sha256, old.cert_sha256);
        let entry = &daemon.store.managed_servers["home"];
        assert_eq!(entry.label, "My home");
        assert_eq!(entry.server_name, "Ghostlink server");
        assert_eq!(entry.version, "0.9.1");
        // The next call opens a client for the new key.
        assert!(daemon.admin_clients.lock().unwrap().is_empty());
        let _ = std::fs::remove_dir_all(&daemon.args.data_dir);
    }

    /// Only the secret may change. A new key for another host, port or certificate is not saved
    /// (the saved key is left as it was), and the answer says what to do; it carries no key.
    #[test]
    fn a_key_for_another_address_or_certificate_is_refused() {
        let mut daemon = with_server("rotate-refused");
        let old = saved_key(&daemon, "home");
        let pin = "ab".repeat(32);
        let cases = [
            ("another host", rotated("192.0.2.2", 8443, NEW_SECRET, &pin)),
            ("another port", rotated("192.0.2.1", 8444, NEW_SECRET, &pin)),
            (
                "another certificate",
                rotated("192.0.2.1", 8443, NEW_SECRET, &"cd".repeat(32)),
            ),
            ("no key", json!({})),
            ("a key that is no key", json!({"management_key": "x"})),
            (
                "a key with an unknown field",
                json!({"management_key": {"apiUrl": "https://192.0.2.1:8443/zyxwvutsrqponmlk9876", "certSha256": pin, "x": 1}}),
            ),
            ("a short secret", rotated("192.0.2.1", 8443, "short", &pin)),
        ];
        for (what, answer) in cases {
            let response = daemon.finish_rotation("home", &old, &answer);
            assert!(!response.success, "{what}");
            let error = response.error.unwrap();
            assert!(
                error.message.contains("sudo ghostlink-server show-key")
                    && error.message.contains("Update key")
                    && error.message.contains("replaced its management key"),
                "{what}: {}",
                error.message
            );
            assert!(!error.message.contains(NEW_SECRET), "{what}");
            assert_eq!(saved_key(&daemon, "home"), old, "{what}: the saved key");
        }
        // The same host written another way is the same host: the case of a name, a default port.
        let same = json!({"management_key": {"apiUrl": format!("https://192.0.2.1:8443/{NEW_SECRET}"), "certSha256": "ab".repeat(32).to_uppercase()}});
        assert!(daemon.finish_rotation("home", &old, &same).success);
        let _ = std::fs::remove_dir_all(&daemon.args.data_dir);
    }

    /// If the new key cannot be saved here, or the server was forgotten meanwhile, the answer is an
    /// error that says the server replaced its key and how to get it: the old one no longer works.
    #[test]
    fn a_key_that_cannot_be_saved_says_how_to_get_the_new_one() {
        let mut daemon = with_server("rotate-unsaved");
        let old = saved_key(&daemon, "home");
        let answer = rotated("192.0.2.1", 8443, NEW_SECRET, &"ab".repeat(32));
        // state.json cannot be replaced: it is a directory.
        let state = daemon.args.data_dir.join("state.json");
        let _ = std::fs::remove_file(&state);
        std::fs::create_dir(&state).unwrap();
        let response = daemon.finish_rotation("home", &old, &answer);
        let error = response.error.expect("the save failed");
        assert!(
            error.message.contains("could not be saved")
                && error.message.contains("sudo ghostlink-server show-key"),
            "{}",
            error.message
        );
        assert_eq!(saved_key(&daemon, "home"), old, "nothing half-saved");
        std::fs::remove_dir(&state).unwrap();
        // Forgotten while the server was being called.
        daemon.store.managed_servers.clear();
        let response = daemon.finish_rotation("home", &old, &answer);
        let error = response.error.expect("no entry");
        assert!(
            error.message.contains("removed from this device")
                && error.message.contains("show-key"),
            "{}",
            error.message
        );
        let _ = std::fs::remove_dir_all(&daemon.args.data_dir);
    }

    /// The request for the operation has no body and one fixed path (see `server_admin`), and an
    /// agent token cannot ask for it (see the contracts).
    #[test]
    fn rotating_the_key_is_an_owner_request() {
        let op = ServerOp::RotateKey {};
        assert_eq!(op.name(), "server_rotate_key");
        let request = Request {
            schema_version: ghostlink_contracts::SCHEMA_VERSION,
            agent_token: None,
            action: Action::ServerCall {
                server: "home".into(),
                op,
            },
        };
        assert!(is_remote(&request.action));
        assert!(!request.action.agent_allowed(true));
    }

    #[test]
    fn adds_that_overlap_cannot_pass_the_server_limit() {
        // Both adds were planned while 31 servers were saved; the first one filled the list.
        let mut daemon = crate::test_daemon("server-limit");
        for n in 0..MAX_MANAGED_SERVERS {
            daemon.store.managed_servers.insert(
                format!("s{n}"),
                ManagedServer {
                    sealed_key: vec![],
                    host: "192.0.2.1".into(),
                    port: 8443,
                    server_name: "test".into(),
                    label: String::new(),
                    version: String::new(),
                    added_unix: 0,
                },
            );
        }
        let key = || {
            ManagementKey::parse(&format!(
                r#"{{"apiUrl":"https://192.0.2.1:8443/abcdefghijklmnop0123","certSha256":"{}"}}"#,
                "ab".repeat(32)
            ))
            .unwrap()
        };
        let late = daemon
            .finish_add("late".into(), key(), false, None, &json!({}))
            .unwrap();
        assert_eq!(late.error.unwrap().code, "invalid_request");
        assert_eq!(daemon.store.managed_servers.len(), MAX_MANAGED_SERVERS);
        // Replacing a saved server is still possible at the limit.
        assert!(
            daemon
                .finish_add("s0".into(), key(), true, None, &json!({}))
                .unwrap()
                .success
        );
        let _ = std::fs::remove_dir_all(&daemon.args.data_dir);
    }
}
