# Roadmap (`deskdrop`)

## Where we are

`deskdrop` `v1.5.0` (`fc7cc3e`) is live in production on the GKE relay behind `/desktop` (`9` declarative `2.0` recipes hot-reloaded from `RECIPES_URI` and Vertex AI `adk-go` `healer` active on `global`) and on the Windows tray worker (`v1.5.0` auto-updated via `/desktop/v1/release/deskdrop.exe`). **Milestones `M1`, `M2`, `M4`, and `M5`**, **`M6.1`–`M6.3`**, and **`M7.1`–`M7.2`** (including live `GET /desktop/v1/drops/{id}/diff` verification) are shipped and verified (`M3.2` live Google Scholar drop removed as out of scope now). Only live Chrome-connected Tier-2 drift verification (`M6.4`) and publishing a `mode: "scroll"` recipe to `RECIPES_URI` (`M7.3`) remain open once Chrome remote debugging is reconnected on Windows. There is no `TODO.md`.

## Milestones at a glance

| # | Milestone | Done when | Target |
| :--- | :--- | :--- | :--- |
| **M6** | Live Tier-2 Drift Healing Verification | Reconnect Chrome on the `v1.5.0` Windows tray worker and verify fast `/fail` return + non-blocking Tier-2 fallback prompt live. | `v1.5.0+` (`P1`, Small) |
| **M7** | Live `RECIPES_URI` Scroll Recipe Rollout | Publish an infinite-scroll (`mode: "scroll"`) recipe to `RECIPES_URI` (now that `v1.5.0` server is live) and verify top-10/25 extraction + `/v1/drops/{id}/diff` end-to-end. | `v1.5.0+` (`P2`, Small) |

## Milestone M6 — Live Tier-2 Drift Healing Verification (`v1.5.0+`)

*Context:* `M6.1`–`M6.3` shipped in `v1.5.0` (`fc7cc3e`, Cloud Build `e0e46acb`), and the Windows tray worker auto-updated to `v1.5.0`. Only the interactive Chrome-connected Tier-2 run remains.

- [ ] **M6.4 (P1, 0.1d) — Verify live Chrome Tier-2 drift healing on `v1.5.0`:** Click `Connect to Open Chrome` in the `v1.5.0` Windows tray (`fallback_gate=tray`) and verify fast `/v1/tasks/{id}/fail` return (`healing_queued: true`) and non-blocking Tier-2 confirmation handling.

## Milestone M7 — Live `RECIPES_URI` Scroll Recipe Rollout (`v1.5.0+`)

*Context:* `M7.1`–`M7.2` shipped in `v1.5.0` (`fc7cc3e`, satisfying the `CRITICAL.md` §2 `ParseJSONStrict` server-first rollout order) and `GET /desktop/v1/drops/{id}/diff` is verified live (`HTTP 200` on `drop_f19d9d1847e66983` vs `drop_c0b689610ec38edc`).

- [ ] **M7.3 (P2, 0.25d) — Publish `mode: "scroll"` recipe to `RECIPES_URI` & verify live feed diff:** Upload a `mode: "scroll"` recipe to `RECIPES_URI` and verify live top-10/25 virtualized-feed extraction and `/v1/drops/{id}/diff` end-to-end once Chrome is connected.

## Unscheduled backlog

None currently queued (`A-11`, `S-13`, and `T-7` shipped in `v1.4.0`–`v1.4.3`; `M3.2` live Scholar check removed).

## Shipped

- **`v1.5.0` (`M6.1`–`M6.3` Async Drift Healer & Non-Blocking Tray + `M7.1`–`M7.2` Scroll Pagination & Drop Diffing, `2026-11-26`):** Shipped detached async server drift healing, pre-approval task lease heartbeats, non-blocking Windows tray `MessageBoxTimeoutW` coordinator, `mode: "scroll"` pagination, and `GET /v1/drops/{id}/diff` with top-N window-shift protection; verified live on GKE + Windows tray `v1.5.0` — [archive](archive/roadmap-history.md#v150-m61-m63-async-healer-tray-fallback-and-m71-m72-scroll-pagination-drop-diffing)

- **v1.4.0–v1.4.3 (`Milestone M5` — CI Smoke, E2E & Test/Arch Hardening, `2026-11-26`):** Shipped `FileStore` + `RECIPES_URI` container smoke (`M5.1`), `state.json` recovery tests (`M5.2`), compiled CLI → relay E2E & escape-analysis rules (`M5.3`), and all 13 `M5` test/CI/arch fixes (`T-1`–`T-6`, `A-8`–`A-10`, `S-9`–`S-12`) — [archive](archive/roadmap-history.md#v140-v143-milestone-m5-ci-smoke-e2e-test-and-architecture-hardening)
- **v1.4.0–v1.4.3 (`M2` Security Hardening, `M2.3` Live Check & Review `P3`s, `2026-11-26`):** Shipped all 15 `M2` security/runtime `P0`–`P1` fixes (`A-1`–`A-7`, `S-1`–`S-8`), 3 `P3` hygiene fixes (`A-11`, `S-13`, `T-7`), and `M2.5` Step 2; verified `M2.3` live on Windows tray `v1.4.3` (`drop_fc52c88259bb7737`) — [archive](archive/roadmap-history.md#v140-v143-m2-security-hardening-m23-live-check-and-review-p3s)
- **`v1.3.1` (`M4` Windows `UIA` Adapter & `AllowedDrivers` Queue Claiming, `2026-11-22`)** — Shipped `AllowedDrivers` task-claim filtering across worker and relay so `driver: "uia"` recipes run independently of Chrome CDP, added `RECIPES_URI` rollout-order invariant in `CRITICAL.md`, and verified live Windows `UIAutomationCore.dll` COM extraction end-to-end — [archive](archive/roadmap-history.md#milestone-m4-release-v131-windows-uia-adapter-cdp-independent-queue-claiming)
- **`v1.3.0` (`ADR-007` & `ADR-008`, `2026-11-22`)** — `502`/`503`/`504` rollout retry, `5xx` stderr logging & `/release/{asset}` `404`, numeric `FieldRule.fallback_regex` + 9-recipe `RECIPES_URI` sync, `reg.exe` `HKCU\...\Run` tray logon, `google-scholar-profile` contract, and Windows `UIA` `TargetDriver` adapter — [archive](archive/roadmap-history.md#release-v130-rollout-resilience-scholar-contract-windows-uia-adapter)
- **Legacy `browser-drop` Retirement (`M2.4`, `2026-11-22`)** — Archived legacy `browser-drop` repository and retired its PostgreSQL database after live Windows PC `v1.1.0` verification — [archive](archive/roadmap-history.md#m24-legacy-browser-drop-retirement-2026-11-22)
- **`v1.1.0` (`ADR-006`, `2026-11-22`)** — Exception-only `AXTree` hybrid grounding + `<canvas>`/WebGL blind-spot gate, bounded declarative `pagination` with pure-Go verifier parity, and `deskdrop schedule` (`schtasks.exe`) + `--quiet-cdp` — [archive](archive/roadmap-history.md#release-v110-axtree-grounding-declarative-pagination-windows-scheduler)
- **`v1.0.0` (`M1`, `2026-11-22`)** — Zero-dependency Go CDP CLI, `TargetDriver` Port, pure-Go HTML verifier (`0` `eval_js`), atomic JSON `FileStore`, hot-reloadable `RECIPES_URI`, `adk-go` exception healer, and live `/desktop` cutover — [archive](archive/roadmap-history.md#milestone-m1-public-v100-release-live-desktop-cutover)
- **Milestone M2 — Rollout Resilience, FieldRule Verification & HKCU Logon (`v1.4.3+`):** Verified `HKCU\...\Run` logon (`M2.4`) and live post-`v1.4.3` `503 UH` rollout (`M2.5`, `4x status` / `0` fan-out) — [archive](archive/roadmap-history.md#milestone-m2---rollout-resilience-fieldrule-verification-hkcu-logon-v143)
