# Roadmap History (`deskdrop`)

Archived shipped milestones moved out of [`../roadmap.md`](../roadmap.md).

---

## M2.4 — Legacy browser-drop Retirement (2026-11-22)

- **M2.4 (`2026-11-22`):** Verified live Windows PC `deskdrop.exe` (`v1.1.0`) extraction end-to-end against `/desktop`, retired legacy `browser-drop`'s CloudNativePG database + NetworkPolicy peer + release/CI entries, backed up and archived `tvaroska/browser-drop`, and kept shared Workload Identity + bearer token secret bindings intact for zero-rotation continuity.

## Release v1.1.0 — AXTree Grounding, Declarative Pagination & Windows Scheduler

- **v1.1.0 (`2026-11-22`, `b582837`):** Shipped exception-only CDP `Accessibility.getFullAXTree` (`axtree.txt`) hybrid grounding with input redaction and `<canvas>`/WebGL blind-spot detection (`UNLABELLED_CANVAS_SURFACE` + `testdata/drift/` compression/selector/canvas benchmarks), bounded declarative `"Next page"` `pagination` (`1..20` pages, disabled/`aria-disabled` stop, cross-page deduplication, pure-Go `VerifyRecipeOnHTMLPages`), and direct `schtasks.exe` Windows Task Scheduler helper (`deskdrop schedule install|uninstall|status` + `sync --quiet-cdp --hide-console`). Rolled out live behind `/desktop` and verified `/desktop/healthz`.

## Milestone M1 — Public v1.0.0 Release & Live /desktop Cutover

- **M1.1 (`2026-11-22`):** Published public `github.com/tvaroska/deskdrop` (`140a6ff`, tag `v1.0.0`) with zero-dependency desktop CLI (`cmd/deskdrop`), `TargetDriver` Port + CDP Adapter, pure-Go HTML verifier (`internal/verify`, zero `eval_js`), atomic JSON `FileStore` relay, hot-reloadable `RECIPES_URI` catalog, exception-only `adk-go` drift healer, and Cloud Build 2nd-gen triggers (`deskdrop-main`, `deskdrop-tag`).
- **M1.2 (`2026-11-22`):** Staged 6 declarative recipes in Cloud Storage (`RECIPES_URI`), deployed `deskdrop:v1.0.0` behind `/desktop` with a `1Gi` PVC `FileStore` and Vertex AI Workload Identity, and passed live smoke checks (keeping legacy `browser-drop` on standby pending Windows PC soak in `M2.2`).
