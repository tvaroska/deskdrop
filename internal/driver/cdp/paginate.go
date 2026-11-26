package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
)

type nextClickState struct {
	Clicked bool   `json:"clicked"`
	Reason  string `json:"reason,omitempty"`
}

func (c *Client) runPaginationLoop(
	sctx context.Context,
	sessionID string,
	step contract.Step,
	initial contract.StepResult,
	maxText int,
) (contract.StepResult, error) {
	pag := step.Pagination
	if pag == nil {
		return initial, nil
	}
	acc := initial
	acc.PagesVisited = 1
	if pag.MaxPages <= 1 {
		return acc, nil
	}
	if step.Items != nil && step.Items.Limit > 0 && len(acc.Items) >= step.Items.Limit {
		return acc, nil
	}

	allowedDomains := contract.StepDomains(step)
	baseDomain := ""
	if len(allowedDomains) > 0 {
		baseDomain = allowedDomains[0]
	} else {
		baseDomain = contract.ExtractDomain(initial.URL)
		if baseDomain != "" {
			allowedDomains = []string{baseDomain}
		}
	}
	if baseDomain == "" {
		return acc, errors.New("pagination requires a valid HTTP/HTTPS base domain")
	}

	clickNextExpr := buildClickNextIfEnabledJS(pag.NextSelector)
	extractExpr := buildDeclarativeExtractJS(step, maxText)

	for pageIdx := 2; pageIdx <= pag.MaxPages; pageIdx++ {
		if _, err := c.verifyAllowedSessionURL(sctx, sessionID, step, baseDomain, allowedDomains, "pagination"); err != nil {
			return acc, err
		}
		var clickRes nextClickState
		if err := c.EvalJSON(sctx, sessionID, clickNextExpr, &clickRes); err != nil {
			return acc, fmt.Errorf("pagination click %q on page %d failed: %w", pag.NextSelector, pageIdx, err)
		}
		if !clickRes.Clicked {
			break
		}

		if pag.WaitMs > 0 {
			select {
			case <-sctx.Done():
				return acc, sctx.Err()
			case <-time.After(time.Duration(pag.WaitMs) * time.Millisecond):
			}
		}

		if pag.WaitSelector != "" {
			if err := c.waitForCondition(sctx, sessionID, pag.WaitSelector, "", step.Expect.NotURLContains); err != nil {
				return acc, err
			}
		}

		if _, err := c.verifyAllowedSessionURL(sctx, sessionID, step, baseDomain, allowedDomains, "pagination"); err != nil {
			return acc, err
		}

		var pageRes contract.StepResult
		if err := c.EvalJSON(sctx, sessionID, extractExpr, &pageRes); err != nil {
			return acc, err
		}

		merged, added, reachedLimit := contract.MergePaginatedStepResult(acc, pageRes, step.Items)
		acc = merged
		acc.PagesVisited = pageIdx

		if reachedLimit || (pag.StopWhenNoNew() && added == 0) {
			break
		}
	}
	return acc, nil
}

func buildClickNextIfEnabledJS(selector string) string {
	selJSON, _ := json.Marshal(selector)
	return fmt.Sprintf(`(() => {
		%s
		const el = deepQueryFirst(document, %s);
		if (!el || !isVisible(el)) {
			return { clicked: false, reason: "missing_or_hidden" };
		}
		if (el.disabled === true || (el.hasAttribute && el.hasAttribute("disabled"))) {
			return { clicked: false, reason: "disabled" };
		}
		if (el.getAttribute && String(el.getAttribute("aria-disabled") || "").toLowerCase() === "true") {
			return { clicked: false, reason: "aria_disabled" };
		}
		if (el.classList && (el.classList.contains("disabled") || el.classList.contains("is-disabled"))) {
			return { clicked: false, reason: "class_disabled" };
		}
		el.click();
		return { clicked: true };
	})()`, shadowDOMHelperJS, string(selJSON))
}
