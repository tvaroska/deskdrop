package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tvaroska/deskdrop/internal/contract"
)

func TestVerifyRecipeOnHTMLDeclarativeItemsAndFields(t *testing.T) {
	htmlDoc := `<!doctype html>
	<html>
	<head><title>Feed Analytics</title></head>
	<body>
		<main>
			<h1>Creator Feed</h1>
			<div class="summary">Followers 4,520</div>
			<div class="feed-card" data-urn="urn:li:activity:111">
				<span class="age">2d</span>
				<p class="body">Deterministic verification beats prompt guesses every time.</p>
				<button aria-label="48 reactions">48</button>
				<button aria-label="12 comments">12</button>
				<a href="/analytics/post-summary/urn:li:activity:111/">1.2K impressions</a>
			</div>
			<div class="feed-card" data-urn="urn:li:activity:222">
				<span class="age">1w</span>
				<p class="body">Second post about Ports, Adapters, and Contracts.</p>
				<button aria-label="15 reactions">15</button>
				<button aria-label="3 comments">3</button>
				<a href="/analytics/post-summary/urn:li:activity:222/">890 impressions</a>
			</div>
		</main>
	</body>
	</html>`

	recipe := contract.Recipe{
		Name:  "feed-analytics",
		Topic: "analytics",
		Steps: []contract.Step{
			{
				ID:  "feed",
				URL: "https://example.com/feed",
				Fields: map[string]contract.FieldRule{
					"followers": {
						Selector: ".summary",
						Regex:    `(?i)Followers\s+([\d,]+)`,
						Parse:    "number",
					},
				},
				Items: &contract.ItemRule{
					Container:     `a[href*="/analytics/post-summary/"]`,
					AnchorClosest: "div.feed-card",
					DedupeBy:      "urn",
					Limit:         10,
					Fields: map[string]contract.FieldRule{
						"urn": {
							Selector: `a[href*="/analytics/post-summary/"]`,
							Attr:     "href",
							Regex:    `(urn:li:activity:\d+)`,
						},
						"url": {
							Selector: `a[href*="/analytics/post-summary/"]`,
							Attr:     "href",
							Regex:    `(urn:li:activity:\d+)`,
							Prefix:   "https://example.com/update/",
							Suffix:   "/",
						},
						"impressions": {
							Selector: `a[href*="/analytics/post-summary/"]`,
							Parse:    "number",
						},
						"reactions": {
							Selector: `button[aria-label*="reaction"]`,
							Attr:     "aria-label|text",
							Parse:    "number",
						},
						"comments": {
							Selector: `button[aria-label*="comment"]`,
							Attr:     "aria-label|text",
							Parse:    "number",
						},
						"text": {
							Selector: "p.body",
							MaxChars: 120,
						},
					},
				},
				Expect: contract.Expectation{
					MinItems:       2,
					RequiredFields: []string{"followers"},
				},
			},
		},
	}

	outcome, err := VerifyRecipeOnHTML(recipe, nil, "feed", htmlDoc, "https://example.com/feed")
	if err != nil {
		t.Fatalf("VerifyRecipeOnHTML returned error: %v", err)
	}
	if !outcome.Passed {
		t.Fatalf("expected verification to pass, got error: %s", outcome.Error)
	}
	if got := outcome.StepResult.Fields["followers"]; got != int64(4520) {
		t.Fatalf("followers = %#v, want int64(4520)", got)
	}
	if len(outcome.StepResult.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(outcome.StepResult.Items))
	}
	if got := outcome.StepResult.Items[0]["impressions"]; got != int64(1200) {
		t.Fatalf("items[0].impressions = %#v, want int64(1200)", got)
	}
	if got := outcome.StepResult.Items[0]["url"]; got != "https://example.com/update/urn:li:activity:111/" {
		t.Fatalf("items[0].url = %#v", got)
	}
	if got := outcome.StepResult.Items[0]["reactions"]; got != int64(48) {
		t.Fatalf("items[0].reactions = %#v, want int64(48)", got)
	}
}

func TestNumericFieldRuleFallsBackWhenPrimarySelectorTextHasNoNumber(t *testing.T) {
	htmlDoc := `<!doctype html><html><body>
		<div class="feed-card" data-urn="urn:li:activity:999">
			<p class="body">Shipping clean declarative selectors without script eval.</p>
			<button aria-label="Open reactions menu">Like</button>
			<a href="/analytics/post-summary/urn:li:activity:999/">View analytics</a>
			<span class="counts">64 11 2,450 impressions</span>
		</div>
	</body></html>`

	recipe := contract.Recipe{
		Name:  "fallback-numeric-test",
		Topic: "analytics",
		Steps: []contract.Step{
			{
				ID:  "feed",
				URL: "https://example.com/feed",
				Items: &contract.ItemRule{
					Container:     `a[href*="/analytics/post-summary/"]`,
					AnchorClosest: "div.feed-card",
					Fields: map[string]contract.FieldRule{
						"impressions": {
							Selector:      `a[href*="/analytics/post-summary/"]`,
							Attr:          "text",
							FallbackRegex: `(?i)([\d,.]+\s*[KkMm]?)\s+impressions\b`,
							Parse:         "number",
						},
						"reactions": {
							Selector:      `button[aria-label*="reaction"]`,
							Attr:          "aria-label|text",
							FallbackRegex: `(?i)\b(\d+)(?:\s+\d+)?\s+[\d,]+\s+impressions`,
							Parse:         "number",
						},
						"ctr_pct": {
							Selector:      `a[href*="/analytics/post-summary/"]`,
							Attr:          "text",
							FallbackRegex: `(?i)([\d.]+)\s+impressions`,
							Parse:         "float",
						},
					},
				},
			},
		},
	}

	out, err := VerifyRecipeOnHTML(recipe, nil, "feed", htmlDoc, "https://example.com/feed")
	if err != nil || !out.Passed {
		t.Fatalf("VerifyRecipeOnHTML failed: passed=%v err=%v outErr=%s", out.Passed, err, out.Error)
	}
	if len(out.StepResult.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(out.StepResult.Items))
	}
	item := out.StepResult.Items[0]
	if got := item["impressions"]; got != int64(2450) {
		t.Fatalf("impressions = %#v, want int64(2450)", got)
	}
	if got := item["reactions"]; got != int64(64) {
		t.Fatalf("reactions = %#v, want int64(64)", got)
	}
	if got := item["ctr_pct"]; got != float64(450) {
		t.Fatalf("ctr_pct = %#v, want float64(450)", got)
	}
}

func TestVerifyRecipeOnHTMLShadowDOMTableAndDriftFailure(t *testing.T) {
	shadowHTML := `<!doctype html><html><body>
		<ytcp-table>
			<div data-shadowroot="open">
				<div role="columnheader">Video</div>
				<div role="columnheader">Views</div>
				<div role="row"><div role="cell">Episode 1</div><div role="cell">12.4K</div></div>
			</div>
		</ytcp-table>
	</body></html>`

	recipe := contract.Recipe{
		Name:  "studio-table",
		Topic: "studio",
		Steps: []contract.Step{
			{
				ID:  "overview",
				URL: "https://studio.example.com/overview",
				Selectors: map[string]string{
					"rows": "ytcp-table >>> [role='row']",
				},
				Expect: contract.Expectation{
					MinTables:       1,
					MinSelectorHits: 1,
				},
			},
		},
	}

	okOut, err := VerifyRecipeOnHTML(recipe, nil, "overview", shadowHTML, "https://studio.example.com/overview")
	if err != nil || !okOut.Passed {
		t.Fatalf("expected shadow table pass, got passed=%v err=%v outcomeErr=%s", okOut.Passed, err, okOut.Error)
	}
	if okOut.TableCount != 1 {
		t.Fatalf("expected 1 ARIA shadow table, got %d", okOut.TableCount)
	}

	// Simulate selector drift where ytcp-table changed to modern-grid.
	driftedHTML := `<!doctype html><html><body><div class="new-grid">Nothing matches</div></body></html>`
	failOut, err := VerifyRecipeOnHTML(recipe, nil, "overview", driftedHTML, "https://studio.example.com/overview")
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if failOut.Passed {
		t.Fatal("expected drifted HTML to fail verification")
	}
}

func TestVerifyRecipeOnHTMLPagesPaginationAndDisabledStop(t *testing.T) {
	pages := []string{
		`<!doctype html><html><body>
			<article class="release"><a class="tag">v1.0.0</a></article>
			<article class="release"><a class="tag">v1.0.1</a></article>
			<button class="next">Next page</button>
		</body></html>`,
		`<!doctype html><html><body>
			<article class="release"><a class="tag">v1.0.1</a></article>
			<article class="release"><a class="tag">v1.0.2</a></article>
			<button class="next" aria-disabled="true">Next page</button>
		</body></html>`,
		`<!doctype html><html><body>
			<article class="release"><a class="tag">v1.0.3</a></article>
		</body></html>`,
	}

	recipe := contract.Recipe{
		Name:  "paged-releases",
		Topic: "releases",
		Steps: []contract.Step{
			{
				ID:  "list",
				URL: "https://releases.example.com/list",
				Items: &contract.ItemRule{
					Container: "article.release",
					DedupeBy:  "tag",
					Limit:     10,
					Fields: map[string]contract.FieldRule{
						"tag": {Selector: "a.tag"},
					},
				},
				Pagination: &contract.PaginationRule{
					NextSelector: "button.next",
					MaxPages:     4,
				},
				Expect: contract.Expectation{
					MinItems: 3,
				},
			},
		},
	}

	out, err := VerifyRecipeOnHTMLPages(recipe, nil, "list", pages, "https://releases.example.com/list")
	if err != nil || !out.Passed {
		t.Fatalf("VerifyRecipeOnHTMLPages failed: passed=%v err=%v outErr=%s", out.Passed, err, out.Error)
	}
	if out.StepResult.PagesVisited != 2 {
		t.Fatalf("PagesVisited = %d, want 2 (page 2 has aria-disabled=true so page 3 is not visited)", out.StepResult.PagesVisited)
	}
	if out.ItemCount != 3 {
		t.Fatalf("ItemCount = %d, want 3 deduplicated items", out.ItemCount)
	}
}

func TestGoogleScholarProfileRecipeOnScrubbedHTMLAndSorryBlock(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "recipes", "google-scholar-profile.json"))
	if err != nil {
		t.Fatalf("read google-scholar-profile.json: %v", err)
	}
	var recipe contract.Recipe
	if err := contract.ParseJSONStrict(raw, &recipe); err != nil {
		t.Fatalf("ParseJSONStrict failed: %v", err)
	}
	if recipe.AllowsComputerUseFallback() {
		t.Fatal("google-scholar-profile must keep allow_computer_use_fallback=false to prevent automated challenge retries")
	}

	profileHTML := contract.ScrubHTML(`<!doctype html><html><head><title>Jane Researcher - Google Scholar</title><script>init()</script></head>
	<body>
		<div id="gsc_prf_in">Jane Researcher</div>
		<div class="gsc_prf_il">Institute of Distributed Systems</div>
		<table id="gsc_rsb_st">
			<thead><tr><th></th><th>All</th><th>Since 2021</th></tr></thead>
			<tbody>
				<tr><td class="gsc_rsb_sc1">Citations</td><td class="gsc_rsb_std">1,842</td><td class="gsc_rsb_std">1,190</td></tr>
				<tr><td class="gsc_rsb_sc1">h-index</td><td class="gsc_rsb_std">19</td><td class="gsc_rsb_std">16</td></tr>
				<tr><td class="gsc_rsb_sc1">i10-index</td><td class="gsc_rsb_std">27</td><td class="gsc_rsb_std">22</td></tr>
			</tbody>
		</table>
		<table id="gsc_a_t"><tbody id="gsc_a_b">
			<tr class="gsc_a_tr">
				<td class="gsc_a_t">
					<a class="gsc_a_at" href="/citations?view_op=view_citation&amp;citation_for_view=abc:1">Deterministic Contract Verification for Agent Toolchains</a>
					<div class="gs_gray">J Researcher, A Coauthor</div>
				</td>
				<td class="gsc_a_c"><a class="gsc_a_ac" href="https://scholar.google.com/scholar?oi=bibs&amp;cites=111">312</a></td>
				<td class="gsc_a_y"><span class="gsc_a_h">2024</span></td>
			</tr>
			<tr class="gsc_a_tr">
				<td class="gsc_a_t">
					<a class="gsc_a_at" href="/citations?view_op=view_citation&amp;citation_for_view=abc:2">Zero-Database State Machines over Atomic Rename</a>
					<div class="gs_gray">J Researcher</div>
				</td>
				<td class="gsc_a_c"><a class="gsc_a_ac" href="https://scholar.google.com/scholar?oi=bibs&amp;cites=222">84</a></td>
				<td class="gsc_a_y"><span class="gsc_a_h">2025</span></td>
			</tr>
		</tbody></table>
	</body></html>`)

	profOut, err := VerifyRecipeOnHTML(recipe, map[string]string{"user_id": "abc"}, "profile_citations", profileHTML, "https://scholar.google.com/citations?user=abc&hl=en")
	if err != nil || !profOut.Passed {
		t.Fatalf("profile_citations verify failed: passed=%v err=%v outErr=%s", profOut.Passed, err, profOut.Error)
	}
	if profOut.StepResult.Fields["citations_total"] != int64(1842) || profOut.StepResult.Fields["citations_5y"] != int64(1190) {
		t.Fatalf("unexpected citation fields: %+v", profOut.StepResult.Fields)
	}
	if profOut.StepResult.Fields["h_index"] != int64(19) || profOut.StepResult.Fields["i10_index"] != int64(27) {
		t.Fatalf("unexpected h/i10 fields: %+v", profOut.StepResult.Fields)
	}
	if len(profOut.StepResult.Items) != 2 || profOut.StepResult.Items[0]["citations"] != int64(312) || profOut.StepResult.Items[0]["year"] != int64(2024) {
		t.Fatalf("unexpected paper items: %+v", profOut.StepResult.Items)
	}

	recHTML := contract.ScrubHTML(`<!doctype html><html><head><title>Google Scholar</title></head><body>
		<div id="gs_res_ccl_mid">
			<div class="gs_r gs_or gs_scl">
				<h3 class="gs_rt"><a href="https://arxiv.org/abs/2601.00001">Bounded Accessibility Trees for UI Grounding</a></h3>
				<div class="gs_a">M Author, K Peer - Systems Conference, 2026 - openreview.net</div>
				<div class="gs_rs">Compact semantic trees cut token overhead while preserving role hierarchy.</div>
				<div class="gs_fl"><a href="/scholar?cites=999">Cited by 41</a></div>
			</div>
		</div>
	</body></html>`)

	recOut, err := VerifyRecipeOnHTML(recipe, nil, "recommended_articles", recHTML, "https://scholar.google.com/")
	if err != nil || !recOut.Passed {
		t.Fatalf("recommended_articles verify failed: passed=%v err=%v outErr=%s", recOut.Passed, err, recOut.Error)
	}
	if len(recOut.StepResult.Items) != 1 || recOut.StepResult.Items[0]["citations"] != int64(41) || recOut.StepResult.Items[0]["year"] != int64(2026) {
		t.Fatalf("unexpected recommended items: %+v", recOut.StepResult.Items)
	}

	// Redirecting to /sorry/ must fail expectation validation so caller classifies AUTH_EXPIRED.
	sorryOut, err := VerifyRecipeOnHTML(recipe, nil, "profile_citations", profileHTML, "https://www.google.com/sorry/index?continue=https://scholar.google.com/citations")
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if sorryOut.Passed {
		t.Fatal("expected /sorry/ redirect URL to fail expectation validation")
	}
}

func TestVerifyRecipeOnAXTreeDesktopUIAAndRedaction(t *testing.T) {
	rawRecipe, err := os.ReadFile(filepath.Join("..", "..", "examples", "recipes", "desktop-task-manager.json"))
	if err != nil {
		t.Fatalf("read desktop-task-manager.json: %v", err)
	}
	var recipe contract.Recipe
	if err := contract.ParseJSONStrict(rawRecipe, &recipe); err != nil {
		t.Fatalf("ParseJSONStrict failed: %v", err)
	}
	if recipe.NormalizedDriver() != "uia" {
		t.Fatalf("recipe.NormalizedDriver() = %q, want uia", recipe.NormalizedDriver())
	}

	axSnapshot := []byte(`[
		{"nodeId": "1", "role": {"value": "ControlType.Window"}, "name": {"value": "Task Manager"}, "childIds": ["2", "3", "4", "5"]},
		{"nodeId": "2", "parentId": "1", "role": {"value": "Text"}, "automationId": "CpuUtilizationText", "name": {"value": "CPU 24%"}},
		{"nodeId": "3", "parentId": "1", "role": {"value": "Text"}, "automationId": "MemoryUsageText", "name": {"value": "Memory 19.5 GB"}},
		{"nodeId": "4", "parentId": "1", "role": {"value": "Edit"}, "isPassword": true, "name": {"value": "Secret Prompt"}, "value": {"value": "s3cr3t-passphrase"}},
		{"nodeId": "5", "parentId": "1", "role": {"value": "DataGrid"}, "childIds": ["6", "7"]},
		{"nodeId": "6", "parentId": "5", "role": {"value": "DataItem"}, "childIds": ["8", "9"]},
		{"nodeId": "8", "parentId": "6", "role": {"value": "Text"}, "automationId": "ColProcessName", "name": {"value": "deskdrop.exe"}},
		{"nodeId": "9", "parentId": "6", "role": {"value": "Text"}, "automationId": "ColMemoryMB", "name": {"value": "18.4 MB"}},
		{"nodeId": "7", "parentId": "5", "role": {"value": "DataItem"}, "childIds": ["10", "11"]},
		{"nodeId": "10", "parentId": "7", "role": {"value": "Text"}, "automationId": "ColProcessName", "name": {"value": "explorer.exe"}},
		{"nodeId": "11", "parentId": "7", "role": {"value": "Text"}, "automationId": "ColMemoryMB", "name": {"value": "94.0 MB"}}
	]`)

	out, err := VerifyRecipeOnAXTreeJSON(recipe, nil, "processes", axSnapshot, "")
	if err != nil || !out.Passed {
		t.Fatalf("VerifyRecipeOnAXTreeJSON failed: passed=%v err=%v outErr=%s", out.Passed, err, out.Error)
	}
	if out.StepResult.Fields["cpu_pct"] != int64(24) || out.StepResult.Fields["memory_gb"] != float64(19.5) {
		t.Fatalf("unexpected UIA fields: %+v", out.StepResult.Fields)
	}
	if out.ItemCount != 2 || out.StepResult.Items[0]["process"] != "deskdrop.exe" || out.StepResult.Items[0]["memory_mb"] != float64(18.4) {
		t.Fatalf("unexpected UIA items: %+v", out.StepResult.Items)
	}
	if strings.Contains(out.StepResult.TextExcerpt, "s3cr3t-passphrase") {
		t.Fatalf("VerifyRecipeOnAXTreeJSON leaked password value in TextExcerpt: %q", out.StepResult.TextExcerpt)
	}
}

func TestA7VerifyNodeTextInlineSpansAriaHiddenAndCSSParity(t *testing.T) {
	htmlDoc := `<!doctype html><html><body>
		<main>
			<div class="feed-card" data-urn="urn:li:activity:777">
				<span class="age">5 min</span>
				<span class="age2">3 months</span>
				<button aria-label="View Reactions" class="skip">Ignore</button>
				<button aria-label="64 Reactions" class="live"><span aria-hidden="true">64</span></button>
				<a href="/analytics/post-summary/urn:li:activity:777/">
					<span aria-hidden="true"><span>1,</span><span>234</span></span><span aria-hidden="true">impressions</span>
				</a>
				<div class="alt">impressions: 980</div>
			</div>
		</main>
	</body></html>`

	recipe := contract.Recipe{
		Name:  "parity-test",
		Topic: "analytics",
		Steps: []contract.Step{
			{
				ID:  "feed",
				URL: "https://example.com/feed",
				Items: &contract.ItemRule{
					Container:     `a[href*="/analytics/post-summary/"]`,
					AnchorClosest: "div.feed-card",
					Fields: map[string]contract.FieldRule{
						"impressions": {
							Selector: `div.feed-card a[href*="/analytics/post-summary/"]`,
							Regex:    `(?i)([\d,]+)\s+impressions`,
							Parse:    "number",
						},
						"reactions": {
							Selector: `button[aria-label*="reaction" i]:not(.skip)`,
							Attr:     "aria-label|text",
							Parse:    "number",
						},
						"age_min": {
							Selector: `span.age`,
							Parse:    "number",
						},
						"alt_group2": {
							Selector: `div.feed-card > div:last-child`,
							Regex:    `(?i)(?:(\d+)\s+impressions|impressions:\s*(\d+))`,
							Parse:    "number",
						},
					},
				},
			},
		},
	}

	out, err := VerifyRecipeOnHTML(recipe, nil, "feed", htmlDoc, "https://example.com/feed")
	if err != nil || !out.Passed || len(out.StepResult.Items) != 1 {
		t.Fatalf("VerifyRecipeOnHTML failed: passed=%v err=%v outErr=%s items=%d", out.Passed, err, out.Error, len(out.StepResult.Items))
	}
	item := out.StepResult.Items[0]
	if item["impressions"] != int64(1234) {
		t.Fatalf("impressions = %#v, want 1234 (aria-hidden + split inline spans)", item["impressions"])
	}
	if item["reactions"] != int64(64) {
		t.Fatalf("reactions = %#v, want 64 ([attr*='x' i] + :not(.skip))", item["reactions"])
	}
	if item["age_min"] != int64(5) {
		t.Fatalf("age_min = %#v, want 5 (not 5e6 from '5 min')", item["age_min"])
	}
	if item["alt_group2"] != int64(980) {
		t.Fatalf("alt_group2 = %#v, want 980 (:last-child + group-2 alternation)", item["alt_group2"])
	}

	// Quoted attribute operators and parentheses inside :not(...) must parse cleanly.
	quotedHTML := `<html><body><div data-u="a*=b" title="ok">MATCH</div><div data-u="a*=b" title="a)b">SKIP</div></body></html>`
	qRecipe := contract.Recipe{
		Name:  "quoted-sel",
		Topic: "t",
		Steps: []contract.Step{{
			ID:  "s1",
			URL: "https://example.com/",
			Fields: map[string]contract.FieldRule{
				"picked": {Selector: `div[data-u="a*=b"]:not([title="a)b"])`},
			},
		}},
	}
	qOut, err := VerifyRecipeOnHTML(qRecipe, nil, "s1", quotedHTML, "https://example.com/")
	if err != nil || !qOut.Passed || qOut.StepResult.Fields["picked"] != "MATCH" {
		t.Fatalf("quoted selector parity failed: passed=%v err=%v fields=%+v", qOut.Passed, err, qOut.StepResult.Fields)
	}
}
