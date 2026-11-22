package verify

import (
	"fmt"
	"strings"

	"golang.org/x/net/html"

	"github.com/tvaroska/deskdrop/internal/contract"
)

// VerifyRecipeOnHTMLPages evaluates a multi-page Step across sequential scrubbed HTML snapshots in pure Go,
// mirroring internal/driver/cdp's pagination loop (checking NextSelector presence/disabled state, deduplicating
// items/rows via contract.MergePaginatedStepResult, and validating expectations on the combined result).
func VerifyRecipeOnHTMLPages(recipe contract.Recipe, vars map[string]string, stepID string, htmlPages []string, landedURL string) (VerifyOutcome, error) {
	if len(htmlPages) == 0 {
		return VerifyOutcome{Passed: false, Error: "htmlPages must contain at least 1 page"}, fmt.Errorf("empty htmlPages")
	}
	resolved, err := contract.ResolveRecipe(recipe, vars)
	if err != nil {
		return VerifyOutcome{Passed: false, Error: err.Error()}, err
	}
	var target *contract.Step
	if strings.TrimSpace(stepID) == "" {
		target = &resolved.Steps[0]
	} else {
		for i := range resolved.Steps {
			if resolved.Steps[i].ID == stepID {
				target = &resolved.Steps[i]
				break
			}
		}
	}
	if target == nil {
		return VerifyOutcome{Passed: false, Error: fmt.Sprintf("step %q not found in recipe %q", stepID, resolved.Name)}, fmt.Errorf("step %q not found", stepID)
	}
	if landedURL == "" {
		landedURL = target.URL
	}

	acc, err := ExtractStepFromHTML(*target, htmlPages[0], landedURL)
	if err != nil {
		return VerifyOutcome{Passed: false, StepID: target.ID, Error: err.Error()}, err
	}

	if target.Pagination != nil {
		pag := *target.Pagination
		acc.PagesVisited = 1
		maxPages := pag.MaxPages
		if len(htmlPages) < maxPages {
			maxPages = len(htmlPages)
		}
		for pageIdx := 2; pageIdx <= maxPages; pageIdx++ {
			if target.Items != nil && target.Items.Limit > 0 && len(acc.Items) >= target.Items.Limit {
				break
			}
			prevDoc, perr := html.Parse(strings.NewReader(htmlPages[pageIdx-2]))
			if perr != nil || !isNextControlEnabledInHTML(prevDoc, pag.NextSelector) {
				break
			}
			pageRes, eerr := ExtractStepFromHTML(*target, htmlPages[pageIdx-1], landedURL)
			if eerr != nil {
				return VerifyOutcome{Passed: false, StepID: target.ID, Error: eerr.Error()}, eerr
			}
			merged, added, reachedLimit := contract.MergePaginatedStepResult(acc, pageRes, target.Items)
			acc = merged
			acc.PagesVisited = pageIdx
			if reachedLimit || (pag.StopWhenNoNew() && added == 0) {
				break
			}
		}
	}

	valErr := contract.ValidateStepResult(*target, acc)
	out := VerifyOutcome{
		Passed:      valErr == nil,
		StepID:      target.ID,
		StepResult:  acc,
		ItemCount:   len(acc.Items),
		FieldCount:  len(acc.Fields),
		TableCount:  len(acc.Tables),
		MetricCount: len(acc.Metrics),
	}
	if valErr != nil {
		out.Error = valErr.Error()
	}
	return out, nil
}

func isNextControlEnabledInHTML(doc *html.Node, selector string) bool {
	el := QueryFirst(doc, selector)
	if el == nil {
		return false
	}
	if _, hidden := lookupAttr(el, "hidden"); hidden {
		return false
	}
	if strings.EqualFold(getAttr(el, "aria-hidden"), "true") {
		return false
	}
	if _, disabled := lookupAttr(el, "disabled"); disabled {
		return false
	}
	if strings.EqualFold(getAttr(el, "aria-disabled"), "true") {
		return false
	}
	for _, cls := range strings.Fields(getAttr(el, "class")) {
		if cls == "disabled" || cls == "is-disabled" {
			return false
		}
	}
	return true
}
