# DeskDrop Agent Instructions (`AGENTS.md`)

## 1. Purpose & Scope
`deskdrop` (`github.com/tvaroska/deskdrop`) is a single-repo, zero-Postgres desktop & browser declarative extraction utility and relay server structured around **Ports, Adapters, and Contracts**:
1. **`cmd/deskdrop` (`deskdrop` / `deskdrop.exe`):** Zero-external-dependency Go CLI & native Windows System Tray (`CGO_ENABLED=0`, `0` non-stdlib Go packages) that attaches over Chrome DevTools Protocol (CDP) to an existing logged-in browser, runs declarative extraction recipes (`0` `eval_js`), and enforces `--fallback-gate=auto|tray` for bounded Tier-2 recovery tasks.
2. **`cmd/server` (`deskdrop-server`):** Distroless Go relay (`65532:65532`, `64Mi` RAM) backed by an atomic JSON `FileStore` (`/data/state.json`), hot-reloadable `RECIPES_URI` catalog (local path or `gs://`), wire-compatible `/v1/*` and `/desktop/v1/*` endpoints, and an exception-only `google.golang.org/adk/v2` (`adk-go`) healer invoked strictly on `SELECTOR_DRIFT`.

## 2. Package Architecture (Ports, Adapters, and Contracts)
- [`internal/contract/`](internal/contract/) — Pure-stdlib schemas (`Recipe`, `Step`, `FieldRule`, `ItemRule`, `PaginationRule`, `AXNode`, `DropReport`, `Task`, `FallbackPlan`, `RecipePatchProposal`, `BusMessage`), compact `AXTree` formatter + `<canvas>`/WebGL blind-spot analyzer (`axtree.go`), cross-page deduplicating merger (`pagination.go`), compact number parsing, HTML privacy scrubbing (`ScrubHTML`), error triage classifier, and outbound air-gap enforcement (`EnforceAirGap`).
- [`internal/driver/`](internal/driver/) — `TargetDriver` Driven Port (`port.go`) and diagnostic `.zip` packaging (`error.json`, scrubbed `dom.html`, compact `axtree.txt`, `screenshot.png`).
- [`internal/driver/cdp/`](internal/driver/cdp/) — Pure-stdlib Browser CDP Adapter (RFC 6455 WebSocket, single-attempt `DevToolsActivePort` approval latch, multi-profile `BrowserContextID` ranking, declarative Shadow-DOM extractor, bounded `"Next page"` pagination loop, exception-only `Accessibility.getFullAXTree` capture, stateless scratch-tab cleanup).
- [`internal/bus/`](internal/bus/) — Pure-stdlib HTTP relay client for `cmd/deskdrop`.
- [`internal/verify/`](internal/verify/) — Pure-Go HTML declarative extractor, multi-page pagination verifier (`VerifyRecipeOnHTMLPages`), and `adk-go` `verify_recipe_on_snapshot` verifier (`golang.org/x/net/html`).
- [`internal/recipes/`](internal/recipes/) — Hot-reloadable `RECIPES_URI` catalog (`local` file/dir + `gs://` with mtime/ETag reload, `HTTP 403` unapproved-URL gate, and atomic write-back on approval).
- [`internal/server/`](internal/server/) — Mutex-guarded `MemoryStore` + atomic JSON `FileStore`, `BlobStore` (`memory`/`local`/`gcs`), dual `/v1/*` and `/desktop/v1/*` HTTP handlers, and human-approval `/v1/proposals/{id}/approve` gate.
- [`internal/healer/`](internal/healer/) — Exception-only `google.golang.org/adk/v2` `SequentialAgent` + `LoopAgent` (`MaxIterations: 3`) + `verify_recipe_on_snapshot` `functiontool` on Vertex AI, hybrid `AXTree + dom.html` grounding with `<canvas>`/WebGL blind-spot detection, and `testdata/drift/` benchmarks (`healer_test.go`, `axtree_bench_test.go`).
- [`cmd/deskdrop/`](cmd/deskdrop/) — Desktop CLI, native Windows System Tray, and `schtasks.exe` Windows Task Scheduler helper (`deskdrop schedule install|uninstall|status` + `--quiet-cdp` / `--hide-console`).
- [`specs/`](specs/) — Frozen syntactic OpenAPI contract ([`deskdrop.yaml`](specs/deskdrop.yaml)), Behavioral Contract ([`deskdrop.behavior.yaml`](specs/deskdrop.behavior.yaml)), and AST rules ([`ast.rules.yaml`](specs/ast.rules.yaml)).
- [`prompts/REGENERATE.md`](prompts/REGENERATE.md) — Single-adapter regenerative blueprint.

## 3. Canonical Commands
- **Full verification gate:** `gate test deskdrop` (runs `scripts/gate.sh`: `gofmt -s`, `go vet ./...`, `go test -race ./...`)
- **Cross-compile CLI, Windows `.exe`, and server:** `make build`
- **Verify zero-dependency desktop CLI invariant:** `go test -run TestZeroExternalDepsInDesktopCLI .`

## 4. Hard Invariants
See [`CRITICAL.md`](CRITICAL.md), [`DECISIONS.md`](DECISIONS.md), and [`docs/roadmap.md`](docs/roadmap.md).
