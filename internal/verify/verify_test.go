package verify

import (
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
