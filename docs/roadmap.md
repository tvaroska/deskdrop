# Roadmap (`deskdrop`)

## Where we are

`deskdrop` `v1.0.0` is implemented locally with zero-dependency desktop CLI (`cmd/deskdrop`), atomic JSON `FileStore` relay (`cmd/server`), hot-reloadable `RECIPES_URI` catalog, pure-Go HTML verifier (`internal/verify`), and exception-only `adk-go` (`google.golang.org/adk/v2`) drift healer with `testdata/drift/` benchmark. Pending final public repository publish and production `/desktop` cutover. There is no `TODO.md`.

## Milestones at a glance

| # | Milestone | Done when | Target |
| :--- | :--- | :--- | :--- |
| **M1** | Public Release & Live `/desktop` Cutover | `github.com/tvaroska/deskdrop` published as `v1.0.0`, live `/desktop` traffic cut over onto `1Gi` PVC `FileStore` + `RECIPES_URI`, smoke checks green. | `v1.0.0` (`P0`, Small) |

## Milestone M1 — Public Release & Live `/desktop` Cutover

- [ ] **M1.1:** Publish clean `main` and `v1.0.0` tag to public `github.com/tvaroska/deskdrop` and verify Cloud Build CI (`cloudbuild.yaml`).
- [ ] **M1.2:** Stage externalized declarative `recipes.json` in Cloud Storage (`RECIPES_URI`), deploy `deskdrop` with `1Gi` PVC + Vertex AI Workload Identity behind `/desktop`, pass live smoke checks, and retire legacy `browser-drop`.

## Unscheduled backlog

- **Google Scholar Profile & Recommended Papers Recipe (`google-scholar-profile`):** External declarative CDP recipe for citation metrics, per-paper stats, and recommended articles, backed by scrubbed HTML snapshot tests and `AUTH_EXPIRED` detection.
- **Accessibility Tree (`AXTree`) Grounding Evaluation:** Benchmark CDP `Accessibility.getFullAXTree` compact snapshots against scrubbed `dom.html` + screenshots for exception-route self-healing, including `<canvas>`/WebGL failure modes.
- **Interactive Pagination & Windows Scheduler Helper:** Optional multi-page `"Next page"` loop step and `schtasks.exe` helper for background `sync`.
- **Native Windows UI Automation (`UIA`) Adapter:** Second `TargetDriver` Adapter inspecting Windows semantic accessibility trees behind the same declarative `Recipe` and `Expect` Contracts.

## Shipped

*(First release `v1.0.0` pending M1 tag & live verification.)*
