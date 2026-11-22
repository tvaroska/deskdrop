# Roadmap (`deskdrop`)

## Where we are

`deskdrop` `v1.1.0` is live in production behind `/desktop` with a zero-dependency desktop CLI (`cmd/deskdrop`), bounded declarative `"Next page"` `pagination`, direct `schtasks.exe` Windows Task Scheduler helper (`deskdrop schedule`), atomic JSON `FileStore` relay (`cmd/server`), hot-reloadable `RECIPES_URI` catalog, and exception-only `adk-go` (`google.golang.org/adk/v2`) drift healer with hybrid `AXTree + dom.html` grounding and `<canvas>`/WebGL blind-spot detection. There is no `TODO.md`.

## Milestones at a glance

| # | Milestone | Done when | Target |
| :--- | :--- | :--- | :--- |
| **M2** | Server Observability, FieldRule Fallback & Post-Soak Retirement | `5xx` stderr logging, numeric `fallback_regex` + `RECIPES_URI` sync, non-Admin Windows `HKCU\...\Run` logon install, and legacy `browser-drop` retirement shipped. | `v1.1.1` (`P0`, Small) |

## Milestone M2 — Server Observability, FieldRule Fallback & Post-Soak Retirement

- [ ] **M2.1 (`5xx` stderr logging & release-asset `404` cleanup):** Log `status >= 500` responses to stderr in `writeAPIError` ([`internal/server/http_helpers.go`](../internal/server/http_helpers.go)) so container error logs surface server faults, and return `404 NOT_FOUND` directly from `handleDownloadRelease` ([`internal/server/http.go`](../internal/server/http.go)) when `/release/{asset}` is absent on disk instead of probing `tools/deskdrop/latest/` in blob storage.
- [ ] **M2.2 (Numeric `FieldRule.fallback_regex` fallback & `RECIPES_URI` recipe sync):** When `FieldRule.Parse` is `"number"` or `"float"` and `raw` contains no parseable number (e.g. `"View analytics"` or `aria-label="Open reactions menu"`), fall back to `fallback_regex` in `evalFieldRule` ([`internal/driver/cdp/js_runtime.go`](../internal/driver/cdp/js_runtime.go) and [`internal/verify/verify.go`](../internal/verify/verify.go)) with parity unit tests, update `impressions`/`reactions` rules in `RECIPES_URI`, and verify non-zero per-item counts live.
- [ ] **M2.3 (Non-Admin Windows logon startup in `deskdrop schedule`):** Register `--mode logon` in `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` ([`cmd/deskdrop/schedule.go`](../cmd/deskdrop/schedule.go)) via `reg.exe` instead of `schtasks.exe /Create /SC ONLOGON` so standard non-elevated PowerShell installs persistent tray auto-start without UAC `Access is denied` errors, with matching `uninstall` and `status` support (keeping `schtasks.exe` for headless `--mode sync`).
- [ ] **M2.4 (Post-soak legacy `browser-drop` retirement):** After verifying the patched extraction cycle on the Windows PC tray worker (`win-pc` already connected on `v1.1.0`), retire legacy `browser-drop` and its PostgreSQL database.

## Unscheduled backlog

- **Google Scholar Profile & Recommended Papers Recipe (`google-scholar-profile`):** External declarative CDP recipe for citation metrics, per-paper stats, and recommended articles, backed by scrubbed HTML snapshot tests and `AUTH_EXPIRED` detection.
- **Native Windows UI Automation (`UIA`) Adapter:** Second `TargetDriver` Adapter inspecting Windows semantic accessibility trees behind the same declarative `Recipe` and `Expect` Contracts.

## Shipped

- **`v1.1.0` (`ADR-006`, `2026-11-22`)** — Exception-only `AXTree` hybrid grounding + `<canvas>`/WebGL blind-spot gate, bounded declarative `pagination` with pure-Go verifier parity, and `deskdrop schedule` (`schtasks.exe`) + `--quiet-cdp` — [archive](archive/roadmap-history.md#release-v110-axtree-grounding-declarative-pagination-windows-scheduler)
- **`v1.0.0` (`M1`, `2026-11-22`)** — Zero-dependency Go CDP CLI, `TargetDriver` Port, pure-Go HTML verifier (`0` `eval_js`), atomic JSON `FileStore`, hot-reloadable `RECIPES_URI`, `adk-go` exception healer, and live `/desktop` cutover — [archive](archive/roadmap-history.md#milestone-m1-public-v100-release-live-desktop-cutover)
