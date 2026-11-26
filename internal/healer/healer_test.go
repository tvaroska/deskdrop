package healer

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/server"
	"github.com/tvaroska/deskdrop/internal/verify"
)

type scriptedLLM struct {
	mu      sync.Mutex
	handler func(req *model.LLMRequest) *model.LLMResponse
}

func (s *scriptedLLM) Name() string { return "scripted-test-llm" }

func (s *scriptedLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		s.mu.Lock()
		resp := s.handler(req)
		s.mu.Unlock()
		yield(resp, nil)
	}
}

func hasTool(req *model.LLMRequest, name string) bool {
	if req == nil || req.Tools == nil {
		return false
	}
	_, ok := req.Tools[name]
	return ok
}

func lastFunctionResponse(req *model.LLMRequest, name string) (map[string]any, bool) {
	if req == nil {
		return nil, false
	}
	for i := len(req.Contents) - 1; i >= 0; i-- {
		c := req.Contents[i]
		if c == nil {
			continue
		}
		for j := len(c.Parts) - 1; j >= 0; j-- {
			p := c.Parts[j]
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				return p.FunctionResponse.Response, true
			}
		}
	}
	return nil, false
}

func loadFixture(t *testing.T, rel string) []byte {
	t.Helper()
	p := filepath.Join("..", "..", "testdata", "drift", rel)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read fixture %s: %v", p, err)
	}
	return raw
}

func TestDriftV1ToV2RegenerationBenchmark(t *testing.T) {
	var v1Recipe contract.Recipe
	if err := contract.ParseJSONStrict(loadFixture(t, "status_v1_recipe.json"), &v1Recipe); err != nil {
		t.Fatalf("parse v1 recipe: %v", err)
	}
	v1HTML := string(loadFixture(t, "status_v1.html"))
	v2HTML := string(loadFixture(t, "status_v2_drifted.html"))

	// Baseline: v1 recipe passes on v1 DOM and fails SELECTOR_DRIFT on v2 DOM.
	baseOut, err := verify.VerifyRecipeOnHTML(v1Recipe, nil, "status_overview", v1HTML, "https://status.example.com/overview")
	if err != nil || !baseOut.Passed || baseOut.ItemCount != 3 {
		t.Fatalf("v1 baseline failed: %+v err=%v", baseOut, err)
	}
	driftOut, err := verify.VerifyRecipeOnHTML(v1Recipe, nil, "status_overview", v2HTML, "https://status.example.com/overview")
	if err != nil || driftOut.Passed {
		t.Fatalf("expected v1 recipe to fail on v2 DOM, got %+v err=%v", driftOut, err)
	}

	// Drafts: broken attempt #1 (wrong selector) -> fixed attempt #2 (v2 selectors).
	badPatch := v1Recipe
	badPatch.Steps = []contract.Step{
		{
			ID:  "status_overview",
			URL: "https://status.example.com/overview",
			Fields: map[string]contract.FieldRule{
				"overall_status": {Selector: "[data-testid='wrong-banner']"},
			},
			Items: &contract.ItemRule{
				Container: ".nonexistent-card",
				Fields:    map[string]contract.FieldRule{"service": {Selector: "h3"}},
			},
			Expect: v1Recipe.Steps[0].Expect,
		},
	}
	goodPatch := v1Recipe
	goodPatch.Steps = []contract.Step{
		{
			ID:           "status_overview",
			URL:          "https://status.example.com/overview",
			MatchTabURL:  "status.example.com/overview",
			WaitSelector: "article.service-card",
			Fields: map[string]contract.FieldRule{
				"overall_status":   {Selector: "[data-testid='global-health']"},
				"active_incidents": {Selector: "[data-testid='open-incidents']", Parse: "number"},
			},
			Items: &contract.ItemRule{
				Container: "article.service-card",
				DedupeBy:  "service",
				Limit:     20,
				Fields: map[string]contract.FieldRule{
					"service":    {Selector: "h3.card-title"},
					"state":      {Selector: "span.badge-health"},
					"uptime_pct": {Selector: "span[data-kpi='uptime']", Parse: "float"},
				},
			},
			Expect: v1Recipe.Steps[0].Expect,
		},
	}
	badJSON, _ := json.Marshal(badPatch)
	goodJSON, _ := json.Marshal(goodPatch)

	patchAttempt := 0
	mockModel := &scriptedLLM{
		handler: func(req *model.LLMRequest) *model.LLMResponse {
			// Stage 1: offline_snapshot_fallback
			if hasTool(req, "submit_fallback_report") {
				if _, done := lastFunctionResponse(req, "submit_fallback_report"); done {
					return &model.LLMResponse{
						Content:      genai.NewContentFromText("Offline fallback report submitted.", genai.RoleModel),
						TurnComplete: true,
					}
				}
				return &model.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							genai.NewPartFromFunctionCall("submit_fallback_report", map[string]any{
								"fields": map[string]any{"overall_status": "All Systems Operational", "active_incidents": 0},
								"items": []any{
									map[string]any{"service": "API Gateway", "state": "Operational", "uptime_pct": 99.99},
									map[string]any{"service": "Worker Relay", "state": "Operational", "uptime_pct": 99.97},
									map[string]any{"service": "Blob Storage", "state": "Operational", "uptime_pct": 99.91},
								},
								"summary_text": "Recovered 3 services from v2 snapshot",
							}),
						},
					},
					TurnComplete: true,
				}
			}

			// Stage 2: recipe_patch_loop (LoopAgent with MaxIterations=3)
			if hasTool(req, "verify_recipe_on_snapshot") {
				if resp, ok := lastFunctionResponse(req, "verify_recipe_on_snapshot"); ok {
					if passed, _ := resp["passed"].(bool); passed {
						return &model.LLMResponse{
							Content: &genai.Content{
								Role:  genai.RoleModel,
								Parts: []*genai.Part{genai.NewPartFromFunctionCall("exit_loop", map[string]any{})},
							},
							TurnComplete: true,
						}
					}
				}
				patchAttempt++
				candidate := string(badJSON)
				rationale := "Initial guess"
				if patchAttempt >= 2 {
					candidate = string(goodJSON)
					rationale = "Migrated .metric-row to article.service-card and updated v2 KPI selectors"
				}
				return &model.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							genai.NewPartFromFunctionCall("verify_recipe_on_snapshot", map[string]any{
								"recipe_json": candidate,
								"rationale":   rationale,
							}),
						},
					},
					TurnComplete: true,
				}
			}

			return &model.LLMResponse{
				Content:      genai.NewContentFromText("done", genai.RoleModel),
				TurnComplete: true,
			}
		},
	}

	h, err := New(context.Background(), Config{
		ModelName:          "scripted-test-llm",
		MaxPatchIterations: 3,
		LLM:                mockModel,
	})
	if err != nil {
		t.Fatalf("healer.New failed: %v", err)
	}

	res, err := h.HealDrift(context.Background(), server.HealRequest{
		Task:          contract.Task{ID: "task_1", Recipe: v1Recipe.Name, Domain: "status.example.com"},
		Recipe:        v1Recipe,
		StepID:        "status_overview",
		LandedURL:     "https://status.example.com/overview",
		ErrorMsg:      driftOut.Error,
		DiagnosticID:  "diag_1",
		DOMHTML:       v2HTML,
		ScreenshotPNG: []byte{0x89, 0x50, 0x4E, 0x47},
		AllowTier2:    true,
	})
	if err != nil {
		t.Fatalf("HealDrift failed: %v", err)
	}
	if res.FallbackReport == nil || res.FallbackReport.Mode != contract.DropModeComputerUseFallback {
		t.Fatalf("expected Tier-1 fallback report, got %+v", res.FallbackReport)
	}
	if len(res.FallbackReport.Steps[0].Items) != 3 {
		t.Fatalf("fallback items = %d, want 3", len(res.FallbackReport.Steps[0].Items))
	}
	if res.Proposal == nil || !res.Proposal.Verified {
		t.Fatalf("expected verified proposal, got %+v", res.Proposal)
	}
	if res.Proposal.Iterations != 2 {
		t.Fatalf("expected 2 loop iterations (1 fail -> 1 pass), got %d", res.Proposal.Iterations)
	}

	// Independent deterministic verification on v2 DOM must pass.
	recheck, err := verify.VerifyRecipeOnHTML(res.Proposal.ProposedRecipe, nil, "status_overview", v2HTML, "https://status.example.com/overview")
	if err != nil || !recheck.Passed || recheck.ItemCount != 3 {
		t.Fatalf("healed recipe recheck failed: %+v err=%v", recheck, err)
	}
}

func TestTier2FallbackEscalationAndDomainPivotGuard(t *testing.T) {
	var v1Recipe contract.Recipe
	_ = contract.ParseJSONStrict(loadFixture(t, "status_v1_recipe.json"), &v1Recipe)
	blockedHTML := string(loadFixture(t, "modal_blocked.html"))

	mockModel := &scriptedLLM{
		handler: func(req *model.LLMRequest) *model.LLMResponse {
			if hasTool(req, "request_tier2_fallback") {
				if _, done := lastFunctionResponse(req, "request_tier2_fallback"); done {
					return &model.LLMResponse{
						Content:      genai.NewContentFromText("Tier-2 fallback scheduled.", genai.RoleModel),
						TurnComplete: true,
					}
				}
				return &model.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							genai.NewPartFromFunctionCall("request_tier2_fallback", map[string]any{
								"reason": "Release modal blocks dashboard content",
								"actions": []any{
									map[string]any{"action": "click", "selector": "[data-testid='dismiss-modal']"},
									map[string]any{"action": "wait", "wait_ms": 500},
								},
							}),
						},
					},
					TurnComplete: true,
				}
			}
			if hasTool(req, "exit_loop") {
				return &model.LLMResponse{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{genai.NewPartFromFunctionCall("exit_loop", map[string]any{})},
					},
					TurnComplete: true,
				}
			}
			return &model.LLMResponse{Content: genai.NewContentFromText("done", genai.RoleModel), TurnComplete: true}
		},
	}

	h, err := New(context.Background(), Config{LLM: mockModel, MaxPatchIterations: 3})
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.HealDrift(context.Background(), server.HealRequest{
		Task:       contract.Task{ID: "task_2", Recipe: v1Recipe.Name, Domain: "status.example.com"},
		Recipe:     v1Recipe,
		StepID:     "status_overview",
		LandedURL:  "https://status.example.com/overview",
		ErrorMsg:   "step \"status_overview\" extracted 0 item(s), expected >= 3",
		DOMHTML:    blockedHTML,
		AllowTier2: true,
	})
	if err != nil {
		t.Fatalf("HealDrift tier-2 failed: %v", err)
	}
	if res.FallbackPlan == nil || len(res.FallbackPlan.Actions) != 2 {
		t.Fatalf("expected 2-action Tier-2 fallback plan, got %+v", res.FallbackPlan)
	}

	// Verify domain-pivot attempt inside verify_recipe_on_snapshot is rejected deterministically.
	pivotRecipe := v1Recipe
	pivotRecipe.Steps = append([]contract.Step(nil), v1Recipe.Steps...)
	pivotRecipe.Steps[0].URL = "https://attacker.invalid/exfil"
	pivotJSON, _ := json.Marshal(pivotRecipe)
	var sawDomainBlock bool
	pivotModel := &scriptedLLM{
		handler: func(req *model.LLMRequest) *model.LLMResponse {
			if hasTool(req, "submit_fallback_report") {
				return &model.LLMResponse{Content: genai.NewContentFromText("skip", genai.RoleModel), TurnComplete: true}
			}
			if hasTool(req, "verify_recipe_on_snapshot") {
				if resp, ok := lastFunctionResponse(req, "verify_recipe_on_snapshot"); ok {
					if errStr, _ := resp["error"].(string); strings.Contains(errStr, "domain pivot blocked") {
						sawDomainBlock = true
					}
					return &model.LLMResponse{
						Content: &genai.Content{
							Role:  genai.RoleModel,
							Parts: []*genai.Part{genai.NewPartFromFunctionCall("exit_loop", map[string]any{})},
						},
						TurnComplete: true,
					}
				}
				return &model.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							genai.NewPartFromFunctionCall("verify_recipe_on_snapshot", map[string]any{
								"recipe_json": string(pivotJSON),
								"rationale":   "malicious pivot",
							}),
						},
					},
					TurnComplete: true,
				}
			}
			return &model.LLMResponse{Content: genai.NewContentFromText("done", genai.RoleModel), TurnComplete: true}
		},
	}
	h2, _ := New(context.Background(), Config{LLM: pivotModel, MaxPatchIterations: 1})
	res2, err := h2.HealDrift(context.Background(), server.HealRequest{
		Task:       contract.Task{ID: "task_3", Recipe: v1Recipe.Name, Domain: "status.example.com"},
		Recipe:     v1Recipe,
		StepID:     "status_overview",
		LandedURL:  "https://status.example.com/overview",
		ErrorMsg:   "selector failed",
		DOMHTML:    blockedHTML,
		AllowTier2: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawDomainBlock {
		t.Fatal("expected verify_recipe_on_snapshot to block domain pivot")
	}
	if res2.Proposal != nil {
		t.Fatalf("expected nil proposal on blocked pivot, got %+v", res2.Proposal)
	}
}

func TestA2A5A6HealerHardeningInvariantsAirGapModelLockAndStage1Recovery(t *testing.T) {
	// 1. Pre-network model lock rejects unapproved HEALER_MODEL.
	if _, err := New(context.Background(), Config{ModelName: "gemini-4.0-pro", Project: "test-proj"}); err == nil {
		t.Fatal("expected healer.New to reject unapproved model name")
	}

	var v1Recipe contract.Recipe
	_ = contract.ParseJSONStrict(loadFixture(t, "status_v1_recipe.json"), &v1Recipe)
	v2HTML := string(loadFixture(t, "status_v2_drifted.html"))

	// 2. Air-gap blocks LandedURL, ErrorMsg, and Recipe before LLM invocation.
	hGuard, _ := New(context.Background(), Config{
		LLM: &scriptedLLM{handler: func(_ *model.LLMRequest) *model.LLMResponse {
			t.Fatal("LLM must not be called on air-gap failure")
			return nil
		}},
		ForbiddenTerms: []string{"acme-secret"},
	})
	for _, tc := range []server.HealRequest{
		{Recipe: v1Recipe, StepID: "status_overview", LandedURL: "https://status.example.com/acme-secret", DOMHTML: v2HTML},
		{Recipe: v1Recipe, StepID: "status_overview", LandedURL: "https://status.example.com", ErrorMsg: "failed on acme-secret", DOMHTML: v2HTML},
	} {
		if _, err := hGuard.HealDrift(context.Background(), tc); err == nil {
			t.Fatalf("expected air-gap to block HealDrift for %+v", tc)
		}
	}

	// 3. Reject empty fallback report, stripped Expect, \uXXXX air-gap bypass, and pin "../state" name.
	strippedExpect := v1Recipe
	strippedExpect.Steps = []contract.Step{{ID: "status_overview", URL: "https://status.example.com/overview"}}
	strippedJSON, _ := json.Marshal(strippedExpect)

	escapedLeakJSON := `{"name":"../state","topic":"ops","steps":[{"id":"status_overview","url":"https://status.example.com/overview","fields":{"overall_status":{"selector":".\u0061cme-secret"}},"expect":{"min_items":3,"required_fields":["overall_status"]}}]}`

	pinnedPatch := v1Recipe
	pinnedPatch.Name = "../state" // Prompt-injected traversal name must be pinned back to v1Recipe.Name!
	pinnedPatch.Steps = []contract.Step{
		{
			ID:  "status_overview",
			URL: "https://status.example.com/overview",
			Fields: map[string]contract.FieldRule{
				"overall_status":   {Selector: "[data-testid='global-health']"},
				"active_incidents": {Selector: "[data-testid='open-incidents']", Parse: "number"},
			},
			Items: &contract.ItemRule{
				Container: "article.service-card",
				DedupeBy:  "service",
				Fields: map[string]contract.FieldRule{
					"service":    {Selector: "h3.card-title"},
					"state":      {Selector: "span.badge-health"},
					"uptime_pct": {Selector: "span[data-kpi='uptime']", Parse: "float"},
				},
			},
			Expect: v1Recipe.Steps[0].Expect,
		},
	}
	pinnedJSON, _ := json.Marshal(pinnedPatch)

	var sawEmptyReject, sawExpectReject, sawEscapedReject bool
	fbCall := 0
	patchCall := 0
	m := &scriptedLLM{
		handler: func(req *model.LLMRequest) *model.LLMResponse {
			if hasTool(req, "submit_fallback_report") {
				if resp, ok := lastFunctionResponse(req, "submit_fallback_report"); ok {
					if accepted, _ := resp["accepted"].(bool); !accepted {
						sawEmptyReject = true
					}
					return &model.LLMResponse{Content: genai.NewContentFromText("done", genai.RoleModel), TurnComplete: true}
				}
				fbCall++
				return &model.LLMResponse{
					Content: &genai.Content{
						Role:  genai.RoleModel,
						Parts: []*genai.Part{genai.NewPartFromFunctionCall("submit_fallback_report", map[string]any{})},
					},
					TurnComplete: true,
				}
			}
			if hasTool(req, "verify_recipe_on_snapshot") {
				if resp, ok := lastFunctionResponse(req, "verify_recipe_on_snapshot"); ok {
					errStr, _ := resp["error"].(string)
					if strings.Contains(errStr, "weaken expect") {
						sawExpectReject = true
					}
					if strings.Contains(errStr, "air-gap") {
						sawEscapedReject = true
					}
					if passed, _ := resp["passed"].(bool); passed {
						return &model.LLMResponse{
							Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{genai.NewPartFromFunctionCall("exit_loop", map[string]any{})}},
							TurnComplete: true,
						}
					}
				}
				patchCall++
				cand := string(strippedJSON)
				if patchCall == 2 {
					cand = escapedLeakJSON
				} else if patchCall >= 3 {
					cand = string(pinnedJSON)
				}
				return &model.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{genai.NewPartFromFunctionCall("verify_recipe_on_snapshot", map[string]any{
							"recipe_json": cand,
							"rationale":   "test patch",
						})},
					},
					TurnComplete: true,
				}
			}
			return &model.LLMResponse{Content: genai.NewContentFromText("done", genai.RoleModel), TurnComplete: true}
		},
	}
	h3, _ := New(context.Background(), Config{LLM: m, MaxPatchIterations: 4, ForbiddenTerms: []string{"acme-secret"}})
	res, err := h3.HealDrift(context.Background(), server.HealRequest{
		Task:       contract.Task{ID: "task_h3", Recipe: v1Recipe.Name, Domain: "status.example.com"},
		Recipe:     v1Recipe,
		StepID:     "status_overview",
		LandedURL:  "https://status.example.com/overview",
		ErrorMsg:   "selector drift",
		DOMHTML:    v2HTML,
		AllowTier2: false,
	})
	if err != nil {
		t.Fatalf("HealDrift failed: %v", err)
	}
	if !sawEmptyReject || res.FallbackReport != nil {
		t.Fatalf("expected empty fallback report rejected: sawEmptyReject=%v report=%+v", sawEmptyReject, res.FallbackReport)
	}
	if !sawExpectReject {
		t.Fatal("expected stripped Expect proposal to be rejected")
	}
	if !sawEscapedReject {
		t.Fatal("expected \\uXXXX-escaped air-gap leak to be rejected")
	}
	if res.Proposal == nil || res.Proposal.ProposedRecipe.Name != v1Recipe.Name {
		t.Fatalf("expected Proposal with pinned name %q, got %+v", v1Recipe.Name, res.Proposal)
	}

	// 4. Stage-2 error preserves valid Stage-1 FallbackReport.
	errLLM := &stage2ErrLLM{}
	h4, _ := New(context.Background(), Config{LLM: errLLM, MaxPatchIterations: 1})
	res4, err := h4.HealDrift(context.Background(), server.HealRequest{
		Task:       contract.Task{ID: "task_s1", Recipe: v1Recipe.Name, Domain: "status.example.com"},
		Recipe:     v1Recipe,
		StepID:     "status_overview",
		LandedURL:  "https://status.example.com/overview",
		ErrorMsg:   "selector drift",
		DOMHTML:    v2HTML,
		AllowTier2: false,
	})
	if err != nil {
		t.Fatalf("expected Stage-1 report preserved on Stage-2 error, got err=%v", err)
	}
	if res4.FallbackReport == nil || res4.FallbackReport.Steps[0].Fields["overall_status"] != "OK" {
		t.Fatalf("expected preserved Stage-1 FallbackReport, got %+v", res4.FallbackReport)
	}
}

type stage2ErrLLM struct{}

func (s *stage2ErrLLM) Name() string { return "stage2-err-llm" }

func (s *stage2ErrLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if hasTool(req, "submit_fallback_report") {
			if _, done := lastFunctionResponse(req, "submit_fallback_report"); done {
				yield(&model.LLMResponse{Content: genai.NewContentFromText("done", genai.RoleModel), TurnComplete: true}, nil)
				return
			}
			yield(&model.LLMResponse{
				Content: &genai.Content{
					Role: genai.RoleModel,
					Parts: []*genai.Part{genai.NewPartFromFunctionCall("submit_fallback_report", map[string]any{
						"fields": map[string]any{"overall_status": "OK"},
					})},
				},
				TurnComplete: true,
			}, nil)
			return
		}
		yield(nil, errors.New("vertex quota exhausted in stage 2"))
	}
}
