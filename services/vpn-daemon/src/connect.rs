//! Connect and switch (code review 2026-11-25, M1). A dynamic `ssconf://` key is fetched from the
//! network (5 seconds of DNS and 10 of HTTPS at most), and no other request may wait that long
//! for the service. So a connect is made in three steps:
//!
//! 1. **Plan**, with the service lock: the profile, whether another server is active or another
//!    connection is being prepared, the cleanup barrier, the sealed key, the tunnel worker.
//! 2. **Fetch**, without the lock, only when the key needs the network. The attempt is registered
//!    (see [`Attempts`]) so that a second connect is `busy` meanwhile, and a disconnect gives it up.
//! 3. **Commit**, with the lock again: the attempt is still wanted, the server is still the one
//!    that was planned, nothing started or got busy, the cleanup barrier is still clear. Then the
//!    session starts.
//!
//! Every key that needs no network goes through the same steps without ever letting go of the lock.

use crate::{failure, Daemon};
use anyhow::Result;
use ghostlink_contracts::{Action, Request, Response};
use ghostlink_engine::{
    access::{self, Resolved},
    profile, storage, worker_session,
};
use std::{
    sync::{
        atomic::{AtomicU64, Ordering},
        Arc, Mutex as StdMutex, MutexGuard,
    },
    time::Duration,
};
use tokio::sync::{Mutex, Notify};

/// Actions served by [`handle`].
pub(crate) fn is_connect(action: &Action) -> bool {
    matches!(action, Action::Connect { .. } | Action::Switch { .. })
}

struct Attempt {
    id: u64,
    name: String,
    cancel: Arc<Notify>,
}

/// The connection that is being prepared while the service lock is not held, if any. The slot
/// has a lock of its own that is never held across an await, so it can be read and cleared
/// anywhere, also when a request is dropped or panics.
#[derive(Default)]
pub(crate) struct Attempts {
    slot: Arc<StdMutex<Option<Attempt>>>,
    last: AtomicU64,
}

fn locked(slot: &StdMutex<Option<Attempt>>) -> MutexGuard<'_, Option<Attempt>> {
    slot.lock().unwrap_or_else(|poisoned| poisoned.into_inner())
}

impl Attempts {
    /// The server a connection is being prepared for.
    fn in_flight(&self) -> Option<String> {
        locked(&self.slot).as_ref().map(|a| a.name.clone())
    }
    /// Registers an attempt unless one is already registered.
    fn begin(&self, name: &str) -> Option<Ticket> {
        let mut slot = locked(&self.slot);
        if slot.is_some() {
            return None;
        }
        let cancel = Arc::new(Notify::new());
        let id = self.last.fetch_add(1, Ordering::Relaxed) + 1;
        *slot = Some(Attempt {
            id,
            name: name.into(),
            cancel: cancel.clone(),
        });
        Some(Ticket {
            id,
            slot: self.slot.clone(),
            cancel,
        })
    }
    /// Gives up the attempt, if there is one: its request answers `cancelled` and starts nothing.
    pub(crate) fn cancel(&self) {
        if let Some(attempt) = locked(&self.slot).take() {
            attempt.cancel.notify_one();
        }
    }
}

/// The right of one request to finish the attempt it registered. Dropping it (the request ended
/// in any way) clears the attempt if it is still registered.
pub(crate) struct Ticket {
    id: u64,
    slot: Arc<StdMutex<Option<Attempt>>>,
    cancel: Arc<Notify>,
}

impl Ticket {
    /// Takes the attempt out of the slot. False when it was already taken: a disconnect gave it up.
    fn finish(&self) -> bool {
        let mut slot = locked(&self.slot);
        if slot.as_ref().is_some_and(|a| a.id == self.id) {
            *slot = None;
            true
        } else {
            false
        }
    }
    async fn cancelled(&self) {
        self.cancel.notified().await
    }
}

impl Drop for Ticket {
    fn drop(&mut self) {
        self.finish();
    }
}

/// What a connect is going to start, as the plan saw it.
pub(crate) struct Target {
    name: String,
    switching: bool,
    openvpn: bool,
    /// The saved key as it was sealed when planned: the commit starts nothing if it changed.
    sealed: Vec<u8>,
    content: String,
}

pub(crate) enum Planned {
    /// The request is answered; nothing is started.
    Answer(Response),
    /// Fetch what the key needs (a ticket: the network, so without the lock), then commit.
    Go(Target, Option<Ticket>),
}

fn busy(message: &str) -> Planned {
    Planned::Answer(Response::err("busy", message))
}

fn cancelled() -> Response {
    Response::err(
        "cancelled",
        "The connection was given up because of a disconnect",
    )
}

/// Parses the key, and fetches a dynamic one. No OpenVPN profile has anything to resolve.
async fn resolve(target: &Target) -> Option<Result<Resolved>> {
    if target.openvpn {
        None
    } else {
        Some(access::resolve(&target.content).await)
    }
}

impl Daemon {
    /// Connect or switch with the service lock held all the way, a dynamic key's fetch included:
    /// for the connection made when the service starts, which nothing waits for.
    pub(crate) async fn connect(
        &mut self,
        name: Option<String>,
        switching: bool,
    ) -> Result<Response> {
        match self.plan_connect(name, switching).await? {
            Planned::Answer(response) => Ok(response),
            Planned::Go(target, ticket) => {
                let resolved = resolve(&target).await;
                self.commit_connect(target, ticket, resolved).await
            }
        }
    }

    /// The answer of a connect to the server that is already running, or to another one while
    /// this is not a switch. Nothing is read or fetched for it: a dynamic key's host may be
    /// unreachable while the tunnel is up, and a key that cannot be read or fetched must not turn
    /// "busy" into "invalid".
    async fn running_answer(&self, name: &str, switching: bool, openvpn: bool) -> Option<Response> {
        if switching || self.session.is_none() {
            return None;
        }
        let status = self.state.read().await.status();
        let active = self
            .session
            .as_ref()
            .is_some_and(|s| crate::session_is_active(s.finished(), &status.status));
        if !active {
            return None;
        }
        if status.profile.as_deref() == Some(name) {
            return Some(Response::ok(status));
        }
        let what = if openvpn { "profile" } else { "server" };
        Some(Response::err(
            "busy",
            &format!("Another {what} is active; use switch"),
        ))
    }

    /// Step 1: everything that is decided before a key is fetched.
    pub(crate) async fn plan_connect(
        &self,
        name: Option<String>,
        switching: bool,
    ) -> Result<Planned> {
        let Some(name) = name.or_else(|| self.store.default_profile.clone()) else {
            return Ok(Planned::Answer(Response::err(
                "not_found",
                "No default VPN profile is configured",
            )));
        };
        let Some(stored) = self.store.profiles.get(&name) else {
            return Ok(Planned::Answer(Response::err(
                "not_found",
                "VPN profile does not exist",
            )));
        };
        let openvpn = stored.summary.kind == "openvpn";
        if let Some(answer) = self.running_answer(&name, switching, openvpn).await {
            return Ok(Planned::Answer(answer));
        }
        // One connection is prepared at a time, a switch included.
        if self.attempts.in_flight().is_some() {
            return Ok(busy(
                "A connection is already being prepared; wait for it or disconnect",
            ));
        }
        // Said before a key is fetched, which can take 15 seconds.
        if let Some(error) = self.cleanup_failure().await {
            return Ok(Planned::Answer(error));
        }
        let sealed = stored.sealed_content.clone();
        let content = String::from_utf8(storage::unseal(&sealed)?)?;
        // Every kind except OpenVPN runs in the managed tunnel worker.
        if !openvpn && !worker_session::worker_available(&self.args.sing_box).await {
            return Ok(Planned::Answer(Response::err(
                "engine_unavailable",
                "Install the pinned managed Ghostlink tunnel worker in the service installation",
            )));
        }
        // Only a dynamic key is fetched; every other key is read from its own text.
        let ticket = if !openvpn && content.trim().starts_with("ssconf://") {
            match self.attempts.begin(&name) {
                Some(ticket) => Some(ticket),
                None => {
                    return Ok(busy(
                        "A connection is already being prepared; wait for it or disconnect",
                    ))
                }
            }
        } else {
            None
        };
        Ok(Planned::Go(
            Target {
                name,
                switching,
                openvpn,
                sealed,
                content,
            },
            ticket,
        ))
    }

    /// Step 3: starts the session if everything planned still holds. `resolved` is what the key
    /// came to (None for OpenVPN).
    pub(crate) async fn commit_connect(
        &mut self,
        target: Target,
        ticket: Option<Ticket>,
        resolved: Option<Result<Resolved>>,
    ) -> Result<Response> {
        // A disconnect since the plan wins: it asked for no connection, and its request has been
        // answered already.
        if ticket.as_ref().is_some_and(|ticket| !ticket.finish()) {
            return Ok(cancelled());
        }
        let Target {
            name,
            switching,
            openvpn,
            sealed,
            content,
        } = target;
        // The server may have been removed or replaced while its key was fetched.
        match self.store.profiles.get(&name) {
            None => return Ok(Response::err("not_found", "VPN profile does not exist")),
            Some(stored) if stored.sealed_content != sealed => {
                return Ok(Response::err(
                    "busy",
                    "The server was changed while the connection was being prepared; connect again",
                ))
            }
            Some(_) => {}
        }
        // Another connection cannot have been planned meanwhile, but a session may be running.
        if let Some(answer) = self.running_answer(&name, switching, openvpn).await {
            return Ok(answer);
        }
        if openvpn {
            return self.commit_openvpn(name, content).await;
        }
        let outbound = match resolved {
            Some(Ok(resolved)) => resolved.outbound,
            _ => return Ok(Response::err(
                "invalid_profile",
                "The saved key is invalid, unsupported or its dynamic configuration is unavailable",
            )),
        };
        #[cfg(unix)]
        if outbound.full_tunnel() && !self.args.allow_full_tunnel {
            return Ok(Response::err("permission_denied", "Full tunnel requires the administrator to authorize route changes with --allow-full-tunnel after protecting SSH access"));
        }
        self.disconnect().await;
        // Looked at again after the old session ended (a switch) and after the time of the fetch.
        if let Some(error) = self.cleanup_failure().await {
            return Ok(error);
        }
        // A server's own override wins over the global split-tunnel policy; read now, so that a
        // change made while the key was fetched applies.
        let policy = self
            .store
            .profiles
            .get(&name)
            .and_then(|stored| stored.routing_override.clone())
            .unwrap_or_else(|| self.store.routing_policy.clone());
        self.session = Some(
            worker_session::start(
                &self.args.sing_box,
                &self.args.data_dir.join("run"),
                name,
                outbound,
                policy,
                // Management APIs of servers administered here stay outside the tunnel.
                self.store
                    .managed_servers
                    .values()
                    .map(|m| (m.host.clone(), m.port))
                    .collect(),
                self.state.clone(),
                self.events.clone(),
                Duration::from_secs(self.args.connection_timeout_seconds),
            )
            .await?,
        );
        Ok(Response::ok(self.state.read().await.status()))
    }

    async fn commit_openvpn(&mut self, name: String, content: String) -> Result<Response> {
        let profile = match profile::validate(&name, &content) {
            Ok(p) => p,
            Err(_) => {
                return Ok(Response::err(
                    "invalid_profile",
                    "Stored profile is invalid or its certificate is expired or not yet valid",
                ))
            }
        };
        #[cfg(unix)]
        if profile.summary.tunnel_mode == "full" && !self.args.allow_full_tunnel {
            return Ok(Response::err("permission_denied", "Full tunnel is disabled by service policy; the administrator must authorize route changes with --allow-full-tunnel after protecting SSH access"));
        }
        if !self.args.openvpn.is_absolute() || !self.args.openvpn.is_file() {
            return Ok(Response::err("engine_unavailable", "Install OpenVPN Community and configure the service with its absolute executable path"));
        }
        self.disconnect().await;
        if let Some(error) = self.cleanup_failure().await {
            return Ok(error);
        }
        let session = ghostlink_engine::management::start(
            &self.args.openvpn,
            &self.args.data_dir.join("run"),
            profile,
            self.state.clone(),
            &self.args.windows_driver,
            self.events.clone(),
            Duration::from_secs(self.args.connection_timeout_seconds),
        )
        .await?;
        self.session = Some(session);
        Ok(Response::ok(self.state.read().await.status()))
    }
}

/// Serves a connect or a switch. Unlike the other requests it lets go of the service lock while a
/// dynamic key is fetched, as the calls to a managed server do (see `remote::handle`).
pub(crate) async fn handle(daemon: &Mutex<Daemon>, request: Request) -> Response {
    let operation = request.action.name();
    let mut guard = daemon.lock().await;
    let actor = match guard.admit(&request) {
        Ok(actor) => actor,
        Err(response) => return response,
    };
    let audit_log = guard.audit_log.clone();
    let (name, switching) = match request.action {
        Action::Connect { profile } => (profile, false),
        Action::Switch { profile } => (Some(profile), true),
        _ => return Response::err("internal", "Not a connection action"),
    };
    let response = match guard.plan_connect(name, switching).await {
        Err(error) => failure(error),
        Ok(Planned::Answer(response)) => response,
        Ok(Planned::Go(target, None)) => {
            let resolved = resolve(&target).await;
            guard
                .commit_connect(target, None, resolved)
                .await
                .unwrap_or_else(failure)
        }
        Ok(Planned::Go(target, Some(ticket))) => {
            drop(guard);
            // A disconnect ends the wait at once; the commit checks it too, for the moment when
            // both happen together.
            let fetched = tokio::select! {
                resolved = resolve(&target) => Some(resolved),
                () = ticket.cancelled() => None,
            };
            match fetched {
                None => cancelled(),
                Some(resolved) => daemon
                    .lock()
                    .await
                    .commit_connect(target, Some(ticket), resolved)
                    .await
                    .unwrap_or_else(failure),
            }
        }
    };
    let outcome = response
        .error
        .as_ref()
        .map_or("success", |e| e.code.as_str());
    let _ = audit_log.record(&actor, operation, outcome);
    response
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::StoredProfile;
    use ghostlink_contracts::{ConnectionState, SCHEMA_VERSION};
    use ghostlink_engine::{netstate::Footprint, profile::ProfileSummary};

    const KEY: &str = "ss://aes-256-gcm:test-only-secret@203.0.113.9:8388";

    fn saved(name: &str, key: &str) -> StoredProfile {
        StoredProfile {
            summary: ProfileSummary {
                name: name.into(),
                kind: "outline".into(),
                display_name: name.into(),
                remotes: vec![],
                tunnel_mode: "full".into(),
                certificate_expires_unix: None,
                sha256: String::new(),
                is_default: false,
            },
            sealed_content: storage::seal(key.as_bytes()).unwrap(),
            routing_override: None,
        }
    }

    /// What the plan would have decided for a saved server.
    fn target(daemon: &Daemon, name: &str, switching: bool) -> Target {
        Target {
            name: name.into(),
            switching,
            openvpn: false,
            sealed: daemon.store.profiles[name].sealed_content.clone(),
            content: KEY.into(),
        }
    }

    async fn resolved() -> Option<Result<Resolved>> {
        Some(access::resolve(KEY).await)
    }

    fn disconnect_request() -> Request {
        Request {
            schema_version: SCHEMA_VERSION,
            agent_token: None,
            action: Action::Disconnect {},
        }
    }

    /// A connection that is being prepared is the only one: its ticket is the right to finish it,
    /// and a request that ends in any way gives the attempt up.
    #[test]
    fn one_attempt_at_a_time_and_a_dropped_ticket_clears_it() {
        let attempts = Attempts::default();
        let first = attempts.begin("home").expect("the first attempt");
        assert!(attempts.begin("office").is_none(), "one at a time");
        assert_eq!(attempts.in_flight().as_deref(), Some("home"));
        drop(first);
        assert!(attempts.in_flight().is_none(), "dropped: cleared");
        // A ticket that was cancelled cannot finish, and cannot clear its successor.
        let old = attempts.begin("home").unwrap();
        attempts.cancel();
        assert!(!old.finish());
        let next = attempts.begin("office").unwrap();
        assert!(!old.finish());
        drop(old);
        assert_eq!(attempts.in_flight().as_deref(), Some("office"));
        assert!(next.finish());
    }

    /// A disconnect gives the attempt up: the connect that already fetched its key starts nothing
    /// (this is the commit's own check; the wait for the fetch is also ended at once).
    #[tokio::test]
    async fn a_disconnect_during_the_fetch_wins_at_the_commit() {
        let mut daemon = crate::test_daemon("connect-cancel");
        daemon.args.allow_full_tunnel = true;
        daemon
            .store
            .profiles
            .insert("home".into(), saved("home", KEY));
        let ticket = daemon.attempts.begin("home").unwrap();
        let answer = daemon.handle(disconnect_request()).await;
        assert!(answer.success);
        assert!(daemon.attempts.in_flight().is_none());
        let answer = daemon
            .commit_connect(
                target(&daemon, "home", false),
                Some(ticket),
                resolved().await,
            )
            .await
            .unwrap();
        assert_eq!(answer.error.unwrap().code, "cancelled");
        assert!(daemon.session.is_none());
        let _ = std::fs::remove_dir_all(&daemon.args.data_dir);
    }

    /// A server that was removed or replaced while its key was fetched is not started.
    #[tokio::test]
    async fn a_server_removed_or_replaced_during_the_fetch_is_not_started() {
        let mut daemon = crate::test_daemon("connect-replaced");
        daemon.args.allow_full_tunnel = true;
        daemon
            .store
            .profiles
            .insert("home".into(), saved("home", KEY));
        let planned = target(&daemon, "home", false);
        // Replaced: the same name, another key. (Where the sealing keeps the text as it is, the
        // same key again would not show as a change, and need not.)
        let other = "ssconf://keys.example.com/other";
        daemon
            .store
            .profiles
            .insert("home".into(), saved("home", other));
        let ticket = daemon.attempts.begin("home");
        let answer = daemon
            .commit_connect(planned, ticket, resolved().await)
            .await
            .unwrap();
        assert_eq!(answer.error.unwrap().code, "busy");
        // Removed.
        let planned = target(&daemon, "home", false);
        daemon.store.profiles.remove("home");
        let ticket = daemon.attempts.begin("home");
        let answer = daemon
            .commit_connect(planned, ticket, resolved().await)
            .await
            .unwrap();
        assert_eq!(answer.error.unwrap().code, "not_found");
        assert!(daemon.session.is_none());
        let _ = std::fs::remove_dir_all(&daemon.args.data_dir);
    }

    async fn barrier(daemon: &Daemon) {
        let mut snapshot = daemon.state.write().await;
        snapshot.status.status = ConnectionState::Failed;
        snapshot.status.last_error = Some("network_cleanup_unverified".into());
        // This machine's own loopback address stands in for what an engine left behind.
        snapshot.footprint = Some(Footprint {
            interface: None,
            addresses: vec!["127.0.0.1".parse().unwrap()],
        });
    }

    /// The cleanup barrier is looked at before the key is fetched, and again before the session
    /// starts: a worker that ended with one while the key was fetched blocks the new session.
    #[tokio::test]
    async fn the_cleanup_barrier_is_checked_before_the_fetch_and_at_the_commit() {
        let mut daemon = crate::test_daemon("connect-barrier");
        daemon.args.allow_full_tunnel = true;
        // Not even a readable key is needed to be told that the cleanup is unverified.
        let mut garbage = saved("home", KEY);
        garbage.sealed_content = vec![1, 2, 3];
        daemon.store.profiles.insert("home".into(), garbage);
        barrier(&daemon).await;
        let planned = daemon
            .plan_connect(Some("home".into()), false)
            .await
            .unwrap();
        let Planned::Answer(answer) = planned else {
            panic!("the plan went on");
        };
        assert_eq!(answer.error.unwrap().code, "cleanup_unverified");
        // The barrier appears after the plan: the commit refuses.
        daemon
            .store
            .profiles
            .insert("home".into(), saved("home", KEY));
        let planned = target(&daemon, "home", false);
        let ticket = daemon.attempts.begin("home");
        let answer = daemon
            .commit_connect(planned, ticket, resolved().await)
            .await
            .unwrap();
        assert_eq!(answer.error.unwrap().code, "cleanup_unverified");
        assert!(daemon.session.is_none());
        let _ = std::fs::remove_dir_all(&daemon.args.data_dir);
    }

    /// While a connection is being prepared, a connect or a switch is busy, whichever server it is
    /// for, and a server that does not exist is still not found.
    #[tokio::test]
    async fn another_connect_is_busy_while_a_connection_is_prepared() {
        let mut daemon = crate::test_daemon("connect-busy");
        daemon
            .store
            .profiles
            .insert("home".into(), saved("home", KEY));
        daemon
            .store
            .profiles
            .insert("office".into(), saved("office", KEY));
        let _ticket = daemon.attempts.begin("home").unwrap();
        for (name, switching) in [("home", false), ("office", false), ("office", true)] {
            let planned = daemon
                .plan_connect(Some(name.into()), switching)
                .await
                .unwrap();
            let Planned::Answer(answer) = planned else {
                panic!("{name} {switching}: the plan went on");
            };
            assert_eq!(answer.error.unwrap().code, "busy", "{name} {switching}");
        }
        let planned = daemon
            .plan_connect(Some("nobody".into()), false)
            .await
            .unwrap();
        let Planned::Answer(answer) = planned else {
            panic!("the plan went on");
        };
        assert_eq!(answer.error.unwrap().code, "not_found");
        let _ = std::fs::remove_dir_all(&daemon.args.data_dir);
    }
}
