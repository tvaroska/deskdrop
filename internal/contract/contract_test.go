package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestA1DomainAllowlistAndResolveHardening(t *testing.T) {
	badRecipes := []Recipe{
		{
			Name:           "file-bypass",
			AllowedDomains: []string{"status.example.com"},
			Steps:          []Step{{ID: "s1", URL: "file" + ":///etc/passwd"}},
		},
		{
			Name:           "rel-match",
			AllowedDomains: []string{"status.example.com"},
			Steps:          []Step{{ID: "s1", MatchTabURL: "/"}},
		},
		{
			Name:           "js-scheme",
			AllowedDomains: []string{"status.example.com"},
			Steps:          []Step{{ID: "s1", URL: "javascript:alert(1)"}},
		},
		{
			Name:           "userinfo-url",
			AllowedDomains: []string{"status.example.com"},
			Steps:          []Step{{ID: "s1", URL: "https://status.example.com@evil.invalid/x"}},
		},
	}
	for _, r := range badRecipes {
		if _, err := ResolveRecipe(r, nil); err == nil {
			t.Errorf("ResolveRecipe(%q) succeeded, want error", r.Name)
		}
	}

	if DomainsMatch("api.example.com", "www.example.com") {
		t.Fatal("DomainsMatch widened www.example.com to api.example.com")
	}
	if !DomainsMatch("www.example.com", "example.com") {
		t.Fatal("DomainsMatch should still allow www.example.com under example.com")
	}
	if !DomainsMatch("www.example.com", "www.example.com") {
		t.Fatal("DomainsMatch should allow exact www.example.com match")
	}

	pathVarRecipe := Recipe{
		Name:     "release-feed",
		Defaults: map[string]string{"org": "example-org", "repo": "example-repo"},
		Steps: []Step{{
			ID:          "releases",
			URL:         "https://releases.example.com/{{org}}/{{repo}}",
			MatchTabURL: "releases.example.com/{{org}}/{{repo}}",
		}},
	}
	doms := InferRecipeDomains(pathVarRecipe)
	if len(doms) != 1 || doms[0] != "releases.example.com" {
		t.Fatalf("InferRecipeDomains(pathVarRecipe) = %v, want [releases.example.com]", doms)
	}
	if err := ValidateRecipeDomains(pathVarRecipe, doms, nil); err != nil {
		t.Fatalf("ValidateRecipeDomains(pathVarRecipe) failed: %v", err)
	}

	pivotDefaultsRecipe := Recipe{
		Name:     "pivot",
		Defaults: map[string]string{"h": "evil.invalid"},
		Steps:    []Step{{ID: "s1", URL: "https://{{h}}/steal"}},
	}
	if err := ValidateRecipeDomains(pivotDefaultsRecipe, []string{"releases.example.com"}, nil); err == nil {
		t.Fatal("expected ValidateRecipeDomains to block defaults host-template pivot")
	}

	plan := FallbackPlan{
		AllowedDomain: "example.com",
		TargetURL:     "https://app.example.com/dash",
		Actions:       []FallbackAction{{Action: "wait", WaitMs: 100}},
	}
	if err := ValidateFallbackPlan(plan, nil); err == nil {
		t.Fatal("expected ValidateFallbackPlan to fail closed on nil allowedDomains")
	}
	if err := ValidateFallbackPlan(plan, []string{"app.example.com"}); err == nil {
		t.Fatal("expected ValidateFallbackPlan to reject plan.AllowedDomain wider than approved recipe domain")
	}
}

func TestA3ScrubHTMLStateMachineHardening(t *testing.T) {
	raw := `<html><body>` +
		`<input title="a>b" data-value="keep-me" value="leaked-after-gt">` +
		`<script>secret1()</script   >` +
		`<textarea name="t">secret2</textarea   >` +
		`<svg><svg><path/></svg>nested-svg-secret</svg>` +
		`<div class="ok">Visible</div>` +
		`<textarea>unclosed-eof-secret`

	scrubbed := ScrubHTML(raw)
	for _, banned := range []string{"leaked-after-gt", "secret1", "secret2", "nested-svg-secret", "unclosed-eof-secret"} {
		if strings.Contains(scrubbed, banned) {
			t.Errorf("ScrubHTML leaked %q in:\n%s", banned, scrubbed)
		}
	}
	if !strings.Contains(scrubbed, `data-value="keep-me"`) {
		t.Errorf("ScrubHTML corrupted data-value attribute in:\n%s", scrubbed)
	}
	if !strings.Contains(scrubbed, "Visible") {
		t.Errorf("ScrubHTML dropped visible text in:\n%s", scrubbed)
	}

	big := strings.Repeat("é", maxScrubbedHTMLBytes)
	out := ScrubHTML(big)
	if len(out) > maxScrubbedHTMLBytes || !utf8.ValidString(out) {
		t.Fatalf("ScrubHTML produced invalid UTF-8 or over-limit output: len=%d valid=%v", len(out), utf8.ValidString(out))
	}
}

func TestA4AXTreeDescendantRedactionAndParentIDOnly(t *testing.T) {
	nodes := []AXNode{
		{NodeID: "1", Role: &AXValue{Value: "RootWebArea"}, Name: &AXValue{Value: "App"}},
		{NodeID: "2", ParentID: "1", Role: &AXValue{Value: "heading"}, Name: &AXValue{Value: "Dashboard"}},
		{NodeID: "3", ParentID: "1", Role: &AXValue{Value: "textbox"}, Name: &AXValue{Value: "Secret Key"}},
		{NodeID: "4", ParentID: "3", Role: &AXValue{Value: "StaticText"}, Name: &AXValue{Value: "sk-live-supersecret"}},
		{NodeID: "5", ParentID: "1", Role: &AXValue{Value: "password"}, Name: &AXValue{Value: "Passphrase"}},
		{NodeID: "6", ParentID: "5", Role: &AXValue{Value: "StaticText"}, Name: &AXValue{Value: "hunter2-secret"}},
	}

	compact, _ := FormatCompactAXTree(nodes, 500)
	if !strings.Contains(compact, "Dashboard") {
		t.Fatalf("FormatCompactAXTree dropped ParentID-only child:\n%s", compact)
	}
	for _, secret := range []string{"sk-live-supersecret", "hunter2-secret"} {
		if strings.Contains(compact, secret) {
			t.Fatalf("FormatCompactAXTree leaked sensitive child StaticText %q:\n%s", secret, compact)
		}
	}

	res, err := ExtractStepFromAXNodes(Step{
		ID:        "s1",
		Selectors: map[string]string{"texts": "statictext"},
	}, nodes, "uia://window/App")
	if err != nil {
		t.Fatalf("ExtractStepFromAXNodes failed: %v", err)
	}
	for _, secret := range []string{"sk-live-supersecret", "hunter2-secret"} {
		if strings.Contains(res.Selectors["texts"], secret) || strings.Contains(res.TextExcerpt, secret) {
			t.Fatalf("ExtractStepFromAXNodes leaked sensitive child StaticText %q: selectors=%+v excerpt=%q", secret, res.Selectors, res.TextExcerpt)
		}
	}
}

func TestA5EnforceAirGapLiteralMatchingRedactionAndGoPaths(t *testing.T) {
	safeText := "Visit https://example.com/" + "go" + "/pricing or /" + "go" + "/docs and " + "go" + "/mod"
	if err := EnforceAirGap(safeText, nil); err != nil {
		t.Fatalf("EnforceAirGap false-blocked URL path: %v", err)
	}
	shortlink := "go" + "/secret-internal-plan"
	for _, sample := range []string{"see " + shortlink + " now", "https://" + shortlink} {
		err := EnforceAirGap(sample, nil)
		if err == nil {
			t.Fatalf("expected EnforceAirGap to block %q", sample)
		}
		if strings.Contains(err.Error(), "secret-internal-plan") {
			t.Fatalf("EnforceAirGap echoed matched secret in error: %v", err)
		}
	}
	if err := EnforceAirGap("fooXbar is safe", []string{"foo.bar"}); err != nil {
		t.Fatalf("regex metacharacter '.' over-blocked literal term: %v", err)
	}
	err := EnforceAirGap("prefix token$123 suffix", []string{"token$123"})
	if err == nil {
		t.Fatal("expected literal term with '$' to block")
	}
	if strings.Contains(err.Error(), "token$123") {
		t.Fatalf("EnforceAirGap echoed forbidden term in error: %v", err)
	}
}

func TestA6ValidateDropReportAirGapAndNonEmptyFallback(t *testing.T) {
	emptyFB := DropReport{
		Recipe: "demo",
		Topic:  "ops",
		Mode:   DropModeComputerUseFallback,
		Steps:  []StepResult{{StepID: "s1", URL: "https://example.com"}},
	}
	if err := ValidateDropReport(emptyFB, nil); err == nil {
		t.Fatal("expected empty fallback DropReport to be rejected")
	}

	leaked := DropReport{
		Recipe: "demo",
		Topic:  "ops",
		Mode:   DropModeDeterministic,
		Steps:  []StepResult{{StepID: "s1", Fields: map[string]any{"note": "secret-client-name"}}},
	}
	if err := ValidateDropReport(leaked, []string{"secret-client-name"}); err == nil {
		t.Fatal("expected ValidateDropReport to enforce extraForbidden air-gap terms")
	}
}

func TestA7ParseCompactNumberWordBoundaryAndSemanticSelectors(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"5 min", 5},
		{"3 months", 3},
		{"2 bytes", 2},
		{"18.4 MB", 18},
		{"1.2K", 1200},
		{"1.2 K impressions", 1200},
		{"3.5m impressions", 3500000},
	}
	for _, tc := range cases {
		got, ok := ParseCompactNumber(tc.in)
		if !ok || got != tc.want {
			t.Errorf("ParseCompactNumber(%q) = (%d, %v), want (%d, true)", tc.in, got, ok, tc.want)
		}
	}

	nodes := []AXNode{
		{NodeID: "1", Role: &AXValue{Value: "window"}, Name: &AXValue{Value: "App"}, ChildIDs: []string{"2"}},
		{NodeID: "2", Role: &AXValue{Value: "group"}, ClassName: "card", ChildIDs: []string{"3", "4"}},
		{NodeID: "3", Role: &AXValue{Value: "button"}, ClassName: "skip", Name: &AXValue{Value: "Ignore Reaction"}},
		{NodeID: "4", Role: &AXValue{Value: "button"}, ClassName: "keep", Name: &AXValue{Value: "64 Reactions"}},
	}
	res, err := ExtractStepFromAXNodes(Step{
		ID: "s1",
		Fields: map[string]FieldRule{
			"last_btn": {Selector: "group.card button:last-child:not(.skip)[name*='reaction' i]", Parse: "number"},
			"alt_num":  {Selector: "button.keep", Regex: `(?i)(?:impressions:\s*(\d+)|(\d+)\s+reactions)`, Parse: "number"},
		},
	}, nodes, "uia://window/App")
	if err != nil {
		t.Fatalf("ExtractStepFromAXNodes failed: %v", err)
	}
	if res.Fields["last_btn"] != int64(64) || res.Fields["alt_num"] != int64(64) {
		t.Fatalf("unexpected semantic selector/regex fields: %+v", res.Fields)
	}
}

func TestHostileAuditRegressionGuards(t *testing.T) {
	// A-3: HTML5 raw-text/RCDATA tags are never self-closing, and nested SVG skips quoted attrs.
	raw := `<textarea/>secret-ta</textarea><script/>secret-js()</script><svg><path d="</svg>"/><text>inside-svg</text></svg><div class="ok">kept</div>`
	scrubbed := ScrubHTML(raw)
	for _, leaked := range []string{"secret-ta", "secret-js", "inside-svg"} {
		if strings.Contains(scrubbed, leaked) {
			t.Fatalf("ScrubHTML leaked %q: %s", leaked, scrubbed)
		}
	}
	if !strings.Contains(scrubbed, "kept") {
		t.Fatalf("ScrubHTML stripped visible body after svg: %s", scrubbed)
	}

	// A-1: Opaque scheme with numeric port rejected; UIA recipe enforces vars and MatchTabURL.
	if got := ExtractDomain("file:8080"); got != "" {
		t.Fatalf("ExtractDomain(file:8080) = %q, want empty", got)
	}
	uiaRecipe := Recipe{
		Name:           "uia-var",
		Topic:          "uia",
		Driver:         "uia",
		AllowedDomains: []string{"uia.local"},
		Defaults:       map[string]string{"target": "uia://window/ok"},
		Steps:          []Step{{ID: "s1", URL: "{{target}}"}},
	}
	if err := ValidateRecipeDomains(uiaRecipe, []string{"uia.local"}, map[string]string{"target": "https://evil.invalid"}); err == nil {
		t.Fatal("expected UIA ValidateRecipeDomains to reject HTTP pivot via vars")
	}

	// A-6: Empty map item [{}] must not satisfy fallback DropReport non-empty guard.
	emptyItemFB := DropReport{
		Recipe: "demo",
		Topic:  "ops",
		Mode:   DropModeComputerUseFallback,
		Steps:  []StepResult{{StepID: "s1", Items: []map[string]any{{}}}},
	}
	if err := ValidateDropReport(emptyItemFB, nil); err == nil {
		t.Fatal("expected fallback DropReport with items:[{}] to be rejected")
	}
}

func TestA8PaginationTableShiftRowCapExplicitZeroWaitAndZeroRequiredField(t *testing.T) {
	// 1. Explicit wait_ms: 0 must stay 0 (not revert to DefaultPaginationWaitMs).
	var r Recipe
	raw := []byte(`{
		"name": "zero-wait",
		"topic": "t",
		"steps": [{
			"id": "s1",
			"url": "https://example.com/list",
			"pagination": {"next_selector": "a.next", "wait_ms": 0}
		}]
	}`)
	if err := ParseJSONStrict(raw, &r); err != nil {
		t.Fatalf("ParseJSONStrict failed: %v", err)
	}
	resolved, err := ResolveRecipe(r, nil)
	if err != nil {
		t.Fatalf("ResolveRecipe failed: %v", err)
	}
	if resolved.Steps[0].Pagination.EffectiveWaitMs() != 0 {
		t.Fatalf("EffectiveWaitMs = %d, want 0", resolved.Steps[0].Pagination.EffectiveWaitMs())
	}

	// Omitted wait_ms defaults to DefaultPaginationWaitMs.
	rDef, err := ResolveRecipe(Recipe{
		Name: "def-wait", Topic: "t",
		Steps: []Step{{ID: "s1", URL: "https://example.com", Pagination: &PaginationRule{NextSelector: "a.next"}}},
	}, nil)
	if err != nil || rDef.Steps[0].Pagination.EffectiveWaitMs() != DefaultPaginationWaitMs {
		t.Fatalf("default wait_ms = %d err=%v, want %d", rDef.Steps[0].Pagination.EffectiveWaitMs(), err, DefaultPaginationWaitMs)
	}

	// 2. Shifted table index matches by header signature across acc.Tables (no duplication; added == 0).
	p1 := StepResult{
		StepID: "s1",
		Tables: []TableData{
			{Headers: []string{"Summary", "Value"}, Rows: [][]string{{"Total", "10"}}},
			{Headers: []string{"Service", "Uptime"}, Rows: [][]string{{"API", "99.9%"}}},
		},
	}
	p2Shifted := StepResult{
		StepID: "s1",
		Tables: []TableData{
			// Table shifted from index 1 -> index 0 on page 2 with identical row.
			{Headers: []string{"Service", "Uptime"}, Rows: [][]string{{"API", "99.9%"}}},
		},
	}
	merged, added, _ := MergePaginatedStepResult(p1, p2Shifted, nil)
	if len(merged.Tables) != 2 {
		t.Fatalf("shifted table duplicated acc.Tables: got %d tables, want 2", len(merged.Tables))
	}
	if added != 0 {
		t.Fatalf("shifted identical table added=%d, want 0 (so StopWhenNoNew triggers)", added)
	}

	// 3. Cross-page merged table rows capped at MaxMergedTableRows.
	var manyRows [][]string
	for i := 0; i < MaxMergedTableRows+50; i++ {
		manyRows = append(manyRows, []string{fmt.Sprintf("row-%d", i)})
	}
	capped, _, _ := MergePaginatedStepResult(
		StepResult{StepID: "s1", Tables: []TableData{{Headers: []string{"ID"}, Rows: [][]string{{"seed"}}}}},
		StepResult{StepID: "s1", Tables: []TableData{{Headers: []string{"ID"}, Rows: manyRows}}},
		nil,
	)
	if len(capped.Tables[0].Rows) != MaxMergedTableRows {
		t.Fatalf("merged rows = %d, want capped at %d", len(capped.Tables[0].Rows), MaxMergedTableRows)
	}

	// 4. RequiredFields accepts valid numeric 0 (int, int64, float64) while rejecting missing/blank strings.
	stepReq := Step{
		ID:     "s1",
		Expect: Expectation{RequiredFields: []string{"incidents", "errors_i", "delta_f"}},
	}
	zeroRes := StepResult{
		StepID: "s1",
		URL:    "https://example.com",
		Fields: map[string]any{"incidents": int64(0), "errors_i": 0, "delta_f": float64(0)},
	}
	if err := ValidateStepResult(stepReq, zeroRes); err != nil {
		t.Fatalf("ValidateStepResult rejected valid numeric 0 on required_fields: %v", err)
	}
	if err := ValidateStepResult(Step{ID: "s1", Expect: Expectation{RequiredFields: []string{"title"}}}, StepResult{
		StepID: "s1", URL: "https://example.com", Fields: map[string]any{"title": "   "},
	}); err == nil {
		t.Fatal("expected blank string required field to fail")
	}
}

func TestA11ParseJSONStrictRejectsTrailingTokens(t *testing.T) {
	validWithComment := []byte("// comment\n{\"name\":\"ok\",\"topic\":\"t\",\"steps\":[{\"id\":\"s1\",\"url\":\"https://example.com\"}]}\n// trailing comment\n")
	var r Recipe
	if err := ParseJSONStrict(validWithComment, &r); err != nil {
		t.Fatalf("valid JSON with comments failed: %v", err)
	}

	smuggled := []byte(`{"name":"ok","topic":"t","steps":[{"id":"s1","url":"https://example.com"}]}{"eval_js":"1"}`)
	if err := ParseJSONStrict(smuggled, &r); err == nil {
		t.Fatal("expected ParseJSONStrict to reject trailing JSON object")
	}
	trailingNum := []byte(`{"name":"ok","topic":"t","steps":[{"id":"s1","url":"https://example.com"}]} 123`)
	if err := ParseJSONStrict(trailingNum, &r); err == nil {
		t.Fatal("expected ParseJSONStrict to reject trailing scalar token")
	}
}

func TestT3AirGapAllBuiltinRulesHermeticHomeAndContractSuite(t *testing.T) {
	// 1. Unit tests remain hermetic even when $HOME/.config/agent-guard/customer_terms.txt exists.
	fakeHome := t.TempDir()
	guardDir := filepath.Join(fakeHome, ".config", "agent-guard")
	if err := os.MkdirAll(guardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(guardDir, "customer_terms.txt"), []byte("# comment\nHostOnlySecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	if err := EnforceAirGap("contains HostOnlySecret in unit test", nil); err != nil {
		t.Fatalf("EnforceAirGap must not read host $HOME implicitly: %v", err)
	}
	loaded := LoadLocalGuardTerms()
	if len(loaded) != 1 || loaded[0] != "HostOnlySecret" {
		t.Fatalf("LoadLocalGuardTerms = %v, want [HostOnlySecret]", loaded)
	}
	if err := EnforceAirGap("contains hostonlysecret", loaded); err == nil {
		t.Fatal("expected EnforceAirGap to block explicitly injected local terms")
	}

	// 2. All builtin air-gap patterns trigger reliably (mutation guard for airgap.go).
	samples := []string{
		"path " + "//" + "depot/project/file.go",
		"see " + "b" + "/12345678 for details",
		"open " + "go" + "/internal-roadmap",
		"url https://" + "go" + "/internal-doc",
		"host foo." + "corp" + ".google.com",
	}
	for _, s := range samples {
		if err := EnforceAirGap(s, nil); err == nil {
			t.Errorf("EnforceAirGap failed to block builtin pattern sample %q", s)
		}
	}

	// 3. ValidateStepResult covers all expectation guards.
	fullStep := Step{
		ID: "s1",
		Expect: Expectation{
			NotURLContains:  []string{"/login"},
			MustContainText: []string{"operational"},
			MinTables:       1,
			MinMetrics:      1,
			MinItems:        1,
			MinSelectorHits: 1,
			RequiredFields:  []string{"status"},
		},
	}
	goodRes := StepResult{
		StepID:    "s1",
		URL:       "https://status.example.com/ok",
		Tables:    []TableData{{Rows: [][]string{{"a"}}}},
		Metrics:   []MetricPair{{Label: "CPU", Value: "10%"}},
		Items:     []map[string]any{{"name": "api"}},
		Selectors: map[string]string{"banner": "Operational"},
		Fields:    map[string]any{"status": "Operational"},
	}
	if err := ValidateStepResult(fullStep, goodRes); err != nil {
		t.Fatalf("expected valid StepResult to pass, got %v", err)
	}
	badCases := []StepResult{
		{StepID: "s1", URL: "https://status.example.com/login"},
		{StepID: "s1", URL: "https://status.example.com/ok"},
		{StepID: "s1", URL: "https://status.example.com/ok", TextExcerpt: "operational"},
		{StepID: "s1", URL: "https://status.example.com/ok", TextExcerpt: "operational", Tables: goodRes.Tables},
		{StepID: "s1", URL: "https://status.example.com/ok", TextExcerpt: "operational", Tables: goodRes.Tables, Metrics: goodRes.Metrics},
		{StepID: "s1", URL: "https://status.example.com/ok", TextExcerpt: "operational", Tables: goodRes.Tables, Metrics: goodRes.Metrics, Items: goodRes.Items},
		{StepID: "s1", URL: "https://status.example.com/ok", TextExcerpt: "operational", Tables: goodRes.Tables, Metrics: goodRes.Metrics, Items: goodRes.Items, Selectors: goodRes.Selectors},
	}
	for i, bc := range badCases {
		if err := ValidateStepResult(fullStep, bc); err == nil {
			t.Errorf("badCase[%d] unexpectedly passed ValidateStepResult", i)
		}
	}

	// 4. ValidateFallbackPlan negative paths (empty actions, wait bounds, click missing selector, scroll 0, userinfo URL).
	badPlans := []FallbackPlan{
		{AllowedDomain: "example.com", TargetURL: "https://example.com", Actions: nil},
		{AllowedDomain: "example.com", TargetURL: "https://user@example.com", Actions: []FallbackAction{{Action: "wait", WaitMs: 10}}},
		{AllowedDomain: "", TargetURL: "https://example.com", Actions: []FallbackAction{{Action: "wait", WaitMs: 10}}},
		{AllowedDomain: "example.com", TargetURL: "https://example.com", Actions: []FallbackAction{{Action: "scroll", DeltaY: 0}}},
		{AllowedDomain: "example.com", TargetURL: "https://example.com", Actions: []FallbackAction{{Action: "wait", WaitMs: 6000}}},
		{AllowedDomain: "example.com", TargetURL: "https://example.com", Actions: []FallbackAction{{Action: "click", Selector: ""}}},
	}
	for i, bp := range badPlans {
		if err := ValidateFallbackPlan(bp, []string{"example.com"}); err == nil {
			t.Errorf("badPlans[%d] unexpectedly passed ValidateFallbackPlan", i)
		}
	}

	// 5. ValidateDropReport negative paths (missing recipe/topic/steps/step_id, invalid mode).
	badDrops := []DropReport{
		{Topic: "t", Steps: []StepResult{{StepID: "s1"}}},
		{Recipe: "r", Steps: []StepResult{{StepID: "s1"}}},
		{Recipe: "r", Topic: "t", Mode: "unknown", Steps: []StepResult{{StepID: "s1"}}},
		{Recipe: "r", Topic: "t"},
		{Recipe: "r", Topic: "t", Steps: []StepResult{{StepID: ""}}},
	}
	for i, bd := range badDrops {
		if err := ValidateDropReport(bd, nil); err == nil {
			t.Errorf("badDrops[%d] unexpectedly passed ValidateDropReport", i)
		}
	}

	// 6. Semantic tree full path (headings, metric cards, tables, ItemRule + AnchorClosest + DedupeBy + Limit, fallback_regex, CSS AST).
	semNodes := []AXNode{
		{NodeID: "1", Role: &AXValue{Value: "window"}, Name: &AXValue{Value: "Console"}, ChildIDs: []string{"2", "3", "4", "8"}},
		{NodeID: "2", Role: &AXValue{Value: "heading"}, Name: &AXValue{Value: "System Metrics"}},
		{NodeID: "3", Role: &AXValue{Value: "group"}, ChildIDs: []string{"3a", "3b"}},
		{NodeID: "3a", Role: &AXValue{Value: "StaticText"}, Name: &AXValue{Value: "Active Jobs"}},
		{NodeID: "3b", Role: &AXValue{Value: "StaticText"}, Name: &AXValue{Value: "0"}},
		{NodeID: "4", Role: &AXValue{Value: "table"}, ChildIDs: []string{"5", "6"}},
		{NodeID: "5", Role: &AXValue{Value: "columnheader"}, Name: &AXValue{Value: "Name"}},
		{NodeID: "6", Role: &AXValue{Value: "row"}, ChildIDs: []string{"7"}},
		{NodeID: "7", Role: &AXValue{Value: "cell"}, Name: &AXValue{Value: "worker-1"}},
		{NodeID: "8", Role: &AXValue{Value: "list"}, ChildIDs: []string{"9", "10"}},
		{NodeID: "9", Role: &AXValue{Value: "listitem"}, AutomationID: "item-1", ChildIDs: []string{"9a", "9b"}},
		{NodeID: "9a", Role: &AXValue{Value: "StaticText"}, Name: &AXValue{Value: "Service A"}},
		{NodeID: "9b", Role: &AXValue{Value: "StaticText"}, Name: &AXValue{Value: "Views: 1.5K"}},
		{NodeID: "10", Role: &AXValue{Value: "listitem"}, AutomationID: "item-2", ChildIDs: []string{"10a", "10b"}},
		{NodeID: "10a", Role: &AXValue{Value: "StaticText"}, Name: &AXValue{Value: "Service B"}},
		{NodeID: "10b", Role: &AXValue{Value: "StaticText"}, Name: &AXValue{Value: "Views: 0"}},
	}
	semRes, err := ExtractStepFromAXNodes(Step{
		ID: "sem",
		Fields: map[string]FieldRule{
			"jobs":    {Selector: "group > statictext:last-child", Parse: "number"},
			"missing": {Selector: "#does-not-exist", Parse: "number"},
		},
		Items: &ItemRule{
			Container: "list > listitem",
			DedupeBy:  "name",
			Limit:     5,
			Fields: map[string]FieldRule{
				"name":  {Selector: "text:first-child"},
				"views": {Selector: "text:nth-child(2)", FallbackRegex: `Views:\s*([\d.]+[Kk]?)`, Parse: "number"},
			},
		},
		Expect: Expectation{RequiredFields: []string{"jobs"}, MinItems: 2, MinTables: 1, MinMetrics: 1},
	}, semNodes, "uia://window/Console")
	if err != nil {
		t.Fatalf("ExtractStepFromAXNodes failed: %v", err)
	}
	if err := ValidateStepResult(Step{ID: "sem", Expect: Expectation{RequiredFields: []string{"jobs"}, MinItems: 2, MinTables: 1, MinMetrics: 1}}, semRes); err != nil {
		t.Fatalf("semantic ValidateStepResult failed: %v (res=%+v)", err, semRes)
	}
	if _, exists := semRes.Fields["missing"]; exists {
		t.Fatalf("expected unmatched top-level numeric field to be omitted, got %+v", semRes.Fields)
	}
	if len(semRes.Headings) != 1 || semRes.Headings[0] != "System Metrics" {
		t.Fatalf("headings = %v, want [System Metrics]", semRes.Headings)
	}
	if semRes.Items[0]["views"] != int64(1500) || semRes.Items[1]["views"] != int64(0) {
		t.Fatalf("unexpected semantic item views: %+v", semRes.Items)
	}
}
