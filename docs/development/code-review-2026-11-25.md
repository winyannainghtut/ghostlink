# Ghostlink v0.9.0 Full Codebase Review

**Review date:** November 25, 2026 (Asia/Singapore)  
**Reviewed version:** v0.9.0  
**Reviewed commit:** `7d419cddda6b8300b512a1606a9c1524774e3be6`  
**Scope:** Rust workspace & desktop shell (`core/vpn-engine`, `services/vpn-daemon`, `cli/ghostlink`, `packages/shared-contracts`, `integrations/mcp-server`, `apps/desktop/src-tauri`), Go worker/server/mesh/IPsec (`core/outline-worker`), Linux & Windows packaging/installers/CI (`packages/`, `scripts/`, `.github/`), and frontend UI/UX summary referencing [ui-ux-review-2026-11-25.md](ui-ux-review-2026-11-25.md).

This report records a static codebase audit and local verification pass on Windows. No production source files were modified.

---

## Summary at a Glance

| ID | Priority | Subsystem | Finding / Opportunity | Primary Locations |
| --- | --- | --- | --- | --- |
| C1 | P1 | Linux Host Scripts | Docker volume mount TOCTOU, unchecked root `$STATE` reads, unsafe `save_private` cleanup & missing sysctl/swap rollbacks | [ghostlink-server.sh](../../packages/linux/server/ghostlink-server.sh#L324-L335), [install-server.sh](../../scripts/install-server.sh#L392-L402) |
| C2 | P1 | Windows ACL & IPC | Pre-ACL file opens (`atomic_write`, `Grant`, `save_download`), dangling-symlink/Unix dir ordering, `pbData`/`cbData == 0`, pipe `GRGW` & accept handle leak | [storage.rs](../../core/vpn-engine/src/storage.rs#L39-L136), [ipc.rs](../../services/vpn-daemon/src/ipc.rs#L25-L104), [src-tauri main.rs](../../apps/desktop/src-tauri/src/main.rs#L98-L107) |
| C3 | P1 | Tunnel State Machine | Unrecoverable `cleanup_unverified` barrier blocks `Disconnect`; OpenVPN abnormal exit misses cleanup barrier | [vpn-daemon main.rs](../../services/vpn-daemon/src/main.rs#L333-L337), [management.rs](../../core/vpn-engine/src/management.rs#L256-L300) |
| C4 | P1 | IPsec Host Firewall | `Agent.cycle` calls `EnsureJumps` instead of `Apply`, failing to restore flushed/reloaded firewall chains | [ipsecagent/agent.go](../../core/outline-worker/internal/ipsecagent/agent.go#L382-L404), [firewall.go](../../core/outline-worker/internal/ipsecagent/firewall.go#L190-L200), [hostfw.go](../../core/outline-worker/internal/hostfw/hostfw.go#L151-L171) |
| C5 | P1 | Go Server API | Concurrent `rotate-key` calls can desync in-memory `a.secret` from `state.json` and `management-key.json` | [api.go](../../core/outline-worker/internal/server/api.go#L262-L286) |
| C6 | P1 | Go Server ASN | Off/on download cancellation & retry-window races; `operatorOf` `count == maxOperator` off-by-one bypasses 60-char cap | [server/asn.go](../../core/outline-worker/internal/server/asn.go#L112-L276), [asn/asn.go](../../core/outline-worker/internal/asn/asn.go#L198-L227) |
| C7 | P1 | Go Server History | `Monitor.Flush` drops `m.mu` before `m.historyMu`, letting concurrent `Monitor.Forget` deletions be resurrected | [history.go](../../core/outline-worker/internal/server/history.go#L269-L368) |
| C8 | P1 | Mesh Server Lifecycle | `MeshServer.Apply` 3s shutdown deadlock with `sync`/`OnChange`, unlocked state reads, listeners closed before `<-accounted` | [server/mesh.go](../../core/outline-worker/internal/server/mesh.go#L95-L119), [dashboard.go](../../core/outline-worker/internal/server/dashboard.go#L63), [ghostlink-server main.go](../../core/outline-worker/cmd/ghostlink-server/main.go#L228-L230) |
| M1 | P2 | Rust Daemon Locking | `connect()` unseals & fetches `ssconf://` before `busy` check; `remote::handle` re-locks `daemon` only for `.audit` | [vpn-daemon main.rs](../../services/vpn-daemon/src/main.rs#L361-L437), [remote.rs](../../services/vpn-daemon/src/remote.rs#L139-L142) |
| M2 | P2 | Worker Supervision | `kill_on_drop(false)` orphan risk, 8s `health.get` `select!` omits `child.wait()`/logs, unverified `controller.get` outside `select!` | [worker_session.rs](../../core/vpn-engine/src/worker_session.rs#L351-L453) |
| M3 | P2 | Protocol & Split Tunnel | `SplitMode::Only` `www.gstatic.com` bootstrap leak, AWG CRLF/`[interface]` DNS drop, WG `+4`/tab/IPv6 parsing, OpenVPN & VLESS edges | [split.rs](../../core/vpn-engine/src/split.rs#L127-L143), [amnezia.rs](../../core/vpn-engine/src/amnezia.rs#L193-L197), [wireguard.rs](../../core/vpn-engine/src/wireguard.rs#L128-L354), [profile.rs](../../core/vpn-engine/src/profile.rs#L191-L266), [vless.rs](../../core/vpn-engine/src/vless.rs#L144-L161) |
| M4 | P2 | Client Contracts | Desktop `30s` vs `75s` IPC timeout; CLI `16 KiB` vs `64 KiB` secret limit; CLI strips `access_key` on `import_error` | [src-tauri main.rs](../../apps/desktop/src-tauri/src/main.rs#L35), [cli main.rs](../../cli/ghostlink/src/main.rs#L922), [cli main.rs](../../cli/ghostlink/src/main.rs#L1620-L1625) |
| M5 | P2 | Mesh / Server Config | Disabled `Mesh.Port`/`IPsec.DNSPort` missing from `Listening()`; `PATCH /mesh {"cidr": ...}` implicitly enables Mesh | [server/mesh.go](../../core/outline-worker/internal/server/mesh.go#L59-L74), [render.go](../../core/outline-worker/internal/server/render.go#L378-L485), [api.go](../../core/outline-worker/internal/server/api.go#L422-L446) |
| M6 | P2 | Mesh Agent Recovery | Clean start into `StatusPending`/`StatusDisabled` after unclean exit skips `teardown()`, leaving `glmesh0`/routes active | [meshagent/agent.go](../../core/outline-worker/internal/meshagent/agent.go#L140-L145) |
| M7 | P2 | Mesh Routing & ACLs | `ValidEndpoint` allows loopback/link-local/`100.96.x.x`; `LocalEndpoints` misses container bridges; `setRoutes` misses coordinator IP & `RT_TABLE_MAIN` filter | [meshproto.go](../../core/outline-worker/internal/meshproto/meshproto.go#L236-L242), [node.go](../../core/outline-worker/internal/meshagent/node.go#L205-L246), [device_linux.go](../../core/outline-worker/internal/meshagent/device_linux.go#L104-L156) |
| M8 | P2 | Server/Mesh/AWG Edges | `Live.Record` leak & >3m clock-skew sensitivity; `ghostlink-mesh leave` vs `serve` race; stale `EngineError`; VICI `ticks`; `awg` shutdown sleep/lock | [live.go](../../core/outline-worker/internal/mesh/live.go#L139-L252), [ghostlink-mesh main.go](../../core/outline-worker/cmd/ghostlink-mesh/main.go#L94-L99), [service.go](../../core/outline-worker/internal/server/service.go#L309-L423), [awg/bind.go](../../core/outline-worker/internal/awg/bind.go#L68-L133) |
| M9 | P2 | Packaging Versions | `~rc.1` vs `-rc.1` Linux filename drift; MSI `Product/@Version` rejects SemVer pre-releases; stale `Go 1.26.10` doc | [build-linux.sh](../../scripts/build-linux.sh#L102-L104), [Ghostlink.wxs](../../packages/windows/Ghostlink.wxs#L3), [package-worker-source.ps1](../../scripts/package-worker-source.ps1#L76) |
| M10 | P2 | Installers & Packages | `install-server.sh` `glmesh0` zone/late `open_ports`/`pipefail`; `install-mesh.sh` port reset; RPM deps & purge; stale MSI stage & dead SID guard; unpinned Alpine | [install-server.sh](../../scripts/install-server.sh#L301-L485), [install-mesh.sh](../../scripts/install-mesh.sh#L103-L120), [ghostlink-server.nfpm.yaml](../../packages/linux/server/ghostlink-server.nfpm.yaml#L46-L52), [build-windows.ps1](../../scripts/build-windows.ps1#L25-L31), [ipsec.Dockerfile](../../packages/linux/server/ipsec.Dockerfile#L8) |
| M11 | P3 | CI & Test Coverage | Floating `@v4`/`@v5` tags, `curl \| sh` `rustup`, `--no-audit` vs `audit-lockfiles.py`, `release.yml` omitting mesh/IPsec suites, inspect-only RPMs | [linux-package.yml](../../.github/workflows/linux-package.yml#L37), [ui.yml](../../.github/workflows/ui.yml#L22), [control-plane.test.mjs](../../tests/integration/control-plane.test.mjs#L299) |
| UI | P1–P3 | Frontend & Desktop UX | Stacked navbars, `1060px` wrap, `!loaded` flash, `busy` locks, `AreaChart` overlap, missing server/mesh controls, tray/logs, accessibility | [ui-ux-review-2026-11-25.md](ui-ux-review-2026-11-25.md) |

---

## 1. Critical & High-Priority Findings (`C1`–`C8`)

### C1 — Host script TOCTOU, unguarded root `$STATE` reads, and missing rollbacks

- **Locations:** [ghostlink-server.sh](../../packages/linux/server/ghostlink-server.sh#L182-L188), [ghostlink-server.sh](../../packages/linux/server/ghostlink-server.sh#L324-L406), [ghostlink-server.sh](../../packages/linux/server/ghostlink-server.sh#L450-L465), [ghostlink-server.sh](../../packages/linux/server/ghostlink-server.sh#L763), [install-server.sh](../../scripts/install-server.sh#L254), [install-server.sh](../../scripts/install-server.sh#L392-L402), [install-server.sh](../../scripts/install-server.sh#L455-L470)
- **Observation:**
  - `ipsec_start` validates `in_state_dir ipsec rm -f status.json` inside a subshell, exits the subshell, and then passes `--volume "$STATE/ipsec:$STATE/ipsec"` to `docker run --user "0:$gid"`.
  - Multiple root helper functions read `$STATE/state.json`, `management-key.json`, and `ipsec/status.json` directly instead of validating via `in_state_dir`.
  - `save_private` checks `-e`/`-L`, enables `set -C`, and runs `rm -f "$output"` on any failure.
  - `ipsec_command enable` writes `$SYSCTL_FILE` and sets `ip_forward=1` before `ipsec_start` runs; `add_swap_file` creates/activates swap without cleanup on failure or Btrfs `chattr +C`.
- **Impact:** A compromised `ghostlink-server` user (owning `$STATE`) can swap `$STATE/ipsec` or state files for symlinks between validation and privileged root/Docker access, or trigger deletion of an existing file via `save_private` failure handling.
- **Recommendation:** Open/bind files using verified directory file descriptors rather than TOCTOU shell path checks, fail closed if `$STATE` subdirectories are symlinks, only remove `$output` if `save_private` itself created it, and add rollback traps for `ipsec_command enable` and `add_swap_file`.

### C2 — Windows ACL TOCTOU, unsafe `save_download`, `pbData` slice guard, and pipe DACL/accept edge

- **Locations:** [storage.rs](../../core/vpn-engine/src/storage.rs#L39-L63), [storage.rs](../../core/vpn-engine/src/storage.rs#L123-L136), [storage.rs](../../core/vpn-engine/src/storage.rs#L309), [cli/main.rs](../../cli/ghostlink/src/main.rs#L790-L803), [cli/main.rs](../../cli/ghostlink/src/main.rs#L1374-L1386), [src-tauri/main.rs](../../apps/desktop/src-tauri/src/main.rs#L98-L107), [ipc.rs](../../services/vpn-daemon/src/ipc.rs#L25), [ipc.rs](../../services/vpn-daemon/src/ipc.rs#L91-L104)
- **Observation:**
  - `atomic_write`, `AgentCommand::Grant`, and `write_private` open destination files with default inherited ACLs before calling `storage::restrict_acl`. In `AgentCommand::Grant`, failing during file write leaves an empty `token_file` while the token already exists in `ghostlinkd`.
  - Desktop `save_download` creates `.html`, `.mobileconfig`, and `.conf` exports without `0o600` on Unix or `restrict_acl` on Windows.
  - `private_dir` calls `path.exists()` (false on dangling symlinks) before checking `symlink_metadata`, and creates Unix directories before setting `0o700`. Cryptographic unprotect constructs `std::slice::from_raw_parts(output.pbData, output.cbData as usize)` without checking `output.cbData == 0`.
  - The Windows named pipe DACL specifies `D:P(A;;GA;;;SY)(A;;GRGW;;;{owner})` (`GENERIC_WRITE` on a `SYSTEM`-owned pipe includes `FILE_CREATE_PIPE_INSTANCE`), and `Listener::accept` retains the connected `NamedPipeServer` in `self.pipe` if `create(&self.endpoint, &self.owner, false)?` fails.
- **Impact:** Brief permission windows on secret material (keys, tokens, exported client configs), possible stalled accept state on Windows IPC pipe creation errors, and UB risk if DPAPI returns zero length.
- **Recommendation:** Create Windows files/directories with an explicit restricted `SECURITY_ATTRIBUTES` SD at open/create time (and `OpenOptionsExt::mode(0o600)` / `DirBuilderExt::mode(0o700)` on Unix), validate `output.cbData > 0`, replace `GRGW` with `FILE_GENERIC_READ | FILE_GENERIC_WRITE` without `FILE_CREATE_PIPE_INSTANCE`, and swap out `self.pipe` before creating the next instance.

### C3 — Unrecoverable `cleanup_unverified` barrier and OpenVPN crash bypass

- **Locations:** [vpn-daemon/main.rs](../../services/vpn-daemon/src/main.rs#L333-L337), [vpn-daemon/main.rs](../../services/vpn-daemon/src/main.rs#L391-L393), [vpn-daemon/main.rs](../../services/vpn-daemon/src/main.rs#L438-L440), [vpn-daemon/main.rs](../../services/vpn-daemon/src/main.rs#L644-L655), [management.rs](../../core/vpn-engine/src/management.rs#L256-L300)
- **Observation:**
  - `Daemon::cleanup_failure` rejects both `connect()` and `Action::Disconnect` whenever `status.last_error` is `"forced_stop_network_cleanup_unverified"` or `"network_cleanup_unverified"`, with no IPC path to clear `last_error`.
  - Conversely, OpenVPN `management::start` sets `"forced_stop_network_cleanup_unverified"` only on a 10s wait timeout; an immediate OpenVPN exit after hold release leaves `reason` as `"engine_exited"` (or `None` during `requested_stop`).
- **Impact:** Once hit, `cleanup_unverified` wedges `ghostlinkd` until the OS service is restarted (`Disconnect` cannot reset it), whereas an OpenVPN crash mid-session fails to trigger the safety barrier when routes/DNS may remain dirty.
- **Recommendation:** Add an explicit acknowledged reset action (or allow verified cleanup retry on `Disconnect`) and treat unexpected non-zero/mid-session OpenVPN termination after interface bring-up as requiring cleanup verification.

### C4 — IPsec agent never restores flushed or reloaded host firewall chains

- **Locations:** [ipsecagent/agent.go](../../core/outline-worker/internal/ipsecagent/agent.go#L382-L404), [ipsecagent/firewall.go](../../core/outline-worker/internal/ipsecagent/firewall.go#L190-L200), [hostfw/hostfw.go](../../core/outline-worker/internal/hostfw/hostfw.go#L151-L171)
- **Observation:** `Agent.cycle` executes `a.FW.EnsureJumps` (`iptables -C` / `-I 1`) every 30s and only runs `a.FW.Apply` (`hostfw.Restore`) when `desired.json` or `limitRules` change. Errors from `EnsureJumps` are not surfaced in `status.json`.
- **Impact:** Any host firewall reload or flush (`iptables -F`, `firewalld --reload`, `ufw reload`) wipes IPsec isolation, NAT, and DNS redirect chains silently; 30s repair fails until `desired.json` changes or the container restarts.
- **Recommendation:** Check chain existence / `EnsureJumps` health in `Agent.cycle`, trigger `a.FW.Apply` automatically whenever chains are missing or jump repair fails, and surface firewall repair failures into `status.json`.

### C5 — Concurrent `rotate-key` secret desync

- **Locations:** [api.go](../../core/outline-worker/internal/server/api.go#L262-L286)
- **Observation:** `rotateKey` commits `s.API.Secret` via `Change`, then updates `a.setSecret(secret)` outside `store.Lock()` before re-locking to call `WriteManagementKey()`.
- **Impact:** Two overlapping `rotate-key` calls can finish in opposite orders, leaving `a.secret` authenticating with the older key while `state.json` and `management-key.json` hold the newer key.
- **Recommendation:** Serialize `rotateKey` (or invoke `a.setSecret` and `WriteManagementKey` under one lock/change sequence so in-memory auth and persisted files update atomically in the same order).

### C6 — ASN download lifecycle races and 60-character operator cap bypass

- **Locations:** [service.go](../../core/outline-worker/internal/server/service.go#L69-L76), [service.go](../../core/outline-worker/internal/server/service.go#L144-L153), [server/asn.go](../../core/outline-worker/internal/server/asn.go#L112-L130), [server/asn.go](../../core/outline-worker/internal/server/asn.go#L161-L276), [api.go](../../core/outline-worker/internal/server/api.go#L959-L969), [asn/asn.go](../../core/outline-worker/internal/asn/asn.go#L198-L227)
- **Observation:**
  - Disabling ASN calls `Drop()` (`m.generation++`, removes files) without cancelling an active HTTP download; startup `Load()` stores `m.table.Store` without re-checking `ASNLookup`/generation; rapid off→on toggles while `m.busy` remain exposed to an aborted download populating `m.failed` without resetting `m.tried` (deferring auto-retry by the 6-hour `asnRetryAfter`). `RefreshASN` and `asnUpdate` read `ASNLookup` outside `store.Lock()`.
  - `asn.operatorOf` terminates truncation on `if count == maxOperator` (`60`). When whitespace collapses at `count == 59`, `count` advances from `59` to `61`, skipping `count == 60`.
- **Impact:** Stale or disabled ASN data can reappear in memory, automatic downloads can stall for 6 hours, and attacker/malformed RIR strings with a space at column 59 bypass the 60-character cap up to `MaxLine` (`4096`).
- **Recommendation:** Tie every download to a cancellable context and `generation` token checked under lock before `Store`/`Drop`, reset `m.tried` on re-enable, and change `count == maxOperator` to `count >= maxOperator`.

### C7 — Deleted-user history resurrection (`Monitor.Flush` vs `Monitor.Forget`)

- **Locations:** [history.go](../../core/outline-worker/internal/server/history.go#L269-L304), [history.go](../../core/outline-worker/internal/server/history.go#L348-L368), [service.go](../../core/outline-worker/internal/server/service.go#L271-L275)
- **Observation:** `Monitor.Flush` snapshots live counters under `m.mu`, releases `m.mu`, and then acquires `m.historyMu`. `Monitor.Forget` removes a deleted user from `m.users` and `m.history` between those two locks.
- **Impact:** Traffic history for a deleted user can be resurrected in `m.history` right after deletion.
- **Recommendation:** Acquire `m.historyMu` before releasing `m.mu` (or check user existence/generation under consistent lock ordering).

### C8 — Mesh coordinator 3s shutdown deadlock, unlocked state reads, and shutdown order

- **Locations:** [server/mesh.go](../../core/outline-worker/internal/server/mesh.go#L95-L119), [server/mesh.go](../../core/outline-worker/internal/server/mesh.go#L302), [dashboard.go](../../core/outline-worker/internal/server/dashboard.go#L63), [ghostlink-server/main.go](../../core/outline-worker/cmd/ghostlink-server/main.go#L202-L206), [ghostlink-server/main.go](../../core/outline-worker/cmd/ghostlink-server/main.go#L228-L230)
- **Observation:**
  - `MeshServer.Apply` holds `m.mu.Lock()` across a 3s `m.server.Shutdown(ctx)` while an in-flight `MeshServer.sync` handler calls `m.service.Change` → `OnChange` → `meshServer.Apply()`.
  - Both `MeshServer.Apply` and `Dashboard.Apply` read `store` state outside their own locks before updating listener state.
  - Main server shutdown closes `meshServer` and `dashboard` *before* awaiting `<-accounted`, allowing `s.Account()` quota enforcement during shutdown to trigger `OnChange` and re-bind `MeshServer`.
- **Impact:** Clean shutdown can stall for 3 seconds on lock inversion or re-open listeners after `Close()` began.
- **Recommendation:** Release `m.mu` around `Shutdown(ctx)` (snapshot and clear server pointer under lock), read configuration state cleanly inside synchronized apply calls, stop `accounted` *before* closing `meshServer`/`dashboard`, and make `Apply` a no-op once closed.

---

## 2. Medium & Lower-Priority Findings (`M1`–`M11`)

### Rust Daemon, Worker Supervision & Protocol Parsers (`M1`–`M4`)

- **M1 — `Daemon::connect` lock stall & redundant `.audit()` lock ([vpn-daemon/main.rs](../../services/vpn-daemon/src/main.rs#L361-L437), [vpn-daemon/main.rs](../../services/vpn-daemon/src/main.rs#L957), [outline.rs](../../core/vpn-engine/src/outline.rs#L178-L196), [remote.rs](../../services/vpn-daemon/src/remote.rs#L139-L142)):**
  - `Daemon::connect` executes `storage::unseal`, `access::resolve` (`5s` DNS + `10s` HTTPS timeouts), and `profile::validate` before checking whether a session is already active (`busy`), stalling `daemon.lock()` and misreporting `invalid_profile`/`permission_denied` instead of `busy`.
  - `remote::handle` re-acquires `daemon.lock().await` upon completion solely to call `.audit(...)` even though `AuditLog` already uses its own `std::sync::Mutex`.
- **M2 — Worker process lifecycle supervision ([worker_session.rs](../../core/vpn-engine/src/worker_session.rs#L351-L453)):**
  - `command.kill_on_drop(false)` can orphan a child process if cancelled before `Session` construction completes.
  - The 8s `health.get` `select!` omits `child.wait()` and log reads, delaying crash detection; `controller.get` runs outside `select!` (up to 3s) without re-verifying `verify_listeners(pid, &[api_port])`.
  - In [management.rs](../../core/vpn-engine/src/management.rs#L257) + [shared-contracts/lib.rs](../../packages/shared-contracts/src/lib.rs#L597-L618), `read_frame` uses a local `Vec` rather than a caller buffer; this is currently benign only because sibling `select!` arms break out of the loop.
- **M3 — Protocol parsers & split-tunnel subtle issues:**
  - **Split-tunnel bootstrap leak ([split.rs](../../core/vpn-engine/src/split.rs#L127-L143), [worker_session.rs](../../core/vpn-engine/src/worker_session.rs#L220-L276), [awg/endpoint.go](../../core/outline-worker/internal/awg/endpoint.go#L422)):** Under `SplitMode::Only` with WireGuard/AmneziaWG, readiness probes for `www.gstatic.com` resolve through `dns_final: "bootstrap"` unless `gstatic.com` is in `policy.domains`.
  - **AmneziaWG DNS injection ([amnezia.rs](../../core/vpn-engine/src/amnezia.rs#L193-L197)):** `config.replacen("[Interface]\n", ..., 1)` silently drops injected `dns1`/`dns2` on `\r\n`, lowercase `[interface]`, or trailing spaces.
  - **WireGuard validation vs worker acceptance ([wireguard.rs](../../core/vpn-engine/src/wireguard.rs#L128-L354), [worker_session.rs](../../core/vpn-engine/src/worker_session.rs#L163-L171)):** `number()` permits `+4`; `replace(' ', "")` keeps `\t` in CIDR/port ranges (passing validation, then failing `awgDevice.IpcSet` at connect time); `endpoint()` accepts unbracketed colons before the final `rsplit_once(':')`.
  - **OpenVPN & VLESS edge checks ([profile.rs](../../core/vpn-engine/src/profile.rs#L191-L266), [vless.rs](../../core/vpn-engine/src/vless.rs#L144-L161)):** `"route-ipv6"` is exempted from duplicate validation before being rejected; `cipher`/`data-ciphers`/`verb`/`resolv-retry` are checked then overwritten by `canonical`. VLESS parsing uses case-sensitive `item != "true"` and overwrite semantics on duplicate query keys (mitigated today because `worker_session::proxy` does not emit `"insecure": true`).
- **M4 — Desktop / CLI contract & timeout discrepancies:**
  - Desktop `vpn_request` times out after `30s` ([src-tauri/main.rs](../../apps/desktop/src-tauri/src/main.rs#L35)) vs `75s` in CLI ([cli/main.rs](../../cli/ghostlink/src/main.rs#L23)) and MCP ([mcp-server/main.rs](../../integrations/mcp-server/src/main.rs#L139)).
  - CLI `read_secret` enforces a `16 KiB` cap ([cli/main.rs](../../cli/ghostlink/src/main.rs#L922)) while `wireguard.rs`, `amnezia.rs`, and the desktop UI allow `64 KiB` ([MyServers.tsx](../../ui/desktop/src/views/MyServers.tsx#L277)).
  - CLI client key issuance strips `access_key` and `config` whenever `output` or `import_as` is set, even if `remote::import_issued` returns an `import_error` ([cli/main.rs](../../cli/ghostlink/src/main.rs#L1620-L1625), [remote.rs](../../services/vpn-daemon/src/remote.rs#L301)).

### Go Mesh, Server & AWG Worker (`M5`–`M8`)

- **M5 — Disabled reserved ports & implicit mesh enable ([server/mesh.go](../../core/outline-worker/internal/server/mesh.go#L59-L74), [server/mesh.go](../../core/outline-worker/internal/server/mesh.go#L437), [render.go](../../core/outline-worker/internal/server/render.go#L378-L485), [api.go](../../core/outline-worker/internal/server/api.go#L422-L446)):** Disabled `Mesh.Port` and `IPsec.DNSPort` values are omitted from `State.Listening()`, letting `PATCH /server` assign colliding ports without checking on re-enable; `PATCH /mesh {"cidr": "..."}` implicitly sets `Enabled = true` when `s.Mesh == nil`.
- **M6 — Mesh agent unclean-exit recovery ([meshagent/agent.go](../../core/outline-worker/internal/meshagent/agent.go#L140-L145)):** If `ghostlink-mesh` crashes with `glmesh0`, routes, or rules installed and returns in `StatusPending` or `StatusDisabled`, `a.applied == nil && a.dev == nil` skips `a.teardown()`, leaving stale mesh networking active while reporting disabled.
- **M7 — Endpoint validation & route protection gaps ([meshproto.go](../../core/outline-worker/internal/meshproto/meshproto.go#L236-L242), [node.go](../../core/outline-worker/internal/meshagent/node.go#L205-L246), [meshagent/agent.go](../../core/outline-worker/internal/meshagent/agent.go#L304-L310), [render.go](../../core/outline-worker/internal/server/render.go#L270-L295), [device_linux.go](../../core/outline-worker/internal/meshagent/device_linux.go#L104-L156), [live.go](../../core/outline-worker/internal/mesh/live.go#L190)):**
  - `ValidEndpoint` allows `127.0.0.1`, `169.254.x.x`, and `100.96.x.x` (`join-here` also uses `127.0.0.1`).
  - `LocalEndpoints` does not exclude `podman`, `lxdbr`, `incusbr`, `cali`, or `cilium` bridges that `forwardingUsers` filters out.
  - `setRoutes` omits the coordinator URL IP from endpoint route protection and passes netlink route filter `0` (all tables) instead of `RT_TABLE_MAIN`.
- **M8 — Live telemetry, VICI tick cadence, stale health & AWG close (`awg/bind.go`):**
  - `Live.Record` can leak `reports[id]` after node deletion ([server/mesh.go](../../core/outline-worker/internal/server/mesh.go#L280), [service.go](../../core/outline-worker/internal/server/service.go#L84-L91)); clock skew >3m breaks relay fallback/recovery ([live.go](../../core/outline-worker/internal/mesh/live.go#L139-L151)); `ghostlink-mesh leave` races a running `serve` process ([ghostlink-mesh/main.go](../../core/outline-worker/cmd/ghostlink-mesh/main.go#L94-L99)).
  - Failed `Engine.Apply` + failed rollback leaves `Health.EngineError` blank until the next 30s tick ([service.go](../../core/outline-worker/internal/server/service.go#L309-L423), [engine.go](../../core/outline-worker/internal/server/engine.go#L158-L171)); `ipsecagent` increments `ticks` on VICI events in addition to the 30s ticker ([ipsecagent/agent.go](../../core/outline-worker/internal/ipsecagent/agent.go#L120-L137)); `awg` `clientBind.Send` sleeps `1s` on `net.ErrClosed` and `Open` updates shutdown state without `connAccess` ([awg/bind.go](../../core/outline-worker/internal/awg/bind.go#L68-L133)).

### Packaging, Installers & CI (`M9`–`M11`)

- **M9 — Pre-release & toolchain version mismatches:**
  - [build-linux.sh](../../scripts/build-linux.sh#L102-L104) and [build-linux-mesh.sh](../../scripts/build-linux-mesh.sh#L53-L54) pass `--target "$out/"` to `nfpm`, producing `~rc.1` artifacts rather than the `-rc.1` naming expected by [build-linux-server.sh](../../scripts/build-linux-server.sh#L113-L114), [install-mesh.sh](../../scripts/install-mesh.sh#L130), [install-server.sh](../../scripts/install-server.sh#L326), and [release-notes.md](../../.github/release-notes.md#L7-L13) (`install-mesh.sh` line [123](../../scripts/install-mesh.sh#L123) also lacks `tr '~_' '--'`).
  - [Ghostlink.wxs](../../packages/windows/Ghostlink.wxs#L3) uses `Version="$(var.Version)"` directly from [release-version.mjs](../../scripts/release-version.mjs#L10), which fails Windows Installer compilation for SemVer pre-release tags.
  - [package-worker-source.ps1](../../scripts/package-worker-source.ps1#L76) references `Go 1.26.10` while [go.mod](../../core/outline-worker/go.mod#L5) pins `toolchain go1.26.11`.
- **M10 — Installer, RPM, MSI & Docker image hardening:**
  - [install-server.sh](../../scripts/install-server.sh#L301-L485) omits adding `glmesh0` to `firewalld`'s `trusted` zone (unlike [install-mesh.sh](../../scripts/install-mesh.sh#L208-L212)), executes `open_ports` *after* starting IPsec/mesh (flushing firewall jumps), exits under `set -euo pipefail` on firewall errors, and matches `deinstall ok config-files` (`lines 217-220`).
  - [install-mesh.sh](../../scripts/install-mesh.sh#L103-L120) can fail under `pipefail` on `ghostlink-mesh version`, does not install `iproute2`, and resets to listen port `51820` on re-run instead of preserving `node.json` `.listen_port`.
  - [ghostlink-server.nfpm.yaml](../../packages/linux/server/ghostlink-server.nfpm.yaml#L46-L52) omits Docker/podman in `rpm.depends`; purging `ghostlink-server` while Docker is stopped ([server-preremove.sh](../../packages/linux/scripts/server-preremove.sh), [server-postremove.sh](../../packages/linux/scripts/server-postremove.sh)) removes state and system user while leaving `--restart unless-stopped` containers behind.
  - [build-windows.ps1](../../scripts/build-windows.ps1#L25-L31) does not clean `$stage` before copying; [Ghostlink.wxs](../../packages/windows/Ghostlink.wxs#L17-L20) overwrites empty `GHOSTLINK_OWNER_SID` with `[UserSID]` (`S-1-5-18` under `SYSTEM`) before `LaunchConditions` (making the guard dead) and lacks `CloseApplication` / `ServiceConfig`.
  - [ipsec.Dockerfile](../../packages/linux/server/ipsec.Dockerfile#L8) and [ipsec-source.sh](../../scripts/ipsec-source.sh#L36) pull non-pinned Alpine apk versions and track `3.24-stable`.
- **M11 — CI hygiene & missing automation coverage:**
  - Workflows pin floating `@v4`/`@v5` action tags, run `curl | sh` for `rustup` ([linux-package.yml](../../.github/workflows/linux-package.yml#L37)), and run `--no-audit` ([ui.yml](../../.github/workflows/ui.yml#L22)) while [audit-lockfiles.py](../../scripts/audit-lockfiles.py) goes unused.
  - [release.yml](../../.github/workflows/release.yml) does not run [mesh.yml](../../.github/workflows/mesh.yml) ([e2e.sh](../../tests/mesh/e2e.sh)) or [ipsec.yml](../../.github/workflows/ipsec.yml) installer tests; `.rpm` packages are inspected with `rpm -qp` but not installed; `GHOSTLINK_TEST_EXPECT_UNPRIVILEGED=1` ([control-plane.test.mjs](../../tests/integration/control-plane.test.mjs#L299)) is unset in CI; Windows CI skips OpenVPN tests and does not build/test the MSI.

---

## 3. UI/UX Summary (`U1`–`U14`)

Full details and UI recommendations are documented in [ui-ux-review-2026-11-25.md](ui-ux-review-2026-11-25.md):

- **P1 (`U1`–`U5`):** Stacked double navbars and two black Dynamic Islands in `Servers I run` (`U1`); navbar wrapping at the default `1060px` desktop window width (`U2`); premature `"No matching clients"` flash before initial load (`U3`); missing `busy` button locks and two-step retry traps in `AddKeySheet`/`NewClient` (`U4`); `AreaChart` Y-axis label overlaps and hardcoded SVG gradient IDs (`U5`).
- **P2 (`U6`–`U12`):** `My servers` disconnected hero/search ergonomics (`U6`); `Add access key` lacking raw `.conf` paste, Paste button, drag-and-drop, and `.ovpn` guidance (`U7`); raw JSON `Doctor`, unused daemon `Action::Logs`, and static Tauri system tray (`U8`); unexposed backend features—key replacement/rotation, ASN **Update now**, Apple `on_demand`, IPsec PSK rotation, and `Mesh` invite/node/link controls (`U9`–`U10`); missing client sorting, multi-key preview, and rules validation (`U11`); missing clipboard `.catch()` handlers and UTF-16 `slice(0, 2)` splitting emoji (`U12`).
- **P3 (`U13`–`U14`):** Low `--fg-3` contrast, missing `data-autofocus`, short toasts, outdated `v0.7.0` screenshots (`Mbit/s`), duplicated app shell, and missing React DOM/component tests.

---

## 4. Verification Results (Observed on Windows)

| Suite / Check | Result | Notes & Scope |
| --- | --- | --- |
| `cd core/outline-worker && go test -tags with_gvisor,with_utls,with_wireguard ./...` | **Pass** (exit `0`) | 10 packages passed (`internal/meshagent` `9.961s`, `internal/server` `100.308s`, 8 cached/ok); 4 packages had `[no test files]` (`cmd/ghostlink-ipsec-agent`, `cmd/ghostlink-mesh`, `internal/ipsecproto`, `internal/version`) |
| `cd core/outline-worker && go vet -tags with_gvisor,with_utls,with_wireguard ./...` | **Pass** (exit `0`) | Clean across all packages |
| Rust unit tests (`ghostlink-engine --lib`, `ghostlink`, `ghostlink-contracts`, `ghostlinkd`, `ghostlink-mcp`, `src-tauri`) | **Pass** (exit `0`) | `ghostlink-engine --lib`: `44 passed; 0 failed`; `ghostlink`: `1`; `ghostlink-contracts`: `6`; `ghostlinkd`: `5`; `ghostlink-mcp`: `0`; `src-tauri`: `2`; `doc-tests`: `0` (executed with host `GetExtendedTcpTable` access) |
| Rust integration (`ghostlink-engine` `worker_exit`, `worker_protocols`, `outline_traffic`) | **Partial / Pass** (exit `0`) | `worker_exit`: `4` scenarios `ok`; `outline_traffic` (`3 ignored`) & `worker_protocols` (`3 ignored`) require `GHOSTLINK_TEST_OUTLINE_WORKER` / `GHOSTLINK_TEST_SING_BOX` |
| Rust `clippy -- -D warnings` & `cargo fmt --check` (workspace + `src-tauri`) | **Pass** (exit `0`) | Clean across all targets |
| Root integration (`npm test` → [control-plane.test.mjs](../../tests/integration/control-plane.test.mjs)) | **Pass** (exit `0`) | `# tests 20`, `# pass 20`, `# fail 0`, `# skipped 0` |
| Python integration (`py -m unittest discover -s tests/integration -p "test_*.py" -v`) | **Pass with skips** (exit `0`) | `Ran 28 tests in 9.195s`, `OK (skipped=13)`: `11` passed in [test_provision.py](../../tests/integration/test_provision.py), `4` passed in [test_windows_installer.py](../../tests/integration/test_windows_installer.py), `13` skipped in [test_linux_packages.py](../../tests/integration/test_linux_packages.py) (requires POSIX shell) |
| UI checks (`cd ui && npm run typecheck && npm test`) | **Pass** (exit `0`) | `tsc --noEmit` + `# tests 25`, `# pass 25`, `# fail 0`, `# skipped 0` |
| Version script (`node scripts/release-version.mjs`) | **Pass** (exit `0`) | Output: `0.9.0` |

**Not exercised on this Windows host:** the 13 skipped Linux package tests, [tests/mesh/e2e.sh](../../tests/mesh/e2e.sh), the 6 ignored worker/sing-box protocol integration tests, live GUI/Tauri rendering, and live MSI/RPM/DEB installations.

---

## Suggested Implementation Roadmap

1. **Phase 1 — Security, Concurrency & Recovery Fixes (`C1`–`C8`):**
   - Harden `ghostlink-server.sh` and `install-server.sh` `$STATE` symlink validation (`ipsec_start` mount & root reads) and Windows atomic/private file ACL creation (`storage.rs`, `cli`, `save_download`, `ipc.rs`).
   - Add an explicit recovery/reset path for `cleanup_unverified`, catch abnormal OpenVPN exits after hold release, and make `ipsecagent` invoke `a.FW.Apply` when firewall chains/jumps are missing.
   - Fix Go server locks and lifecycle races: `rotateKey`, ASN context cancellation + `maxOperator` boundary (`>=`), `Monitor.Flush`/`Forget` lock ordering, `MeshServer.Apply` shutdown lock, and shutdown ordering (`<-accounted` before listener teardown).
2. **Phase 2 — Daemon, Mesh & Installer Reliability (`M1`–`M10`):**
   - Check `busy` before `unseal`/`ssconf://` network fetches, move remote `.audit()` off `daemon.lock()`, tighten `worker_session` supervision, align `split.rs`/`amnezia.rs`/`wireguard.rs`/`profile.rs` edge parsing, and reconcile Desktop `30s`/`75s` timeouts and CLI `16 KiB`/`64 KiB` caps.
   - Fix `meshagent` stale-teardown on restart, endpoint/bridge filtering, `RT_TABLE_MAIN` protection, reserved port accounting, `nfpm` `-rc.1` pre-release naming, WiX pre-release versioning/`CloseApplication`, and `install-server.sh`/`install-mesh.sh` idempotency.
3. **Phase 3 — CI Hardening & UI Roadmap (`M11`, `U1`–`U14`):**
   - Pin CI action SHAs, wire `audit-lockfiles.py`, run `mesh.yml`/`ipsec.yml` and RPM install verification in release gating, and execute the 3-phase UI/UX plan in [ui-ux-review-2026-11-25.md](ui-ux-review-2026-11-25.md).

---

## Status in 0.9.1 (2026-11-26)

Added by the maintainers after the fixes. Each finding was re-checked against the code first. **Fixed** means a test fails with the old behaviour; the details and the proofs are in `verification.md` (*v0.9.1*) and the CHANGELOG.

| ID | Verdict | What was done |
| --- | --- | --- |
| C1 | Partly real | Fixed: root reads of the state go through `nofollow_cat` and `in_state_dir`; `save_private` removes only a file it created; `ipsec enable` and `--add-swap` roll back; the IPsec directory is re-checked while the component starts. **Not closed:** the swap of `$STATE/ipsec` for a symlink that is put back at once between the check and Docker's mount (a shell cannot hand Docker an open directory); it needs the exchange directory out of the server account's reach |
| C2 | Real (one sub-item partly) | Fixed: secrets are created private (`private.rs`), `agent grant` makes its token file first, `save_download` exports are private, `private_dir` checks `symlink_metadata` first, the DPAPI zero-length guard, the pipe rights (read and write without create-instance for the user of a SYSTEM service) and `accept` retrying the next instance. The `cbData == 0` guard is defensive (DPAPI returned a non-null pointer, so its test passes on the old code too), and a dangling link already failed before, with a worse message |
| C3 | Real | Fixed: the barrier lifts at connect, switch and disconnect after the OS shows the tunnel adapter and its addresses are gone, and stays otherwise; an OpenVPN that crashes after bring-up sets it. Not covered: routes, firewall rules and DNS are not inspected (Windows removes them with the adapter) |
| C4 | Real, worse than reported | Fixed: the old `EnsureJumps` put jumps back into emptied chains; the agent now re-applies the whole rule set when a jump or chain is missing and shows a failed repair in `status.json` |
| C5 | Real | Fixed: one rotation at a time |
| C6 | Partly real | The claim that `tried` was not reset was wrong (`Drop` reset it); the real effect was an aborted download that kept running and no list after off and on. Fixed with a cancellable download tied to a generation, under the store lock. The suggested `>=` alone does not fix the 60-character cap (59 letters, a space, a letter gives 61): the check now follows the write |
| C7 | Real | Fixed: `historyMu` before `mu` in `Flush` and `Forget` |
| C8 | Real | Fixed: `Apply` does not hold its lock across `Shutdown`, reads its state inside its turn and does nothing after `Close`; shutdown stops the accounting loop before the listeners |
| M1 | Real | Fixed: `busy` before unseal and fetch; the audit line no longer re-locks the daemon. **Deferred:** the daemon lock is still held during a legitimate dynamic `ssconf://` fetch (the fix splits `connect` into plan, fetch and commit) |
| M2 | Partly real | Fixed: the service lock is taken before the worker is spawned (a cancelled start left a running worker); the controller request runs in a `select!` and re-verifies the listener before the secret is sent. **Not real:** the health `select!` missing `child.wait()` (a dead worker resets the request, so the crash is seen after 1.2 s either way). The `read_frame` note is benign |
| M3 | Mostly real | Fixed: the AmneziaWG DNS lines, `+4`, tabs and unbracketed IPv6 endpoints in WireGuard and AmneziaWG keys, VLESS repeats and insecure flags, and the health probe's lookup in Only mode. **Not real or no effect:** the tab inside a CIDR (addresses are re-rendered) and the OpenVPN `route-ipv6`, `verb`, `resolv-retry` and `cipher` items (the match rejects `route-ipv6`; the others are overwritten by the canonical profile and the ciphers act only as a gate) |
| M4 | Real | Fixed: one 75 s timeout for the CLI, the MCP bridge and the desktop shell; `key add` reads 64 KiB; the key stays in the answer when it was not saved |
| M5 | Real | Fixed: the ports of a mesh that is off and of IPsec's DNS port while IPsec is off stay reserved (UDP 500 and 4500 are not reserved); a cidr-only `PATCH /mesh` no longer turns the mesh on |
| M6 | Real | Fixed: a node that starts pending or disabled takes down what an earlier run left, once |
| M7 | Partly real | Fixed: link-local, unspecified, multicast and broadcast are refused; the bridge list matches the server's; the coordinator's address is protected in `setRoutes`; in-mesh and loopback peer endpoints are ignored. **Kept on purpose:** loopback stays a valid endpoint (`join-here` on a host named 127.0.0.1 sends it; refusing it would take old agents offline) and is never offered. **Not real:** the route filter 0 already means the main table in the netlink module used |
| M8 | Mostly real | Fixed: the `Live.Record` leak and the clock skew (handshakes are dated by the coordinator's clock), the engine error after a failed rollback, the VICI ticks, the AmneziaWG `Send` sleep and the `Open` lock (hygiene: no live race). `leave` against a running `serve`: partly real, the harmful outcomes are fixed; a cross-process lock is deferred |
| M9 | Real | Fixed: package names for pre-releases, the MSI version for pre-release tags (an rc and its final release share one MSI version), the Go version in `BUILD.txt` |
| M10 | Mostly real | Fixed: `open_ports` order and firewalld reload, the trusted zone for the mesh interface, error handling, the `deinstall ok config-files` match, `install-mesh.sh` port and `iproute`, purge with Docker stopped, the stale MSI stage, the owner-SID guard. **Deferred:** RPM Docker dependencies, `CloseApplication` and `ServiceConfig`. **Not changed:** apk version pins (Alpine removes superseded versions; the base image is pinned by digest) |
| M11 | Partly done | Fixed: actions pinned by commit, release gating on the mesh and IPsec tests, the unprivileged check on Linux. **Deferred:** the `rustup` script (no verified checksum for arm64), `audit-lockfiles.py` as a gate, `.rpm` install tests, OpenVPN and the MSI in the Windows jobs |
| UI | See the UI review | Status table in `ui-ux-review-2026-11-25.md` |

