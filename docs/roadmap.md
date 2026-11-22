# Roadmap (`deskdrop`)

## Where we are

`deskdrop` `v1.1.0` is live in production behind `/desktop` (with legacy `browser-drop` and its PostgreSQL database retired). All repository code, examples, contracts, ADRs (`ADR-007`, `ADR-008`), and `-race` conformance tests for **Milestones `M2`–`M4`** are **implemented locally (pending release & live Windows PC verification)** across `internal/bus/`, `internal/server/`, `internal/contract/`, `internal/verify/`, `internal/driver/cdp/`, `internal/driver/uia/`, `cmd/deskdrop/`, and `examples/recipes/`. There is no `TODO.md`.

## Milestones at a glance

| # | Milestone | Done when | Target |
| :--- | :--- | :--- | :--- |
| **M2** | Rollout Resilience, Server Observability, FieldRule Fallback & HKCU Logon | `502`/`503`/`504` retry backoff across Go `DesktopClient` and message-board CLI callers, `5xx` stderr logging + `/release/{asset}` `404` + `blobs.go` cleanup, numeric `FieldRule.fallback_regex` + versioned `RECIPES_URI` sync, and `reg.exe` `HKCU\...\Run` tray logon (`ADR-007`) shipped and verified on Windows PC. | `v1.1.1` (`P0`, Small) |
| **M3** | Google Scholar Profile & Recommended Papers Declarative Recipe | Declarative `google-scholar-profile` recipe (`examples/recipes/google-scholar-profile.json` + live `RECIPES_URI`) extracts citation metrics, per-paper stats, and recommended articles on an open tab (`reuse_open_tab: true`) with scrubbed HTML parity tests and `AUTH_EXPIRED` / challenge triage. | `v1.2.0` (`P2`, Small) |
| **M4** | Native Windows UI Automation (`UIA`) `TargetDriver` Adapter | Pure-stdlib Go (`CGO_ENABLED=0`) `internal/driver/uia/` Driven Adapter inspects Windows accessibility trees via `UIAutomationCore.dll` COM vtables behind `TargetDriver`, passing Linux CI tree-fixture conformance tests and live Windows desktop extraction. | `v1.3.0` (`P3`, Medium) |

## Milestone M2 — Rollout Resilience, Server Observability, FieldRule Fallback & HKCU Logon (`v1.1.1`)

*Bottleneck / rollout order:* Single-replica `Recreate` RWO-PVC rollouts produce a `15–60s` `HTTP 503` window while re-attaching `/data`; land `M2.1` client retry before cutting the server deploy.

- [ ] **M2.1 (`502`/`503`/`504` rollout retry across Go `DesktopClient` & message-board CLI — implemented, pending `v1.3.0` release):** Bounded `502`/`503`/`504`/`429` exponential backoff implemented across `GET` (`GetDesktopStatus`, `ListApprovedRecipes`, `DownloadReleaseBinary`), JSON `POST`, and rewound multipart uploads in [`internal/bus/service.go`](../internal/bus/service.go) with `httptest` unit tests in [`internal/bus/service_test.go`](../internal/bus/service_test.go) alongside companion message-board CLI `_http_req` retry.
- [ ] **M2.2 (`5xx` stderr logging, release-asset `404` & `blobs.go` cleanup — implemented locally, pending release):** `status >= 500` stderr logging (`method`, `path`, `status`, `code`, `message`) in `writeAPIError` ([`internal/server/http_helpers.go`](../internal/server/http_helpers.go)), direct `404 NOT_FOUND` from `handleDownloadRelease` ([`internal/server/http.go`](../internal/server/http.go)) without blob-storage fallback, and `"tools/"` prefix-bypass removal in `GCSBlobStore.objectName` ([`internal/server/blobs.go`](../internal/server/blobs.go)) implemented and verified in [`internal/server/server_test.go`](../internal/server/server_test.go).
- [ ] **M2.3 (Numeric `FieldRule.fallback_regex` fallback & versioned `RECIPES_URI` catalog sync — implemented, pending live Windows PC verification):** Numeric `FieldRule.Parse` (`"number"` / `"float"`) fallback to `fallback_regex` against `scope` implemented identically across [`internal/driver/cdp/js_runtime.go`](../internal/driver/cdp/js_runtime.go), [`internal/verify/verify.go`](../internal/verify/verify.go), and [`internal/contract/semantic_eval.go`](../internal/contract/semantic_eval.go) with JS/Go parity unit tests and versioned `scripts/recipes/*.json` catalog synced; pending `RECIPES_URI` upload and live Windows PC worker verification.
- [ ] **M2.4 (Replace `schtasks.exe` scheduled `sync` with non-Admin `HKCU\...\Run` tray logon — implemented locally, pending Windows PC verification):** Replaced `schtasks.exe` (`--mode`, `--every`, `--hide-console`, `--quiet-cdp`) with direct `reg.exe` `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` tray logon registration (`0` shell interpolation) across [`cmd/deskdrop/schedule.go`](../cmd/deskdrop/schedule.go), [`cmd/deskdrop/schedule_test.go`](../cmd/deskdrop/schedule_test.go), and [`cmd/deskdrop/main.go`](../cmd/deskdrop/main.go), preserving `hideConsoleWindow()` in [`cmd/deskdrop/tray_windows.go`](../cmd/deskdrop/tray_windows.go) and `SessionPool.ProbeQuiet` in [`internal/driver/cdp/pool.go`](../internal/driver/cdp/pool.go), with [`CRITICAL.md`](../CRITICAL.md), [`AGENTS.md`](../AGENTS.md), [`README.md`](../README.md), and `ADR-007` in [`DECISIONS.md`](../DECISIONS.md) synced.

## Milestone M3 — Google Scholar Profile & Recommended Papers Declarative Recipe (`v1.2.0`)

*Bottleneck / kill risk:* Google Scholar aggressively rate-limits and CAPTCHA-gates cold automated navigation. Never attempt CAPTCHA bypass; enforce single-page `reuse_open_tab: true` attachment and fail closed into the `AUTH_EXPIRED` domain circuit breaker on login/sorry/challenge redirects.

- [ ] **M3.1 (Declarative `google-scholar-profile` recipe & offline snapshot parity — implemented locally, pending release):** Sanitized [`examples/recipes/google-scholar-profile.json`](../examples/recipes/google-scholar-profile.json) (`reuse_open_tab: true`, `allow_computer_use_fallback: false`, `0` `eval_js`), root-tab `match_tab_url` `$` anchoring in [`internal/driver/cdp/extract.go`](../internal/driver/cdp/extract.go), scrubbed HTML parity fixtures in [`internal/verify/verify_test.go`](../internal/verify/verify_test.go), and `/sorry/` + login `AUTH_EXPIRED` classification tests in [`internal/contract/contract_test.go`](../internal/contract/contract_test.go) implemented and verified.
- [ ] **M3.2 (`RECIPES_URI` registration & live Windows Chrome verification):** Sync `google-scholar-profile` into version-controlled `scripts/recipes/` and live `RECIPES_URI`, then verify a clean `$0`-token `deskdrop run google-scholar-profile` drop against an open Windows Chrome session.

## Milestone M4 — Native Windows UI Automation (`UIA`) `TargetDriver` Adapter (`v1.3.0`)

*Bottleneck / kill risk:* Pure-stdlib Go (`CGO_ENABLED=0`, `0` external modules) requires manual `x64` COM vtable bindings against `UIAutomationCore.dll` / `ole32.dll` (`BSTR` `SysFreeString` lifecycle), and Linux Cloud Build cannot execute Win32 COM syscalls. Split pure-Go semantic tree evaluation (verified in Linux CI) from a minimal read-only Windows COM transport verified on `win-pc`.

- [ ] **M4.1 (Cross-platform semantic tree evaluator & `TargetDriver` conformance — implemented locally, pending release):** Extended [`internal/contract/types.go`](../internal/contract/types.go), [`internal/contract/axtree.go`](../internal/contract/axtree.go), [`internal/contract/semantic_eval.go`](../internal/contract/semantic_eval.go), and [`internal/verify/verify.go`](../internal/verify/verify.go) (`VerifyRecipeOnAXTree` / `VerifyRecipeOnAXTreeJSON`) with `role`, `name`, `automation_id`, and `class_name` selectors plus mandatory `password`/`edit` value redaction (`[REDACTED]`), verified in Linux CI tests.
- [ ] **M4.2 (`internal/driver/uia/` Win32 COM Adapter, `--driver cdp|uia` CLI & live Windows test — implemented locally, pending Windows PC verification):** Implemented [`internal/driver/uia/`](../internal/driver/uia) (`//go:build windows` typed COM `IUIAutomation` reader + `!windows` stub + `StaticTreeProvider`, `0` external Go modules), wired `--driver cdp|uia` and `Recipe.Driver` in [`cmd/deskdrop/main.go`](../cmd/deskdrop/main.go) and [`cmd/deskdrop/sync.go`](../cmd/deskdrop/sync.go), added [`examples/recipes/desktop-task-manager.json`](../examples/recipes/desktop-task-manager.json) (`ADR-008`), and verified Windows `.exe` cross-compilation; pending live extraction run on the Windows PC worker.

## Unscheduled backlog

*None — all current backlog items are scheduled in `M2`–`M4` above.*

## Shipped

- **Legacy `browser-drop` Retirement (`M2.4`, `2026-11-22`)** — Archived legacy `browser-drop` repository and retired its PostgreSQL database after live Windows PC `v1.1.0` verification — [archive](archive/roadmap-history.md#m24-legacy-browser-drop-retirement-2026-11-22)
- **`v1.1.0` (`ADR-006`, `2026-11-22`)** — Exception-only `AXTree` hybrid grounding + `<canvas>`/WebGL blind-spot gate, bounded declarative `pagination` with pure-Go verifier parity, and `deskdrop schedule` (`schtasks.exe`) + `--quiet-cdp` — [archive](archive/roadmap-history.md#release-v110-axtree-grounding-declarative-pagination-windows-scheduler)
- **`v1.0.0` (`M1`, `2026-11-22`)** — Zero-dependency Go CDP CLI, `TargetDriver` Port, pure-Go HTML verifier (`0` `eval_js`), atomic JSON `FileStore`, hot-reloadable `RECIPES_URI`, `adk-go` exception healer, and live `/desktop` cutover — [archive](archive/roadmap-history.md#milestone-m1-public-v100-release-live-desktop-cutover)
