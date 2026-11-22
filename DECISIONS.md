# Architectural Decisions (`DECISIONS.md`)

## ADR-001: Zero-Dependency Go CLI Over Browser CDP Behind `TargetDriver` Port (2026-11-21)
- **Context:** Extracting structured data from authenticated desktop web sessions should not require Python, Node.js, or Playwright bundles, nor trigger `navigator.webdriver` login blocks, while remaining ready for future native desktop accessibility adapters (`UIA`).
- **Decision:** Keep `cmd/deskdrop` 100% pure Go stdlib (`CGO_ENABLED=0`) behind `internal/driver.TargetDriver`, shipping Browser CDP (`internal/driver/cdp`) first with a single-attempt `DevToolsActivePort` approval latch and immediate stateless scratch-tab closure.

## ADR-002: Strictly Declarative Rules (`0` `eval_js`) Verified Identically in JS and Pure Go (2026-11-21)
- **Context:** Embedding arbitrary JavaScript snippets (`eval_js`) in recipes mixes code execution with configuration and makes offline verification impossible without a live browser.
- **Decision:** Retire `eval_js` completely in favor of declarative `FieldRule` and `ItemRule` contracts evaluated identically in browser JS (`internal/driver/cdp/extract.go`) and pure Go (`internal/verify` via `golang.org/x/net/html`), enabling deterministic offline verification against scrubbed `dom.html` snapshots.

## ADR-003: Atomic JSON `FileStore` & Hot-Reloadable `RECIPES_URI` Over PostgreSQL (2026-11-21)
- **Context:** A multi-table PostgreSQL database is unnecessary operational overhead for a single-replica personal relay coordinating one desktop worker, while hardcoding personal recipes in git prevents keeping the repository public and clean.
- **Decision:** Persist relay state via a mutex-guarded `MemoryStore` wrapped by atomic temp-file + `fsync` + rename `FileStore` (`/data/state.json`), and load approved recipes dynamically from `RECIPES_URI` (local file/dir or `gs://` with mtime/ETag reload), rejecting unapproved ad-hoc URLs with `HTTP 403`.

## ADR-004: Exception-Only `adk-go` Healer, `--fallback-gate=auto|tray` & Approval-Gated Write-Back (2026-11-21)
- **Context:** Running vision/LLM agents on every page extraction burns tokens and increases failure surface, whereas purely brittle selectors fail silently whenever a dashboard renames CSS classes.
- **Decision:** Run `google.golang.org/adk/v2` (`SequentialAgent` + `LoopAgent` with `MaxIterations: 3` and pure-Go `verify_recipe_on_snapshot` `functiontool`) strictly on `SELECTOR_DRIFT`: Tier 1 extracts offline from `screenshot.png` + scrubbed `dom.html`; Tier 2 optionally requests `1` bounded stateless follow-up (`<= 3` safe `{scroll, wait, click}` actions on the allowlisted domain, governed by `--fallback-gate=auto|tray`); verified recipe patches are stored as pending `RecipePatchProposal`s and written back to `RECIPES_URI` only upon explicit `POST /v1/proposals/{id}/approve`.

## ADR-005: Air Gap & Portable Repository Hygiene (2026-11-21)
- **Context:** `deskdrop` is a public reference repository that must build and pass tests cleanly on any workstation or Cloud Build container without leaking personal accounts, internal paths, or customer data.
- **Decision:** Enforce `hygiene_test.go` in `scripts/gate.sh` and `cloudbuild.yaml`: zero `.github/workflows/`, zero workstation paths or absolute file-URI links, `100%` resolving relative Markdown links, zero non-stdlib deps in `cmd/deskdrop`, and outbound `contract.EnforceAirGap` (`HTTP 422`) on both client and server.

## ADR-006: Hybrid `AXTree + dom.html` Exception Grounding, Bounded Pagination & Quiet `schtasks.exe` Sync (2026-11-22)
- **Context:** Pure `dom.html` snapshots carry verbose wrapper markup, yet pure CDP `Accessibility.getFullAXTree` snapshots omit HTML classes and `data-*` attributes needed to synthesize valid CSS selectors — and both are blind when KPIs render inside unlabelled `<canvas>`/WebGL surfaces. Separately, multi-page feeds need bounded `"Next page"` traversal without arbitrary scripts, and unattended Windows `schtasks.exe` timers must never flash console windows or spam Chrome 144+ `DevToolsActivePort` prompts.
- **Decision:**
  1. **Hybrid `AXTree` + `dom.html` + Canvas Blind-Spot Gate:** Capture `Accessibility.getFullAXTree` (`axtree.txt`) strictly on exception/fallback routes (`0` happy-path calls), redacting input values and stripping `ignored`/`InlineTextBox` noise (`>3x` compression vs. raw CDP JSON and `>2x` vs. utility-class HTML). Pair `axtree.txt` (semantic roles/states) with `dom.html` (CSS classes/attributes for `verify_recipe_on_snapshot`), and flag unlabelled `<canvas>`/WebGL surfaces (`UNLABELLED_CANVAS_SURFACE`) so Tier-1 recovers values from `screenshot.png` while the patch drafter abstains from unresolvable CSS selectors.
  2. **Declarative Pagination (`Step.Pagination`):** Support bounded `"Next page"` loops (`1..20` pages, same-domain lock, disabled/`aria-disabled` detection, early stop on `0` new items/rows or reaching `items.limit`) backed by shared cross-page deduplication (`contract.MergePaginatedStepResult`) and pure-Go multi-page verification (`verify.VerifyRecipeOnHTMLPages`).
  3. **Direct `schtasks.exe` Helper & `--quiet-cdp`:** Add `deskdrop schedule install|uninstall|status` invoking `schtasks.exe` directly (`0` shell interpolation) in either periodic `sync` (`--hide-console --quiet-cdp`, reporting idle worker heartbeats and exiting `0` without touching `DevToolsActivePort`) or `ONLOGON` persistent `tray` mode.
