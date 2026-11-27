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
			const val = evalFieldRule(document.body || document.documentElement, rule, true);
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
		const children = cur.children || cur.childNodes || [];
		for (let i = children.length - 1; i >= 0; i--) {
			if (children[i].nodeType === 1 || children[i].nodeType === 11) {
				stack.push(children[i]);
			}
		}
		if (cur.shadowRoot) stack.push(cur.shadowRoot);
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

function shouldInsertBoundarySpace(prev, next) {
	if (!prev || !next) return false;
	const lastCh = prev[prev.length - 1];
	const firstCh = next[0];
	if (/\s/.test(lastCh) || /\s/.test(firstCh)) return false;
	if ((lastCh === "," || lastCh === ".") && /\d/.test(firstCh)) return false;
	if (/\d/.test(lastCh) && (firstCh === "," || firstCh === "." || firstCh === "%")) return false;
	if ((lastCh === "$" || lastCh === "\u20ac" || lastCh === "\u00a3" || lastCh === "+" || lastCh === "-") && /\d/.test(firstCh)) return false;
	if (/^[,.;:!?%)\]]$/.test(firstCh)) return false;
	if (lastCh === "(" || lastCh === "[") return false;
	return true;
}

function cleanText(el) {
	if (!el) return "";
	if (el.childNodes && el.childNodes.length > 0) {
		let buf = "";
		let pendingSpace = false;
		const skipTags = { SCRIPT: 1, STYLE: 1, SVG: 1, NOSCRIPT: 1 };
		const walk = (cur) => {
			if (!cur) return;
			if (cur.nodeType === 3) {
				const txt = String(cur.nodeValue || "");
				if (!txt) return;
				if (pendingSpace && buf.length > 0) {
					if (shouldInsertBoundarySpace(buf, txt)) buf += " ";
					pendingSpace = false;
				}
				buf += txt;
				return;
			}
			if (cur.nodeType === 1) {
				const tag = (cur.tagName || "").toUpperCase();
				if (skipTags[tag] || cur.hidden) return;
				if (typeof window !== "undefined" && window.getComputedStyle) {
					try {
						const st = window.getComputedStyle(cur);
						if (st && (st.display === "none" || st.visibility === "hidden")) return;
					} catch (_) {}
				}
				if (buf.length > 0) pendingSpace = true;
				if (cur.shadowRoot) {
					for (const sc of (cur.shadowRoot.childNodes || [])) walk(sc);
				}
				for (const ch of (cur.childNodes || [])) walk(ch);
				if (buf.length > 0) pendingSpace = true;
				return;
			}
			if (cur.childNodes) {
				for (const ch of cur.childNodes) walk(ch);
			}
		};
		walk(el);
		return normalizeWS(buf);
	}
	const raw = (el.innerText !== undefined && el.innerText !== "") ? el.innerText : (el.textContent || "");
	return normalizeWS(raw);
}

function parseCompactFloat(raw) {
	const cleaned = normalizeWS(raw).replace(/,/g, "");
	const m = cleaned.match(/([+-]?(?:\d+(?:\.\d+)?|\.\d+))(?:\s*([KkMmBb])\b)?/);
	if (!m) return { ok: false, val: 0 };
	let n = parseFloat(m[1]);
	if (!Number.isFinite(n)) return { ok: false, val: 0 };
	const suf = (m[2] || "").toUpperCase();
	if (suf === "K") n *= 1000;
	else if (suf === "M") n *= 1000000;
	else if (suf === "B") n *= 1000000000;
	if (!Number.isFinite(n)) return { ok: false, val: 0 };
	return { ok: true, val: n };
}

function compileCompatRegex(pattern) {
	let pat = String(pattern || "");
	const flagSet = new Set();
	let ungreedy = false;
	pat = pat.replace(/\(\?([imsU]+)\)/g, (_, fl) => {
		for (const ch of fl) {
			if (ch === "i" || ch === "m" || ch === "s") flagSet.add(ch);
			if (ch === "U") ungreedy = true;
		}
		return "";
	});
	const multiline = flagSet.has("m");
	let translated = "";
	let inClass = false;
	for (let i = 0; i < pat.length; i++) {
		const ch = pat[i];
		if (ch === "\\" && i + 1 < pat.length) {
			const nxt = pat[i + 1];
			if (!inClass && !multiline && nxt === "A") {
				translated += "^";
				i++;
				continue;
			}
			if (!inClass && !multiline && nxt === "z") {
				translated += "$";
				i++;
				continue;
			}
			translated += ch + nxt;
			i++;
			continue;
		}
		if (ch === "[") inClass = true;
		else if (ch === "]" && inClass) inClass = false;
		if (!inClass && ungreedy && (ch === "*" || ch === "+" || ch === "?")) {
			const prev = translated.length > 0 ? translated[translated.length - 1] : "";
			if (prev !== "(" && pat[i + 1] !== "?") {
				translated += ch + "?";
				continue;
			}
		}
		translated += ch;
	}
	const flags = Array.from(flagSet).join("");
	try {
		return new RegExp(translated, flags);
	} catch (_) {
		const downgraded = translated.replace(/\(\?([ims]+):/g, (_, fl) => {
			for (const ch of fl) flagSet.add(ch);
			return "(?:";
		});
		return new RegExp(downgraded, Array.from(flagSet).join(""));
	}
}

function applyRegexCapture(text, pattern, group) {
	if (!pattern) return text;
	try {
		const re = compileCompatRegex(pattern);
		const m = String(text || "").match(re);
		if (!m) return "";
		if (group > 0) {
			return m[group] !== undefined ? m[group] : "";
		}
		if (m.length === 1) return m[0];
		for (let i = 1; i < m.length; i++) {
			if (m[i] !== undefined && m[i] !== "") return m[i];
		}
		return "";
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

function safeClosest(el, selector) {
	if (!el || !selector || !el.closest) return null;
	try {
		return el.closest(selector);
	} catch (_) {
		return null;
	}
}

function evalFieldRule(scopeEl, rule, omitUnmatchedNumeric) {
	if (!scopeEl || !rule) return undefined;
	let el = scopeEl;
	if (rule.selector) {
		el = deepQueryFirst(scopeEl, rule.selector);
	}
	if (el && rule.closest) {
		el = safeClosest(el, rule.closest);
	}
	let raw = el ? readAttrOrText(el, rule.attr) : "";
	if (raw && rule.regex) {
		raw = applyRegexCapture(raw, rule.regex, rule.group || 0);
	}
	raw = normalizeWS(raw);
	if (rule.fallback_regex) {
		let needFallback = !raw;
		if (!needFallback && (rule.parse === "number" || rule.parse === "float")) {
			needFallback = !parseCompactFloat(raw).ok;
		}
		if (needFallback) {
			raw = normalizeWS(applyRegexCapture(cleanText(scopeEl), rule.fallback_regex, rule.fallback_group || 0));
		}
	}
	if (!raw) {
		if (rule.optional) return undefined;
		if (rule.parse === "number" || rule.parse === "float") {
			return omitUnmatchedNumeric ? undefined : 0;
		}
		return "";
	}
	if (rule.max_chars && rule.max_chars > 0) {
		const runes = Array.from(raw);
		if (runes.length > rule.max_chars) {
			raw = runes.slice(0, rule.max_chars).join("").trim();
		}
	}
	if (rule.prefix) raw = rule.prefix + raw;
	if (rule.suffix) raw = raw + rule.suffix;

	if (rule.parse === "number") {
		const p = parseCompactFloat(raw);
		if (p.ok) {
			const r = Math.round(p.val);
			if (Number.isFinite(r) && r >= -9223372036854775808 && r < 9223372036854775808) {
				return r;
			}
		}
		return omitUnmatchedNumeric ? undefined : 0;
	}
	if (rule.parse === "float") {
		const p = parseCompactFloat(raw);
		if (p.ok) return p.val;
		return omitUnmatchedNumeric ? undefined : 0;
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
			let anc = safeClosest(match, itemRule.anchor_closest);
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

function splitCSSTopLevel(str, sep) {
	const s = String(str || "");
	const out = [];
	let start = 0;
	let inBracket = false;
	let inParen = 0;
	let inQuote = "";
	for (let i = 0; i < s.length; i++) {
		const c = s[i];
		if (inQuote) {
			if (c === "\\" && i + 1 < s.length) {
				i++;
				continue;
			}
			if (c === inQuote) inQuote = "";
			continue;
		}
		if (c === "'" || c === "\"") inQuote = c;
		else if (c === "[") inBracket = true;
		else if (c === "]") inBracket = false;
		else if (c === "(") inParen++;
		else if (c === ")" && inParen > 0) inParen--;
		else if (c === sep && !inBracket && inParen === 0) {
			out.push(s.slice(start, i).trim());
			start = i + 1;
		}
	}
	out.push(s.slice(start).trim());
	return out;
}

function splitShadowHops(group) {
	const s = String(group || "");
	const out = [];
	let start = 0;
	let inBracket = false;
	let inParen = 0;
	let inQuote = "";
	for (let i = 0; i < s.length; i++) {
		const c = s[i];
		if (inQuote) {
			if (c === "\\" && i + 1 < s.length) {
				i++;
				continue;
			}
			if (c === inQuote) inQuote = "";
			continue;
		}
		if (c === "'" || c === "\"") inQuote = c;
		else if (c === "[") inBracket = true;
		else if (c === "]") inBracket = false;
		else if (c === "(") inParen++;
		else if (c === ")" && inParen > 0) inParen--;
		else if (!inBracket && inParen === 0 && c === ">" && s[i + 1] === ">" && s[i + 2] === ">") {
			const part = s.slice(start, i).trim();
			if (part) out.push(part);
			i += 2;
			start = i + 1;
		}
	}
	const tail = s.slice(start).trim();
	if (tail) out.push(tail);
	return out;
}

function deepQueryGroup(root, group) {
	const parts = splitShadowHops(group);
	if (!parts.length) return [];
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

function deepQueryAll(root, selector) {
	const groups = splitCSSTopLevel(selector, ",").filter(Boolean);
	if (groups.length === 0) return [];
	const matched = new Set();
	for (const g of groups) {
		for (const m of deepQueryGroup(root, g)) {
			matched.add(m);
		}
	}
	if (matched.size <= 1) return Array.from(matched);
	const ordered = [];
	for (const el of allShadowElements(root)) {
		if (matched.has(el)) {
			ordered.push(el);
			matched.delete(el);
		}
	}
	for (const rem of matched) ordered.push(rem);
	return ordered;
}

function deepQueryFirst(root, selector) {
	const all = deepQueryAll(root, selector);
	return all.length ? all[0] : null;
}

const blockBoundaryTags = {
	DIV: 1, P: 1, LI: 1, UL: 1, OL: 1, DL: 1, DT: 1, DD: 1,
	TR: 1, TD: 1, TH: 1, TABLE: 1, THEAD: 1, TBODY: 1, TFOOT: 1,
	H1: 1, H2: 1, H3: 1, H4: 1, H5: 1, H6: 1,
	SECTION: 1, ARTICLE: 1, HEADER: 1, FOOTER: 1, NAV: 1, ASIDE: 1, MAIN: 1,
	BR: 1, HR: 1, BLOCKQUOTE: 1, PRE: 1, BUTTON: 1, A: 1, LABEL: 1, SUMMARY: 1, FIGCAPTION: 1
};

function collectVisibleText(root, maxChars) {
	const start = (root && root.body) ? root.body : root;
	if (!start || maxChars <= 0) return "";
	let buf = "";
	let pendingSpace = false;
	const skipTags = { SCRIPT: 1, STYLE: 1, NOSCRIPT: 1, SVG: 1 };
	const walk = (node) => {
		if (!node || buf.length >= maxChars * 4) return;
		if (node.nodeType === 3) {
			const txt = String(node.nodeValue || "");
			if (!txt) return;
			if (pendingSpace && buf.length > 0) {
				if (shouldInsertBoundarySpace(buf, txt)) buf += " ";
				pendingSpace = false;
			}
			buf += txt;
			return;
		}
		if (node.nodeType === 1) {
			const tag = (node.tagName || "").toUpperCase();
			if (skipTags[tag] || node.hidden) return;
			if (typeof window !== "undefined" && window.getComputedStyle) {
				try {
					const st = window.getComputedStyle(node);
					if (st && (st.display === "none" || st.visibility === "hidden" || st.opacity === "0")) return;
				} catch (_) {}
			}
			if (blockBoundaryTags[tag] && buf.length > 0) {
				pendingSpace = true;
			}
			if (node.shadowRoot) {
				for (const sc of (node.shadowRoot.childNodes || [])) walk(sc);
			}
			for (const ch of (node.childNodes || [])) walk(ch);
			if (buf.length > 0) {
				pendingSpace = true;
			}
			return;
		}
		for (const ch of (node.childNodes || [])) walk(ch);
	};
	walk(start);
	const norm = normalizeWS(buf);
	const runes = Array.from(norm);
	if (runes.length <= maxChars) return norm;
	return runes.slice(0, maxChars).join("").trim();
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
