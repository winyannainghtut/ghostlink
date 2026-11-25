# Ghostlink UI/UX and Frontend Review

**Review date:** November 25, 2026 (Asia/Singapore)  
**Reviewed version:** v0.9.0  
**Reviewed commit:** `7d419cddda6b8300b512a1606a9c1524774e3be6`  
**Scope:** `ui/` (desktop app, web dashboard, shared admin views, glass kit, charts, styles), `apps/desktop/src-tauri/`, and alignment with `packages/shared-contracts`, `services/vpn-daemon`, and `core/outline-worker/internal/server`.

This report records a static codebase and visual-artifact review of Ghostlink's UI/UX and frontend integration. No production source files were modified, and no live runtime GUI session was executed; visual observations reference the source code and the existing screenshots under `docs/images/`.

---

## Summary at a Glance

| ID | Priority | Area | Finding / Opportunity | Primary Locations |
| --- | --- | --- | --- | --- |
| U1 | P1 | Navigation & Layout | Stacked double navbars and two black Islands inside `Servers I run` workspaces | [desktop App.tsx](../../ui/desktop/src/App.tsx#L44-L89), [ServersIRun.tsx](../../ui/desktop/src/views/ServersIRun.tsx#L220-L251) |
| U2 | P1 | Navigation & Layout | Fixed `lg:w-[368px]` navbar slots squeeze and wrap center controls at default `1060px` desktop width | [tauri.conf.json](../../apps/desktop/src-tauri/tauri.conf.json#L9), [desktop App.tsx](../../ui/desktop/src/App.tsx#L47-L70), [dashboard App.tsx](../../ui/dashboard/src/App.tsx#L104-L126) |
| U3 | P1 | Loading & Empty States | `Clients` flashes `"No matching clients"` before initial load; other views show bare text or blank grids | [Clients.tsx](../../ui/admin/views/Clients.tsx#L75-L90), [ServersIRun.tsx](../../ui/desktop/src/views/ServersIRun.tsx#L100-L149) |
| U4 | P1 | Forms & Mutation Safety | Missing `busy` locks on Save/Create actions and partial-failure traps in two-step create/import flows | [MyServers.tsx](../../ui/desktop/src/views/MyServers.tsx#L249-L272), [NewClient.tsx](../../ui/admin/views/NewClient.tsx#L82-L91), [ClientSheet.tsx](../../ui/admin/views/ClientSheet.tsx#L88-L104) |
| U5 | P1 | Charts | `AreaChart` Y-axis labels draw inside the plot area over live curves; gradient IDs are hardcoded | [AreaChart.tsx](../../ui/shared/AreaChart.tsx#L53-L99), [BarChart.tsx](../../ui/shared/BarChart.tsx#L43-L50) |
| U6 | P2 | Desktop UX | `My servers` shows search on empty state, lacks empty-search message, and leaves 4 empty `—` tiles in hero | [MyServers.tsx](../../ui/desktop/src/views/MyServers.tsx#L62-L129) |
| U7 | P2 | Desktop UX | `Add access key` lacks paste button, drag-and-drop, `.conf` text paste, and OpenVPN guidance | [MyServers.tsx](../../ui/desktop/src/views/MyServers.tsx#L230-L342), [src-tauri/src/main.rs](../../apps/desktop/src-tauri/src/main.rs#L12-L25) |
| U8 | P2 | Desktop UX & Shell | Diagnostics dumps raw JSON; `logs` action and interactive/live system tray are unimplemented | [Settings.tsx](../../ui/desktop/src/views/Settings.tsx#L64-L88), [src-tauri/src/main.rs](../../apps/desktop/src-tauri/src/main.rs#L119-L158) |
| U9 | P2 | Backend Capabilities | Implemented backend features have no UI (key rotation/replace, ASN update, On-Demand, IPsec PSK rotation) | [api.go](../../core/outline-worker/internal/server/api.go#L262-L286), [routes.ts](../../ui/desktop/src/routes.ts#L19-L37), [Settings.tsx](../../ui/admin/views/Settings.tsx#L126-L159) |
| U10 | P2 | Mesh UI | `Mesh` hides open invites/revoke, node rename/masquerade, mesh name/CIDR, link traffic, and map zoom/pan | [Mesh.tsx](../../ui/admin/views/Mesh.tsx#L197-L398), [MeshMap.tsx](../../ui/shared/MeshMap.tsx#L32-L224) |
| U11 | P2 | Clients & Rules UX | No client sorting, single-protocol preview on multi-key cards, and unvalidated raw-text rules/split editors | [Clients.tsx](../../ui/admin/views/Clients.tsx#L98-L266), [RulesEditor.tsx](../../ui/admin/RulesEditor.tsx#L43-L53), [SplitSheet.tsx](../../ui/desktop/src/views/SplitSheet.tsx#L52-L106) |
| U12 | P2 | Clipboard & Unicode | Missing `.catch()` handlers on several `writeText` calls; UTF-16 `slice(0, 2)` splits emoji/non-BMP titles | [NewClient.tsx](../../ui/admin/views/NewClient.tsx#L142), [Mesh.tsx](../../ui/admin/views/Mesh.tsx#L346), [MyServers.tsx](../../ui/desktop/src/views/MyServers.tsx#L182) |
| U13 | P3 | Accessibility & Contrast | `--fg-3` contrast below WCAG AA; missing `data-autofocus`, popover keyboard nav, and persistent banners | [styles.css](../../ui/shared/styles.css#L38-L80), [glass.tsx](../../ui/shared/glass.tsx#L257-L368), [Popover.tsx](../../ui/shared/Popover.tsx#L7-L63) |
| U14 | P3 | Docs, Charts & Hygiene | Outdated v0.7.0 `Mbit/s` screenshots, `Bars` 8-item silent cutoff, duplicated shell nav, no DOM/UI render tests | [Bars.tsx](../../ui/shared/Bars.tsx#L10), [dashboard-overview.png](../../docs/images/dashboard-overview.png), [ui/tests/](../../ui/tests/) |

**Priority levels:**
- **P1:** Causes visible layout breakage, misleading states, data/action mistakes, or chart overlaps in primary workflows.
- **P2:** Missing ergonomics, unexposed backend features, or UX friction in common admin/client flows.
- **P3:** Visual polish, accessibility refinements, documentation drift, and structural/test maintainability.

---

## 1. Layout, Navigation & Loading State Issues

### U1 — Stacked navbars and competing Dynamic Islands in `Servers I run`

- **Locations:** [ui/desktop/src/App.tsx](../../ui/desktop/src/App.tsx#L44-L89), [ui/desktop/src/views/ServersIRun.tsx](../../ui/desktop/src/views/ServersIRun.tsx#L220-L251)
- **Observation:** When opening a server under **Servers I run**, `WorkspaceBody` renders a second full-width glass bar immediately below the app's sticky header. Both bars share the same visual language: two `Segmented` pill controls (`My servers | Servers I run | Settings` above `Overview | Clients | Network`), two black Dynamic Island pills (`ConnectionIsland` for local tunnel status above `Island` for remote server status), and two right-aligned CTAs (`Add server` above `New client`).
- **Impact:** Increases vertical chrome, blurs the distinction between local client connection state and remote server state, and places two similarly styled primary buttons directly on top of each other.
- **Recommendation:**
  - Replace the top-level `+ Add server` CTA when inside a workspace or fold the server workspace navigation into a sleeker breadcrumb/sub-header with a visually distinct server-status pill (for example, frosted/tinted surface rather than an identical solid-black Dynamic Island).
  - Consider collapsing the outer `My servers | Servers I run | Settings` bar or moving to a compact left rail on wider desktop windows when managing servers.

### U2 — Navbar wrapping at default `1060px` desktop window size

- **Locations:** [apps/desktop/src-tauri/tauri.conf.json](../../apps/desktop/src-tauri/tauri.conf.json#L9), [ui/desktop/src/App.tsx](../../ui/desktop/src/App.tsx#L47-L70), [ui/dashboard/src/App.tsx](../../ui/dashboard/src/App.tsx#L104-L126)
- **Observation:** The desktop window launches at `1060 × 760`. Both shells fix the left brand slot and right action slot to `lg:w-[368px]` (`736px` combined) from `1024px` upward. Inside a `1060px` window, less than `260px` remains for the `flex-1` center slot holding both the 3-option `Segmented` switcher and the Island. When connected (`Connected · server · 00:00:00 · ↓ ... ↑ ...`), the center `flex-wrap` container overflows onto two rows.
- **Impact:** The sticky header doubles in height at default window dimensions and shifts vertically whenever the VPN connects or disconnects.
- **Recommendation:**
  - Use a responsive CSS grid (`grid-cols-[auto_1fr_auto]`) or shrinkable `minmax` tracks instead of hardcoding `368px` on `lg`.
  - Move `MotionButton` and `FinishPicker` into a compact Appearance popover (or keep them solely in Settings on narrower viewports) so the top bar stays single-row at `1060px`.

### U3 — Premature `"No matching clients"` flash and inconsistent loading states

- **Locations:** [ui/admin/views/Clients.tsx](../../ui/admin/views/Clients.tsx#L75-L90), [ui/desktop/src/views/ServersIRun.tsx](../../ui/desktop/src/views/ServersIRun.tsx#L100-L149), [ui/desktop/src/views/ServersIRun.tsx](../../ui/desktop/src/views/ServersIRun.tsx#L201), [ui/admin/views/ClientSheet.tsx](../../ui/admin/views/ClientSheet.tsx#L109), [ui/admin/views/Network.tsx](../../ui/admin/views/Network.tsx#L95)
- **Observation:**
  - In `Clients.tsx`, `{loaded && !users.length ? (...) : !shown.length ? (<Empty title="No matching clients">) : ...}` drops into the `!shown.length` branch while `loaded` is `false`, flashing `"No matching clients"` until the first `/users` call resolves.
  - In `ServersIRun.tsx`, while `servers === null`, nothing is shown in the grid; opening a workspace shows a bare centered `<p>Connecting to …</p>`.
  - `ClientSheet` (`KeysTab`), `Network` (`Rules`), and `dashboard/src/App.tsx` (`phase === 'loading'`) use plain `<p>` / `<Note>` loading strings instead of matching card skeletons.
- **Recommendation:** Guard `!loaded` with glass skeleton cards/rows matching the card/table layout so views do not flash empty-search messages or shift height on initial load.

---

## 2. Forms, State Safety & Error Handling

### U4 — Missing `busy` states and two-step partial-failure traps

- **Locations:**
  - Missing in-flight `busy` guards: [ClientSheet.tsx](../../ui/admin/views/ClientSheet.tsx#L88-L104) (`KeysTab` `create` and `SettingsTab` `save`), [Settings.tsx](../../ui/admin/views/Settings.tsx#L40-L81) (`SettingsSheet` `save`), [Mesh.tsx](../../ui/admin/views/Mesh.tsx#L324-L336) (`InviteSheet` `create`) and [Mesh.tsx](../../ui/admin/views/Mesh.tsx#L434-L445) (`RulesSheet` save), [SplitSheet.tsx](../../ui/desktop/src/views/SplitSheet.tsx#L45-L57), [MyServers.tsx](../../ui/desktop/src/views/MyServers.tsx#L348-L350) (`RenameSheet`).
  - Partial-failure traps: [MyServers.tsx](../../ui/desktop/src/views/MyServers.tsx#L249-L272) (`AddKeySheet`), [NewClient.tsx](../../ui/admin/views/NewClient.tsx#L82-L91).
  - Stale form state on background refresh: [ClientSheet.tsx](../../ui/admin/views/ClientSheet.tsx#L331-L387) (`SettingsTab`).
- **Observation:**
  - Double-clicking `Create key`, `Create invite`, or `Save` triggers duplicate requests (creating multiple keys or invites) because the buttons are not disabled while the async call runs.
  - `AddKeySheet` executes `access_key_import` then `profile_rename`. If `profile_rename` fails, the key remains saved under a generated ID while the modal stays open with an error; clicking **Add server** again imports a duplicate entry with a timestamp suffix.
  - `NewClient` executes `POST /users` then `POST /users/:id/keys`. If key creation fails, retrying submits `POST /users` again (failing on duplicate user name) rather than issuing the key for the already-created user.
  - `SplitSheet` initializes controls to `'full'` before `routing_policy_get` resolves without disabling Save while fetching.
- **Recommendation:**
  - Disable submit buttons and show loading labels during every mutation.
  - Track created user/profile IDs in state so a partial-failure retry resumes step 2 (key creation or rename) rather than repeating step 1.
  - Sync `SettingsTab` dirty state intentionally (and disable **Save** until changes exist or reset cleanly after save).

### U12 — Unhandled clipboard rejections and unsafe UTF-16 slicing

- **Locations:** [NewClient.tsx](../../ui/admin/views/NewClient.tsx#L142), [Mesh.tsx](../../ui/admin/views/Mesh.tsx#L346), [ServersIRun.tsx](../../ui/desktop/src/views/ServersIRun.tsx#L352), [MyServers.tsx](../../ui/desktop/src/views/MyServers.tsx#L182)
- **Observation:**
  - Three `navigator.clipboard.writeText(...).then(...)` calls lack a `.catch(...)` handler (unlike [keys.tsx](../../ui/admin/keys.tsx#L144)). If clipboard access is rejected (browser permission, WebView focus loss, non-HTTPS context), no failure toast appears.
  - `ServerCard` computes its 2-letter avatar with `title.slice(0, 2).toUpperCase()`, which splits surrogate pairs (e.g., flag/emoji prefixes) into replacement glyphs.
- **Recommendation:** Extract a shared `copyText(text, okMsg)` helper in `ui/shared/` with error handling and `execCommand('copy')` fallback if needed, and slice Unicode titles by code points (`[...title].slice(0, 2).join('')`).

---

## 3. Charts & Data Visualization

### U5 — `AreaChart` tick collision, hardcoded SVG IDs, and `BarChart` readability

- **Locations:** [ui/shared/AreaChart.tsx](../../ui/shared/AreaChart.tsx#L53-L99), [ui/shared/BarChart.tsx](../../ui/shared/BarChart.tsx#L43-L76), [ui/shared/Bars.tsx](../../ui/shared/Bars.tsx#L10), [docs/images/dashboard-overview.png](../../docs/images/dashboard-overview.png)
- **Observation:**
  - `AreaChart` uses `right = 4` and renders Y-axis tick labels inside the plot right at `x={width - right}` (`textAnchor="end"`). In `docs/images/dashboard-overview.png`, `5.0 Mbit/s` sits directly over the live edge of the download line.
  - `AreaChart` (`#fill-down`, `#fill-up`) and `BarChart` (`#bar-down`, `#bar-up`) use fixed SVG `<linearGradient>` IDs, which will clash if two charts are ever rendered simultaneously.
  - `BarChart` renders only start/end X-axis timestamps even across `7d` (168 bars) and `90d` views, has no intermediate horizontal gridlines, and relies on `onPointerEnter` (making touch tap-inspection unreliable).
  - `Bars` runs `items.slice(0, 8)` silently, hiding clients or top services past rank 8 without any `"+N more"` summary or expand toggle.
- **Recommendation:** Reserve a right or left axis gutter (or render pill-backed tick labels), switch gradient IDs to React `useId()`, add tap/click selection plus midpoint ticks to `BarChart`, and add a compact `"+N more"` / expand control to `Bars`.

---

## 4. Desktop App UX (`ui/desktop/` & `apps/desktop/src-tauri/`)

### U6 — `My servers` empty hero & card interaction polish

- **Locations:** [ui/desktop/src/views/MyServers.tsx](../../ui/desktop/src/views/MyServers.tsx#L62-L129), [ui/desktop/src/views/MyServers.tsx](../../ui/desktop/src/views/MyServers.tsx#L162-L228)
- **Observation:**
  - When disconnected, the hero card renders four large `Stat` boxes all showing `—`, and the target server to connect is buried inside small caption text.
  - With `0` saved servers, the search bar and `"0 on this device"` label still appear above the empty-state card. With `> 0` saved servers and an unmatched search, nothing is shown below the search bar.
  - `ServerCard` does not surface `tunnel_mode` (`Full` vs `Split`) inline, and clicking the card header/body does nothing.
- **Recommendation:**
  - Hide the search bar when `profiles.length === 0`, and render an `<Empty title="No matching servers">` card with a **Clear search** button when `shown.length === 0`.
  - When disconnected, show a compact target-server pill (name, protocol, host, `Full`/`Split` badge) or collapse empty `—` stat boxes into a cleaner idle layout. Make `ServerCard` header clicks open `Connection details` (or select as active target).

### U7 — `Add access key` input ergonomics and `.conf` / OpenVPN clarity

- **Locations:** [ui/desktop/src/views/MyServers.tsx](../../ui/desktop/src/views/MyServers.tsx#L230-L342), [apps/desktop/src-tauri/src/main.rs](../../apps/desktop/src-tauri/src/main.rs#L12-L25)
- **Observation:**
  - Long `vpn://` or `vless://` URIs are awkward in a single-line `type="password"` input.
  - Although the daemon (`access::handles`) already supports pasting raw WireGuard/AmneziaWG `[Interface]` config text, `AddKeySheet` rejects anything not starting with `ss://`, `ssconf://`, `vless://`, or `vpn://` in the text box—forcing users to save a `.conf` file first.
  - There is no **Paste** button, no drag-and-drop file zone, and no clear UI note that `.ovpn` files must be imported via CLI.
- **Recommendation:** Support pasting raw WireGuard/AmneziaWG `.conf` text directly (routed to `profile_import`), add a **Paste** button and drag-and-drop file dropzone, show a parsed preview badge (protocol + host + port) before adding, and explicitly note `.conf` vs. CLI-only `.ovpn`.

### U8 — Raw JSON Diagnostics, missing Audit Logs viewer & static System Tray

- **Locations:** [ui/desktop/src/views/Settings.tsx](../../ui/desktop/src/views/Settings.tsx#L64-L88), [packages/shared-contracts/src/lib.rs](../../packages/shared-contracts/src/lib.rs#L61-L64), [services/vpn-daemon/src/main.rs](../../services/vpn-daemon/src/main.rs#L674-L692), [apps/desktop/src-tauri/src/main.rs](../../apps/desktop/src-tauri/src/main.rs#L119-L158)
- **Observation:**
  - **Run diagnostics** prints raw `JSON.stringify(doctor, null, 2)` in a `<pre>`.
  - `Action::Logs { tail }` exists in both `shared-contracts` and `vpn-daemon`, returning structured audit entries, but is completely unused in the desktop UI.
  - The Tauri tray menu (`Open Ghostlink`, `Disconnect`, `Quit Ghostlink`) and tooltip (`"Ghostlink"`) never update with live connection state, cannot connect or switch profiles from the tray, and give no preference/indication around close-to-tray behavior.
- **Recommendation:**
  - Render `doctor` as structured status cards/rows (`Facts` + **Copy report** button) paired with a tabbed **Service logs** view using `Action::Logs`.
  - Sync Tauri tray tooltip and menu items with connection status (showing connected server, offering `Connect` / server switching when disconnected) and surface OS notifications/status updates if a tunnel fails while minimized to tray.

---

## 5. Missing UI for Existing Backend Capabilities

### U9 — Key rotation, in-place replacement, ASN refresh & Apple/IPsec options

- **Locations:**
  - Management key rotation & in-place server replacement: [api.go](../../core/outline-worker/internal/server/api.go#L262-L286), [remote.rs](../../services/vpn-daemon/src/remote.rs#L155-L270), [ServersIRun.tsx](../../ui/desktop/src/views/ServersIRun.tsx#L22-L44)
  - Local profile replacement: [main.rs](../../services/vpn-daemon/src/main.rs#L456-L523), [MyServers.tsx](../../ui/desktop/src/views/MyServers.tsx#L194-L214)
  - Provider (ASN) manual refresh: [routes.ts](../../ui/desktop/src/routes.ts#L37), [api.go](../../core/outline-worker/internal/server/api.go#L959-L969), [Settings.tsx](../../ui/admin/views/Settings.tsx#L126-L136)
  - Apple `on_demand` & IPsec PSK rotation: [api.go](../../core/outline-worker/internal/server/api.go#L479-L481), [api.go](../../core/outline-worker/internal/server/api.go#L1035), [routes.ts](../../ui/desktop/src/routes.ts#L19), [ClientSheet.tsx](../../ui/admin/views/ClientSheet.tsx#L140-L150)
- **Observation:**
  1. **Management key replacement/rotation:** `ManagedServerAdd { replace: true }` preserves the server's custom `label`, and `POST /server/rotate-key` rotates the server's management key, but the UI forces users to **Forget** and re-add the server (losing the custom name). Note: `routes.ts` and `ServerOp` do not yet expose `rotate-key` to the desktop app either, while the web dashboard could call `POST /server/rotate-key` directly.
  2. **Local key replacement:** `AccessKeyImport { replace: true }` and `ProfileImport { replace: true }` preserve `routing_override`, yet `MyServers` only offers **Remove server** (which wipes per-server split-tunneling rules).
  3. **Provider lookup refresh:** `POST /asn/update` is already mapped in `routes.ts` and `ServerOp::AsnUpdate`, but `SettingsSheet` has no **Update list now** button and does not poll while `server.asn.updating` is `true`.
  4. **Apple On-Demand & IPsec PSK rotation:** `?on_demand=1` on `.mobileconfig` downloads and `rotate_ipsec_psk` on `PATCH /server` are both implemented in the API and Rust contracts, yet missing from the UI.

### U10 — Mesh administration & map usability gaps

- **Locations:** [ui/admin/views/Mesh.tsx](../../ui/admin/views/Mesh.tsx), [ui/shared/MeshMap.tsx](../../ui/shared/MeshMap.tsx), [ui/desktop/src/routes.ts](../../ui/desktop/src/routes.ts#L30-L31), [packages/shared-contracts/src/lib.rs](../../packages/shared-contracts/src/lib.rs#L205-L275)
- **Observation:**
  - **Active invites cannot be viewed or revoked:** `MeshInfo.invites` and `GET/DELETE /mesh/invites` are already wired, yet `Mesh.tsx` never lists open invites or allows revoking multi-use invites.
  - **Node rename & masquerade toggle missing:** `MeshNodeChange` supports `name` and `masquerade` (and `MeshNode` includes `masquerade`), but `NodePanel` does not show or edit either field.
  - **Mesh name, CIDR & invite note missing:** `MeshPatch` supports `name` and `cidr` (before nodes join), and `MeshInvite` supports `note`, all unexposed in `Mesh.tsx`.
  - **Link throughput & map scaling:** `MeshLink` includes `rx_bps`, `tx_bps`, and `handshake_age_s`, which are omitted from `NodePanel`'s Links list. `MeshMap` has no zoom/pan for dense topologies and can overlap subnet pills when nodes are dragged near one another.

### U11 — Client sorting, multi-key clarity & rules/split-tunnel editors

- **Locations:** [ui/admin/views/Clients.tsx](../../ui/admin/views/Clients.tsx#L98-L266), [ui/admin/views/ClientSheet.tsx](../../ui/admin/views/ClientSheet.tsx#L294-L305), [ui/admin/views/NewClient.tsx](../../ui/admin/views/NewClient.tsx#L146-L238), [ui/admin/RulesEditor.tsx](../../ui/admin/RulesEditor.tsx#L43-L53), [ui/desktop/src/views/SplitSheet.tsx](../../ui/desktop/src/views/SplitSheet.tsx#L52-L106)
- **Observation:**
  - **Clients list/grid:** No sort control (by name, online status, live speed, data usage, or last seen) in either Cards or List view (`MaxUsers = 500`). Multi-key `ClientCard` items show only `primaryKey(user)`'s protocol name, hiding the client's other protocols. `UsageTab`'s *Connected from* list hides `first_unix`.
  - **NewClient flow:** Shows `"Tunnel address: Not needed"` even for Shadowsocks/VLESS. After **Use on this device** in the desktop app, there is no quick **Connect now** action.
  - **Rules & Split Tunneling:** Raw multi-line `<textarea>`s give no per-line syntax validation, item counts, or clear buttons. `SplitSheet` uses a Windows-only placeholder (`chrome.exe`) and still fails on invalid hidden entries if the user edits a list and switches back to `Everything uses the VPN`.

---

## 6. Visual Design, Accessibility & Code Hygiene

### U13 — Contrast, focus restoration & toast ergonomics

- **Locations:** [ui/shared/styles.css](../../ui/shared/styles.css#L38-L80), [ui/shared/glass.tsx](../../ui/shared/glass.tsx#L257-L368), [ui/shared/Popover.tsx](../../ui/shared/Popover.tsx#L7-L63), [ui/dashboard/src/App.tsx](../../ui/dashboard/src/App.tsx#L77-L90)
- **Observation:**
  - `--fg-3` (`0.4` alpha in Obsidian, `0.42` in Daylight) drops below WCAG AA `4.5:1` contrast for `11.5px`–`12.5px` secondary text over frosted surfaces and drifting wave crests.
  - Only 4 sheets mark a field with `data-autofocus`; all others (`ClientSheet`, `SettingsSheet`, `SplitSheet`, `InviteSheet`, `RulesSheet`, `SetupHelp`, `UIProvider` confirmation sheet) default focus to the top-right `×` Close button.
  - `Popover` does not manage focus entry/return or arrow-key navigation (`role="menu"` / `role="menuitem"`).
  - Toasts auto-dismiss in `3.2 s` (`6 s` for errors) with no dismiss button and no pause-on-hover—too brief for multi-sentence guidance like `turnedOn()` firewall port notices or profile passwords.
  - The dashboard `SignIn` card provides no input to paste a login link or token manually when `#t=` is missing or expired.

### U14 — Outdated visual assets, duplicated shell, and UI test coverage

- **Locations:** [docs/images/](../../docs/images/dashboard-overview.png), [ui/desktop/src/App.tsx](../../ui/desktop/src/App.tsx), [ui/dashboard/src/App.tsx](../../ui/dashboard/src/App.tsx), [ui/tests/](../../ui/tests/)
- **Observation:**
  - All four screenshots in `docs/images/` show `v0.7.0`, `Mbit/s` speeds, and `"Last handshake"` labels instead of current `v0.9.0` bytes-per-second units and `"Last seen"`.
  - The header shell markup (`Mark`, `MotionButton`, `FinishPicker`, 3-slot `nav`) is duplicated between `ui/desktop/src/App.tsx` and `ui/dashboard/src/App.tsx`.
  - `ui/tests/` thoroughly covers pure `.ts` modules (`routes`, `limits`, `protocols`, `format`, `provider`, `api`, `desktop`), but has no component/DOM tests for React views, sheets, or charts.

---

## Suggested Implementation Roadmap

1. **Phase 1 — Quick Fixes & Visual Polish:**
   - Fix `Clients.tsx` `!loaded` flash, `MyServers.tsx` empty/search states and Unicode slicing, `AreaChart` label margins and `useId()` gradients, `busy` flags on all mutation buttons, `copyText` helper with `.catch()`, and `--fg-3` contrast / `data-autofocus` defaults.
2. **Phase 2 — Navigation & Desktop Experience:**
   - Fix `1060px` navbar wrapping, unify `Servers I run` workspace header with `App` shell, improve `My servers` disconnected hero + clickable cards, enhance `AddKeySheet` (paste raw `.conf`, paste button, drag-and-drop), format `Doctor` nicely + add `Logs` viewer, and wire live/interactive Tauri tray state.
3. **Phase 3 — Expose Remaining Server & Mesh Capabilities:**
   - Add in-place server/access-key replacement and management-key rotation, ASN **Update now**, Apple `on_demand` and IPsec PSK rotation, `Mesh` active invites list/revoke, node rename/`masquerade`/link throughput, `Clients` sorting + multi-key badges, inline rules validation, and refreshed `docs/images/` screenshots.

---

## Status in 0.9.1 (2026-11-26)

Added by the maintainers after the fixes. Each finding was re-checked in the code first, and the old 0.9.0 build was run through the same probes (all five P1 findings reproduced) before the new one. Details: `verification.md` (*v0.9.1*) and the CHANGELOG.

| ID | Verdict | What was done |
| --- | --- | --- |
| U1 | Real | Fixed: a server's page has its own header (tabs, one frosted status pill, one **New client**), and the app bar drops **Add server** while a server is open |
| U2 | Partly real | The bar did wrap at 1060 px, but it did not jump on connect (it was 120 px tall idle and connected). Fixed: one shared `AppBar`, 68 px and one row at 1060 px, idle and connected; finish and motion moved into an **Appearance** button |
| U3 | Real | Fixed: placeholders while loading, in place of "No matching clients" and bare loading text; errors show instead of a placeholder that never ends |
| U4 | Real (NewClient and RenameSheet partly) | Fixed: a one-at-a-time lock on every mutation button (a double click sent two `key_create` requests), resume after a failed second step in **NewClient** and **Add access key**, client **Save** only for edits, split **Save** waits for the policy (saving earlier overwrote it with "full") |
| U5 | Real | Fixed: an axis gutter for the scale labels, `useId()` gradient ids (only a problem with two charts on one page, which cannot happen today), **+N more** in `Bars`, time labels and tap selection in `BarChart` |
| U6 | Real | Fixed: no search box or empty tiles with no servers, a target pill while idle, "No matching servers" with **Clear search**, a clickable card and a **Split** badge |
| U7 | Real | Fixed: **Paste**, WireGuard and AmneziaWG text pasted as it is, a preview of protocol, host and port, the `.ovpn` note. **Deferred:** drag-and-drop (Tauri takes file drops before the page unless `dragDropEnabled` is off) |
| U8 | Real | Fixed: diagnostics as rows with **Copy report**, a **Service log** page from the `logs` action, and the tray tooltip follows the connection. **Not done:** an interactive tray menu |
| U9 | Real | Fixed: **Update key** (replace with the name kept), **Replace key** for saved servers, **Update the list now**, Apple **Connect automatically**, **Replace the group key**. **Not done:** rotating the management key from the app (needs a new `ServerOp` and a re-seal in the daemon) |
| U10 | Real | Fixed: open invites with **Revoke** and a note, node rename and masquerade, traffic and handshake age per link, mesh name and network. **Not done:** zoom and pan of the map |
| U11 | Partly real | Fixed: a sort control, protocol badges, inline validation of the rules and split lists (only exact mirrors of the Go and Rust validators block **Save**). **Not real:** "Tunnel address: Not needed" is correct for Shadowsocks and VLESS. **Deferred:** **Connect now** after **Use on this device** |
| U12 | Real | Fixed: `useUI().copy()` never throws and falls back to `execCommand`; avatar letters are cut by grapheme |
| U13 | Real | Fixed: `--fg-3` was 2.75 to 3.41 to 1 and is now at least 5.19 to 1 in all three finishes (measured by sampling pixels); first-field focus, Cancel focused in destructive confirmations, arrow keys in menus, toasts that last, close and pause, a sign-in field for a pasted link. Noted, not fixed: Daylight's warning yellow is about 2.4 to 1 on a light card |
| U14 | Partly done | Fixed: `BarChart` ticks and tap, the shared `AppBar`, the four screenshots in `docs/images/` retaken (0.9.1, bytes per second). **Not done:** React render tests (49 tests of pure modules were added) |

