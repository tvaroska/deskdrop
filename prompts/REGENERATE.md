# Regenerative Blueprint (`prompts/REGENERATE.md`)

DeskDrop is designed around **Ports, Adapters, and Contracts** so any single bounded Driven Adapter can be deleted and regenerated from its frozen contract triad and conformance tests without touching the rest of the repository.

## Target Regenerable Adapter: Pure-Go HTML & Semantic Verifier (`internal/verify/`)

- **Scope:** `internal/verify/selector.go`, `internal/verify/verify.go`, and `internal/verify/paginate.go` (`~1,000` LOC total across the 3 files; shared pure-stdlib CSS selector AST parsing lives in [`internal/contract/selector_ast.go`](../internal/contract/selector_ast.go) and semantic tree evaluation lives in [`internal/contract/semantic_eval.go`](../internal/contract/semantic_eval.go) + [`internal/contract/semantic_tree.go`](../internal/contract/semantic_tree.go)).
- **Exported Contracts:** `VerifyRecipeOnHTML`, `VerifyRecipeOnHTMLPages`, `VerifyRecipeOnAXTree`, `VerifyRecipeOnAXTreeJSON`, `ExtractStepFromHTML`, `ExtractStepFromAXTree`, `QueryAll`, `QueryFirst`, `Matches`, `Closest`.
- **Contract Triad:**
  1. **Type & Rule Contract:** [`internal/contract/types.go`](../internal/contract/types.go) (`Recipe`, `Step`, `FieldRule` including `fallback_regex`/`fallback_group`, `ItemRule`, `Expectation`, `StepResult`), [`internal/contract/pagination.go`](../internal/contract/pagination.go), [`internal/contract/number.go`](../internal/contract/number.go), [`internal/contract/selector_ast.go`](../internal/contract/selector_ast.go), and [`internal/contract/semantic_eval.go`](../internal/contract/semantic_eval.go)
  2. **Behavioral & API Contract:** [`specs/deskdrop.behavior.yaml`](../specs/deskdrop.behavior.yaml) and [`specs/deskdrop.yaml`](../specs/deskdrop.yaml)
  3. **Structural AST Rules:** [`specs/ast.rules.yaml`](../specs/ast.rules.yaml)
- **Verification Gate:** `internal/verify/verify_test.go` + `internal/healer/healer_test.go` (`testdata/drift/` v1-to-v2 benchmark) + `scripts/gate.sh` (`< 10s`).

## Why a Naive Prompt Fails

A one-paragraph prompt ("parse HTML with CSS selectors in Go") routinely drops:
- Shadow-DOM `>>>` combinators over flattened `<div data-shadowroot="open">` captures, `:last-child`, `:not(...)`, and `[attr*="x" i]` case-insensitive attribute matching via `contract.ParseCSSSelectorList`
- Attribute fallback chains (`aria-label|text`), numeric `fallback_regex` evaluation when primary text is non-numeric, and ancestor climbing (`anchor_closest` + `closest`)
- Compact number parsing (`1.2K` -> `1200`, `3.5M` -> `3500000`, word-bounded so `"5 min"` stays `5`) and inline-span text spacing matching the in-browser JS engine
- Multi-page pagination parity (`VerifyRecipeOnHTMLPages` + `contract.MergePaginatedStepResult`) and semantic `AXNode`/Windows `UIA` tree verification (`VerifyRecipeOnAXTree` / `VerifyRecipeOnAXTreeJSON`)
- Strict `ValidateStepResult` enforcement (`min_items`, `required_fields` accepting numeric `0`, `min_tables`, `not_url_contains`)

## Step-by-Step Regeneration Commands

```bash
# 1. Delete the adapter implementation while preserving contracts and tests
rm -f internal/verify/selector.go internal/verify/verify.go internal/verify/paginate.go

# 2. Regenerate in a clean session (Claude Code example)
claude -p "Read AGENTS.md, CRITICAL.md, internal/contract/types.go, internal/contract/selector_ast.go, internal/contract/pagination.go, internal/contract/number.go, internal/contract/semantic_eval.go, specs/deskdrop.behavior.yaml, and internal/verify/verify_test.go. Regenerate internal/verify/selector.go, internal/verify/verify.go, and internal/verify/paginate.go using only Go stdlib, internal/contract, and golang.org/x/net/html so all tests in internal/verify and internal/healer pass with -race. Do not modify test files."

# 3. Verify conformance, AST rules, and full repository gate
./scripts/gate.sh
```

## Secondary Regenerable Adapter: Atomic `FileStore` (`internal/server/store_file.go`)

Delete `internal/server/store_file.go` while keeping `internal/server/store.go`, `internal/server/store_memory.go`, `internal/server/store_file_test.go`, and `specs/deskdrop.behavior.yaml`, then regenerate and verify with `./scripts/gate.sh`.
