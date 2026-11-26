package healer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/server"
	"github.com/tvaroska/deskdrop/internal/verify"
)

func TestAXTreeGroundingCompressionAndHybridSelectorSynthesis(t *testing.T) {
	var v1Recipe contract.Recipe
	if err := contract.ParseJSONStrict(loadFixture(t, "status_v1_recipe.json"), &v1Recipe); err != nil {
		t.Fatalf("parse v1 recipe: %v", err)
	}
	divSoupHTML := string(loadFixture(t, "shadow_divsoup_v2.html"))
	scrubbedDOM := contract.ScrubHTML(divSoupHTML)
	rawAX := loadFixture(t, "shadow_divsoup_v2_ax.json")

	nodes, err := contract.ParseAXTreeJSON(rawAX)
	if err != nil {
		t.Fatalf("ParseAXTreeJSON failed: %v", err)
	}
	compactAX, stats := contract.FormatCompactAXTree(nodes, len(rawAX))
	t.Logf("grounding sizes: raw_ax_json=%dB compact_ax=%dB scrubbed_dom=%dB (kept_nodes=%d/%d)",
		stats.RawBytes, stats.CompactBytes, len(scrubbedDOM), stats.KeptNodes, stats.TotalNodes)

	// Empirical compression assertions: compact AXTree must be substantially smaller than both
	// raw CDP Accessibility.getFullAXTree JSON and utility-class div-soup dom.html.
	if stats.CompactBytes >= stats.RawBytes/3 {
		t.Fatalf("expected >3x compression vs raw CDP AX JSON, got compact=%d raw=%d", stats.CompactBytes, stats.RawBytes)
	}
	if stats.CompactBytes >= len(scrubbedDOM)/2 {
		t.Fatalf("expected >2x compression vs scrubbed div-soup HTML, got compact=%d dom=%d", stats.CompactBytes, len(scrubbedDOM))
	}
	if stats.CanvasBlindSpot {
		t.Fatalf("unexpected CanvasBlindSpot on semantic HTML fixture: %+v", stats)
	}
	for _, token := range []string{"All Systems Operational", "API Gateway", "99.99%", "Worker Relay", "Blob Storage"} {
		if !strings.Contains(compactAX, token) {
			t.Fatalf("compact AXTree dropped semantic value %q:\n%s", token, compactAX)
		}
	}

	// Prove AXTree-only selector synthesis gap: AXTree omits CSS classes (.v2-svc-card) and data-* attributes
	// (data-metric-id="sla"), so an AXTree-only role/tag guess fails pure-Go HTML verification.
	axOnlyGuess := v1Recipe
	axOnlyGuess.Steps = []contract.Step{
		{
			ID:  "status_overview",
			URL: "https://status.example.com/overview",
			Fields: map[string]contract.FieldRule{
				"overall_status": {Selector: "[role='status']"},
			},
			Items: &contract.ItemRule{
				Container: "[role='listitem']",
				Fields:    map[string]contract.FieldRule{"service": {Selector: "h3"}},
			},
			Expect: v1Recipe.Steps[0].Expect,
		},
	}
	axGuessOut, err := verify.VerifyRecipeOnHTML(axOnlyGuess, nil, "status_overview", scrubbedDOM, "https://status.example.com/overview")
	if err != nil || axGuessOut.Passed {
		t.Fatalf("expected AXTree-only guessed role selectors to fail CSS verification, got %+v err=%v", axGuessOut, err)
	}

	// Hybrid AXTree + dom.html synthesis: AXTree pinpoints semantic targets while dom.html provides exact selectors.
	hybridPatch := v1Recipe
	hybridPatch.Steps = []contract.Step{
		{
			ID:  "status_overview",
			URL: "https://status.example.com/overview",
			Fields: map[string]contract.FieldRule{
				"overall_status":   {Selector: "[data-status-pill='overall']"},
				"active_incidents": {Selector: "[data-incident-counter='open']", Parse: "number"},
			},
			Items: &contract.ItemRule{
				Container: "section.v2-svc-card",
				DedupeBy:  "service",
				Limit:     20,
				Fields: map[string]contract.FieldRule{
					"service":    {Selector: "h3.svc-title"},
					"state":      {Selector: "span.svc-state"},
					"uptime_pct": {Selector: "span[data-metric-id='sla']", Parse: "float"},
				},
			},
			Expect: v1Recipe.Steps[0].Expect,
		},
	}
	hybridJSON, _ := json.Marshal(hybridPatch)

	var sawAXTreeInPrompt bool
	mockModel := &scriptedLLM{
		handler: func(req *model.LLMRequest) *model.LLMResponse {
			for _, c := range req.Contents {
				if c == nil {
					continue
				}
				for _, p := range c.Parts {
					if p != nil && strings.Contains(p.Text, "Compact AXTree (axtree.txt):") {
						sawAXTreeInPrompt = true
					}
				}
			}
			if hasTool(req, "submit_fallback_report") {
				if _, done := lastFunctionResponse(req, "submit_fallback_report"); done {
					return &model.LLMResponse{Content: genai.NewContentFromText("ok", genai.RoleModel), TurnComplete: true}
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
							}),
						},
					},
					TurnComplete: true,
				}
			}
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
				return &model.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							genai.NewPartFromFunctionCall("verify_recipe_on_snapshot", map[string]any{
								"recipe_json": string(hybridJSON),
								"rationale":   "Mapped AXTree semantic headings/KPIs onto section.v2-svc-card and data-metric-id='sla'",
							}),
						},
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
		Task:       contract.Task{ID: "task_ax_1", Recipe: v1Recipe.Name, Domain: "status.example.com"},
		Recipe:     v1Recipe,
		StepID:     "status_overview",
		LandedURL:  "https://status.example.com/overview",
		ErrorMsg:   "step \"status_overview\" extracted 0 item(s), expected >= 3",
		DOMHTML:    scrubbedDOM,
		AXTreeText: compactAX,
		AllowTier2: true,
	})
	if err != nil {
		t.Fatalf("HealDrift failed: %v", err)
	}
	if !sawAXTreeInPrompt {
		t.Fatal("expected Compact AXTree to be injected into healer prompt")
	}
	if res.Proposal == nil || !res.Proposal.Verified || res.Proposal.Iterations != 1 {
		t.Fatalf("expected 1-iteration verified hybrid proposal, got %+v", res.Proposal)
	}
}

func TestCanvasWebGLBlindSpotMultimodalFallbackAndPatchAbstention(t *testing.T) {
	var v1Recipe contract.Recipe
	_ = contract.ParseJSONStrict(loadFixture(t, "status_v1_recipe.json"), &v1Recipe)

	canvasHTML := contract.ScrubHTML(string(loadFixture(t, "canvas_webgl_metrics.html")))
	nodes, err := contract.ParseAXTreeJSON(loadFixture(t, "canvas_webgl_metrics_ax.json"))
	if err != nil {
		t.Fatal(err)
	}
	compactAX, stats := contract.FormatCompactAXTree(nodes, 300)
	if !stats.CanvasBlindSpot {
		t.Fatalf("expected CanvasBlindSpot=true on unlabelled WebGL canvas, got %+v\n%s", stats, compactAX)
	}

	var (
		sawCanvasWarning bool
		sawScreenshot    bool
	)
	mockModel := &scriptedLLM{
		handler: func(req *model.LLMRequest) *model.LLMResponse {
			for _, c := range req.Contents {
				if c == nil {
					continue
				}
				for _, p := range c.Parts {
					if p == nil {
						continue
					}
					if strings.Contains(p.Text, "UNLABELLED_CANVAS_SURFACE") {
						sawCanvasWarning = true
					}
					if p.InlineData != nil && p.InlineData.MIMEType == "image/png" {
						sawScreenshot = true
					}
				}
			}
			if hasTool(req, "submit_fallback_report") {
				if _, done := lastFunctionResponse(req, "submit_fallback_report"); done {
					return &model.LLMResponse{Content: genai.NewContentFromText("Recovered from screenshot.", genai.RoleModel), TurnComplete: true}
				}
				return &model.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{
							genai.NewPartFromFunctionCall("submit_fallback_report", map[string]any{
								"fields": map[string]any{"overall_status": "All Systems Operational"},
								"items": []any{
									map[string]any{"service": "API Gateway", "state": "Operational", "uptime_pct": 99.99},
								},
								"summary_text": "Read KPIs visually from WebGL screenshot",
							}),
						},
					},
					TurnComplete: true,
				}
			}
			if hasTool(req, "exit_loop") {
				// Patch drafter abstains immediately because CSS selectors cannot read WebGL pixels.
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
		Task:          contract.Task{ID: "task_canvas_1", Recipe: v1Recipe.Name, Domain: "status.example.com"},
		Recipe:        v1Recipe,
		StepID:        "status_overview",
		LandedURL:     "https://status.example.com/overview",
		ErrorMsg:      "step \"status_overview\" extracted 0 item(s), expected >= 3",
		DOMHTML:       canvasHTML,
		AXTreeText:    compactAX,
		ScreenshotPNG: []byte{0x89, 0x50, 0x4E, 0x47},
		AllowTier2:    false,
	})
	if err != nil {
		t.Fatalf("HealDrift on canvas failed: %v", err)
	}
	if !sawCanvasWarning || !sawScreenshot {
		t.Fatalf("expected UNLABELLED_CANVAS_SURFACE warning (%v) and PNG screenshot (%v)", sawCanvasWarning, sawScreenshot)
	}
	if res.FallbackReport == nil {
		t.Fatal("expected Tier-1 multimodal fallback report from screenshot.png")
	}
	if res.Proposal != nil {
		t.Fatalf("expected nil Proposal when metrics exist only inside unlabelled <canvas>, got %+v", res.Proposal)
	}
}
