# Roadmap (`deskdrop`)

## Where we are

`deskdrop` `v1.5.1` (`f32afaa`) is live in production on the GKE relay behind `/desktop` (`10` declarative `2.0` recipes hot-reloaded from `RECIPES_URI` and Vertex AI `adk-go` `healer` active on `global`, serving `/desktop/v1/release/deskdrop.exe` `v1.5.1` for automatic SHA-256 tray self-update). **Milestones `M1`, `M2`, `M4`, `M5`, `M6`, `M7`, and `M8`** are 100% shipped and verified live (`M3.2` live Google Scholar drop removed as out of scope; `28/28 = 100.0%` non-control mutation kill rate on `M8`). No active milestones are currently queued. There is no `TODO.md`.

## Milestones at a glance

| # | Milestone | Done when | Target |
| :--- | :--- | :--- | :--- |

## Unscheduled backlog

- None currently scheduled.

## Shipped

- **`v1.5.1` (`Milestone M8` — Sanitizer, UIA & Contract Hardening, `2026-11-27`):** Closed both `P0`s (`ScrubHTML` quote-swallow/recombination bypass, Windows `UIA` partial-tree silence) and all `14` `P1` air-gap, number/selector parity, CDP launch/latch, `HKCU\...\Run` quoting, bounded stdin, error logging, OpenAPI, and Node loopback E2E gaps (`28/28 = 100.0%` non-control mutant kill rate), deployed live in `v1.5.1` — [archive](archive/roadmap-history.md#v151-milestone-m8-sanitizer-uia-and-contract-hardening)

- **`v1.5.0` Live Verification (`M6.4` & `M7.3`, `2026-11-26`):** Verified live Chrome-connected Tier-2 drift healing (`healing_queued: true` + non-blocking tray confirmation) on the `v1.5.0` Windows worker and published a live `mode: "scroll"` virtualized-feed recipe to `RECIPES_URI` (`10` approved recipes) with end-to-end `/v1/drops/{id}/diff` verification — [archive](archive/roadmap-history.md#v150-m64-and-m73-live-tier-2-healing-and-scroll-recipe-verification)
- **`v1.5.0` (`M6.1`–`M6.3` Async Drift Healer & Non-Blocking Tray + `M7.1`–`M7.2` Scroll Pagination & Drop Diffing, `2026-11-26`):** Shipped detached async server drift healing, pre-approval task lease heartbeats, non-blocking Windows tray `MessageBoxTimeoutW` coordinator, `mode: "scroll"` pagination, and `GET /v1/drops/{id}/diff` with top-N window-shift protection; verified live on GKE + Windows tray `v1.5.0` — [archive](archive/roadmap-history.md#v150-m61-m63-async-healer-tray-fallback-and-m71-m72-scroll-pagination-drop-diffing)

- **v1.4.0–v1.4.3 (`Milestone M5` — CI Smoke, E2E & Test/Arch Hardening, `2026-11-26`):** Shipped `FileStore` + `RECIPES_URI` container smoke (`M5.1`), `state.json` recovery tests (`M5.2`), compiled CLI → relay E2E & escape-analysis rules (`M5.3`), and all 13 `M5` test/CI/arch fixes (`T-1`–`T-6`, `A-8`–`A-10`, `S-9`–`S-12`) — [archive](archive/roadmap-history.md#v140-v143-milestone-m5-ci-smoke-e2e-test-and-architecture-hardening)
- **v1.4.0–v1.4.3 (`M2` Security Hardening, `M2.3` Live Check & Review `P3`s, `2026-11-26`):** Shipped all 15 `M2` security/runtime `P0`–`P1` fixes (`A-1`–`A-7`, `S-1`–`S-8`), 3 `P3` hygiene fixes (`A-11`, `S-13`, `T-7`), and `M2.5` Step 2; verified `M2.3` live on Windows tray `v1.4.3` (`drop_fc52c88259bb7737`) — [archive](archive/roadmap-history.md#v140-v143-m2-security-hardening-m23-live-check-and-review-p3s)
- **`v1.3.1` (`M4` Windows `UIA` Adapter & `AllowedDrivers` Queue Claiming, `2026-11-22`)** — Shipped `AllowedDrivers` task-claim filtering across worker and relay so `driver: "uia"` recipes run independently of Chrome CDP, added `RECIPES_URI` rollout-order invariant in `CRITICAL.md`, and verified live Windows `UIAutomationCore.dll` COM extraction end-to-end — [archive](archive/roadmap-history.md#milestone-m4-release-v131-windows-uia-adapter-cdp-independent-queue-claiming)
- **`v1.3.0` (`ADR-007` & `ADR-008`, `2026-11-22`)** — `502`/`503`/`504` rollout retry, `5xx` stderr logging & `/release/{asset}` `404`, numeric `FieldRule.fallback_regex` + 9-recipe `RECIPES_URI` sync, `reg.exe` `HKCU\...\Run` tray logon, `google-scholar-profile` contract, and Windows `UIA` `TargetDriver` adapter — [archive](archive/roadmap-history.md#release-v130-rollout-resilience-scholar-contract-windows-uia-adapter)
- **Legacy `browser-drop` Retirement (`M2.4`, `2026-11-22`)** — Archived legacy `browser-drop` repository and retired its PostgreSQL database after live Windows PC `v1.1.0` verification — [archive](archive/roadmap-history.md#m24-legacy-browser-drop-retirement-2026-11-22)
- **`v1.1.0` (`ADR-006`, `2026-11-22`)** — Exception-only `AXTree` hybrid grounding + `<canvas>`/WebGL blind-spot gate, bounded declarative `pagination` with pure-Go verifier parity, and `deskdrop schedule` (`schtasks.exe`) + `--quiet-cdp` — [archive](archive/roadmap-history.md#release-v110-axtree-grounding-declarative-pagination-windows-scheduler)
- **`v1.0.0` (`M1`, `2026-11-22`)** — Zero-dependency Go CDP CLI, `TargetDriver` Port, pure-Go HTML verifier (`0` `eval_js`), atomic JSON `FileStore`, hot-reloadable `RECIPES_URI`, `adk-go` exception healer, and live `/desktop` cutover — [archive](archive/roadmap-history.md#milestone-m1-public-v100-release-live-desktop-cutover)
- **Milestone M2 — Rollout Resilience, FieldRule Verification & HKCU Logon (`v1.4.3+`):** Verified `HKCU\...\Run` logon (`M2.4`) and live post-`v1.4.3` `503 UH` rollout (`M2.5`, `4x status` / `0` fan-out) — [archive](archive/roadmap-history.md#milestone-m2---rollout-resilience-fieldrule-verification-hkcu-logon-v143)
