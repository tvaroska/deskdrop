# Roadmap (`deskdrop`)

## Where we are

`deskdrop` `v1.1.0` is live in production behind `/desktop` with a zero-dependency desktop CLI (`cmd/deskdrop`), bounded declarative `"Next page"` `pagination`, direct `schtasks.exe` Windows Task Scheduler helper (`deskdrop schedule`), atomic JSON `FileStore` relay (`cmd/server`), hot-reloadable `RECIPES_URI` catalog, and exception-only `adk-go` (`google.golang.org/adk/v2`) drift healer with hybrid `AXTree + dom.html` grounding and `<canvas>`/WebGL blind-spot detection. There is no `TODO.md`.

## Milestones at a glance

| # | Milestone | Done when | Target |
| :--- | :--- | :--- | :--- |
| **M2** | Server Observability & Post-Soak Legacy Retirement | `5xx` stderr logging + release `404` cleanup shipped, Windows PC `deskdrop.exe` soak verified, and legacy `browser-drop` retired. | `v1.1.1` (`P0`, Small) |

## Milestone M2 — Server Observability & Post-Soak Legacy Retirement

- [ ] **M2.1 (`5xx` stderr logging & release-asset `404` cleanup):** Log `status >= 500` responses to stderr in `writeAPIError` ([`internal/server/http_helpers.go`](../internal/server/http_helpers.go)) so container error logs surface server faults, and return `404 NOT_FOUND` directly from `handleDownloadRelease` ([`internal/server/http.go`](../internal/server/http.go)) when `/release/{asset}` is absent on disk instead of probing `tools/deskdrop/latest/` in blob storage.
- [ ] **M2.2 (Windows PC soak & legacy cleanup):** Switch the Windows PC worker to `deskdrop.exe` (`--fallback-gate=tray` or `deskdrop schedule install`), verify one live extraction cycle end-to-end, and retire legacy `browser-drop` + its PostgreSQL database.

## Unscheduled backlog

- **Google Scholar Profile & Recommended Papers Recipe (`google-scholar-profile`):** External declarative CDP recipe for citation metrics, per-paper stats, and recommended articles, backed by scrubbed HTML snapshot tests and `AUTH_EXPIRED` detection.
- **Native Windows UI Automation (`UIA`) Adapter:** Second `TargetDriver` Adapter inspecting Windows semantic accessibility trees behind the same declarative `Recipe` and `Expect` Contracts.

## Shipped

- **`v1.1.0` (`ADR-006`, `2026-11-22`)** — Exception-only `AXTree` hybrid grounding + `<canvas>`/WebGL blind-spot gate, bounded declarative `pagination` with pure-Go verifier parity, and `deskdrop schedule` (`schtasks.exe`) + `--quiet-cdp` — [archive](archive/roadmap-history.md#release-v110-axtree-grounding-declarative-pagination-windows-scheduler)
- **`v1.0.0` (`M1`, `2026-11-22`)** — Zero-dependency Go CDP CLI, `TargetDriver` Port, pure-Go HTML verifier (`0` `eval_js`), atomic JSON `FileStore`, hot-reloadable `RECIPES_URI`, `adk-go` exception healer, and live `/desktop` cutover — [archive](archive/roadmap-history.md#milestone-m1-public-v100-release-live-desktop-cutover)
