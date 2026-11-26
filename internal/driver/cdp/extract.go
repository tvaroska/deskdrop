package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/driver"
)

// ExecuteStep attaches to an open matching tab (or opens a stateless scratch tab), evaluates
// declarative extraction rules (zero arbitrary JS), validates expectations, and immediately closes
// any scratch tab on both success and failure.
func (c *Client) ExecuteStep(ctx context.Context, step contract.Step) (contract.StepResult, error) {
	timeout := 25 * time.Second
	if step.TimeoutMs > 0 {
		timeout = time.Duration(step.TimeoutMs) * time.Millisecond
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	matchPattern := step.MatchTabURL
	if matchPattern == "" && step.ReuseOpenTab {
		matchPattern = step.URL
	}

	if matchPattern != "" && step.ReuseOpenTab {
		pages, lerr := c.ListPages(sctx)
		if lerr == nil {
			c.mu.Lock()
			prefSession := c.preferredSession
			c.mu.Unlock()
			bestCtx := ""
			if prefSession != "" {
				bestCtx = c.ResolveBrowserContextID(sctx, step.URL)
			}
			for _, p := range pages {
				if bestCtx != "" && p.BrowserContextID != "" && p.BrowserContextID != bestCtx {
					continue
				}
				if matchesTabPattern(p.URL, matchPattern) {
					sessionID, err := c.AttachPage(sctx, p.TargetID)
					if err == nil {
						_ = c.Call(sctx, sessionID, "Runtime.enable", nil, nil)
						res, stepErr := c.runAttachedStep(sctx, sessionID, step, true)
						c.DetachPage(context.Background(), sessionID)
						if stepErr == nil && p.BrowserContextID != "" {
							c.SetVerifiedContextID(p.BrowserContextID)
						}
						return res, stepErr
					}
				}
			}
		}
	}

	if step.URL == "" {
		return contract.StepResult{}, fmt.Errorf("no open tab matched %q and step.url is empty", matchPattern)
	}

	candidates := c.RankBrowserContextIDs(sctx, step.URL)
	if len(candidates) == 0 {
		candidates = []string{""}
	}

	candTimeout := timeout
	if len(candidates) > 1 && candTimeout > 18*time.Second {
		candTimeout = 18 * time.Second
	}

	c.mu.Lock()
	verifiedCtx := c.verifiedContextID
	c.mu.Unlock()
	targetDomain := contract.ExtractDomain(step.URL)

	var lastRes contract.StepResult
	var lastErr error
	for i, candCtx := range candidates {
		if ctx.Err() != nil {
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			break
		}
		cctx, ccancel := context.WithTimeout(ctx, candTimeout)
		scratchID, sessionID, err := c.CreateScratchPageWithContext(cctx, step.URL, candCtx)
		if err != nil {
			ccancel()
			lastErr = err
			continue
		}
		res, stepErr := c.runAttachedStep(cctx, sessionID, step, false)
		ccancel()
		// Always close the scratch tab immediately (stateless, ADR-004).
		_ = c.CloseScratchPage(context.Background(), scratchID, sessionID)
		if stepErr == nil {
			if candCtx != "" {
				c.SetVerifiedContextID(candCtx)
			}
			return res, nil
		}
		lastRes = res
		lastErr = stepErr

		// Stop multi-profile fallback when the verified/logged-in profile reached the target domain
		// and failed with a non-auth error (e.g. SELECTOR_DRIFT), so another unauthenticated Chrome
		// profile cannot mask SELECTOR_DRIFT with a /login AUTH_EXPIRED error.
		if shouldStopProfileFallback(stepErr, targetDomain, candCtx, verifiedCtx) {
			return lastRes, lastErr
		}
		if i+1 < len(candidates) {
			continue
		}
	}
	return lastRes, lastErr
}

func shouldStopProfileFallback(stepErr error, targetDomain string, candCtx string, verifiedCtx string) bool {
	if stepErr == nil {
		return false
	}
	landedURL := ""
	var execErr *driver.StepExecError
	if errors.As(stepErr, &execErr) {
		landedURL = execErr.Partial.URL
	}
	triage := contract.ClassifyExtractionError(stepErr.Error(), landedURL, targetDomain, execErr != nil)
	if triage.Code == contract.ErrorCodeAuthExpired || triage.Action == contract.TriageHoldDomainAuth {
		return false
	}
	if targetDomain != "" && landedURL != "" {
		if landedDom := contract.ExtractDomain(landedURL); landedDom != "" && contract.DomainsMatch(landedDom, targetDomain) {
			return true
		}
	}
	if verifiedCtx != "" && candCtx == verifiedCtx && triage.Code == contract.ErrorCodeSelectorDrift {
		return true
	}
	return false
}

// ExecuteFallback opens a fresh stateless scratch tab on the allowlisted domain, executes at most
// 3 safe actions ({scroll, wait, click}), captures fresh scrubbed HTML + AXTree + PNG screenshot +
// declarative extraction, and closes the scratch tab unconditionally.
func (c *Client) ExecuteFallback(ctx context.Context, step contract.Step, plan contract.FallbackPlan) (driver.FallbackExecResult, error) {
	allowedDomains := contract.StepDomains(step)
	if len(allowedDomains) == 0 {
		return driver.FallbackExecResult{}, errors.New("fallback step has no approved HTTP/HTTPS domain")
	}
	if err := contract.ValidateFallbackPlan(plan, allowedDomains); err != nil {
		return driver.FallbackExecResult{}, err
	}
	timeout := 30 * time.Second
	if step.TimeoutMs > 0 {
		timeout = time.Duration(step.TimeoutMs) * time.Millisecond
	}
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	scratchID, sessionID, err := c.CreateScratchPage(fctx, plan.TargetURL)
	if err != nil {
		return driver.FallbackExecResult{}, err
	}
	defer func() {
		_ = c.CloseScratchPage(context.Background(), scratchID, sessionID)
	}()

	if err := c.waitDocumentReady(fctx, sessionID); err != nil {
		return driver.FallbackExecResult{}, fmt.Errorf("fallback page load failed: %w", err)
	}
	// Fail-closed URL/domain/login check BEFORE executing any interactive action.
	if _, err := c.verifyAllowedSessionURL(fctx, sessionID, step, plan.AllowedDomain, allowedDomains, "fallback"); err != nil {
		return driver.FallbackExecResult{}, err
	}

	actionsRun := 0
	for _, act := range plan.Actions {
		switch strings.ToLower(strings.TrimSpace(act.Action)) {
		case "scroll":
			expr := fmt.Sprintf("(() => { window.scrollBy(0, %d); return true; })()", act.DeltaY)
			if err := c.EvalJSON(fctx, sessionID, expr, nil); err != nil {
				return driver.FallbackExecResult{}, fmt.Errorf("fallback scroll failed: %w", err)
			}
		case "wait":
			select {
			case <-fctx.Done():
				return driver.FallbackExecResult{}, fctx.Err()
			case <-time.After(time.Duration(act.WaitMs) * time.Millisecond):
			}
		case "click":
			var clicked bool
			if err := c.EvalJSON(fctx, sessionID, buildDeepClickJS(act.Selector), &clicked); err != nil {
				return driver.FallbackExecResult{}, fmt.Errorf("fallback click %q failed: %w", act.Selector, err)
			}
		}
		actionsRun++
		// Re-verify allowlisted domain and NotURLContains after every action.
		if _, err := c.verifyAllowedSessionURL(fctx, sessionID, step, plan.AllowedDomain, allowedDomains, "fallback"); err != nil {
			return driver.FallbackExecResult{}, err
		}
	}

	maxText := step.MaxTextChars
	if maxText <= 0 {
		maxText = 1200
	}
	extractExpr := buildDeclarativeExtractJS(step, maxText)
	var stepRes contract.StepResult
	if err := c.EvalJSON(fctx, sessionID, extractExpr, &stepRes); err != nil {
		return driver.FallbackExecResult{}, fmt.Errorf("fallback extraction failed: %w", err)
	}
	stepRes.StepID = step.ID
	if stepRes.StepID == "" {
		stepRes.StepID = plan.StepID
	}
	if stepRes.StepID == "" {
		stepRes.StepID = "fallback"
	}

	pngBytes, err := c.CaptureScreenshotPNG(fctx, sessionID)
	if err != nil {
		return driver.FallbackExecResult{}, fmt.Errorf("fallback screenshot capture failed: %w", err)
	}
	var rawHTML string
	if err := c.EvalJSON(fctx, sessionID, buildScrubbedHTMLJS(), &rawHTML); err != nil {
		return driver.FallbackExecResult{}, fmt.Errorf("fallback dom capture failed: %w", err)
	}
	scrubbed := contract.ScrubHTML(rawHTML)
	axTree, _, err := c.CaptureCompactAXTree(fctx, sessionID)
	if err != nil {
		return driver.FallbackExecResult{}, fmt.Errorf("fallback axtree capture failed: %w", err)
	}

	return driver.FallbackExecResult{
		StepResult:    stepRes,
		ScreenshotPNG: pngBytes,
		DOMHTML:       scrubbed,
		AXTreeText:    axTree,
		ActionsRun:    actionsRun,
	}, nil
}

func (c *Client) verifyAllowedSessionURL(
	ctx context.Context,
	sessionID string,
	step contract.Step,
	planAllowed string,
	allowedDomains []string,
	phase string,
) (string, error) {
	var curURL string
	if err := c.EvalJSON(ctx, sessionID, "location.href || ''", &curURL); err != nil {
		return "", fmt.Errorf("%s URL check failed: %w", phase, err)
	}
	curURL = strings.TrimSpace(curURL)
	if curURL == "" {
		return "", fmt.Errorf("%s landed on empty URL", phase)
	}
	lowerURL := strings.ToLower(curURL)
	for _, bad := range step.Expect.NotURLContains {
		if bad != "" && strings.Contains(lowerURL, strings.ToLower(bad)) {
			return "", fmt.Errorf("step %q landed on blocked/login URL %q (matched %q)", step.ID, curURL, bad)
		}
	}
	landedDomain := contract.ExtractDomain(curURL)
	if landedDomain == "" {
		return "", fmt.Errorf("%s navigated to non-HTTP URL %q", phase, curURL)
	}
	if planAllowed != "" && !contract.DomainsMatch(landedDomain, planAllowed) {
		return "", fmt.Errorf("%s navigated off allowlisted domain %q to %q", phase, planAllowed, landedDomain)
	}
	if len(allowedDomains) > 0 {
		matched := false
		for _, dom := range allowedDomains {
			if contract.DomainsMatch(landedDomain, dom) {
				matched = true
				break
			}
		}
		if !matched {
			return "", fmt.Errorf("%s navigated off approved recipe domains %v to %q", phase, allowedDomains, landedDomain)
		}
	}
	return curURL, nil
}

func (c *Client) runAttachedStep(sctx context.Context, sessionID string, step contract.Step, reused bool) (contract.StepResult, error) {
	if !reused {
		if err := c.waitDocumentReady(sctx, sessionID); err != nil {
			return contract.StepResult{}, c.wrapStepError(sessionID, step.ID, contract.StepResult{}, err)
		}
	}

	if step.WaitSelector != "" || step.WaitText != "" {
		if err := c.waitForCondition(sctx, sessionID, step.WaitSelector, step.WaitText, step.Expect.NotURLContains); err != nil {
			return contract.StepResult{}, c.wrapStepError(sessionID, step.ID, contract.StepResult{}, err)
		}
	}

	if step.ClickIfPresent != "" {
		var clicked bool
		if err := c.EvalJSON(sctx, sessionID, buildDeepClickJS(step.ClickIfPresent), &clicked); err == nil && clicked {
			select {
			case <-sctx.Done():
				return contract.StepResult{}, sctx.Err()
			case <-time.After(450 * time.Millisecond):
			}
		}
	}

	if step.ClickSelector != "" {
		_ = c.waitForCondition(sctx, sessionID, step.ClickSelector, "", step.Expect.NotURLContains)
		var clicked bool
		if err := c.EvalJSON(sctx, sessionID, buildDeepClickJS(step.ClickSelector), &clicked); err != nil {
			return contract.StepResult{}, c.wrapStepError(sessionID, step.ID, contract.StepResult{}, err)
		}
		if !clicked {
			return contract.StepResult{}, c.wrapStepError(
				sessionID, step.ID, contract.StepResult{},
				fmt.Errorf("click_selector %q did not match any element", step.ClickSelector),
			)
		}
	}

	if step.WaitMs > 0 {
		select {
		case <-sctx.Done():
			return contract.StepResult{}, sctx.Err()
		case <-time.After(time.Duration(step.WaitMs) * time.Millisecond):
		}
	}

	maxText := step.MaxTextChars
	if maxText <= 0 {
		maxText = 1200
	}

	extractExpr := buildDeclarativeExtractJS(step, maxText)
	var res contract.StepResult
	if err := c.EvalJSON(sctx, sessionID, extractExpr, &res); err != nil {
		return contract.StepResult{}, c.wrapStepError(sessionID, step.ID, contract.StepResult{}, err)
	}
	res.StepID = step.ID
	res.ReusedTab = reused

	if step.Pagination != nil {
		pagedRes, perr := c.runPaginationLoop(sctx, sessionID, step, res, maxText)
		if perr != nil {
			return contract.StepResult{}, c.wrapStepError(sessionID, step.ID, pagedRes, perr)
		}
		res = pagedRes
	}

	if err := contract.ValidateStepResult(step, res); err != nil {
		return contract.StepResult{}, c.wrapStepError(sessionID, step.ID, res, err)
	}

	return res, nil
}

func (c *Client) wrapStepError(sessionID string, stepID string, partial contract.StepResult, err error) error {
	dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if strings.TrimSpace(partial.URL) == "" {
		var curURL string
		if uerr := c.EvalJSON(dctx, sessionID, "String(location.href || '')", &curURL); uerr == nil && strings.TrimSpace(curURL) != "" {
			partial.URL = curURL
		}
	}
	pngBytes, _ := c.CaptureScreenshotPNG(dctx, sessionID)
	var rawHTML string
	_ = c.EvalJSON(dctx, sessionID, buildScrubbedHTMLJS(), &rawHTML)
	axTree, _, _ := c.CaptureCompactAXTree(dctx, sessionID)

	return &driver.StepExecError{
		StepID:        stepID,
		Partial:       partial,
		Err:           err,
		ScreenshotPNG: pngBytes,
		DOMHTML:       contract.ScrubHTML(rawHTML),
		AXTreeText:    axTree,
	}
}

func (c *Client) waitDocumentReady(ctx context.Context, sessionID string) error {
	settle := c.settleDelay
	if settle <= 0 {
		settle = 350 * time.Millisecond
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if err := c.EvalJSON(ctx, sessionID, "document.readyState", &state); err == nil {
			if state == "complete" || state == "interactive" {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(settle):
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return nil
}

func matchesTabPattern(pageURL string, pattern string) bool {
	uRaw := strings.TrimSpace(pageURL)
	pRaw := strings.TrimSpace(pattern)
	if uRaw == "" || pRaw == "" {
		return false
	}
	pu, err := url.Parse(uRaw)
	if err != nil {
		return false
	}
	pageScheme := strings.ToLower(pu.Scheme)
	if pageScheme != "http" && pageScheme != "https" {
		return false
	}
	pageHost := strings.ToLower(strings.TrimSuffix(pu.Hostname(), "."))
	if pageHost == "" {
		return false
	}

	anchored := strings.HasSuffix(pRaw, "$")
	base := strings.TrimSuffix(pRaw, "$")
	if base == "" {
		return false
	}

	hasScheme := strings.Contains(base, "://")
	parseTarget := base
	if !hasScheme {
		if strings.HasPrefix(base, "/") {
			return false
		}
		parseTarget = "https://" + base
	}
	patURL, err := url.Parse(parseTarget)
	if err != nil {
		return false
	}
	patHost := strings.ToLower(strings.TrimSuffix(patURL.Hostname(), "."))
	if patHost == "" {
		return false
	}
	if hasScheme {
		if strings.ToLower(patURL.Scheme) != pageScheme || pageHost != patHost {
			return false
		}
	} else {
		// Enforce host boundary: exact match or www. prefix (never evil-scholar.google.com).
		if pageHost != patHost && pageHost != "www."+patHost {
			return false
		}
	}

	pagePath := strings.ToLower(pu.EscapedPath())
	if pagePath == "" {
		pagePath = "/"
	}
	patPath := strings.ToLower(patURL.EscapedPath())
	pageQuery := strings.ToLower(pu.RawQuery)
	patQuery := strings.ToLower(patURL.RawQuery)
	patHasQuery := strings.Contains(base, "?")

	if anchored {
		normPatPath := patPath
		if normPatPath == "" {
			normPatPath = "/"
		}
		pathMatch := pagePath == normPatPath || (strings.HasSuffix(normPatPath, "/") && pagePath+"/" == normPatPath)
		if !pathMatch {
			return false
		}
		if patHasQuery {
			return pageQuery == patQuery
		}
		return true
	}

	if patHasQuery {
		pagePQ := pagePath + "?" + pageQuery
		patPQ := patPath + "?" + patQuery
		return strings.Contains(pagePQ, patPQ)
	}
	if patPath == "" || patPath == "/" {
		return true
	}
	return strings.HasPrefix(pagePath, patPath)
}

func (c *Client) waitForCondition(ctx context.Context, sessionID string, selector string, text string, forbiddenURLSubstrings []string) error {
	selJSON, _ := json.Marshal(selector)
	txtJSON, _ := json.Marshal(strings.ToLower(text))
	forbJSON, _ := json.Marshal(forbiddenURLSubstrings)
	expr := fmt.Sprintf(`(() => {
		%s
		const sel = %s;
		const needle = %s;
		const forb = %s || [];
		const href = String(location.href || '');
		const hrefLower = href.toLowerCase();
		for (const pat of forb) {
			if (pat && hrefLower.includes(String(pat).toLowerCase())) {
				return { ok: false, blockedUrl: href, matched: pat };
			}
		}
		if (forb.length > 0 && document.querySelector('[data-testid="loginButton"], a[href="/login"], a[href*="/i/flow/login"], form[action*="/uas/login"]')) {
			if (!sel || !deepQueryFirst(document, sel)) {
				return { ok: false, blockedUrl: href, matched: 'login_prompt_dom' };
			}
		}
		if (sel && !deepQueryFirst(document, sel)) return { ok: false };
		if (needle) {
			const allText = collectVisibleText(document, 50000).toLowerCase();
			if (!allText.includes(needle)) return { ok: false };
		}
		return { ok: true };
	})()`, shadowDOMHelperJS, string(selJSON), string(txtJSON), string(forbJSON))

	started := time.Now()
	for {
		var state struct {
			OK         bool   `json:"ok"`
			BlockedURL string `json:"blockedUrl"`
			Matched    string `json:"matched"`
		}
		if err := c.EvalJSON(ctx, sessionID, expr, &state); err == nil {
			if state.OK {
				return nil
			}
			if state.BlockedURL != "" && time.Since(started) >= 600*time.Millisecond {
				return fmt.Errorf("landed on blocked/login URL %q (matched %q)", state.BlockedURL, state.Matched)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for selector=%q text=%q", selector, text)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
