# Roadmap (`deskdrop`)

## Where we are

`deskdrop` `v1.0.0` is live in production behind `/desktop` with a zero-dependency desktop CLI (`cmd/deskdrop`), atomic JSON `FileStore` relay (`cmd/server`), hot-reloadable `RECIPES_URI` catalog, and exception-only `adk-go` (`google.golang.org/adk/v2`) drift healer.

**Implemented locally (pending `v1.1.0` release):**
- Compact, privacy-redacted CDP `Accessibility.getFullAXTree` (`axtree.txt`) exception grounding paired with `dom.html`, deterministic `<canvas>`/WebGL blind-spot detection (`UNLABELLED_CANVAS_SURFACE`), and offline compression/selector/canvas benchmarks (`internal/healer/axtree_bench_test.go`).
- Bounded declarative `"Next page"` `pagination` (`1..20` pages, disabled/`aria-disabled` stop, same-domain lock, cross-page item & table-row deduplication) with pure-Go multi-page verifier parity (`internal/verify/paginate.go`).
- Direct `schtasks.exe` Windows Task Scheduler helper (`deskdrop schedule install|uninstall|status`) and quiet scheduled `sync` (`--quiet-cdp`, `--hide-console`).

There is no `TODO.md`.

## Milestones at a glance

| # | Milestone | Done when | Target |
| :--- | :--- | :--- | :--- |
| **M2** | `v1.1.0` Release, Server Observability & Post-Soak Retirement | `v1.1.0` (`AXTree`, `pagination`, `schedule`, `5xx` stderr logging, release `404`) live on `/desktop`, Windows PC soak verified, and legacy `browser-drop` retired. | `v1.1.0` (`P0`, Small) |

## Milestone M2 — `v1.1.0` Release, Server Observability & Post-Soak Retirement

- [ ] **M2.1 (`v1.1.0` release):** Commit, tag, and roll out `deskdrop v1.1.0` (`AXTree` hybrid grounding + `<canvas>` blind-spot gate, declarative `pagination`, and `deskdrop schedule` Windows Task Scheduler helper) via Cloud Build (`cloudbuild.yaml`) and verify `/v1/release/SHA256SUMS.txt`.
- [ ] **M2.2 (`5xx` stderr logging & release-asset `404` cleanup):** Log `status >= 500` responses to stderr in `writeAPIError` ([`internal/server/http_helpers.go`](../internal/server/http_helpers.go)) so container error logs surface server faults, and return `404 NOT_FOUND` directly from `handleDownloadRelease` ([`internal/server/http.go`](../internal/server/http.go)) when `/release/{asset}` is absent on disk instead of probing `tools/deskdrop/latest/` in blob storage.
- [ ] **M2.3 (Windows PC soak & legacy cleanup):** Switch the Windows PC worker to `deskdrop.exe` (`--fallback-gate=tray` or `deskdrop schedule install`), verify one live extraction cycle end-to-end, and retire legacy `browser-drop` + its PostgreSQL database.

## Unscheduled backlog

- **Google Scholar Profile & Recommended Papers Recipe (`google-scholar-profile`):** External declarative CDP recipe for citation metrics, per-paper stats, and recommended articles, backed by scrubbed HTML snapshot tests and `AUTH_EXPIRED` detection.
- **Native Windows UI Automation (`UIA`) Adapter:** Second `TargetDriver` Adapter inspecting Windows semantic accessibility trees behind the same declarative `Recipe` and `Expect` Contracts.

## Shipped

- **`v1.0.0` (`M1`, `2026-11-22`)** — Zero-dependency Go CDP CLI, `TargetDriver` Port, pure-Go HTML verifier (`0` `eval_js`), atomic JSON `FileStore`, hot-reloadable `RECIPES_URI`, `adk-go` exception healer, and live `/desktop` cutover — [archive](archive/roadmap-history.md#milestone-m1-public-v100-release-live-desktop-cutover)
