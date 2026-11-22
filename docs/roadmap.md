# Roadmap (`deskdrop`)

## Where we are

`deskdrop` `v1.1.0` is live in production behind `/desktop` (with legacy `browser-drop` and its PostgreSQL database retired), running a zero-dependency desktop CLI (`cmd/deskdrop`), bounded declarative `"Next page"` `pagination`, atomic JSON `FileStore` relay (`cmd/server`), hot-reloadable `RECIPES_URI` catalog, and exception-only `adk-go` (`google.golang.org/adk/v2`) drift healer with hybrid `AXTree + dom.html` grounding and `<canvas>`/WebGL blind-spot detection. Once Milestone `M2` (`v1.1.1`) ships, `deskdrop` enters steady-state `Keep` mode (`0` scheduled milestones). There is no `TODO.md`.

## Milestones at a glance

| # | Milestone | Done when | Target |
| :--- | :--- | :--- | :--- |
| **M2** | Server Observability, FieldRule Fallback & HKCU Logon Startup | `5xx` stderr logging, `/release/{asset}` `404` + `blobs.go` cleanup, numeric `fallback_regex` + versioned `RECIPES_URI` sync, and non-Admin `HKCU\...\Run` tray logon registration (`ADR-007`) shipped and verified on the Windows PC tray worker. | `v1.1.1` (`P0`, Small) |

## Milestone M2 — Server Observability, FieldRule Fallback & HKCU Logon Startup

- [ ] **M2.1 (`5xx` stderr logging, release-asset `404` & `blobs.go` cleanup):** Log `status >= 500` responses (`method`, `path`, `status`, `code`, `message`) to stderr in `writeAPIError` ([`internal/server/http_helpers.go`](../internal/server/http_helpers.go)) so container error logs surface server faults, return `404 NOT_FOUND` directly from `handleDownloadRelease` ([`internal/server/http.go`](../internal/server/http.go)) when `/release/{asset}` is absent on disk instead of probing blob storage, and remove dead `"tools/"` prefix-bypass logic in `GCSBlobStore.objectName` ([`internal/server/blobs.go`](../internal/server/blobs.go)). *(Deploy prerequisite: land client-side `502`/`503`/`504` backoff in the message-board caller first to absorb single-replica RWO-PVC `Recreate` rollouts.)*
- [ ] **M2.2 (Numeric `FieldRule.fallback_regex` fallback & versioned `RECIPES_URI` sync):** When `FieldRule.Parse` is `"number"` or `"float"` and `raw` fails compact number parsing (e.g. `"View analytics"` or `aria-label="Open reactions menu"`), fall back to `fallback_regex` against `scope` in `evalFieldRule` ([`internal/driver/cdp/js_runtime.go`](../internal/driver/cdp/js_runtime.go) and [`internal/verify/verify.go`](../internal/verify/verify.go)) with JS/Go parity unit tests; check the live declarative `RECIPES_URI` catalog into version control (replacing stale pre-cutover files), update `impressions`/`reactions` rules, and verify non-zero per-item counts live.
- [ ] **M2.3 (Replace `schtasks.exe` scheduled `sync` with non-Admin `HKCU\...\Run` tray logon):** Delete periodic scheduled `sync` (`schtasks.exe`, `--mode`, `--every`, and `sync`'s `--hide-console`/`--quiet-cdp` flags across [`cmd/deskdrop/schedule.go`](../cmd/deskdrop/schedule.go), [`cmd/deskdrop/schedule_test.go`](../cmd/deskdrop/schedule_test.go), and [`cmd/deskdrop/main.go`](../cmd/deskdrop/main.go)) and replace `deskdrop schedule install|uninstall|status` with direct `reg.exe` `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` tray logon registration (zero shell interpolation). **Preserve** `hideConsoleWindow()` in [`cmd/deskdrop/tray_windows.go`](../cmd/deskdrop/tray_windows.go) (so the dual CLI/tray console-subsystem binary hides its console window at logon) and `SessionPool.ProbeQuiet` in [`internal/driver/cdp/pool.go`](../internal/driver/cdp/pool.go) (used by the 6s passive tray status probe). Sync [`CRITICAL.md`](../CRITICAL.md), [`AGENTS.md`](../AGENTS.md), [`README.md`](../README.md), and append `ADR-007` superseding `ADR-006` §3 in [`DECISIONS.md`](../DECISIONS.md).

## Unscheduled backlog

- **Native Windows UI Automation (`UIA`) Adapter (gated on concrete non-browser target):** Second `TargetDriver` Adapter inspecting Windows semantic accessibility trees behind the same declarative `Recipe` and `Expect` Contracts. Held unscheduled (`Keep` freeze after `v1.1.1`) until a concrete Windows-native desktop workflow cannot be reached via Browser CDP, due to pure-stdlib Go (`CGO_ENABLED=0`) `UIAutomationCore.dll` COM vtable complexity (~800+ LOC) and Linux CI hardware limits.

## Shipped

- **Legacy `browser-drop` Retirement (`M2.4`, `2026-11-22`)** — Archived legacy `browser-drop` repository and retired its PostgreSQL database after live Windows PC `v1.1.0` verification — [archive](archive/roadmap-history.md#m24-legacy-browser-drop-retirement-2026-11-22)
- **`v1.1.0` (`ADR-006`, `2026-11-22`)** — Exception-only `AXTree` hybrid grounding + `<canvas>`/WebGL blind-spot gate, bounded declarative `pagination` with pure-Go verifier parity, and `deskdrop schedule` (`schtasks.exe`) + `--quiet-cdp` — [archive](archive/roadmap-history.md#release-v110-axtree-grounding-declarative-pagination-windows-scheduler)
- **`v1.0.0` (`M1`, `2026-11-22`)** — Zero-dependency Go CDP CLI, `TargetDriver` Port, pure-Go HTML verifier (`0` `eval_js`), atomic JSON `FileStore`, hot-reloadable `RECIPES_URI`, `adk-go` exception healer, and live `/desktop` cutover — [archive](archive/roadmap-history.md#milestone-m1-public-v100-release-live-desktop-cutover)
