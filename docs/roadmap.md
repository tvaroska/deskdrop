# Roadmap (`deskdrop`)

## Where we are

`deskdrop` `v1.3.1` (`ADR-007`, `ADR-008`) is live in production behind `/desktop` with the 9-recipe declarative `2.0` catalog hot-reloaded in `RECIPES_URI` and the Windows PC tray worker auto-updated to `v1.3.1`. **Milestone `M4`** (native Windows `UIA` `TargetDriver` adapter + CDP-independent `driver: "uia"` queue claiming) and server/relay resilience (`M2.1`, `M2.2`, `M3.1`) are shipped and verified live end-to-end on the Windows PC; remaining items cover interactive Chrome/registry checks (`M2.3`, `M2.4`, `M3.2`) once Chrome remote debugging is approved on the Windows desktop. There is no `TODO.md`.

## Milestones at a glance

| # | Milestone | Done when | Target |
| :--- | :--- | :--- | :--- |
| **M2** | Rollout Resilience, Server Observability, FieldRule Fallback & HKCU Logon | Live `linkedin-analytics` drop confirms non-zero per-item `impressions`/`reactions` via `fallback_regex` and `deskdrop.exe schedule install` registers `HKCU\...\Run\DeskDrop` on the Windows PC. | `v1.3.2` (`P0`, Small) |
| **M3** | Google Scholar Profile & Recommended Papers Declarative Recipe | Live `$0`-token `google-scholar-profile` drop completes cleanly against an open Google Scholar Chrome tab on the Windows PC. | `v1.3.2` (`P2`, Small) |
| **M5** | Production-Mode CI Smoke, `state.json` Compatibility & Relay E2E | `cloudbuild.yaml` smoke tests `FileStore` + `RECIPES_URI` + restart persistence and compiled CLI → relay loopback E2E, legacy `state.json` fixtures pass, and `AGENTS.md` enforces escape analysis. | `v1.4.0` (`P1`, Small) |

## Milestone M2 — Rollout Resilience, Server Observability, FieldRule Fallback & HKCU Logon (`v1.3.2`)

*Bottleneck / rollout order:* Single-replica `Recreate` RWO-PVC rollouts produce a `15–60s` `HTTP 503` window while re-attaching `/data`; `M2.1`–`M2.2` shipped, and Chrome 144+ `DevToolsActivePort` requires a one-time interactive `"Allow"` click after the Windows tray binary restarts on self-update.

- [ ] **M2.3 (Live Windows PC verification of `FieldRule.fallback_regex` on `linkedin-analytics` — deployed in `v1.3.1`, task queued):** Engine parity ([`internal/driver/cdp/js_runtime.go`](../internal/driver/cdp/js_runtime.go), [`internal/verify/verify.go`](../internal/verify/verify.go), [`internal/contract/semantic_eval.go`](../internal/contract/semantic_eval.go)) and `RECIPES_URI` catalog sync (`9` recipes) are live; verify queued `linkedin-analytics` drop extracts non-zero per-item `impressions` and `reactions` once Chrome remote debugging is approved on the Windows PC. *Evidence 2026-11-23:* latest drop (`2026-11-22T16:03:52Z`, 20 posts, 2,217 followers) still has `impressions=0`, `reactions=0`, `reposts=0` on all 8 posts matched by `brand_stats_sync.py sync --dry-run`, while `comments` parse fine (1–3). First check which worker version and recipe produced that drop: if it was `v1.3.1` + the synced catalog, `fallback_regex` is not matching the live `recent-activity` card text and needs a fix before the next re-request.
- [ ] **M2.4 (Run `deskdrop.exe schedule install` `HKCU\...\Run` logon registration on Windows PC — deployed in `v1.3.1`):** `reg.exe` `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` tray logon implementation (`ADR-007`, [`cmd/deskdrop/schedule.go`](../cmd/deskdrop/schedule.go)) is live on the Windows worker; run `deskdrop.exe schedule install --fallback-gate tray` + `deskdrop.exe schedule status` on the Windows PC to verify registry logon persistence.

## Milestone M3 — Google Scholar Profile & Recommended Papers Declarative Recipe (`v1.3.2`)

*Bottleneck / kill risk:* Google Scholar aggressively rate-limits and CAPTCHA-gates cold automated navigation. Never attempt CAPTCHA bypass; enforce single-page `reuse_open_tab: true` attachment and fail closed into the `AUTH_EXPIRED` domain circuit breaker on login/sorry/challenge redirects.

- [ ] **M3.2 (Live Windows Chrome verification of `google-scholar-profile`):** Verify a clean `$0`-token `google-scholar-profile` drop against an open Google Scholar profile tab in the Windows Chrome session (`--var user_id=<scholar_id>` or open `/citations` tab).

## Milestone M5 — Production-Mode CI Smoke, `state.json` Compatibility & Relay E2E (`v1.4.0`)

Close the CI production-mode parity and state-evolution gaps identified in the 2026-11-22 GKE testing audit.

- [ ] **M5.1 (P1, 0.25d) — Production-mode `FileStore` + `RECIPES_URI` container smoke in `cloudbuild.yaml`:** Update `verify-nonroot-and-healthcheck` in [`../cloudbuild.yaml`](../cloudbuild.yaml) to run `deskdrop:ci` with `STORE_BACKEND=file` (writable `/data` mount) and a real `RECIPES_URI` fixture file instead of memory-only defaults, verify authenticated `/desktop/v1/tasks` enqueue/list, restart the container, and assert state persists across restart. Accept: `cloudbuild.yaml` smoke tests `FileStore` persistence and recipe loading.
- [ ] **M5.2 (P2, 0.25d) — Legacy `state.json` schema-compatibility & corrupt-file recovery tests:** Add versioned fixture tests in [`../internal/server/store_file_test.go`](../internal/server/store_file_test.go) verifying older `state.json` snapshots load cleanly without dropping messages/tasks/proposals and truncated or corrupt temp files fail safely without clobbering good state. Accept: `go test -race ./...` passes with backward-compatibility and corruption recovery cases.
- [ ] **M5.3 (P2, 0.5d) — Compiled CLI → Relay loopback E2E in CI & escape-analysis checklist:** Add an automated in-CI E2E test running the compiled `deskdrop` binary against a local drift fixture + live `deskdrop:ci` relay (`enqueue -> claim -> drop -> bus message`) and add the E2E/prod → unit escape-analysis rule to [`../AGENTS.md`](../AGENTS.md). Accept: Cloud Build runs the compiled CLI → relay E2E flow and `AGENTS.md` documents escape analysis.

## Unscheduled backlog

*None — all current backlog items are scheduled in `M2`–`M3` above.*

## Shipped

- **`v1.3.1` (`M4` Windows `UIA` Adapter & `AllowedDrivers` Queue Claiming, `2026-11-22`)** — Shipped `AllowedDrivers` task-claim filtering across worker and relay so `driver: "uia"` recipes run independently of Chrome CDP, added `RECIPES_URI` rollout-order invariant in `CRITICAL.md`, and verified live Windows `UIAutomationCore.dll` COM extraction end-to-end — [archive](archive/roadmap-history.md#milestone-m4-release-v131-windows-uia-adapter-cdp-independent-queue-claiming)
- **`v1.3.0` (`ADR-007` & `ADR-008`, `2026-11-22`)** — `502`/`503`/`504` rollout retry, `5xx` stderr logging & `/release/{asset}` `404`, numeric `FieldRule.fallback_regex` + 9-recipe `RECIPES_URI` sync, `reg.exe` `HKCU\...\Run` tray logon, `google-scholar-profile` contract, and Windows `UIA` `TargetDriver` adapter — [archive](archive/roadmap-history.md#release-v130-rollout-resilience-scholar-contract-windows-uia-adapter)
- **Legacy `browser-drop` Retirement (`M2.4`, `2026-11-22`)** — Archived legacy `browser-drop` repository and retired its PostgreSQL database after live Windows PC `v1.1.0` verification — [archive](archive/roadmap-history.md#m24-legacy-browser-drop-retirement-2026-11-22)
- **`v1.1.0` (`ADR-006`, `2026-11-22`)** — Exception-only `AXTree` hybrid grounding + `<canvas>`/WebGL blind-spot gate, bounded declarative `pagination` with pure-Go verifier parity, and `deskdrop schedule` (`schtasks.exe`) + `--quiet-cdp` — [archive](archive/roadmap-history.md#release-v110-axtree-grounding-declarative-pagination-windows-scheduler)
- **`v1.0.0` (`M1`, `2026-11-22`)** — Zero-dependency Go CDP CLI, `TargetDriver` Port, pure-Go HTML verifier (`0` `eval_js`), atomic JSON `FileStore`, hot-reloadable `RECIPES_URI`, `adk-go` exception healer, and live `/desktop` cutover — [archive](archive/roadmap-history.md#milestone-m1-public-v100-release-live-desktop-cutover)

