package contract

import (
	"strings"
	"testing"
)

func TestParseJSONStrictRejectsLegacyEvalJS(t *testing.T) {
	raw := []byte(`{
		"name": "bad",
		"topic": "t",
		"steps": [{"id": "s1", "url": "https://example.com", "eval_js": "1+1"}]
	}`)
	var r Recipe
	if err := ParseJSONStrict(raw, &r); err == nil {
		t.Fatal("expected ParseJSONStrict to reject legacy eval_js field")
	}
}

func TestParseCompactNumberAndFloat(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1,234", 1234},
		{"1.2K", 1200},
		{"3.5m impressions", 3500000},
		{"42 reactions", 42},
	}
	for _, tc := range cases {
		got, ok := ParseCompactNumber(tc.in)
		if !ok || got != tc.want {
			t.Errorf("ParseCompactNumber(%q) = (%d, %v), want (%d, true)", tc.in, got, ok, tc.want)
		}
	}
}

func TestScrubHTMLStripsActiveAndSensitiveContent(t *testing.T) {
	raw := `<html><head><style>.a{color:red}</style><script>alert(1)</script></head>` +
		`<body><svg><path d="M0 0"/></svg>` +
		`<input type="password" value="super-secret-token">` +
		`<input name="csrf" value='abc123'>` +
		`<textarea name="notes">private note</textarea>` +
		`<div class="card">Public Metric 42</div></body></html>`

	scrubbed := ScrubHTML(raw)
	for _, banned := range []string{"alert(1)", "color:red", "<svg", "super-secret-token", "abc123", "private note"} {
		if strings.Contains(scrubbed, banned) {
			t.Errorf("ScrubHTML leaked %q in output:\n%s", banned, scrubbed)
		}
	}
	if !strings.Contains(scrubbed, "Public Metric 42") {
		t.Errorf("ScrubHTML stripped legitimate content:\n%s", scrubbed)
	}
}

func TestValidateFallbackPlanSafetyEnvelope(t *testing.T) {
	good := FallbackPlan{
		AllowedDomain: "example.com",
		TargetURL:     "https://app.example.com/dashboard",
		Actions: []FallbackAction{
			{Action: "click", Selector: "button.dismiss-modal"},
			{Action: "wait", WaitMs: 500},
			{Action: "scroll", DeltaY: 600},
		},
	}
	if err := ValidateFallbackPlan(good, []string{"example.com"}); err != nil {
		t.Fatalf("expected valid fallback plan, got: %v", err)
	}

	// Reject >3 actions.
	tooMany := good
	tooMany.Actions = append(tooMany.Actions, FallbackAction{Action: "scroll", DeltaY: 200})
	if err := ValidateFallbackPlan(tooMany, []string{"example.com"}); err == nil {
		t.Fatal("expected >3 actions to be rejected")
	}

	// Reject text input / navigation actions.
	badAction := good
	badAction.Actions = []FallbackAction{{Action: "type", Selector: "input"}}
	if err := ValidateFallbackPlan(badAction, []string{"example.com"}); err == nil {
		t.Fatal("expected unsupported 'type' action to be rejected")
	}

	// Reject cross-domain target URL.
	crossDomain := good
	crossDomain.TargetURL = "https://evil.org/steal"
	if err := ValidateFallbackPlan(crossDomain, []string{"example.com"}); err == nil {
		t.Fatal("expected cross-domain URL to be rejected")
	}
}

func TestClassifyExtractionError(t *testing.T) {
	if got := ClassifyExtractionError("step \"s\" extracted 0 item(s), expected >= 1", "https://example.com", "", true); got.Code != ErrorCodeSelectorDrift {
		t.Fatalf("expected SELECTOR_DRIFT, got %+v", got)
	}
	if got := ClassifyExtractionError("step \"s\" landed on blocked/login URL", "https://accounts.google.com/signin", "studio.youtube.com", false); got.Code != ErrorCodeAuthExpired || got.Domain != "studio.youtube.com" {
		t.Fatalf("expected AUTH_EXPIRED with fallback domain, got %+v", got)
	}
	if got := ClassifyExtractionError("fallback declined by user in tray", "https://example.com", "example.com", false); got.Code != ErrorCodeFallbackDeclined {
		t.Fatalf("expected FALLBACK_DECLINED, got %+v", got)
	}
	// Google Scholar /sorry/ rate-limit and login redirects must fail closed into AUTH_EXPIRED on scholar.google.com.
	if got := ClassifyExtractionError(
		`step "profile_citations" landed on blocked/login URL "https://www.google.com/sorry/index?continue=https://scholar.google.com/citations" (matched "/sorry/")`,
		"https://www.google.com/sorry/index?continue=https://scholar.google.com/citations",
		"scholar.google.com",
		false,
	); got.Code != ErrorCodeAuthExpired || got.Action != TriageHoldDomainAuth || got.Domain != "scholar.google.com" {
		t.Fatalf("expected /sorry/ redirect to trip AUTH_EXPIRED on scholar.google.com, got %+v", got)
	}
	if got := ClassifyExtractionError(
		"unusual traffic detected",
		"https://ipv4.google.com/sorry/index",
		"scholar.google.com",
		true,
	); got.Code != ErrorCodeAuthExpired || got.Domain != "scholar.google.com" {
		t.Fatalf("expected ipv4 /sorry/ challenge to trip AUTH_EXPIRED on scholar.google.com, got %+v", got)
	}
	if got := ClassifyExtractionError(
		"please sign in to continue",
		"https://accounts.google.com/v3/signin/identifier?continue=https://scholar.google.com/",
		"scholar.google.com",
		false,
	); got.Code != ErrorCodeAuthExpired || got.Domain != "scholar.google.com" {
		t.Fatalf("expected accounts.google.com login redirect to trip AUTH_EXPIRED on scholar.google.com, got %+v", got)
	}
}

func TestFormatCompactAXTreeRedactsInputsAndDetectsCanvasBlindSpot(t *testing.T) {
	raw := []byte(`{
		"nodes": [
			{"nodeId": "1", "role": {"value": "RootWebArea"}, "name": {"value": "Portal"}, "childIds": ["2", "3", "4", "5"]},
			{"nodeId": "2", "ignored": true, "role": {"value": "generic"}, "childIds": ["6"]},
			{"nodeId": "6", "role": {"value": "heading"}, "name": {"value": "Overview"}, "properties": [{"name": "level", "value": {"value": 1}}], "childIds": ["7"]},
			{"nodeId": "7", "role": {"value": "StaticText"}, "name": {"value": "Overview"}, "childIds": ["8"]},
			{"nodeId": "8", "role": {"value": "InlineTextBox"}, "name": {"value": "Overview"}},
			{"nodeId": "3", "role": {"value": "textbox"}, "name": {"value": "API Token"}, "value": {"value": "super-secret-value"}},
			{"nodeId": "4", "role": {"value": "generic"}, "childIds": ["9"]},
			{"nodeId": "9", "role": {"value": "button"}, "name": {"value": "Next page"}, "properties": [{"name": "disabled", "value": {"value": true}}]},
			{"nodeId": "5", "role": {"value": "canvas"}}
		]
	}`)
	nodes, err := ParseAXTreeJSON(raw)
	if err != nil {
		t.Fatalf("ParseAXTreeJSON failed: %v", err)
	}
	compact, stats := FormatCompactAXTree(nodes, len(raw))
	if strings.Contains(compact, "super-secret-value") {
		t.Fatalf("FormatCompactAXTree leaked textbox value:\n%s", compact)
	}
	if !strings.Contains(compact, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] for textbox value:\n%s", compact)
	}
	if strings.Contains(compact, "InlineTextBox") {
		t.Fatalf("expected InlineTextBox to be stripped:\n%s", compact)
	}
	if !strings.Contains(compact, "[UNGROUNDED_PIXEL_SURFACE]") || stats.UnlabelledCanvases != 1 {
		t.Fatalf("expected unlabelled canvas marker, stats=%+v\n%s", stats, compact)
	}
	if !strings.Contains(compact, `(disabled=true)`) {
		t.Fatalf("expected salient disabled property in compact output:\n%s", compact)
	}
	if !stats.CanvasBlindSpot {
		t.Fatalf("expected CanvasBlindSpot=true when unlabelled canvas has <=3 text nodes, got %+v", stats)
	}
}

func TestPaginationContractValidationAndMergeDedupe(t *testing.T) {
	rcp, err := ResolveRecipe(Recipe{
		Name:  "paged",
		Topic: "releases",
		Defaults: map[string]string{
			"next_btn": "button.next-page",
		},
		Steps: []Step{
			{
				ID:  "s1",
				URL: "https://example.com/releases",
				Items: &ItemRule{
					Container: "article.rel",
					DedupeBy:  "tag",
					Limit:     3,
					Fields:    map[string]FieldRule{"tag": {Selector: "a"}},
				},
				Pagination: &PaginationRule{
					NextSelector: "{{next_btn}}",
				},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("ResolveRecipe failed: %v", err)
	}
	if rcp.Steps[0].Pagination.NextSelector != "button.next-page" || rcp.Steps[0].Pagination.MaxPages != DefaultPaginationMaxPages {
		t.Fatalf("unexpected resolved pagination: %+v", rcp.Steps[0].Pagination)
	}

	// Reject unbounded max_pages > 20.
	_, err = ResolveRecipe(Recipe{
		Name:  "bad-paged",
		Topic: "t",
		Steps: []Step{
			{
				ID:         "s1",
				URL:        "https://example.com",
				Pagination: &PaginationRule{NextSelector: "a.next", MaxPages: 99},
			},
		},
	}, nil)
	if err == nil {
		t.Fatal("expected max_pages > 20 to fail validation")
	}

	p1 := StepResult{
		StepID: "s1",
		Items: []map[string]any{
			{"tag": "v1.0.0"},
			{"tag": "v1.0.1"},
		},
		Tables: []TableData{{Headers: []string{"Col"}, Rows: [][]string{{"R1"}}}},
	}
	p2 := StepResult{
		StepID: "s1",
		Items: []map[string]any{
			{"tag": "v1.0.1"}, // duplicate across page boundary
			{"tag": "v1.0.2"},
			{"tag": "v1.0.3"}, // should be capped by Limit=3
		},
		Tables: []TableData{{Headers: []string{"Col"}, Rows: [][]string{{"R1"}, {"R2"}}}},
	}
	merged, added, reachedLimit := MergePaginatedStepResult(p1, p2, rcp.Steps[0].Items)
	if len(merged.Items) != 3 || !reachedLimit {
		t.Fatalf("merged items = %d (reachedLimit=%v), want 3 (true)", len(merged.Items), reachedLimit)
	}
	if added != 2 {
		t.Fatalf("added = %d, want 2 (1 unique item + 1 unique table row)", added)
	}
	if len(merged.Tables[0].Rows) != 2 {
		t.Fatalf("merged table rows = %d, want 2", len(merged.Tables[0].Rows))
	}
}
