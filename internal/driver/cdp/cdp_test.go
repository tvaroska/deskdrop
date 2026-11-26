package cdp

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/driver"
	"github.com/tvaroska/deskdrop/internal/verify"
)

func TestSessionPoolSingleAttemptApprovalLatch(t *testing.T) {
	pool := NewSessionPool()
	dialCount := 0
	pool.discoverFn = func(_ context.Context, _ string, _ string) (string, DiscoverySource, error) {
		return "ws://127.0.0.1:1/devtools/browser/fake", SourceActivePort, nil
	}
	pool.connectFn = func(_ context.Context, _ string) (*Client, error) {
		dialCount++
		return nil, errors.New("user dismissed prompt")
	}

	ctx := context.Background()
	if _, err := pool.Acquire(ctx, "", ""); err == nil {
		t.Fatal("expected first Acquire to fail")
	}
	if dialCount != 1 {
		t.Fatalf("dialCount = %d, want 1", dialCount)
	}
	if !pool.ActivePortAttempted() {
		t.Fatal("expected ActivePortAttempted to be true")
	}

	// Second call must hit the latch without re-dialing Chrome.
	if _, err := pool.Acquire(ctx, "", ""); !errors.Is(err, ErrApprovalLatchActive) {
		t.Fatalf("expected ErrApprovalLatchActive, got %v", err)
	}
	if dialCount != 1 {
		t.Fatalf("latch failed to suppress second dial: dialCount = %d", dialCount)
	}

	// Explicit user reset permits one fresh attempt.
	pool.ResetApprovalLatch()
	if _, err := pool.Acquire(ctx, "", ""); err == nil {
		t.Fatal("expected Acquire after reset to run connectFn")
	}
	if dialCount != 2 {
		t.Fatalf("dialCount after reset = %d, want 2", dialCount)
	}
}

func TestBuildDiagnosticZipScrubsHTMLAndEnforcesAirGap(t *testing.T) {
	stepErr := &driver.StepExecError{
		StepID: "metrics",
		Err:    errors.New("step \"metrics\" extracted 0 item(s), expected >= 1"),
		DOMHTML: `<html><head><script>secretScript()</script><style>body{}</style></head>` +
			`<body><svg><rect/></svg><input value="my-secret-pass"><div class="card">Hello</div></body></html>`,
		AXTreeText:    "[RootWebArea] \"Dash\"\n  [heading] \"Hello\" (level=1)",
		ScreenshotPNG: []byte{0x89, 0x50, 0x4E, 0x47},
	}

	zipBytes, err := driver.BuildDiagnosticZip("demo", stepErr, nil)
	if err != nil {
		t.Fatalf("BuildDiagnosticZip failed: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("invalid zip: %v", err)
	}
	entries := make(map[string][]byte)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(rc)
		_ = rc.Close()
		entries[f.Name] = raw
	}
	for _, name := range []string{"error.json", "dom.html", "axtree.txt", "screenshot.png"} {
		if _, ok := entries[name]; !ok {
			t.Fatalf("missing %s in diagnostic zip", name)
		}
	}
	domStr := string(entries["dom.html"])
	for _, banned := range []string{"secretScript", "<style", "<svg", "my-secret-pass"} {
		if strings.Contains(domStr, banned) {
			t.Errorf("dom.html leaked %q", banned)
		}
	}

	// Air-gap violation must block zip creation.
	leakedErr := &driver.StepExecError{
		StepID:  "metrics",
		Err:     errors.New("selector failed"),
		DOMHTML: `<html><body>forbidden-client-token</body></html>`,
	}
	if _, err := driver.BuildDiagnosticZip("demo", leakedErr, []string{"forbidden-client-token"}); err == nil {
		t.Fatal("expected BuildDiagnosticZip to block air-gap violation")
	}
}

func TestExecuteStepAndFallbackCloseScratchTabOnFailure(t *testing.T) {
	var (
		mu             sync.Mutex
		closedTargetID string
		methods        []string
	)

	srv := newMockCDPServer(t, func(method string, params map[string]any) (any, error) {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()

		switch method {
		case "Target.getTargets":
			return map[string]any{"targetInfos": []TargetInfo{}}, nil
		case "Target.createTarget":
			return map[string]any{"targetId": "scratch-1"}, nil
		case "Target.attachToTarget":
			return map[string]any{"sessionId": "sess-1"}, nil
		case "Target.closeTarget":
			mu.Lock()
			if tid, ok := params["targetId"].(string); ok {
				closedTargetID = tid
			}
			mu.Unlock()
			return map[string]any{"success": true}, nil
		case "Page.captureScreenshot":
			return map[string]any{"data": base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4E, 0x47})}, nil
		case "Accessibility.getFullAXTree":
			return map[string]any{
				"nodes": []map[string]any{
					{"nodeId": "1", "role": map[string]any{"value": "RootWebArea"}, "name": map[string]any{"value": "Dash"}, "childIds": []string{"2"}},
					{"nodeId": "2", "role": map[string]any{"value": "heading"}, "name": map[string]any{"value": "Dash"}},
				},
			}, nil
		case "Runtime.evaluate":
			expr, _ := params["expression"].(string)
			switch {
			case expr == "document.readyState":
				return map[string]any{"result": map[string]any{"type": "string", "value": "complete"}}, nil
			case strings.Contains(expr, "location.href || ''"):
				return map[string]any{"result": map[string]any{"type": "string", "value": "https://example.com/dash"}}, nil
			case strings.Contains(expr, "cloneClean"):
				return map[string]any{"result": map[string]any{"type": "string", "value": "<html><body><div class='v2'>Data</div></body></html>"}}, nil
			case strings.Contains(expr, "window.scrollBy"):
				return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
			case strings.Contains(expr, "evalItemRule"):
				return map[string]any{
					"result": map[string]any{
						"type": "object",
						"value": map[string]any{
							"title":        "Dash",
							"url":          "https://example.com/dash",
							"headings":     []string{"Dash"},
							"text_excerpt": "Data",
						},
					},
				}, nil
			default:
				return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
			}
		default:
			return map[string]any{}, nil
		}
	})
	defer srv.Close()

	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	client, err := Connect(context.Background(), wsURL)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	client.settleDelay = time.Millisecond
	defer client.Close()

	// Step expects >=1 item so it fails with StepExecError AND must still close scratch-1 immediately.
	step := contract.Step{
		ID:  "dash",
		URL: "https://example.com/dash",
		Expect: contract.Expectation{
			MinItems: 1,
		},
	}
	_, err = client.ExecuteStep(context.Background(), step)
	var execErr *driver.StepExecError
	if !errors.As(err, &execErr) {
		t.Fatalf("expected *driver.StepExecError, got %v", err)
	}
	if !strings.Contains(execErr.DOMHTML, "Data") {
		t.Fatalf("expected scrubbed DOMHTML on error, got %q", execErr.DOMHTML)
	}
	if !strings.Contains(execErr.AXTreeText, `[heading] "Dash"`) {
		t.Fatalf("expected compact AXTreeText on error, got %q", execErr.AXTreeText)
	}
	mu.Lock()
	gotClosed := closedTargetID
	mu.Unlock()
	if gotClosed != "scratch-1" {
		t.Fatalf("expected scratch tab to be closed immediately on failure, got %q", gotClosed)
	}

	// Tier-2 fallback also opens a fresh scratch tab, executes <=3 actions, and closes it.
	mu.Lock()
	closedTargetID = ""
	mu.Unlock()
	fbRes, err := client.ExecuteFallback(context.Background(), step, contract.FallbackPlan{
		AllowedDomain: "example.com",
		TargetURL:     "https://example.com/dash",
		Actions: []contract.FallbackAction{
			{Action: "scroll", DeltaY: 400},
			{Action: "wait", WaitMs: 10},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteFallback failed: %v", err)
	}
	if fbRes.ActionsRun != 2 {
		t.Fatalf("ActionsRun = %d, want 2", fbRes.ActionsRun)
	}
	if len(fbRes.ScreenshotPNG) == 0 || !strings.Contains(fbRes.DOMHTML, "Data") {
		t.Fatalf("expected screenshot + scrubbed HTML from fallback")
	}
	mu.Lock()
	gotClosed = closedTargetID
	mu.Unlock()
	if gotClosed != "scratch-1" {
		t.Fatalf("expected fallback scratch tab closed immediately, got %q", gotClosed)
	}
}

func TestHappyPathSkipsAXTreeAndPaginationLoopsUntilDisabled(t *testing.T) {
	var (
		mu       sync.Mutex
		axCalls  int
		curPage  = 1
		nextHits int
	)

	srv := newMockCDPServer(t, func(method string, params map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()

		switch method {
		case "Target.getTargets":
			return map[string]any{"targetInfos": []TargetInfo{}}, nil
		case "Target.createTarget":
			return map[string]any{"targetId": "scratch-paged"}, nil
		case "Target.attachToTarget":
			return map[string]any{"sessionId": "sess-paged"}, nil
		case "Target.closeTarget":
			return map[string]any{"success": true}, nil
		case "Accessibility.getFullAXTree":
			axCalls++
			return map[string]any{"nodes": []any{}}, nil
		case "Runtime.evaluate":
			expr, _ := params["expression"].(string)
			switch {
			case expr == "document.readyState":
				return map[string]any{"result": map[string]any{"type": "string", "value": "complete"}}, nil
			case strings.Contains(expr, "location.href || ''"):
				return map[string]any{"result": map[string]any{"type": "string", "value": "https://releases.example.com/list"}}, nil
			case strings.Contains(expr, "aria_disabled"):
				nextHits++
				if curPage >= 3 {
					return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"clicked": false, "reason": "disabled"}}}, nil
				}
				curPage++
				return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"clicked": true}}}, nil
			case strings.Contains(expr, "evalItemRule"):
				var pageItems []map[string]any
				switch curPage {
				case 1:
					pageItems = []map[string]any{{"tag": "v1.0.0"}, {"tag": "v1.0.1"}}
				case 2:
					pageItems = []map[string]any{{"tag": "v1.0.1"}, {"tag": "v1.0.2"}}
				case 3:
					pageItems = []map[string]any{{"tag": "v1.0.3"}}
				}
				return map[string]any{
					"result": map[string]any{
						"type": "object",
						"value": map[string]any{
							"title": "Releases",
							"url":   "https://releases.example.com/list",
							"items": pageItems,
						},
					},
				}, nil
			default:
				return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
			}
		default:
			return map[string]any{}, nil
		}
	})
	defer srv.Close()

	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	client, err := Connect(context.Background(), wsURL)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	client.settleDelay = time.Millisecond
	defer client.Close()

	step := contract.Step{
		ID:  "releases",
		URL: "https://releases.example.com/list",
		Items: &contract.ItemRule{
			Container: "article.release",
			DedupeBy:  "tag",
			Limit:     10,
			Fields:    map[string]contract.FieldRule{"tag": {Selector: "a.tag"}},
		},
		Pagination: &contract.PaginationRule{
			NextSelector: "button.next",
			MaxPages:     5,
			WaitMs:       contract.IntPtr(5),
		},
		Expect: contract.Expectation{
			MinItems: 4,
		},
	}

	res, err := client.ExecuteStep(context.Background(), step)
	if err != nil {
		t.Fatalf("ExecuteStep with pagination failed: %v", err)
	}
	if res.PagesVisited != 3 {
		t.Fatalf("PagesVisited = %d, want 3 (stopped when page 3 Next disabled)", res.PagesVisited)
	}
	if len(res.Items) != 4 {
		t.Fatalf("merged items = %d, want 4 deduplicated tags across 3 pages", len(res.Items))
	}
	mu.Lock()
	gotAX := axCalls
	gotNext := nextHits
	mu.Unlock()
	if gotAX != 0 {
		t.Fatalf("happy-path extraction must call Accessibility.getFullAXTree 0 times, got %d", gotAX)
	}
	if gotNext != 3 {
		t.Fatalf("expected 3 Next-control probes (click p1->p2, click p2->p3, disabled on p3), got %d", gotNext)
	}
}

func TestJSEvalFieldRuleNumericFallbackParity(t *testing.T) {
	if !strings.Contains(shadowDOMHelperJS, `!parseCompactFloat(raw).ok`) {
		t.Fatal("shadowDOMHelperJS missing numeric parseCompactFloat fallback_regex check")
	}
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" || os.Getenv("DESKDROP_REQUIRE_NODE") == "1" {
			t.Fatalf("node is required in CI for JS/Go runtime parity tests: %v", err)
		}
		t.Skipf("node not found in PATH; skipping JS runtime execution parity: %v", err)
	}

	script := shadowDOMHelperJS + `
const card = {
	nodeType: 1,
	innerText: "Shipping clean declarative selectors. Like View analytics 64 11 2,450 impressions",
	children: [],
	querySelectorAll(sel) {
		if (sel.includes("post-summary")) {
			return [{ nodeType: 1, innerText: "View analytics", getAttribute: () => null, children: [] }];
		}
		if (sel.includes("reaction")) {
			return [{ nodeType: 1, innerText: "Like", getAttribute: (k) => k === "aria-label" ? "Open reactions menu" : null, children: [] }];
		}
		return [];
	}
};
const imp = evalFieldRule(card, {
	selector: "a[href*='/analytics/post-summary/']",
	attr: "text",
	fallback_regex: "(?i)([\\d,.]+\\s*[KkMm]?)\\s+impressions\\b",
	parse: "number"
});
const react = evalFieldRule(card, {
	selector: "button[aria-label*='reaction']",
	attr: "aria-label|text",
	fallback_regex: "(?i)\\b(\\d+)(?:\\s+\\d+)?\\s+[\\d,]+\\s+impressions",
	parse: "number"
});
console.log(JSON.stringify({ imp, react }));
`
	out, runErr := exec.Command(nodeBin, "-e", script).CombinedOutput()
	if runErr != nil {
		t.Fatalf("node JS runtime test failed: %v\n%s", runErr, string(out))
	}
	var got struct {
		Imp   int64 `json:"imp"`
		React int64 `json:"react"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("unmarshal node output %q: %v", string(out), err)
	}
	if got.Imp != 2450 || got.React != 64 {
		t.Fatalf("JS evalFieldRule parity = %+v, want {Imp:2450 React:64}", got)
	}

	// A-7 JS runtime parity: parseCompactFloat \b boundary, inline span cleanText, group-2 alternation & RE2 flags/anchors.
	parityScript := shadowDOMHelperJS + `
const inlineCard = {
	nodeType: 1,
	tagName: "DIV",
	childNodes: [
		{ nodeType: 1, tagName: "SPAN", childNodes: [
			{ nodeType: 1, tagName: "SPAN", childNodes: [{ nodeType: 3, nodeValue: "1," }] },
			{ nodeType: 1, tagName: "SPAN", childNodes: [{ nodeType: 3, nodeValue: "234" }] },
		]},
		{ nodeType: 1, tagName: "SPAN", childNodes: [{ nodeType: 3, nodeValue: "impressions" }] }
	]
};
console.log(JSON.stringify({
	fiveMin: parseCompactFloat("5 min").val,
	threeMonths: parseCompactFloat("3 months").val,
	compactK: parseCompactFloat("1.2K").val,
	inlineText: cleanText(inlineCard),
	altGroup2: applyRegexCapture("impressions: 980", "(?i)(?:(\\d+)\\s+impressions|impressions:\\s*(\\d+))", 0),
	anchorMatch: applyRegexCapture("Start 42 End", "\\A(?i)start\\s+(\\d+)\\s+end\\z", 0)
}));
`
	out2, runErr2 := exec.Command(nodeBin, "-e", parityScript).CombinedOutput()
	if runErr2 != nil {
		t.Fatalf("node A-7 parity test failed: %v\n%s", runErr2, string(out2))
	}
	var got2 struct {
		FiveMin     float64 `json:"fiveMin"`
		ThreeMonths float64 `json:"threeMonths"`
		CompactK    float64 `json:"compactK"`
		InlineText  string  `json:"inlineText"`
		AltGroup2   string  `json:"altGroup2"`
		AnchorMatch string  `json:"anchorMatch"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out2), &got2); err != nil {
		t.Fatalf("unmarshal A-7 output %q: %v", string(out2), err)
	}
	if got2.FiveMin != 5 || got2.ThreeMonths != 3 || got2.CompactK != 1200 {
		t.Fatalf("JS parseCompactFloat boundary mismatch: %+v", got2)
	}
	if got2.InlineText != "1,234 impressions" {
		t.Fatalf("JS cleanText inline span mismatch: got %q, want %q", got2.InlineText, "1,234 impressions")
	}
	if got2.AltGroup2 != "980" || got2.AnchorMatch != "42" {
		t.Fatalf("JS applyRegexCapture parity mismatch: %+v", got2)
	}
}

func TestS7ExecuteFallbackAndPaginationFailClosed(t *testing.T) {
	var (
		mu       sync.Mutex
		curURL   = "https://evil.invalid/phish"
		clicks   int
		urlError bool
	)
	srv := newMockCDPServer(t, func(method string, params map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "Target.getTargets":
			return map[string]any{"targetInfos": []TargetInfo{}}, nil
		case "Target.createTarget":
			return map[string]any{"targetId": "scratch-fb"}, nil
		case "Target.attachToTarget":
			return map[string]any{"sessionId": "sess-fb"}, nil
		case "Target.closeTarget":
			return map[string]any{"success": true}, nil
		case "Runtime.evaluate":
			expr, _ := params["expression"].(string)
			switch {
			case expr == "document.readyState":
				return map[string]any{"result": map[string]any{"type": "string", "value": "complete"}}, nil
			case strings.Contains(expr, "location.href || ''"):
				if urlError {
					return nil, errors.New("eval failed")
				}
				return map[string]any{"result": map[string]any{"type": "string", "value": curURL}}, nil
			case strings.Contains(expr, "el.click()"):
				clicks++
				return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
			case strings.Contains(expr, "aria_disabled"):
				return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"clicked": true}}}, nil
			case strings.Contains(expr, "evalItemRule"):
				return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"url": "https://example.com/list", "items": []map[string]any{{"id": "1"}}}}}, nil
			default:
				return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
			}
		default:
			return map[string]any{}, nil
		}
	})
	defer srv.Close()

	client, err := Connect(context.Background(), "ws://"+strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	client.settleDelay = time.Millisecond
	defer client.Close()

	step := contract.Step{
		ID:     "dash",
		URL:    "https://example.com/dash",
		Expect: contract.Expectation{NotURLContains: []string{"/login"}},
	}

	// 1. Off-domain plan rejected before opening tab.
	if _, err := client.ExecuteFallback(context.Background(), step, contract.FallbackPlan{
		AllowedDomain: "evil.invalid",
		TargetURL:     "https://evil.invalid/x",
		Actions:       []contract.FallbackAction{{Action: "click", Selector: "button"}},
	}); err == nil {
		t.Fatal("expected ExecuteFallback to reject off-domain plan")
	}

	// 2. Immediate off-domain redirect aborts BEFORE executing click action.
	if _, err := client.ExecuteFallback(context.Background(), step, contract.FallbackPlan{
		AllowedDomain: "example.com",
		TargetURL:     "https://example.com/dash",
		Actions:       []contract.FallbackAction{{Action: "click", Selector: "button"}},
	}); err == nil {
		t.Fatal("expected ExecuteFallback to abort on off-domain redirect")
	}
	mu.Lock()
	if clicks != 0 {
		mu.Unlock()
		t.Fatalf("expected 0 clicks after off-domain redirect, got %d", clicks)
	}
	curURL = "https://example.com/login"
	mu.Unlock()

	// 3. NotURLContains login URL aborts BEFORE click.
	if _, err := client.ExecuteFallback(context.Background(), step, contract.FallbackPlan{
		AllowedDomain: "example.com",
		TargetURL:     "https://example.com/dash",
		Actions:       []contract.FallbackAction{{Action: "click", Selector: "button"}},
	}); err == nil || !strings.Contains(err.Error(), "blocked/login") {
		t.Fatalf("expected blocked/login abort, got %v", err)
	}

	// 4. Pagination fails closed on non-HTTP file URL and on location.href eval error.
	mu.Lock()
	curURL = "file" + ":///etc/passwd"
	mu.Unlock()
	pagedStep := contract.Step{
		ID:         "list",
		URL:        "https://example.com/list",
		Pagination: &contract.PaginationRule{NextSelector: "button.next", MaxPages: 3, WaitMs: contract.IntPtr(1)},
	}
	if _, err := client.ExecuteStep(context.Background(), pagedStep); err == nil {
		t.Fatal("expected pagination to fail closed on non-HTTP file URL")
	}

	mu.Lock()
	urlError = true
	mu.Unlock()
	if _, err := client.ExecuteStep(context.Background(), pagedStep); err == nil {
		t.Fatal("expected pagination to fail closed when location.href errors")
	}
}

func TestS8MatchesTabPatternAndMultiProfileDriftPreservation(t *testing.T) {
	// 1. matchesTabPattern host boundary and $-anchor query matching.
	cases := []struct {
		url     string
		pattern string
		want    bool
	}{
		{"https://scholar.google.com/", "scholar.google.com/$", true},
		{"https://scholar.google.com?hl=en", "scholar.google.com/$", true},
		{"https://scholar.google.com/citations?user=abc", "scholar.google.com/$", false},
		{"https://evil-scholar.google.com/", "scholar.google.com/$", false},
		{"https://other.com/scholar.google.com/", "scholar.google.com/$", false},
		{"https://scholar.google.com/citations?user=abc", "scholar.google.com/citations?user=abc$", true},
		{"https://scholar.google.com/citations?user=xyz", "scholar.google.com/citations?user=abc$", false},
		{"https://evil-scholar.google.com/citations", "scholar.google.com/citations", false},
		{"https://www.linkedin.com/in/demo/recent-activity/", "linkedin.com/in/demo/recent-activity", true},
	}
	for _, tc := range cases {
		if got := matchesTabPattern(tc.url, tc.pattern); got != tc.want {
			t.Errorf("matchesTabPattern(%q, %q) = %v, want %v", tc.url, tc.pattern, got, tc.want)
		}
	}

	// 2. Multi-profile ExecuteStep stops on SELECTOR_DRIFT from logged-in profile 1 instead of masking with profile 2 /login.
	var (
		mu          sync.Mutex
		createdCtxs []string
	)
	srv := newMockCDPServer(t, func(method string, params map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "Target.getTargets":
			return map[string]any{
				"targetInfos": []TargetInfo{
					{TargetID: "t1", Type: "page", URL: "https://status.example.com/home", BrowserContextID: "ctx-authed"},
					{TargetID: "t2", Type: "page", URL: "https://other.example.org/", BrowserContextID: "ctx-loggedout"},
				},
			}, nil
		case "Target.createTarget":
			cid, _ := params["browserContextId"].(string)
			createdCtxs = append(createdCtxs, cid)
			return map[string]any{"targetId": "scratch-" + cid}, nil
		case "Target.attachToTarget":
			tid, _ := params["targetId"].(string)
			return map[string]any{"sessionId": "sess-" + tid}, nil
		case "Target.closeTarget":
			return map[string]any{"success": true}, nil
		case "Page.captureScreenshot":
			return map[string]any{"data": base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4E, 0x47})}, nil
		case "Accessibility.getFullAXTree":
			return map[string]any{"nodes": []any{}}, nil
		case "Runtime.evaluate":
			expr, _ := params["expression"].(string)
			if expr == "document.readyState" {
				return map[string]any{"result": map[string]any{"type": "string", "value": "complete"}}, nil
			}
			if strings.Contains(expr, "evalItemRule") {
				// First profile is logged in on target domain but has selector drift (0 items).
				return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{
					"title": "Status",
					"url":   "https://status.example.com/overview",
				}}}, nil
			}
			return map[string]any{"result": map[string]any{"type": "string", "value": "<html><body></body></html>"}}, nil
		default:
			return map[string]any{}, nil
		}
	})
	defer srv.Close()

	client, err := Connect(context.Background(), "ws://"+strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	client.settleDelay = time.Millisecond
	defer client.Close()

	_, err = client.ExecuteStep(context.Background(), contract.Step{
		ID:     "overview",
		URL:    "https://status.example.com/overview",
		Expect: contract.Expectation{MinItems: 1, NotURLContains: []string{"/login"}},
	})
	if err == nil {
		t.Fatal("expected selector drift error")
	}
	mu.Lock()
	ctxs := append([]string(nil), createdCtxs...)
	mu.Unlock()
	if len(ctxs) != 1 || ctxs[0] != "ctx-authed" {
		t.Fatalf("expected ExecuteStep to stop after logged-in profile drift, tried contexts=%v", ctxs)
	}

	// 3. Pagination must block page-1 off-domain/login redirect BEFORE clicking NextSelector.
	var pagClicked atomic.Int32
	pagSrv := newMockCDPServer(t, func(method string, params map[string]any) (any, error) {
		if method == "Runtime.evaluate" {
			expr, _ := params["expression"].(string)
			if strings.Contains(expr, "location.href") {
				return map[string]any{"result": map[string]any{"type": "string", "value": "https://evil.invalid/login"}}, nil
			}
			if strings.Contains(expr, "deepQueryFirst") {
				pagClicked.Add(1)
				return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"clicked": true}}}, nil
			}
		}
		return map[string]any{}, nil
	})
	defer pagSrv.Close()
	pagClient, err := Connect(context.Background(), "ws://"+strings.TrimPrefix(pagSrv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer pagClient.Close()
	_, err = pagClient.runPaginationLoop(
		context.Background(),
		"sess-1",
		contract.Step{
			ID:         "p1",
			URL:        "https://approved.example.com/list",
			Expect:     contract.Expectation{NotURLContains: []string{"/login"}},
			Pagination: &contract.PaginationRule{NextSelector: "button.next", MaxPages: 3},
		},
		contract.StepResult{URL: "https://evil.invalid/login"},
		500,
	)
	if err == nil {
		t.Fatal("expected pagination to fail closed when page 1 redirected off approved step domain")
	}
	if pagClicked.Load() != 0 {
		t.Fatalf("expected 0 Next clicks on off-domain page 1, got %d", pagClicked.Load())
	}
}

func newMockCDPServer(t *testing.T, handler func(method string, params map[string]any) (any, error)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Sec-WebSocket-Key")
		h := sha1.Sum([]byte(key + wsGUID))
		accept := base64.StdEncoding.EncodeToString(h[:])

		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("hijack unsupported")
			return
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+accept+"\r\n\r\n")
		br := bufrw.Reader
		for {
			payload, err := readServerWSFrame(br)
			if err != nil {
				return
			}
			var req struct {
				ID     int64          `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(payload, &req) != nil {
				continue
			}
			res, callErr := handler(req.Method, req.Params)
			resp := map[string]any{"id": req.ID}
			if callErr != nil {
				resp["error"] = map[string]any{"code": -32000, "message": callErr.Error()}
			} else {
				resp["result"] = res
			}
			raw, _ := json.Marshal(resp)
			if err := writeServerWSFrame(conn, raw); err != nil {
				return
			}
		}
	}))
}

func readServerWSFrame(br *bufio.Reader) ([]byte, error) {
	var b2 [2]byte
	if _, err := io.ReadFull(br, b2[:]); err != nil {
		return nil, err
	}
	opcode := b2[0] & 0x0f
	if opcode == 0x8 {
		return nil, io.EOF
	}
	masked := (b2[1] & 0x80) != 0
	length := uint64(b2[1] & 0x7f)
	if length == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	} else if length == 127 {
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(br, mask[:]); err != nil {
			return nil, err
		}
	}
	data := make([]byte, int(length))
	if _, err := io.ReadFull(br, data); err != nil {
		return nil, err
	}
	if masked {
		for i := range data {
			data[i] ^= mask[i%4]
		}
	}
	return data, nil
}

func writeServerWSFrame(conn net.Conn, payload []byte) error {
	var hdr [10]byte
	hdr[0] = 0x81
	n := len(payload)
	off := 2
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 65535:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		off = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		off = 10
	}
	if _, err := conn.Write(hdr[:off]); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}

func TestT2JSRuntimeScrubberItemRulePaginationAndDriftParity(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" || os.Getenv("DESKDROP_REQUIRE_NODE") == "1" {
			t.Fatalf("node is required in CI for JS/Go runtime parity tests: %v", err)
		}
		t.Skipf("node not found in PATH; skipping JS parity test: %v", err)
	}

	// Verify Go-vs-JS parity on testdata/drift/status_v1.html + buildScrubbedHTMLJS + buildClickNextIfEnabledJS + parseCompactFloat K/M/B.
	v1RecipeRaw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "drift", "status_v1_recipe.json"))
	if err != nil {
		t.Fatalf("read status_v1_recipe.json: %v", err)
	}
	var v1Recipe contract.Recipe
	if err := contract.ParseJSONStrict(v1RecipeRaw, &v1Recipe); err != nil {
		t.Fatalf("parse v1 recipe: %v", err)
	}
	v1HTMLBytes, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "drift", "status_v1.html"))
	if err != nil {
		t.Fatalf("read status_v1.html: %v", err)
	}
	goRes, err := verify.ExtractStepFromHTML(v1Recipe.Steps[0], string(v1HTMLBytes), "https://status.example.com/overview")
	if err != nil {
		t.Fatalf("ExtractStepFromHTML failed: %v", err)
	}

	itemRuleJSON, _ := json.Marshal(v1Recipe.Steps[0].Items)
	htmlJSON, _ := json.Marshal(string(v1HTMLBytes))
	scrubJS := buildScrubbedHTMLJS()
	clickNextJS := buildClickNextIfEnabledJS("button.next")

	script := fmt.Sprintf(`
%s
// Minimal DOM mock for Node parity checks (scrubber, evalItemRule, parseCompactFloat, buildClickNextIfEnabledJS).
class TextNode {
	constructor(val) { this.nodeType = 3; this.nodeValue = val; }
	get outerHTML() { return this.nodeValue; }
}
class Elem {
	constructor(tag, attrs = {}, children = []) {
		this.nodeType = 1;
		this.tagName = tag.toUpperCase();
		this._attrs = new Map(Object.entries(attrs));
		this.childNodes = [];
		this.children = [];
		this.parentElement = null;
		this.hidden = this._attrs.has("hidden");
		this.disabled = this._attrs.has("disabled");
		this.clicked = false;
		this.classList = { contains: (c) => (this.getAttribute("class") || "").split(/\s+/).includes(c) };
		for (const ch of children) this.appendChild(ch);
	}
	get attributes() {
		const out = [];
		for (const [name, value] of this._attrs.entries()) out.push({ name, value });
		return out;
	}
	getAttribute(k) { return this._attrs.has(k) ? this._attrs.get(k) : null; }
	hasAttribute(k) { return this._attrs.has(k); }
	setAttribute(k, v) { this._attrs.set(k, String(v)); }
	appendChild(ch) {
		this.childNodes.push(ch);
		if (ch.nodeType === 1) { ch.parentElement = this; this.children.push(ch); }
	}
	click() { this.clicked = true; }
	get outerHTML() {
		const tag = this.tagName.toLowerCase();
		const aStr = Array.from(this._attrs.entries()).map(([k, v]) => ' ' + k + '="' + v + '"').join('');
		return '<' + tag + aStr + '>' + this.childNodes.map(c => c.outerHTML).join('') + '</' + tag + '>';
	}
	matchesSimple(sel) {
		sel = sel.trim();
		const mTag = sel.match(/^[a-zA-Z0-9_-]+/);
		if (mTag && this.tagName.toLowerCase() !== mTag[0].toLowerCase()) return false;
		const classes = Array.from(sel.matchAll(/\.([a-zA-Z0-9_-]+)/g)).map(m => m[1]);
		const have = (this.getAttribute("class") || "").split(/\s+/);
		for (const c of classes) if (!have.includes(c)) return false;
		return true;
	}
	querySelectorAll(sel) {
		const out = [];
		const walk = (n) => {
			for (const ch of n.children) {
				if (ch.matchesSimple(sel)) out.push(ch);
				walk(ch);
			}
		};
		walk(this);
		return out;
	}
	closest(sel) {
		let cur = this;
		while (cur) {
			if (cur.matchesSimple(sel)) return cur;
			cur = cur.parentElement;
		}
		return null;
	}
}

// Parse status_v1.html rows into mock DOM to compare evalItemRule against Go verify.
const rawHTML = %s;
const body = new Elem("body");
const rowRe = /<div class="metric-row">([\s\S]*?)<\/div>/g;
let rm;
while ((rm = rowRe.exec(rawHTML)) !== null) {
	const inner = rm[1];
	const svc = (inner.match(/class="svc-name">([^<]+)</) || [,""])[1];
	const st = (inner.match(/class="svc-state">([^<]+)</) || [,""])[1];
	const up = (inner.match(/class="svc-uptime">([^<]+)</) || [,""])[1];
	body.appendChild(new Elem("div", { class: "metric-row" }, [
		new Elem("span", { class: "svc-name" }, [new TextNode(svc)]),
		new Elem("span", { class: "svc-state" }, [new TextNode(st)]),
		new Elem("span", { class: "svc-uptime" }, [new TextNode(up)]),
	]));
}
body.appendChild(new Elem("script", {}, [new TextNode("evilScript()")]));
body.appendChild(new Elem("input", { type: "password", value: "top-secret-pw", onclick: "bad()" }));
body.appendChild(new Elem("textarea", {}, [new TextNode("private-textarea")]));
const btnEnabled = new Elem("button", { class: "next" }, [new TextNode("Next")]);
body.appendChild(btnEnabled);

const htmlRoot = new Elem("html", {}, [body]);
global.window = { getComputedStyle: () => ({ display: "block", visibility: "visible", opacity: "1" }) };
global.document = {
	documentElement: htmlRoot,
	body: body,
	children: [htmlRoot],
	childNodes: [htmlRoot],
	createElement: (tag) => new Elem(tag),
	createTextNode: (v) => new TextNode(v),
	querySelectorAll: (sel) => htmlRoot.querySelectorAll(sel),
};

const scrubbed = (%s);
const items = evalItemRule(document, %s);
const clickEnabled = (%s);

// Now disable Next button and re-run buildClickNextIfEnabledJS.
btnEnabled.setAttribute("aria-disabled", "true");
const clickDisabled = (%s);

console.log(JSON.stringify({
	scrubbed: scrubbed,
	items: items,
	clickEnabled: clickEnabled,
	btnClicked: btnEnabled.clicked,
	clickDisabled: clickDisabled,
	kVal: parseCompactFloat("2.5K").val,
	mVal: parseCompactFloat("1.5M").val,
	bVal: parseCompactFloat("3.2B").val,
}));
`, shadowDOMHelperJS, string(htmlJSON), scrubJS, string(itemRuleJSON), clickNextJS, clickNextJS)

	out, runErr := exec.Command(nodeBin, "-e", script).CombinedOutput()
	if runErr != nil {
		t.Fatalf("node T-2 parity script failed: %v\n%s", runErr, string(out))
	}
	var got struct {
		Scrubbed      string           `json:"scrubbed"`
		Items         []map[string]any `json:"items"`
		ClickEnabled  nextClickState   `json:"clickEnabled"`
		BtnClicked    bool             `json:"btnClicked"`
		ClickDisabled nextClickState   `json:"clickDisabled"`
		KVal          float64          `json:"kVal"`
		MVal          float64          `json:"mVal"`
		BVal          float64          `json:"bVal"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &got); err != nil {
		t.Fatalf("unmarshal T-2 output %q: %v", string(out), err)
	}
	for _, banned := range []string{"evilScript", "top-secret-pw", "private-textarea", "onclick"} {
		if strings.Contains(got.Scrubbed, banned) {
			t.Errorf("buildScrubbedHTMLJS leaked %q:\n%s", banned, got.Scrubbed)
		}
	}
	if len(got.Items) != len(goRes.Items) {
		t.Fatalf("JS items len=%d != Go verify items len=%d", len(got.Items), len(goRes.Items))
	}
	for i := range goRes.Items {
		if got.Items[i]["service"] != goRes.Items[i]["service"] || got.Items[i]["state"] != goRes.Items[i]["state"] || got.Items[i]["uptime_pct"] != goRes.Items[i]["uptime_pct"] {
			t.Errorf("item[%d] JS=%+v != Go=%+v", i, got.Items[i], goRes.Items[i])
		}
	}
	if !got.ClickEnabled.Clicked || !got.BtnClicked {
		t.Fatalf("expected enabled Next click to succeed, got %+v clicked=%v", got.ClickEnabled, got.BtnClicked)
	}
	if got.ClickDisabled.Clicked || got.ClickDisabled.Reason != "aria_disabled" {
		t.Fatalf("expected aria-disabled Next click to stop, got %+v", got.ClickDisabled)
	}
	if got.KVal != 2500 || got.MVal != 1500000 || got.BVal != 3200000000 {
		t.Fatalf("parseCompactFloat K/M/B mismatch: %+v", got)
	}
}

func TestS9ClientCloseRaceMarshalCleanupAndWSFramingLimits(t *testing.T) {
	// 1. WSConn fragmented message reassembly, Ping->Pong, cumulative continuation cap, and unexpected continuation.
	c1, s1 := net.Pipe()
	defer c1.Close()
	defer s1.Close()
	ws := &WSConn{conn: c1, br: bufio.NewReader(c1), maxMessageBytes: 16, writeTimeout: 200 * time.Millisecond}

	go func() {
		// Send Ping frame (opcode 0x9, payload "hi"), then fragmented text ("hello " + "world"), then oversized fragments ("1234567890" + "1234567890").
		_, _ = s1.Write([]byte{0x89, 0x02, 'h', 'i'})
		// Read expected Pong response from client so pipe doesn't block.
		pongHdr := make([]byte, 8)
		_, _ = io.ReadFull(s1, pongHdr)
		_, _ = s1.Write([]byte{0x01, 0x06, 'h', 'e', 'l', 'l', 'o', ' '})
		_, _ = s1.Write([]byte{0x80, 0x05, 'w', 'o', 'r', 'l', 'd'})
		_, _ = s1.Write([]byte{0x01, 0x0a, '0', '1', '2', '3', '4', '5', '6', '7', '8', '9'})
		_, _ = s1.Write([]byte{0x80, 0x0a, '0', '1', '2', '3', '4', '5', '6', '7', '8', '9'})
	}()

	msg, err := ws.ReadText()
	if err != nil || string(msg) != "hello world" {
		t.Fatalf("fragmented ReadText = (%q, %v), want ('hello world', nil)", string(msg), err)
	}
	if _, err := ws.ReadText(); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected cumulative continuation overflow error, got %v", err)
	}

	// Unexpected continuation frame without initial text frame must error.
	c2, s2 := net.Pipe()
	defer c2.Close()
	defer s2.Close()
	ws2 := &WSConn{conn: c2, br: bufio.NewReader(c2)}
	go func() {
		_, _ = s2.Write([]byte{0x80, 0x02, 'a', 'b'})
	}()
	if _, err := ws2.ReadText(); err == nil {
		t.Fatal("expected unexpected continuation frame error")
	}

	// 2. Client.Call marshal error must NOT leak c.pending; Close unblocks in-flight Call cleanly under -race.
	startedCh := make(chan struct{})
	blockCh := make(chan struct{})
	srv := newMockCDPServer(t, func(method string, _ map[string]any) (any, error) {
		if method == "Slow.op" {
			close(startedCh)
			<-blockCh
		}
		return map[string]any{}, nil
	})
	defer func() {
		close(blockCh)
		srv.Close()
	}()
	client, err := Connect(context.Background(), "ws://"+strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Call(context.Background(), "", "Bad.marshal", map[string]any{"fn": func() {}}, nil); err == nil {
		t.Fatal("expected json.Marshal error for func param")
	}
	client.mu.Lock()
	leaked := len(client.pending)
	client.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("c.pending leaked %d entry after marshal error", leaked)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Call(context.Background(), "", "Slow.op", nil, nil)
	}()
	select {
	case <-startedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("mock CDP server did not receive Slow.op request")
	}
	_ = client.Close()
	select {
	case cerr := <-errCh:
		if cerr == nil {
			t.Fatal("expected error when client closed during in-flight Call")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight Call hung after Client.Close()")
	}

	// 3. Cancelled context before ExecuteStep must return context error, never (StepResult{}, nil).
	cancelCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := client.ExecuteStep(cancelCtx, contract.Step{ID: "s1", URL: "https://status.example.com"}); err == nil {
		t.Fatal("expected cancelled context ExecuteStep to return error, not nil")
	}
}

func TestT5ProbeQuietWaitConditionDiscoverAndLauncher(t *testing.T) {
	// 1. SessionPool.ProbeQuiet respects approval latch without dialing DevToolsActivePort and connects quietly to HTTP /json/version.
	pool := NewSessionPool()
	pool.activePortAttempted = true
	conn, label := pool.ProbeQuiet(context.Background(), "http://127.0.0.1:1")
	if conn || !strings.Contains(label, "Disconnected") {
		t.Fatalf("ProbeQuiet latched = (%v, %q), want (false, Disconnected...)", conn, label)
	}

	mockCDP := newMockCDPServer(t, func(_ string, _ map[string]any) (any, error) { return map[string]any{}, nil })
	defer mockCDP.Close()
	wsURL := "ws://" + strings.TrimPrefix(mockCDP.URL, "http://")
	verSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `{"webSocketDebuggerUrl":%q}`, wsURL)
	}))
	defer verSrv.Close()

	conn2, label2 := pool.ProbeQuiet(context.Background(), verSrv.URL)
	if !conn2 || !strings.Contains(label2, "Connected") {
		t.Fatalf("ProbeQuiet HTTP endpoint = (%v, %q), want connected", conn2, label2)
	}
	if pool.CurrentClient() == nil {
		t.Fatal("expected CurrentClient() after ProbeQuiet connect")
	}
	pool.Close()

	// 2. readDevToolsActivePort & DiscoverBrowserWSWithSource coverage.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte("9222\n/devtools/browser/abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gotWS, err := readDevToolsActivePort(dir)
	if err != nil || gotWS != "ws://127.0.0.1:9222/devtools/browser/abc" {
		t.Fatalf("readDevToolsActivePort = (%q, %v)", gotWS, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte("badport\n/ws\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDevToolsActivePort(dir); err == nil {
		t.Fatal("expected invalid port error")
	}
	expWS, src, err := DiscoverBrowserWSWithSource(context.Background(), verSrv.URL, "")
	if err != nil || expWS != wsURL || src != SourceExplicit {
		t.Fatalf("DiscoverBrowserWSWithSource(http) = (%q, %q, %v)", expWS, src, err)
	}
	expWS2, src2, err := DiscoverBrowserWSWithSource(context.Background(), "ws://127.0.0.1:9222/x", "")
	if err != nil || expWS2 != "ws://127.0.0.1:9222/x" || src2 != SourceExplicit {
		t.Fatalf("DiscoverBrowserWSWithSource(ws) = (%q, %q, %v)", expWS2, src2, err)
	}

	// 3. waitForCondition success and timeout.
	waitCalls := 0
	waitSrv := newMockCDPServer(t, func(method string, _ map[string]any) (any, error) {
		if method == "Runtime.evaluate" {
			waitCalls++
			if waitCalls >= 2 {
				return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"ok": true}}}, nil
			}
			return map[string]any{"result": map[string]any{"type": "object", "value": map[string]any{"ok": false}}}, nil
		}
		return map[string]any{}, nil
	})
	defer waitSrv.Close()
	wc, err := Connect(context.Background(), "ws://"+strings.TrimPrefix(waitSrv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer wc.Close()
	if err := wc.waitForCondition(context.Background(), "s1", ".ready", "ok", nil); err != nil {
		t.Fatalf("waitForCondition failed: %v", err)
	}
	tctx, tcancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer tcancel()
	waitCalls = -100
	if err := wc.waitForCondition(tctx, "s1", ".never", "", nil); err == nil {
		t.Fatal("expected waitForCondition timeout error")
	}

	// 4. launcher.go profile discovery (Local State + directory scan + MatchBrowserProfile).
	chromeDir := t.TempDir()
	localState := `{"profile":{"last_used":"Profile 1","info_cache":{"Default":{"name":"Personal","user_name":"alice@example.com"},"Profile 1":{"name":"Work","user_name":"work@example.com"}}}}`
	if err := os.WriteFile(filepath.Join(chromeDir, "Local State"), []byte(localState), 0o600); err != nil {
		t.Fatal(err)
	}
	profs := ListBrowserProfiles("chrome", chromeDir)
	if len(profs) != 3 || profs[0].DirName != "Profile 1" || !profs[2].IsDedicated {
		t.Fatalf("ListBrowserProfiles(Local State) = %+v", profs)
	}
	if m, ok := MatchBrowserProfile(profs, "alice@example.com"); !ok || m.DirName != "Default" {
		t.Fatalf("MatchBrowserProfile email = (%+v, %v)", m, ok)
	}
	if m, ok := MatchBrowserProfile(profs, "dedicated"); !ok || !m.IsDedicated {
		t.Fatalf("MatchBrowserProfile dedicated = (%+v, %v)", m, ok)
	}

	// Directory scan fallback when Local State is absent.
	scanDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scanDir, "Default"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scanDir, "Default", "Preferences"), []byte(`{"profile":{"name":"Scanned"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	scanned := ListBrowserProfiles("chrome", scanDir)
	if len(scanned) < 2 || scanned[0].Name != "Scanned" {
		t.Fatalf("ListBrowserProfiles(scan fallback) = %+v", scanned)
	}
}
