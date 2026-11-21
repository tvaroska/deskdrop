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
}
