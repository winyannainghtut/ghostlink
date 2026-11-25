# Ghostlink web UI

React, TypeScript, Tailwind CSS and Lucide icons, built with Vite.

| Folder | What |
|---|---|
| `shared/` | The glass component library: surfaces, pill controls, tabs, the switch, sheets, popovers, toasts, placeholders, charts, the glass-wave background, the mesh map, the navigation bar (`AppBar`, used by both apps) and the appearance menu |
| `admin/` | The server-administration views (overview, clients, network, mesh, settings, new client) and their parts, such as the speed limit control (`admin/SpeedLimit.tsx`). They are used by both apps through a transport (`admin/api.ts`) |
| `dashboard/` | The server dashboard shell. Its transport is this server's `/api` |
| `desktop/` | The desktop app: My servers, Servers I run (the admin views over the local service) and Settings. `desktop/src/routes.ts` maps the admin views' requests onto the service's `server_call` operations |
| `tests/` | `npm test`: the desktop mapping against the Rust `ServerOp` contract, the formatting (`shared/format.ts`), speed and data limits and the client settings form (`admin/limits.ts`), the protocol new clients and keys start with, the firewall hint after turning a protocol on and the Apple profile address (`admin/protocols.ts`), the client order and protocol badges (`admin/clients.ts`), the line-by-line checks of the rules and split-tunneling lists (`admin/ruleCheck.ts`), chart layout and "+N more" (`shared/chart.ts`), one-at-a-time buttons and resumable flows (`shared/lock.ts`, `shared/steps.ts`), clipboard, avatar letters and toast timing (`shared/clipboard.ts`, `shared/text.ts`, `shared/toast.ts`), the key preview of Add access key (`desktop/src/keyPreview.ts`), the diagnostics rows and service log (`desktop/src/diagnostics.ts`), the pasted dashboard login link (`dashboard/src/loginLink.ts`), and the desktop app's local IDs and failure messages. Tests import plain `.ts` modules without imports, so pure logic lives in such modules rather than in `.tsx` files |

```sh
cd ui
npm ci --ignore-scripts      # exact versions from package-lock.json
npm run build                # typecheck, then build the dashboard and the desktop UI
npm test                     # the request mapping, formatting, limits, protocols and desktop helpers
GHOSTLINK_DASHBOARD=http://127.0.0.1:8080 npm run dev:dashboard   # live reload against a real dashboard
```

**Output.** The dashboard is built into `core/outline-worker/internal/server/dashboard/`, where `ghostlink-server` embeds it. The desktop app is built into `apps/desktop/ui/`, which Tauri bundles; rebuild the Go server or the Tauri app afterwards to pick up the new files.
- The build is committed, so the Go module builds without Node.
- The **Web UI** workflow rebuilds both and fails if the committed files differ.
- Don't edit the files there; change `ui/` and rebuild.

**Content-Security-Policy.** The dashboard's CSP allows scripts, styles and connections only from its own origin, with no inline scripts or style attributes. Images may also be `data:` or `blob:` URLs: QR codes are shown as `data:image/svg+xml` (`svgURL` in `admin/api.ts`). The desktop app's CSP (`apps/desktop/src-tauri/tauri.conf.json`) allows `data:` images for the same reason. So:
- style with classes, not `style="…"`. React's `style` prop is applied through the CSSOM, which the CSP allows; use it only for computed positions.
- keep fonts and icons bundled. The fonts are Plus Jakarta Sans and JetBrains Mono (SIL OFL 1.1, Latin subsets), self-hosted from `@fontsource-variable`. Apple devices use San Francisco first.
- `assetsInlineLimit` is 0, so no bundled file becomes a `data:` URI.

`internal/server/dashboard_test.go` checks the embedded build: a single script and stylesheet, flat file names, and no inline code.

**Design rules.**
- Frosted surfaces (`.glass`, 28 px radius; inner panels 24 px) over three slow glass waves. The waves can be set to Subtle, Calm or Still, and are still when the system asks for reduced motion.
- Three finishes: Obsidian `#090A0D`, Titanium `#111318` and Daylight `#F2F4F8`. Each is a set of CSS variables on `<html data-finish>`.
- Sentence-case labels; no uppercase, letter-spaced micro-labels.
- Monospace (`.mono`) only for addresses, CIDRs, keys, ports and interface names.
- Tabular figures (`.num`) for every live number.
- Layouts use the whole window, up to 2400 px. On wide screens the card grids add columns. Use rem breakpoints (`min-[112.5rem]:`), because Tailwind sorts px breakpoints before its rem ones.
- A frosted element can't blur through a frosted parent. Render floating panels that must blur the page (such as the Island's) in a portal.

**Kit notes.**
- `cx()` only joins class names, and a kit component's own utilities often come later in the stylesheet than the caller's. To override one (a card's padding, a radius), add `!`: `p-0!`, `sm:p-5!`.
- Sheets are modal `<dialog>`s. Mark the field to focus first with `data-autofocus`: React's `autoFocus` runs before the dialog opens. A sheet with nothing to type focuses its main (`cta`) footer button; a confirmation of something destructive focuses Cancel. Popovers focus their first item (or the one marked `data-autofocus`), move with the arrow keys and give the focus back.
- Toasts are shown inside the topmost open sheet, above its backdrop. They stay as long as their text needs (`shared/toast.ts`), pause while the pointer or the keyboard is on them, and have a close button. Copy with `useUI().copy(text, done)`: it never throws, tries the older `execCommand` way when the clipboard API refuses, and says in a toast which happened.
- A button that changes something runs through `useBusy()` (`shared/hooks.ts`): one run at a time, even for two clicks in the same instant, and the button shows its "Saving…" label. A flow of dependent requests (a client, then its key; a saved key, then its name) keeps the results of the steps that worked with `step()` (`shared/steps.ts`), so a retry resumes where it failed.
- Show placeholders (`Skeleton`, `CardSkeleton`, wrapped in `Loading`) of the finished layout while data is on its way, never an empty state that is not true yet. `Bars` takes `undefined` for "loading" and `null` or `[]` for "nothing".
- The navigation bar is `AppBar`: one row from 1024 px up, also at the desktop app's 1060 px window. Its middle takes what the sides leave; the live speeds in the status capsule show only when the middle is at least 820 px wide (a container query, `@min-[820px]:`), so they never push the bar onto a second row. A page header under the bar (a server's workspace) is not a second bar: `Tabs` (underlined) and the frosted `Island tone="glass"`.
- Charts keep a gutter on the right for their scale labels (`axisGutter`), and give each gradient an id from `useId()`.
