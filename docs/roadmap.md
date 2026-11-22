# Roadmap (`deskdrop`)

## Where we are

`deskdrop` `v1.3.0` (`ADR-007`, `ADR-008`) is live in production behind `/desktop` with the 9-recipe declarative `2.0` catalog hot-reloaded in `RECIPES_URI` and the Windows PC tray worker auto-updated to `v1.3.0`. Server/relay rollout resilience (`M2.1`), `5xx` stderr observability + container-disk release asset handling (`M2.2`), `google-scholar-profile` contract/parity (`M3.1`), and cross-platform semantic `AXNode` evaluation (`M4.1`) are shipped and verified live; remaining items cover interactive Windows PC execution checks (`M2.3`, `M2.4`, `M3.2`, `M4.2`) once Chrome remote debugging is approved on the Windows desktop. There is no `TODO.md`.

## Milestones at a glance

| # | Milestone | Done when | Target |
| :--- | :--- | :--- | :--- |
| **M2** | Rollout Resilience, Server Observability, FieldRule Fallback & HKCU Logon | Live `linkedin-analytics` drop confirms non-zero per-item `impressions`/`reactions` via `fallback_regex` and `deskdrop.exe schedule install` registers `HKCU\...\Run\DeskDrop` on the Windows PC. | `v1.3.1` (`P0`, Small) |
| **M3** | Google Scholar Profile & Recommended Papers Declarative Recipe | Live `$0`-token `google-scholar-profile` drop completes cleanly against an open Google Scholar Chrome tab on the Windows PC. | `v1.3.1` (`P2`, Small) |
| **M4** | Native Windows UI Automation (`UIA`) `TargetDriver` Adapter | Live `desktop-windows` / `desktop-task-manager` extraction completes on the Windows PC (and `drainQueue` allows `driver: "uia"` task claiming even when Chrome CDP is disconnected). | `v1.3.1` (`P3`, Small) |

## Milestone M2 — Rollout Resilience, Server Observability, FieldRule Fallback & HKCU Logon (`v1.3.1`)

*Bottleneck / rollout order:* Single-replica `Recreate` RWO-PVC rollouts produce a `15–60s` `HTTP 503` window while re-attaching `/data`; `M2.1`–`M2.2` shipped in `v1.3.0`, and Chrome 144+ `DevToolsActivePort` requires a one-time interactive `"Allow"` click after the Windows tray binary restarts on self-update.

- [ ] **M2.3 (Live Windows PC verification of `FieldRule.fallback_regex` on `linkedin-analytics` — deployed in `v1.3.0`, task queued):** Engine parity ([`internal/driver/cdp/js_runtime.go`](../internal/driver/cdp/js_runtime.go), [`internal/verify/verify.go`](../internal/verify/verify.go), [`internal/contract/semantic_eval.go`](../internal/contract/semantic_eval.go)) and `RECIPES_URI` catalog sync (`9` recipes) are live in `v1.3.0`; verify queued `linkedin-analytics` drop extracts non-zero per-item `impressions` and `reactions` once Chrome remote debugging is approved on the Windows PC.
- [ ] **M2.4 (Run `deskdrop.exe schedule install` `HKCU\...\Run` logon registration on Windows PC — deployed in `v1.3.0`):** `reg.exe` `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` tray logon implementation (`ADR-007`, [`cmd/deskdrop/schedule.go`](../cmd/deskdrop/schedule.go)) is live in `v1.3.0` on the Windows worker; run `deskdrop.exe schedule install --fallback-gate tray` + `deskdrop.exe schedule status` on the Windows PC to verify registry logon persistence.

## Milestone M3 — Google Scholar Profile & Recommended Papers Declarative Recipe (`v1.3.1`)

*Bottleneck / kill risk:* Google Scholar aggressively rate-limits and CAPTCHA-gates cold automated navigation. Never attempt CAPTCHA bypass; enforce single-page `reuse_open_tab: true` attachment and fail closed into the `AUTH_EXPIRED` domain circuit breaker on login/sorry/challenge redirects.

- [ ] **M3.2 (Live Windows Chrome verification of `google-scholar-profile`):** Verify a clean `$0`-token `google-scholar-profile` drop against an open Google Scholar profile tab in the Windows Chrome session (`--var user_id=<scholar_id>` or open `/citations` tab).

## Milestone M4 — Native Windows UI Automation (`UIA`) `TargetDriver` Adapter (`v1.3.1`)

*Bottleneck / kill risk:* Pure-stdlib Go (`CGO_ENABLED=0`, `0` external modules) requires manual `x64` COM vtable bindings against `UIAutomationCore.dll` / `ole32.dll` (`BSTR` `SysFreeString` lifecycle), and [`cmd/deskdrop/sync.go`](../cmd/deskdrop/sync.go) `drainQueue` currently skips `ClaimNextTask` when `ws.BrowserConnected == false`, blocking `driver: "uia"` recipes while Chrome CDP awaits interactive approval.

- [ ] **M4.2 (Live Windows `UIA` COM verification & CDP-independent `uia` queue claiming — `v1.3.0` binary live on Windows PC):** [`internal/driver/uia/`](../internal/driver/uia) (`ADR-008`), `--driver cdp|uia`, [`examples/recipes/desktop-task-manager.json`](../examples/recipes/desktop-task-manager.json), and `desktop-windows` / `desktop-task-manager` in `RECIPES_URI` are live in `v1.3.0` (`desktop-windows` task queued); verify live Windows `UIAutomationCore.dll` extraction and decouple `driver: "uia"` task claiming in [`cmd/deskdrop/sync.go`](../cmd/deskdrop/sync.go) from `ws.BrowserConnected`.

## Unscheduled backlog

*None — all current backlog items are scheduled in `M2`–`M4` above.*

## Shipped

- **`v1.3.0` (`ADR-007` & `ADR-008`, `2026-11-22`)** — `502`/`503`/`504` rollout retry, `5xx` stderr logging & `/release/{asset}` `404`, numeric `FieldRule.fallback_regex` + 9-recipe `RECIPES_URI` sync, `reg.exe` `HKCU\...\Run` tray logon, `google-scholar-profile` contract, and Windows `UIA` `TargetDriver` adapter — [archive](archive/roadmap-history.md#release-v130-rollout-resilience-scholar-contract-windows-uia-adapter)
- **Legacy `browser-drop` Retirement (`M2.4`, `2026-11-22`)** — Archived legacy `browser-drop` repository and retired its PostgreSQL database after live Windows PC `v1.1.0` verification — [archive](archive/roadmap-history.md#m24-legacy-browser-drop-retirement-2026-11-22)
- **`v1.1.0` (`ADR-006`, `2026-11-22`)** — Exception-only `AXTree` hybrid grounding + `<canvas>`/WebGL blind-spot gate, bounded declarative `pagination` with pure-Go verifier parity, and `deskdrop schedule` (`schtasks.exe`) + `--quiet-cdp` — [archive](archive/roadmap-history.md#release-v110-axtree-grounding-declarative-pagination-windows-scheduler)
- **`v1.0.0` (`M1`, `2026-11-22`)** — Zero-dependency Go CDP CLI, `TargetDriver` Port, pure-Go HTML verifier (`0` `eval_js`), atomic JSON `FileStore`, hot-reloadable `RECIPES_URI`, `adk-go` exception healer, and live `/desktop` cutover — [archive](archive/roadmap-history.md#milestone-m1-public-v100-release-live-desktop-cutover)

