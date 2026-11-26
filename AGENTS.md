# DeskDrop Agent Instructions (`AGENTS.md`)
<!-- doc-tier: full -->

## 1. Purpose & Scope
`deskdrop` (`github.com/tvaroska/deskdrop`) is a single-repo, zero-Postgres desktop & browser declarative extraction utility and relay server structured around **Ports, Adapters, and Contracts**:
1. **`cmd/deskdrop` (`deskdrop` / `deskdrop.exe`):** Zero-external-dependency Go CLI & native Windows System Tray (`CGO_ENABLED=0`, `0` non-stdlib Go packages) that attaches over Chrome DevTools Protocol (`--driver cdp`) or native Windows UI Automation (`--driver uia`), runs declarative extraction recipes (`0` `eval_js`), registers non-Admin `HKCU\...\Run` tray logon via `reg.exe`, and enforces `--fallback-gate=auto|tray` for bounded Tier-2 recovery tasks.
2. **`cmd/server` (`deskdrop-server`):** Distroless Go relay (`65532:65532`, `64Mi` RAM) backed by an atomic JSON `FileStore` (`/data/state.json`), hot-reloadable `RECIPES_URI` catalog (local path or `gs://`), wire-compatible `/v1/*` and `/desktop/v1/*` endpoints, `5xx` stderr error logging, and an exception-only `google.golang.org/adk/v2` (`adk-go`) healer invoked strictly on `SELECTOR_DRIFT`.

## 2. Package Architecture (Ports, Adapters, and Contracts)
- [`internal/contract/`](internal/contract/) — Pure-stdlib schemas (`Recipe`, `Step`, `FieldRule`, `ItemRule`, `PaginationRule` [`click` & `scroll`], `DropDiff`, `AXNode`, `DropReport`, `Task`, `FallbackPlan`, `RecipePatchProposal`, `BusMessage`), pure-stdlib semantic tree evaluator (`semantic_eval.go`), compact `AXTree` formatter + `<canvas>`/WebGL blind-spot analyzer + password/edit redaction (`axtree.go`), cross-page deduplicating merger (`pagination.go`), top-N window-shift-guarded drop diffing (`diff.go`), compact number parsing, HTML privacy scrubbing (`ScrubHTML`), error triage classifier (`/sorry/` + login `AUTH_EXPIRED`), and outbound air-gap enforcement (`EnforceAirGap`).
- [`internal/driver/`](internal/driver/) — `TargetDriver` Driven Port (`port.go`) and diagnostic `.zip` packaging (`error.json`, scrubbed `dom.html`, compact `axtree.txt`, `screenshot.png`).
- [`internal/driver/cdp/`](internal/driver/cdp/) — Pure-stdlib Browser CDP Adapter (RFC 6455 WebSocket, single-attempt `DevToolsActivePort` approval latch, `SessionPool.ProbeQuiet`, multi-profile `BrowserContextID` ranking, declarative Shadow-DOM extractor, bounded `"click"` & `"scroll"` pagination loop, exception-only `Accessibility.getFullAXTree` capture, stateless scratch-tab cleanup).
- [`internal/driver/uia/`](internal/driver/uia/) — Pure-stdlib (`CGO_ENABLED=0`) Native Windows UI Automation (`UIA`) Driven Adapter (`UIAutomationCore.dll` / `ole32.dll` / `oleaut32.dll` typed COM vtable reader on Windows + offline `StaticTreeProvider` conformance adapter on Linux).
- [`internal/bus/`](internal/bus/) — Pure-stdlib HTTP relay client for `cmd/deskdrop` with bounded `502`/`503`/`504` rollout retry across `GET`, JSON `POST`, and multipart diagnostic uploads.
- [`internal/verify/`](internal/verify/) — Pure-Go HTML declarative extractor, multi-page `"click"`/`"scroll"` pagination verifier (`VerifyRecipeOnHTMLPages`), semantic `AXNode`/`UIA` verifier (`VerifyRecipeOnAXTree`), and `adk-go` `verify_recipe_on_snapshot` verifier (`golang.org/x/net/html`).
- [`internal/recipes/`](internal/recipes/) — Hot-reloadable `RECIPES_URI` catalog (`local` file/dir + `gs://` with mtime/ETag reload, `HTTP 403` unapproved-URL gate, and atomic write-back on approval).
- [`internal/server/`](internal/server/) — Mutex-guarded `MemoryStore` + atomic JSON `FileStore`, `BlobStore` (`memory`/`local`/`gcs`), dual `/v1/*` and `/desktop/v1/*` HTTP handlers (including `/v1/drops/{id}/diff`), bounded async detached `SELECTOR_DRIFT` healer queue, `5xx` stderr error logging, container-disk `/release/{asset}`, and human-approval `/v1/proposals/{id}/approve` gate.
- [`internal/healer/`](internal/healer/) — Exception-only `google.golang.org/adk/v2` `SequentialAgent` + `LoopAgent` (`MaxIterations: 3`) + `verify_recipe_on_snapshot` `functiontool` on Vertex AI, hybrid `AXTree + dom.html` grounding with `<canvas>`/WebGL blind-spot detection, and `testdata/drift/` benchmarks (`healer_test.go`, `axtree_bench_test.go`).
- [`cmd/deskdrop/`](cmd/deskdrop/) — Desktop CLI (`--driver cdp|uia`), non-blocking `trayCoordinator` + native Windows System Tray (`hideConsoleWindow()`, `MessageBoxTimeoutW` auto-decline), pre-prompt lease heartbeating, and non-Admin `reg.exe` `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` tray logon helper (`deskdrop schedule install|uninstall|status`).
- [`specs/`](specs/) — Frozen syntactic OpenAPI contract ([`deskdrop.yaml`](specs/deskdrop.yaml)), Behavioral Contract ([`deskdrop.behavior.yaml`](specs/deskdrop.behavior.yaml)), and AST rules ([`ast.rules.yaml`](specs/ast.rules.yaml)).
- [`prompts/REGENERATE.md`](prompts/REGENERATE.md) — Single-adapter regenerative blueprint.

## 3. Canonical Commands
- **Full verification gate:** `gate test deskdrop` (runs `scripts/gate.sh`: `gofmt -s`, `go vet ./...`, Windows `go vet`, `ast-grep scan -r specs/ast.rules.yaml`, `go test -race -count=1 ./...`)
- **Cross-compile CLI, Windows `.exe`, and server:** `make build`
- **Verify zero-dependency desktop CLI invariant:** `go test -run TestZeroExternalDepsInDesktopCLI .`

## 4. E2E / Production → Unit Escape-Analysis Rule
Whenever a defect escapes unit tests and is caught only in compiled CLI → relay loopback E2E, container smoke (`verify-nonroot-and-healthcheck`), or live production:
1. **Explain the escape:** Record why existing unit tests passed (missing boundary condition, unexercised error branch, OS-tag skew, or missing JS/Go parity check).
2. **Add a targeted unit/mutation regression test:** Before closing the fix, add a deterministic unit test (and verify a 1-line mutant fails it) covering that exact failure mode so it cannot regress without a full E2E run.
3. **Keep E2E focused on wiring:** Loopback E2E proves binary packaging, HTTP/WebSocket serialization, `FileStore` restart persistence, and CLI flag plumbing — never rely on E2E alone to guard domain invariants.

## 5. Hard Invariants
See [`CRITICAL.md`](CRITICAL.md), [`DECISIONS.md`](DECISIONS.md), and [`docs/roadmap.md`](docs/roadmap.md).
