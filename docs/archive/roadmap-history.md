# Roadmap History (`deskdrop`)

Archived shipped milestones moved out of [`../roadmap.md`](../roadmap.md).

---

## Milestone M1 — Public v1.0.0 Release & Live /desktop Cutover

- **M1.1 (`2026-11-22`):** Published public `github.com/tvaroska/deskdrop` (`140a6ff`, tag `v1.0.0`) with zero-dependency desktop CLI (`cmd/deskdrop`), `TargetDriver` Port + CDP Adapter, pure-Go HTML verifier (`internal/verify`, zero `eval_js`), atomic JSON `FileStore` relay, hot-reloadable `RECIPES_URI` catalog, exception-only `adk-go` drift healer, and Cloud Build 2nd-gen triggers (`deskdrop-main`, `deskdrop-tag`).
- **M1.2 (`2026-11-22`):** Staged 6 declarative recipes in Cloud Storage (`RECIPES_URI`), deployed `deskdrop:v1.0.0` behind `/desktop` with a `1Gi` PVC `FileStore` and Vertex AI Workload Identity, and passed live smoke checks (keeping legacy `browser-drop` on standby pending Windows PC soak in `M2.2`).
