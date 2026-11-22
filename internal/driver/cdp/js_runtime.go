package cdp

import (
	"encoding/json"
	"fmt"

	"github.com/tvaroska/deskdrop/internal/contract"
)

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
