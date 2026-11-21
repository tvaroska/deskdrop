package cdp

import (
	"context"
	"encoding/json"
	"fmt"
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
				if strings.Contains(strings.ToLower(p.URL), strings.ToLower(matchPattern)) {
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

	var lastRes contract.StepResult
	var lastErr error
	for i, candCtx := range candidates {
		if ctx.Err() != nil {
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
		if i+1 < len(candidates) {
			continue
		}
	}
	return lastRes, lastErr
}

// ExecuteFallback opens a fresh stateless scratch tab on the allowlisted domain, executes at most
// 3 safe actions ({scroll, wait, click}), captures fresh scrubbed HTML + PNG screenshot + declarative
// extraction, and closes the scratch tab unconditionally.
func (c *Client) ExecuteFallback(ctx context.Context, step contract.Step, plan contract.FallbackPlan) (driver.FallbackExecResult, error) {
	if err := contract.ValidateFallbackPlan(plan, nil); err != nil {
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

	_ = c.waitDocumentReady(fctx, sessionID)

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
	}

	// Verify we did not navigate off the allowlisted domain.
	var landedURL string
	_ = c.EvalJSON(fctx, sessionID, "location.href || ''", &landedURL)
	if landedDomain := contract.ExtractDomain(landedURL); landedDomain != "" && !contract.DomainsMatch(landedDomain, plan.AllowedDomain) {
		return driver.FallbackExecResult{}, fmt.Errorf("fallback navigated off allowlisted domain %q to %q", plan.AllowedDomain, landedDomain)
	}

	maxText := step.MaxTextChars
	if maxText <= 0 {
		maxText = 1200
	}
	extractExpr := buildDeclarativeExtractJS(step, maxText)
	var stepRes contract.StepResult
	_ = c.EvalJSON(fctx, sessionID, extractExpr, &stepRes)
	stepRes.StepID = step.ID
	if stepRes.StepID == "" {
		stepRes.StepID = plan.StepID
	}
	if stepRes.StepID == "" {
		stepRes.StepID = "fallback"
	}

	pngBytes, _ := c.CaptureScreenshotPNG(fctx, sessionID)
	var rawHTML string
	_ = c.EvalJSON(fctx, sessionID, buildScrubbedHTMLJS(), &rawHTML)
	scrubbed := contract.ScrubHTML(rawHTML)

	return driver.FallbackExecResult{
		StepResult:    stepRes,
		ScreenshotPNG: pngBytes,
		DOMHTML:       scrubbed,
		ActionsRun:    actionsRun,
	}, nil
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

	if err := contract.ValidateStepResult(step, res); err != nil {
		return contract.StepResult{}, c.wrapStepError(sessionID, step.ID, res, err)
	}

	return res, nil
}

func (c *Client) wrapStepError(sessionID string, stepID string, partial contract.StepResult, err error) error {
	dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pngBytes, _ := c.CaptureScreenshotPNG(dctx, sessionID)
	var rawHTML string
	_ = c.EvalJSON(dctx, sessionID, buildScrubbedHTMLJS(), &rawHTML)

	return &driver.StepExecError{
		StepID:        stepID,
		Partial:       partial,
		Err:           err,
		ScreenshotPNG: pngBytes,
		DOMHTML:       contract.ScrubHTML(rawHTML),
	}
}

func (c *Client) waitDocumentReady(ctx context.Context, sessionID string) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if err := c.EvalJSON(ctx, sessionID, "document.readyState", &state); err == nil {
			if state == "complete" || state == "interactive" {
				time.Sleep(350 * time.Millisecond)
				return nil
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

func buildDeepClickJS(selector string) string {
	selJSON, _ := json.Marshal(selector)
	return fmt.Sprintf(`(() => {
		%s
		const el = deepQueryFirst(document, %s);
		if (!el) return false;
		el.click();
		return true;
	})()`, shadowDOMHelperJS, string(selJSON))
}

// buildDeclarativeExtractJS evaluates declarative Selectors, Fields, Items, Tables, Cards, and Headings
// with parity to internal/verify's pure-Go HTML evaluator.
func buildDeclarativeExtractJS(step contract.Step, maxText int) string {
	selJSON, _ := json.Marshal(step.Selectors)
	fieldsJSON, _ := json.Marshal(step.Fields)
	itemsJSON, _ := json.Marshal(step.Items)
	return fmt.Sprintf(`(() => {
		%s
		const selMap = %s || {};
		const fieldRules = %s || {};
		const itemRule = %s;

		const selectorOut = {};
		for (const [key, sel] of Object.entries(selMap)) {
			const matches = deepQueryAll(document, sel);
			selectorOut[key] = matches.map(n => cleanText(n)).filter(Boolean).join(" | ");
		}

		const fieldsOut = {};
		for (const [key, rule] of Object.entries(fieldRules)) {
			const val = evalFieldRule(document.body || document.documentElement, rule);
			if (val !== undefined) fieldsOut[key] = val;
		}

		const itemsOut = itemRule ? evalItemRule(document, itemRule) : [];

		const headings = [];
		const seenHeadings = new Set();
		for (const el of allShadowElements(document)) {
			const tag = (el.tagName || "").toUpperCase();
			const role = el.getAttribute ? el.getAttribute("role") : "";
			if ((tag === "H1" || tag === "H2" || tag === "H3" || role === "heading") && isVisible(el)) {
				const t = cleanText(el);
				if (t && t.length <= 140 && !seenHeadings.has(t)) {
					seenHeadings.add(t);
					headings.push(t);
					if (headings.length >= 20) break;
				}
			}
		}

		const metrics = %v ? extractMetricCards(document) : [];
		const tables = %v ? extractAllTables(document) : [];
		const excerpt = collectVisibleText(document, %d);

		return {
			title: document.title || "",
			url: location.href || "",
			headings: headings,
			metrics: metrics,
			selectors: Object.keys(selectorOut).length ? selectorOut : undefined,
			fields: Object.keys(fieldsOut).length ? fieldsOut : undefined,
			items: itemsOut.length ? itemsOut : undefined,
			tables: tables,
			text_excerpt: excerpt
		};
	})()`, shadowDOMHelperJS, string(selJSON), string(fieldsJSON), string(itemsJSON), step.WantCards(), step.WantTables(), maxText)
}

// buildScrubbedHTMLJS serializes a scrubbed HTML snapshot (stripping script, style, svg, noscript,
// input values, and textarea text, while flattening open Shadow DOM subtrees so Go HTML verification works).
func buildScrubbedHTMLJS() string {
	return `(() => {
		function cloneClean(node) {
			if (!node) return null;
			if (node.nodeType === 3) return document.createTextNode(node.nodeValue || "");
			if (node.nodeType !== 1) return null;
			const tag = (node.tagName || "").toUpperCase();
			if (tag === "SCRIPT" || tag === "STYLE" || tag === "SVG" || tag === "NOSCRIPT") return null;
			const copy = document.createElement(tag.toLowerCase());
			if (node.attributes) {
				for (const attr of node.attributes) {
					const name = attr.name.toLowerCase();
					if (name.startsWith("on")) continue;
					if (tag === "INPUT" && name === "value") {
						copy.setAttribute("value", "");
						continue;
					}
					copy.setAttribute(attr.name, attr.value);
				}
			}
			if (tag === "TEXTAREA") {
				return copy;
			}
			if (node.shadowRoot) {
				const srWrap = document.createElement("div");
				srWrap.setAttribute("data-shadowroot", "open");
				for (const child of node.shadowRoot.childNodes) {
					const c = cloneClean(child);
					if (c) srWrap.appendChild(c);
				}
				copy.appendChild(srWrap);
			}
			for (const child of node.childNodes) {
				const c = cloneClean(child);
				if (c) copy.appendChild(c);
			}
			return copy;
		}
		const root = cloneClean(document.documentElement);
		if (!root) return "";
		return "<!doctype html>\n" + root.outerHTML;
	})()`
}

const shadowDOMHelperJS = `
function allShadowElements(root) {
	const out = [];
	const stack = [root];
	while (stack.length) {
		const cur = stack.pop();
		if (!cur) continue;
		if (cur.nodeType === 1) out.push(cur);
		if (cur.shadowRoot) stack.push(cur.shadowRoot);
		const children = cur.children || cur.childNodes || [];
		for (let i = children.length - 1; i >= 0; i--) {
			if (children[i].nodeType === 1 || children[i].nodeType === 11) {
				stack.push(children[i]);
			}
		}
	}
	return out;
}

function isVisible(el) {
	if (!el || el.nodeType !== 1) return false;
	if (el.hidden || el.getAttribute("aria-hidden") === "true") return false;
	const st = window.getComputedStyle(el);
	if (!st || st.display === "none" || st.visibility === "hidden" || st.opacity === "0") return false;
	return true;
}

function normalizeWS(raw) {
	return String(raw || "").replace(/[\u200b\u00a0\ufeff]+/g, " ").replace(/\s+/g, " ").trim();
}

function cleanText(el) {
	if (!el) return "";
	const raw = (el.innerText !== undefined && el.innerText !== "") ? el.innerText : (el.textContent || "");
	return normalizeWS(raw);
}

function parseCompactFloat(raw) {
	const cleaned = normalizeWS(raw).replace(/,/g, "");
	const m = cleaned.match(/(-?\d+(?:\.\d+)?)\s*([KkMmBb])?/);
	if (!m) return { ok: false, val: 0 };
	let n = parseFloat(m[1]);
	if (Number.isNaN(n)) return { ok: false, val: 0 };
	const suf = (m[2] || "").toUpperCase();
	if (suf === "K") n *= 1000;
	else if (suf === "M") n *= 1000000;
	else if (suf === "B") n *= 1000000000;
	return { ok: true, val: n };
}

function compileCompatRegex(pattern) {
	let pat = String(pattern || "");
	let flags = "";
	const flagPrefix = pat.match(/^\(\?([ims]+)\)/);
	if (flagPrefix) {
		flags = flagPrefix[1];
		pat = pat.slice(flagPrefix[0].length);
	}
	return new RegExp(pat, flags);
}

function applyRegexCapture(text, pattern, group) {
	if (!pattern) return text;
	try {
		const re = compileCompatRegex(pattern);
		const m = String(text || "").match(re);
		if (!m) return "";
		const idx = group > 0 ? group : (m.length > 1 ? 1 : 0);
		return m[idx] !== undefined ? m[idx] : "";
	} catch (_) {
		return "";
	}
}

function readAttrOrText(el, attrSpec) {
	if (!el) return "";
	const spec = String(attrSpec || "text").trim();
	const parts = spec ? spec.split("|") : ["text"];
	for (const rawPart of parts) {
		const p = rawPart.trim();
		let val = "";
		if (!p || p === "text") {
			val = cleanText(el);
		} else if (el.getAttribute) {
			val = normalizeWS(el.getAttribute(p) || "");
		}
		if (val) return val;
	}
	return "";
}

function evalFieldRule(scopeEl, rule) {
	if (!scopeEl || !rule) return undefined;
	let el = scopeEl;
	if (rule.selector) {
		el = deepQueryFirst(scopeEl, rule.selector);
	}
	if (el && rule.closest && el.closest) {
		el = el.closest(rule.closest);
	}
	let raw = el ? readAttrOrText(el, rule.attr) : "";
	if (raw && rule.regex) {
		raw = applyRegexCapture(raw, rule.regex, rule.group || 0);
	}
	if (!raw && rule.fallback_regex) {
		raw = applyRegexCapture(cleanText(scopeEl), rule.fallback_regex, rule.fallback_group || 0);
	}
	raw = normalizeWS(raw);
	if (!raw) {
		if (rule.optional) return undefined;
		if (rule.parse === "number" || rule.parse === "float") return 0;
		return "";
	}
	if (rule.max_chars && rule.max_chars > 0 && raw.length > rule.max_chars) {
		raw = raw.slice(0, rule.max_chars).trim();
	}
	if (rule.prefix) raw = rule.prefix + raw;
	if (rule.suffix) raw = raw + rule.suffix;

	if (rule.parse === "number") {
		const p = parseCompactFloat(raw);
		return p.ok ? Math.round(p.val) : 0;
	}
	if (rule.parse === "float") {
		const p = parseCompactFloat(raw);
		return p.ok ? p.val : 0;
	}
	return raw;
}

function evalItemRule(root, itemRule) {
	if (!itemRule || !itemRule.container) return [];
	const matches = deepQueryAll(root, itemRule.container);
	const limit = itemRule.limit && itemRule.limit > 0 ? itemRule.limit : 50;
	const seenScopes = new Set();
	const seenKeys = new Set();
	const out = [];

	for (const match of matches) {
		let scope = match;
		if (itemRule.anchor_closest) {
			let anc = match.closest ? match.closest(itemRule.anchor_closest) : null;
			if (!anc) {
				let cur = match;
				while (cur.parentElement && cur.parentElement !== document.body && cur.parentElement.tagName !== "MAIN") {
					const others = deepQueryAll(cur.parentElement, itemRule.container);
					if (others.length > 1) break;
					cur = cur.parentElement;
				}
				anc = cur;
			}
			scope = anc;
		}
		if (!scope || seenScopes.has(scope)) continue;
		seenScopes.add(scope);

		const row = {};
		const fields = itemRule.fields || {};
		for (const [k, fRule] of Object.entries(fields)) {
			const val = evalFieldRule(scope, fRule);
			if (val !== undefined) row[k] = val;
		}
		if (itemRule.dedupe_by) {
			const dk = String(row[itemRule.dedupe_by] ?? "").trim();
			if (dk && seenKeys.has(dk)) continue;
			if (dk) seenKeys.add(dk);
		}
		if (Object.keys(row).length > 0) {
			out.push(row);
			if (out.length >= limit) break;
		}
	}
	return out;
}

function deepQueryAll(root, selector) {
	const parts = selector.split(">>>").map(s => s.trim()).filter(Boolean);
	let scopes = [root];
	for (const part of parts) {
		const next = [];
		const seen = new Set();
		for (const scope of scopes) {
			const rootsToCheck = [scope];
			for (const sub of allShadowElements(scope)) {
				if (sub.shadowRoot) rootsToCheck.push(sub.shadowRoot);
			}
			for (const r of rootsToCheck) {
				if (!r.querySelectorAll) continue;
				try {
					for (const match of r.querySelectorAll(part)) {
						if (!seen.has(match)) {
							seen.add(match);
							next.push(match);
						}
					}
				} catch (_) {}
			}
		}
		scopes = next;
	}
	return scopes;
}

function deepQueryFirst(root, selector) {
	const all = deepQueryAll(root, selector);
	return all.length ? all[0] : null;
}

function collectVisibleText(root, maxChars) {
	const chunks = [];
	let total = 0;
	const walk = (node) => {
		if (!node || total >= maxChars) return;
		if (node.nodeType === 3) {
			const t = normalizeWS(node.nodeValue || "");
			if (t) {
				chunks.push(t);
				total += t.length + 1;
			}
			return;
		}
		if (node.nodeType === 1) {
			const tag = (node.tagName || "").toUpperCase();
			if (tag === "SCRIPT" || tag === "STYLE" || tag === "NOSCRIPT" || tag === "SVG") return;
			if (!isVisible(node)) return;
			if (node.shadowRoot) walk(node.shadowRoot);
		}
		const kids = node.childNodes || [];
		for (let i = 0; i < kids.length && total < maxChars; i++) {
			walk(kids[i]);
		}
	};
	walk(root.body || root);
	return chunks.join(" ").slice(0, maxChars);
}

function extractMetricCards(root) {
	const out = [];
	const seen = new Set();
	const numberLike = /^[$€£+-]?\d[\d,.]*\s*([%kKmMbBhHsS]|min|hrs|views|pts)?$/;

	for (const el of allShadowElements(root)) {
		if (!isVisible(el)) continue;
		const tag = (el.tagName || "").toLowerCase();
		const isCard = tag.includes("metric") || tag.includes("kpi") || tag.includes("stat") ||
			(el.getAttribute && (el.getAttribute("role") === "group" || el.hasAttribute("data-metric")));
		if (!isCard) continue;

		const lines = (el.innerText || "").split(/\n+/).map(s => s.trim()).filter(Boolean);
		if (lines.length >= 2 && lines.length <= 6) {
			const label = lines[0];
			const val = lines.find((ln, idx) => idx > 0 && (numberLike.test(ln) || /\d/.test(ln)));
			if (label && val && label.length <= 80 && val.length <= 60) {
				const key = label + "=" + val;
				if (!seen.has(key)) {
					seen.add(key);
					out.push({ label: label, value: val });
					if (out.length >= 30) break;
				}
			}
		}
	}
	return out;
}

function extractAllTables(root) {
	const tables = [];
	for (const el of allShadowElements(root)) {
		if (!isVisible(el)) continue;
		const tag = (el.tagName || "").toUpperCase();
		const role = el.getAttribute ? (el.getAttribute("role") || "") : "";
		if (tag === "TABLE") {
			const headers = Array.from(el.querySelectorAll("thead th, tr:first-child th")).map(c => cleanText(c)).filter(Boolean);
			const rows = [];
			for (const tr of el.querySelectorAll("tbody tr, tr")) {
				const cells = Array.from(tr.querySelectorAll("td")).map(c => cleanText(c));
				if (cells.some(Boolean)) rows.push(cells);
				if (rows.length >= 100) break;
			}
			if (rows.length > 0) tables.push({ headers: headers, rows: rows });
		} else if (role === "table" || role === "grid" || tag === "YTCP-TABLE") {
			const sub = allShadowElements(el);
			const headers = sub.filter(n => n.getAttribute && n.getAttribute("role") === "columnheader").map(c => cleanText(c)).filter(Boolean);
			const rowEls = sub.filter(n => n.getAttribute && n.getAttribute("role") === "row");
			const rows = [];
			for (const r of rowEls) {
				const rSub = allShadowElements(r);
				const cells = rSub.filter(n => n.getAttribute && (n.getAttribute("role") === "cell" || n.getAttribute("role") === "gridcell")).map(c => cleanText(c));
				if (cells.some(Boolean)) rows.push(cells);
				if (rows.length >= 100) break;
			}
			if (rows.length > 0) tables.push({ headers: headers, rows: rows });
		}
		if (tables.length >= 10) break;
	}
	return tables;
}
`
