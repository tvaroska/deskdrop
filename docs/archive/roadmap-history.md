# Roadmap History (`deskdrop`)

Archived shipped milestones moved out of [`../roadmap.md`](../roadmap.md).

---

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
