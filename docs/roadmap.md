# Roadmap (`deskdrop`)

## Where we are

`deskdrop` `v1.1.0` is live in production behind `/desktop` (with legacy `browser-drop` and its PostgreSQL database retired), running a zero-dependency desktop CLI (`cmd/deskdrop`), bounded declarative `"Next page"` `pagination`, atomic JSON `FileStore` relay (`cmd/server`), hot-reloadable `RECIPES_URI` catalog, and exception-only `adk-go` (`google.golang.org/adk/v2`) drift healer with hybrid `AXTree + dom.html` grounding and `<canvas>`/WebGL blind-spot detection. All open relay, desktop CLI, recipe, and adapter work is consolidated and scheduled across `M2`–`M4` below. There is no `TODO.md`.

## Milestones at a glance

| # | Milestone | Done when | Target |
| :--- | :--- | :--- | :--- |
| **M2** | Rollout Resilience, Server Observability, FieldRule Fallback & HKCU Logon | `502`/`503`/`504` retry backoff across Go `DesktopClient` and message-board CLI callers, `5xx` stderr logging + `/release/{asset}` `404` + `blobs.go` cleanup, numeric `FieldRule.fallback_regex` + versioned `RECIPES_URI` sync, and `reg.exe` `HKCU\...\Run` tray logon (`ADR-007`) shipped and verified on Windows PC. | `v1.1.1` (`P0`, Small) |
| **M3** | Google Scholar Profile & Recommended Papers Declarative Recipe | Declarative `google-scholar-profile` recipe (`examples/recipes/google-scholar-profile.json` + live `RECIPES_URI`) extracts citation metrics, per-paper stats, and recommended articles on an open tab (`reuse_open_tab: true`) with scrubbed HTML parity tests and `AUTH_EXPIRED` / challenge triage. | `v1.2.0` (`P2`, Small) |
| **M4** | Native Windows UI Automation (`UIA`) `TargetDriver` Adapter | Pure-stdlib Go (`CGO_ENABLED=0`) `internal/driver/uia/` Driven Adapter inspects Windows accessibility trees via `UIAutomationCore.dll` COM vtables behind `TargetDriver`, passing Linux CI tree-fixture conformance tests and live Windows desktop extraction. | `v1.3.0` (`P3`, Medium) |

## Milestone M2 — Rollout Resilience, Server Observability, FieldRule Fallback & HKCU Logon (`v1.1.1`)

*Bottleneck / rollout order:* Single-replica `Recreate` RWO-PVC rollouts produce a `15–60s` `HTTP 503` window while re-attaching `/data`; land `M2.1` client retry before cutting the `v1.1.1` server deploy.

- [ ] **M2.1 (`502`/`503`/`504` rollout retry across Go `DesktopClient` & message-board CLI):** Add bounded `502`/`503`/`504` exponential backoff to `GET` (`GetDesktopStatus`, `ListApprovedRecipes`, `DownloadReleaseBinary`) and idempotent/multipart requests in [`internal/bus/service.go`](../internal/bus/service.go) (with `httptest` unit tests in `internal/bus/service_test.go`), and land matching `502`/`503`/`504` backoff on `GET`, `ack`, and `Idempotency-Key` `POST` calls in the companion Python message-board CLI (`_http_req` + unit tests).
- [ ] **M2.2 (`5xx` stderr logging, release-asset `404` & `blobs.go` cleanup):** Log `status >= 500` responses (`method`, `path`, `status`, `code`, `message`) to stderr in `writeAPIError` ([`internal/server/http_helpers.go`](../internal/server/http_helpers.go)) so container error logs surface server faults, return `404 NOT_FOUND` directly from `handleDownloadRelease` ([`internal/server/http.go`](../internal/server/http.go)) when `/release/{asset}` is absent on disk instead of probing blob storage, and remove dead `"tools/"` prefix-bypass logic in `GCSBlobStore.objectName` ([`internal/server/blobs.go`](../internal/server/blobs.go)).
- [ ] **M2.3 (Numeric `FieldRule.fallback_regex` fallback & versioned `RECIPES_URI` catalog sync):** When `FieldRule.Parse` is `"number"` or `"float"` and `raw` fails compact number parsing (e.g. `"View analytics"` or `aria-label="Open reactions menu"`), fall back to `fallback_regex` against `scope` in `evalFieldRule` ([`internal/driver/cdp/js_runtime.go`](../internal/driver/cdp/js_runtime.go) and [`internal/verify/verify.go`](../internal/verify/verify.go)) with JS/Go parity unit tests; sync the 6 live declarative recipes from `RECIPES_URI` back into version-controlled `scripts/recipes/*.json` (replacing pre-cutover `eval_js` files), update `impressions`/`reactions` rules, and verify non-zero per-item counts live on the Windows PC worker.
- [ ] **M2.4 (Replace `schtasks.exe` scheduled `sync` with non-Admin `HKCU\...\Run` tray logon):** Delete periodic scheduled `sync` (`schtasks.exe`, `--mode`, `--every`, and `sync`'s `--hide-console`/`--quiet-cdp` flags across [`cmd/deskdrop/schedule.go`](../cmd/deskdrop/schedule.go), [`cmd/deskdrop/schedule_test.go`](../cmd/deskdrop/schedule_test.go), and [`cmd/deskdrop/main.go`](../cmd/deskdrop/main.go)) and replace `deskdrop schedule install|uninstall|status` with direct `reg.exe` `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` tray logon registration (zero shell interpolation). **Preserve** `hideConsoleWindow()` in [`cmd/deskdrop/tray_windows.go`](../cmd/deskdrop/tray_windows.go) (so the dual CLI/tray console-subsystem binary hides its console window at logon) and `SessionPool.ProbeQuiet` in [`internal/driver/cdp/pool.go`](../internal/driver/cdp/pool.go) (used by the 6s passive tray status probe). Sync [`CRITICAL.md`](../CRITICAL.md), [`AGENTS.md`](../AGENTS.md), [`README.md`](../README.md), and append `ADR-007` superseding `ADR-006` §3 in [`DECISIONS.md`](../DECISIONS.md).

## Milestone M3 — Google Scholar Profile & Recommended Papers Declarative Recipe (`v1.2.0`)

*Bottleneck / kill risk:* Google Scholar aggressively rate-limits and CAPTCHA-gates cold automated navigation. Never attempt CAPTCHA bypass; enforce single-page `reuse_open_tab: true` attachment and fail closed into the `AUTH_EXPIRED` domain circuit breaker on login/sorry/challenge redirects.

- [ ] **M3.1 (Declarative `google-scholar-profile` recipe & offline snapshot parity):** Author sanitized `examples/recipes/google-scholar-profile.json` under [`examples/recipes/`](../examples/recipes) extracting total/5y citations, h-index, i10-index, per-paper title/year/citations, and homepage recommended articles (`reuse_open_tab: true`, `0` `eval_js`), backed by scrubbed HTML fixtures in [`internal/verify/verify_test.go`](../internal/verify/verify_test.go) and `/sorry/` + login `AUTH_EXPIRED` classification tests in [`internal/contract/contract_test.go`](../internal/contract/contract_test.go).
- [ ] **M3.2 (`RECIPES_URI` registration & live Windows Chrome verification):** Sync `google-scholar-profile` into version-controlled `scripts/recipes/` and live `RECIPES_URI`, then verify a clean `$0`-token `deskdrop run google-scholar-profile` drop against an open Windows Chrome session.

## Milestone M4 — Native Windows UI Automation (`UIA`) `TargetDriver` Adapter (`v1.3.0`)

*Bottleneck / kill risk:* Pure-stdlib Go (`CGO_ENABLED=0`, `0` external modules) requires manual `x64` COM vtable bindings against `UIAutomationCore.dll` / `ole32.dll` (`VARIANT` / `BSTR` `SysFreeString` lifecycle), and Linux Cloud Build cannot execute Win32 COM syscalls. Split pure-Go semantic tree evaluation (verified in Linux CI) from a minimal read-only Windows COM transport verified on `win-pc`.

- [ ] **M4.1 (Cross-platform semantic tree evaluator & `TargetDriver` conformance):** Extend [`internal/contract/types.go`](../internal/contract/types.go) and [`internal/verify/verify.go`](../internal/verify/verify.go) so declarative `FieldRule`, `ItemRule`, and `Expectation` rules evaluate identically over serialized `AXNode` / `UIA` semantic trees (`role`, `name`, `automation_id` selectors + password/edit value redaction) in offline Linux CI tests without a Windows host.
- [ ] **M4.2 (`internal/driver/uia/` Win32 COM Adapter, `--driver cdp|uia` CLI & live Windows test):** Implement `internal/driver/uia/` (`//go:build windows` COM `IUIAutomation` reader + `!windows` stub satisfying [`internal/driver/port.go`](../internal/driver/port.go) and [`hygiene_test.go`](../hygiene_test.go) zero-external-dep invariant), wire `--driver cdp|uia` (and `Recipe.Driver`) in [`cmd/deskdrop/main.go`](../cmd/deskdrop/main.go), add a desktop example recipe in [`examples/recipes/`](../examples/recipes), and verify live extraction on the Windows PC worker.

## Unscheduled backlog

*None — all current backlog items are scheduled in `M2`–`M4` above.*

## Shipped

- **Legacy `browser-drop` Retirement (`M2.4`, `2026-11-22`)** — Archived legacy `browser-drop` repository and retired its PostgreSQL database after live Windows PC `v1.1.0` verification — [archive](archive/roadmap-history.md#m24-legacy-browser-drop-retirement-2026-11-22)
- **`v1.1.0` (`ADR-006`, `2026-11-22`)** — Exception-only `AXTree` hybrid grounding + `<canvas>`/WebGL blind-spot gate, bounded declarative `pagination` with pure-Go verifier parity, and `deskdrop schedule` (`schtasks.exe`) + `--quiet-cdp` — [archive](archive/roadmap-history.md#release-v110-axtree-grounding-declarative-pagination-windows-scheduler)
- **`v1.0.0` (`M1`, `2026-11-22`)** — Zero-dependency Go CDP CLI, `TargetDriver` Port, pure-Go HTML verifier (`0` `eval_js`), atomic JSON `FileStore`, hot-reloadable `RECIPES_URI`, `adk-go` exception healer, and live `/desktop` cutover — [archive](archive/roadmap-history.md#milestone-m1-public-v100-release-live-desktop-cutover)
