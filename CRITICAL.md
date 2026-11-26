# Critical Invariants (`CRITICAL.md`)

## 1. Zero-External-Dependency Desktop Binary & Zero Postgres
- `cmd/deskdrop` must link strictly Go standard library packages (`0` external Go modules, `CGO_ENABLED=0`, verified in `hygiene_test.go`) across both the Browser CDP (`internal/driver/cdp/`) and Windows UI Automation (`internal/driver/uia/`) adapters.
- The relay uses an atomic JSON `FileStore` (`/data/state.json`), never PostgreSQL or `github.com/lib/pq`.

## 2. Declarative Extraction Only & Approved Use-Cases (`RECIPES_URI`)
- `eval_js` is retired completely (`contract.ParseJSONStrict` hard-rejects unknown fields). All extraction uses declarative `selectors`, `fields` (`FieldRule`), `items` (`ItemRule`), and bounded `pagination` (`PaginationRule` `mode: "click" | "scroll"`, `1..20` pages on the same allowlisted domain) evaluated identically across browser JS (`internal/driver/cdp`), pure Go HTML (`internal/verify`), and serialized `AXNode`/`UIA` semantic trees (`internal/contract/semantic_eval.go` + `internal/verify`).
- Unapproved recipes or ad-hoc URLs outside `RECIPES_URI` are rejected with `HTTP 403`.
- Rollout order: because `ReloadableCatalog` decodes `RECIPES_URI` with `contract.ParseJSONStrict` (`DisallowUnknownFields`), always deploy the updated `deskdrop-server` image (`ship deskdrop <tag>`) *before* uploading any `RECIPES_URI` bundle that introduces new `Recipe`, `Step`, or `FieldRule` JSON keys.

## 3. Privacy-Scrubbed Exception Diagnostics & Stateless Tabs
- Happy-path runs capture `0` screenshots, `0` HTML dumps, `0` `AXTree` dumps, and consume `$0` LLM tokens.
- On failure, `<script>`, `<style>`, `<svg>`, `<noscript>`, `<input value=...>`, and `<textarea>` contents are scrubbed from `dom.html`, password and editable input values (`password`, `edit`, `textbox`, `searchbox`, `combobox`, `spinbutton`) in `AXNode`/`axtree.txt` are redacted to `[REDACTED]`, `contract.EnforceAirGap` runs on both client and server (`HTTP 422`), and every scratch tab closes immediately.
- Passive background tray probes (`SessionPool.ProbeQuiet`) must never re-dial Chrome 144+ `DevToolsActivePort` without an explicit user reconnect (single-attempt approval latch), and Windows logon registration (`deskdrop schedule`) uses non-Admin `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` via `reg.exe` (`0` shell interpolation).

## 4. Bounded Exception Healing, Anti-Bot Fail-Closed & Human Approval Gate
- `adk-go` runs strictly on `SELECTOR_DRIFT` when `allow_computer_use_fallback` is `true`. Login, `/sorry/`, CAPTCHA, and challenge redirects fail closed immediately into the `AUTH_EXPIRED` domain circuit breaker (never attempting CAPTCHA bypass).
- Tier-2 follow-up tasks are capped at `1` stateless re-open with `1..3` safe `{scroll, wait, click}` actions on the allowlisted domain (zero text input, zero cross-domain navigation), governed locally by `--fallback-gate=auto|tray`.
- Verified `RecipePatchProposal` objects are written back to `RECIPES_URI` strictly upon explicit human approval (`POST /v1/proposals/{id}/approve`), with domain-pivot protection enforced in code.

## 5. Portable Repository Hygiene (`ADR-005`)
- CI/CD runs exclusively on Google Cloud Build ([`cloudbuild.yaml`](cloudbuild.yaml)); never add `.github/workflows/`.
- Keep zero personal handles, live hostnames, customer identifiers, or workstation paths in git.
