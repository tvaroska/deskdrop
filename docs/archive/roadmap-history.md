# Roadmap History (`deskdrop`)

Archived shipped milestones moved out of [`../roadmap.md`](../roadmap.md).

---

## v1.4.0-v1.4.3 Milestone M5: CI Smoke, E2E, Test and Architecture Hardening

*Archived from [`../roadmap.md`](../roadmap.md) on 2026-11-26.*

Close the CI production-mode parity and state-evolution gaps identified in the 2026-11-22 GKE testing audit.

- [x] **M5.1 (P1, 0.25d) — Production-mode `FileStore` + `RECIPES_URI` container smoke in `cloudbuild.yaml`:** Updated `verify-nonroot-and-healthcheck` in [`cloudbuild.yaml`](../../cloudbuild.yaml) to run `deskdrop:ci` with `STORE_BACKEND=file` + `RECIPES_URI` and verify `/desktop/v1/tasks` persistence across container restart. *(Shipped in `v1.4.0`–`v1.4.3`, Cloud Build `61badebd`.)*
- [x] **M5.2 (P2, 0.25d) — Legacy `state.json` schema-compatibility & corrupt-file recovery tests:** Added versioned fixture and corruption-recovery tests in [`internal/server/store_file_test.go`](../../internal/server/store_file_test.go) (`FileStore` rollback updated root-safe for CI in `v1.4.1`). *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **M5.3 (P2, 0.5d) — Compiled CLI → Relay loopback E2E in CI & escape-analysis checklist:** Added [`e2e_test.go`](../../e2e_test.go), `cloudbuild.yaml` compiled CLI → relay E2E flow, and the E2E/prod → unit escape-analysis rule in [`AGENTS.md`](../../AGENTS.md). *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **T-1 (P1):** Added negative security and `DirBlobStore`/`GCSBlobStore` parity tests in `internal/server/server_test.go` and `internal/server/blobs_test.go`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **T-2 (P1):** Enforced `DESKDROP_REQUIRE_NODE=1` JS/Go parity checks in `cloudbuild.yaml` (`node:22-bookworm`) and expanded `internal/driver/cdp/cdp_test.go`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **T-3 (P1):** Added hermetic guard-term isolation and direct `internal/contract/contract_test.go` suites. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **A-8 (P2):** Fixed header-matched pagination table merging with row cap, `wait_ms: 0`, and numeric `0` handling on `required_fields`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **A-9 (P2):** Split `semantic_eval.go` into `semantic_tree.go`, `semantic_eval.go`, and shared `selector_ast.go` consumed by `internal/verify/selector.go`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **A-10 (P2):** Synced `specs/deskdrop.yaml` and `prompts/REGENERATE.md` with `v1.4.0`+. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **S-9 (P2):** Fixed `cdp.Client` close race/pending leak and bounded `ws.go` continuation frames and write deadlines. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **S-10 (P2):** Unified `SHA256SUMS.txt`, added `.dockerignore`, trapped GHCR token cleanup, digest-pinned Trivy `0.70.0`, and sanitized `ci/trigger.yaml`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **S-11 (P2):** Fixed commented-config shadowing, Windows tray `uia` mode, and `provider_windows.go` COM safety (`GetCurrentPatternAs`, `cur` release). *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **S-12 (P2):** Logged healer errors, closed stuck fallback tasks, hardened `GCSBlobStore`/`DirBlobStore`, separated `ScopeAdmin`, and inferred auth-breaker domain. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **T-4 (P2):** Aligned `scripts/gate.sh`, `cloudbuild.yaml`, and `Makefile` on Windows `go vet`, `go test -race -count=1 ./...`, and `ast-grep`, and checked `linux`+`windows` in `TestZeroExternalDepsInDesktopCLI`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **T-5 (P2):** Added `internal/driver/cdp/cdp_test.go` tests for `matchesTabPattern` (`$`), `ProbeQuiet`, pagination/fallback domain guards, `DevToolsActivePort`, and `ws.go` framing. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **T-6 (P2):** Extended `hygiene_test.go` (`CLAUDE.md`, `.env*`, link confinement, `ADR-005` patterns) and `specs/ast.rules.yaml` (`CommandContext`, shells, `pgx`, `adk`/`genai` isolation). *(Shipped in `v1.4.0`–`v1.4.3`.)*

## v1.4.0-v1.4.3 M2 Security Hardening, M2.3 Live Check and Review P3s

*Archived from [`../roadmap.md`](../roadmap.md) on 2026-11-26.*

- [x] **M2.3 (Live Windows PC verification of `FieldRule.fallback_regex` on `linkedin-analytics` — verified `2026-11-26T15:31:12Z`):** Queued task `task_693aebb2f234b7de` completed on `win-pc` (`v1.4.3`, `session=Boris`) producing `drop_fc52c88259bb7737` (`followers: 2219`) with non-zero per-item `impressions` (`521`, `289`, `322`, `274`, `488`) and `reactions` (`1`, `2`, `5`, `5`, `6`), confirming [`internal/driver/cdp/js_runtime.go`](../../internal/driver/cdp/js_runtime.go), [`internal/verify/verify.go`](../../internal/verify/verify.go), and [`internal/contract/semantic_eval.go`](../../internal/contract/semantic_eval.go) `A-7` parity live against LinkedIn `recent-activity`.

### Review findings track — 2026-11-25 (`M2` security/runtime `P0`–`P1` + `P3` hygiene)

*Baseline:* `HEAD` `8606b04` (`v1.3.1`, `5.1 / 10` strict Dutch grade across `arch-core` `4.8`, `surface-ops` `5.4`, `tests-gates` `5.2`), resolved in `4fbf6a5` (`M2`) and `55fe7b1`–`042c1f7` (`v1.4.0`–`v1.4.3`, Cloud Build `61badebd`).

- [x] **A-1 (P0):** Fix domain allowlist and domain-pivot bypasses across `ResolveRecipe`, `InferRecipeDomains`, `ValidateAndResolveTask`, and `healer.go` — `internal/contract/desktop.go:160-231`, `internal/contract/types.go:253-355`, `internal/recipes/catalog.go:302-354`, `internal/healer/healer.go:263-284`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **A-2 (P0):** Fix `SaveRecipe` path traversal, unpinned `ProposedRecipe.Name`, silent reload-fail catalog wipe, and cache poisoning — `internal/recipes/catalog.go:357-414`, `internal/healer/healer.go:257-306`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **S-1 (P0):** Verify SHA-256 on self-update and fail closed on truncated HTTP reads — `cmd/deskdrop/sync.go:405-445`, `internal/bus/service.go:518-530`, `cmd/deskdrop/tray_windows.go:653-679`. *(Shipped in `v1.4.0`–`v1.4.3`; `win-pc` auto-updated to `v1.4.3`.)*
- [x] **S-2 (P0):** Fail closed on proposal approval (`!exists`, name mismatch, empty resolved domains, and `SaveRecipe`-before-`ReviewProposal` TOCTOU) — `internal/server/http_proposals.go:421-506`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **A-3 (P1):** Replace regex `ScrubHTML` with a stdlib HTML scanner to close redaction bypasses and UTF-8 truncation bugs — `internal/contract/scrub.go:10-38`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **A-4 (P1):** Redact `StaticText` children of sensitive `AXNode` inputs, fix `ParentID`-only drops, and drop dead canvas helper — `internal/contract/axtree.go:63-95,175-328`, `internal/contract/semantic_eval.go:28-53`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **A-5 (P1):** Harden `EnforceAirGap` matching, error redaction, host decoupling, and healer air-gap coverage — `internal/contract/airgap.go:12-71`, `internal/healer/healer.go:111-120,280-312,394-415`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **A-6 (P1):** Enforce healer Rule-0 invariants, non-empty fallback reports, Stage-1 recovery, and pre-network model lock — `internal/healer/healer.go:63-96,142-178,240-320,418-436`, `internal/contract/desktop.go:301-320`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **A-7 (P1):** Fix `M2.3` `ParseCompactFloat` suffix explosion and JS/Go extractor/selector parity — `internal/contract/number.go:13`, `internal/driver/cdp/js_runtime.go:157-198`, `internal/verify/verify.go:525-555`, `internal/verify/selector.go:53-157,367-419`, `internal/contract/semantic_eval.go:589-675`. *(Shipped in `v1.4.0`–`v1.4.3` and verified live via `drop_fc52c88259bb7737`.)*
- [x] **S-3 (P1):** Enforce strict zip bounds and full-bundle air-gap scanning on diagnostic uploads — `internal/server/http_helpers.go:141-205`, `internal/server/http_proposals.go:43-56,128-155`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **S-4 (P1):** Make `FileStore` rollback on persist failure, prune idempotency maps, and guard task state transitions — `internal/server/store_file.go:56-306`, `internal/server/store_memory.go:144-151,265-272,440-442,505-627`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **S-5 (P1):** Stop caller `meta.ErrorCode` from overriding triage and triggering healer on `AUTH_EXPIRED` — `internal/server/http_proposals.go:51-54,150-153`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **S-6 (P1):** Resolve task drivers before locking `MemoryStore.mu` in `handleClaimTask` — `internal/server/http.go:484-502,523-539`, `internal/server/store_memory.go:465-490`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **S-7 (P1):** Enforce approved recipe domains and fail-closed URL checks in `ExecuteFallback` and `paginate.go` — `internal/driver/cdp/extract.go:109-188`, `internal/driver/cdp/paginate.go:69-81`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **S-8 (P1):** Fix multi-profile `SELECTOR_DRIFT` error masking and `M3.1` `matchesTabPattern` `$`-anchor bugs — `internal/driver/cdp/extract.go:74-103,302-321`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **A-11 (P3):** Refresh `architecture_card.svg` and `DECISIONS.md` path, and reject trailing tokens in `ParseJSONStrict` — `docs/assets/architecture_card.svg`, `DECISIONS.md:9`, `internal/contract/types.go`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **S-13 (P3):** Tighten relay client auth, CLI version header, status count, and browser reaping — `internal/bus/service.go`, `cmd/deskdrop/main.go`, `internal/server/http.go`, `internal/driver/cdp/launcher.go`. *(Shipped in `v1.4.0`–`v1.4.3`.)*
- [x] **T-7 (P3):** Add zero-test package coverage (`cmd/server/main_test.go`, `internal/driver/port_test.go`), CLI/healer negative paths, and `VerifyRecipeOnHTML`/`VerifyRecipeOnAXTree` checks on `examples/recipes/*.json` (`hygiene_test.go`). *(Shipped in `v1.4.0`–`v1.4.3`.)*

## Milestone M4 & Release v1.3.1 — Windows UIA Adapter & CDP-Independent Queue Claiming

- **M4.2 (`2026-11-22`, `268ea91`):** Shipped [`internal/driver/uia/`](../../internal/driver/uia) (`ADR-008`, pure-stdlib `CGO_ENABLED=0` `IUIAutomation` x64 COM vtable reader with BFS level-order walk and `SysFreeString` cleanup), `--driver cdp|uia` + `Recipe.Driver`, [`examples/recipes/desktop-task-manager.json`](../../examples/recipes/desktop-task-manager.json), `RECIPES_URI` rollout-order rule in [`CRITICAL.md`](../../CRITICAL.md), and `ClaimTaskRequest.AllowedDrivers` across [`cmd/deskdrop/sync.go`](../../cmd/deskdrop/sync.go), [`internal/bus/service.go`](../../internal/bus/service.go), and [`internal/server/http.go`](../../internal/server/http.go). Verified live on the Windows PC worker (`v1.3.1` automatic tray self-update -> live `desktop-windows` `UIAutomationCore.dll` COM drop completed while keeping pending `CDP` tasks untouched).

## Release v1.3.0 — Rollout Resilience, Scholar Contract & Windows UIA Adapter

- **M2.1 (`2026-11-22`, `49c05d7`):** Shipped bounded `502`/`503`/`504`/`429` exponential backoff across `GET` (`GetDesktopStatus`, `ListApprovedRecipes`, `DownloadReleaseBinary`), JSON `POST`, and rewound multipart uploads in [`internal/bus/service.go`](../../internal/bus/service.go) + [`internal/bus/service_test.go`](../../internal/bus/service_test.go) and companion message-board CLI `_http_req` retry; verified live across the `v1.3.0` `Recreate` rollout and automatic Windows tray self-update.
- **M2.2 (`2026-11-22`, `49c05d7`):** Shipped `status >= 500` stderr logging (`method`, `path`, `status`, `code`, `message`) in `writeAPIError` ([`internal/server/http_helpers.go`](../../internal/server/http_helpers.go)), direct `404 NOT_FOUND` from `handleDownloadRelease` ([`internal/server/http.go`](../../internal/server/http.go)) without blob-storage fallback, and `"tools/"` prefix-bypass removal in `GCSBlobStore.objectName` ([`internal/server/blobs.go`](../../internal/server/blobs.go)); verified live against `/v1/release/SHA256SUMS.txt`, `/v1/release/deskdrop.exe`, and invalid asset probes.
- **M3.1 (`2026-11-22`, `49c05d7`):** Shipped sanitized [`examples/recipes/google-scholar-profile.json`](../../examples/recipes/google-scholar-profile.json) (`reuse_open_tab: true`, `allow_computer_use_fallback: false`, `0` `eval_js`), root-tab `match_tab_url` `$` anchoring in [`internal/driver/cdp/extract.go`](../../internal/driver/cdp/extract.go), scrubbed HTML parity fixtures in [`internal/verify/verify_test.go`](../../internal/verify/verify_test.go), and `/sorry/` + login `AUTH_EXPIRED` classification in [`internal/contract/desktop.go`](../../internal/contract/desktop.go), registered live in `RECIPES_URI`.
- **M4.1 (`2026-11-22`, `49c05d7`):** Shipped [`internal/contract/types.go`](../../internal/contract/types.go), [`internal/contract/axtree.go`](../../internal/contract/axtree.go), [`internal/contract/semantic_eval.go`](../../internal/contract/semantic_eval.go), and [`internal/verify/verify.go`](../../internal/verify/verify.go) (`VerifyRecipeOnAXTree` / `VerifyRecipeOnAXTreeJSON`) with `role`, `name`, `automation_id`, and `class_name` selectors plus mandatory `password`/`edit` value redaction (`[REDACTED]`), verified in Cloud Build CI and deployed in `v1.3.0`.

## M2.4 — Legacy browser-drop Retirement (2026-11-22)

- **M2.4 (`2026-11-22`):** Verified live Windows PC `deskdrop.exe` (`v1.1.0`) extraction end-to-end against `/desktop`, retired legacy `browser-drop`'s CloudNativePG database + NetworkPolicy peer + release/CI entries, backed up and archived `tvaroska/browser-drop`, and kept shared Workload Identity + bearer token secret bindings intact for zero-rotation continuity.

## Release v1.1.0 — AXTree Grounding, Declarative Pagination & Windows Scheduler

- **v1.1.0 (`2026-11-22`, `b582837`):** Shipped exception-only CDP `Accessibility.getFullAXTree` (`axtree.txt`) hybrid grounding with input redaction and `<canvas>`/WebGL blind-spot detection (`UNLABELLED_CANVAS_SURFACE` + `testdata/drift/` compression/selector/canvas benchmarks), bounded declarative `"Next page"` `pagination` (`1..20` pages, disabled/`aria-disabled` stop, cross-page deduplication, pure-Go `VerifyRecipeOnHTMLPages`), and direct `schtasks.exe` Windows Task Scheduler helper (`deskdrop schedule install|uninstall|status` + `sync --quiet-cdp --hide-console`). Rolled out live behind `/desktop` and verified `/desktop/healthz`.

## Milestone M1 — Public v1.0.0 Release & Live /desktop Cutover

- **M1.1 (`2026-11-22`):** Published public `github.com/tvaroska/deskdrop` (`140a6ff`, tag `v1.0.0`) with zero-dependency desktop CLI (`cmd/deskdrop`), `TargetDriver` Port + CDP Adapter, pure-Go HTML verifier (`internal/verify`, zero `eval_js`), atomic JSON `FileStore` relay, hot-reloadable `RECIPES_URI` catalog, exception-only `adk-go` drift healer, and Cloud Build 2nd-gen triggers (`deskdrop-main`, `deskdrop-tag`).
- **M1.2 (`2026-11-22`):** Staged 6 declarative recipes in Cloud Storage (`RECIPES_URI`), deployed `deskdrop:v1.0.0` behind `/desktop` with a `1Gi` PVC `FileStore` and Vertex AI Workload Identity, and passed live smoke checks (keeping legacy `browser-drop` on standby pending Windows PC soak in `M2.2`).

## Milestone M2 - Rollout Resilience, FieldRule Verification & HKCU Logon (`v1.4.3+`)

*Archived from [`../roadmap.md`](../roadmap.md) on 2026-11-26.*

*Bottleneck / rollout order:* Single-replica `Recreate` RWO-PVC rollouts produce a `15–60s` `HTTP 503` window while re-attaching `/data`; `M2.1`–`M2.3`, `M2.5` Step 2, and `A-1`–`A-7`/`S-1`–`S-8` are shipped in `v1.3.0`–`v1.4.3`, and Chrome 144+ `DevToolsActivePort` requires a one-time interactive `"Allow"` click after a tray binary restart.

- [x] **M2.4 (Run `deskdrop.exe schedule install` `HKCU\...\Run` logon registration on Windows PC — live since `v1.3.1`):** `reg.exe` `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` tray logon implementation (`ADR-007`, [`cmd/deskdrop/schedule.go`](../../cmd/deskdrop/schedule.go)) is live on the `v1.4.3` Windows worker; confirmed installed and verified on the Windows PC (`2026-11-26`).
- [x] **M2.5 (P3, 0.25d) — Stop tray sync retry bursts during relay `Recreate` rollouts (`F-2026-11-23-002`):** Step 2 (`4fbf6a5`, `v1.4.0`–`v1.4.3`) and Step 1 verified via Cloud Logging (`2026-11-26`): confirmed all `15` `2026-11-22` `DeskDrop/1.0` `503 UH` rows (`20:02:52–20:02:56Z` `7x`, `20:46:29–20:46:34Z` `8x`, `0` `AgentBus/2.0`) and confirmed the post-`v1.4.3` `2026-11-26T16:06:40–16:06:43Z` rollout produced `4x` `GET /desktop/v1/status` (`<= 4` `503 UH` rows/tick) with `0` `heartbeat` or `claim` fan-out.
