package main_test

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
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

	"golang.org/x/net/html"

	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/verify"
)

const e2ePNG1x1Base64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

// mockCDPServer speaks /json/version and RFC 6455 CDP JSON-RPC over loopback so the compiled
// deskdrop binary can run full end-to-end extraction and drift diagnostics without Chrome.
type mockCDPServer struct {
	mu         sync.Mutex
	html       string
	url        string
	step       contract.Step
	nodeBin    string
	usedNodeJS atomic.Bool
	server     *httptest.Server
}

func startMockCDPServer(t *testing.T, step contract.Step, targetURL string, initialHTML string) *mockCDPServer {
	t.Helper()
	nodeBin, _ := exec.LookPath("node")
	m := &mockCDPServer{
		html:    initialHTML,
		url:     targetURL,
		step:    step,
		nodeBin: nodeBin,
	}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json/version":
			wsURL := "ws://" + r.Host + "/devtools/browser"
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(`{"Browser":"MockChrome/144","webSocketDebuggerUrl":%q}`, wsURL)))
		case "/devtools/browser":
			m.serveWS(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockCDPServer) setHTML(html string) {
	m.mu.Lock()
	m.html = html
	m.mu.Unlock()
}

func (m *mockCDPServer) serveWS(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing websocket key", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(h[:])
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n")
	if err := rw.Flush(); err != nil {
		return
	}

	br := rw.Reader
	for {
		frame, rerr := readE2EWSFrame(br)
		if rerr != nil {
			return
		}
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(frame, &req); err != nil {
			continue
		}
		result := m.handleCDPMethod(req.Method, req.Params)
		respBytes, _ := json.Marshal(map[string]any{
			"id":     req.ID,
			"result": result,
		})
		if err := writeE2EWSFrame(conn, respBytes); err != nil {
			return
		}
	}
}

func (m *mockCDPServer) handleCDPMethod(method string, rawParams json.RawMessage) any {
	m.mu.Lock()
	html := m.html
	targetURL := m.url
	step := m.step
	m.mu.Unlock()

	switch method {
	case "Target.getTargets":
		return map[string]any{
			"targetInfos": []map[string]any{
				{"targetId": "tab-1", "type": "page", "url": targetURL, "title": "Status Dashboard"},
			},
		}
	case "Target.createTarget":
		return map[string]any{"targetId": "scratch-1"}
	case "Target.attachToTarget":
		return map[string]any{"sessionId": "sess-1"}
	case "Page.captureScreenshot":
		return map[string]any{"data": e2ePNG1x1Base64}
	case "Accessibility.getFullAXTree":
		return map[string]any{
			"nodes": []map[string]any{
				{"nodeId": "1", "role": map[string]any{"value": "RootWebArea"}, "name": map[string]any{"value": "Status Dashboard"}},
			},
		}
	case "Runtime.evaluate":
		var params struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(rawParams, &params)
		expr := params.Expression
		switch {
		case strings.Contains(expr, "document.readyState"):
			return map[string]any{"result": map[string]any{"value": "complete"}}
		case strings.Contains(expr, "cloneNode(true)") || strings.Contains(expr, "cloneClean"):
			if m.nodeBin != "" {
				val, err := evalExprInNodeDOM(m.nodeBin, html, targetURL, expr)
				if err != nil {
					return map[string]any{"exceptionDetails": map[string]any{"text": err.Error()}}
				}
				return map[string]any{"result": map[string]any{"value": val}}
			}
			if os.Getenv("DESKDROP_REQUIRE_NODE") == "1" {
				return map[string]any{"exceptionDetails": map[string]any{"text": "node required by DESKDROP_REQUIRE_NODE=1"}}
			}
			return map[string]any{"result": map[string]any{"value": contract.ScrubHTML(html)}}
		case strings.Contains(expr, "evalFieldRule"):
			if m.nodeBin != "" {
				val, err := evalExprInNodeDOM(m.nodeBin, html, targetURL, expr)
				if err != nil {
					return map[string]any{"exceptionDetails": map[string]any{"text": err.Error()}}
				}
				m.usedNodeJS.Store(true)
				return map[string]any{"result": map[string]any{"value": val}}
			}
			if os.Getenv("DESKDROP_REQUIRE_NODE") == "1" {
				return map[string]any{"exceptionDetails": map[string]any{"text": "node required by DESKDROP_REQUIRE_NODE=1"}}
			}
			extracted, _ := verify.ExtractStepFromHTML(step, html, targetURL)
			return map[string]any{"result": map[string]any{"value": extracted}}
		case strings.Contains(expr, "location.href"):
			return map[string]any{"result": map[string]any{"value": targetURL}}
		default:
			return map[string]any{"result": map[string]any{"value": true}}
		}
	default:
		return map[string]any{}
	}
}

type e2eDOMAttr struct {
	K string `json:"k"`
	V string `json:"v"`
}

type e2eDOMNode struct {
	Type     int          `json:"t"`
	Tag      string       `json:"g,omitempty"`
	Text     string       `json:"v,omitempty"`
	Attrs    []e2eDOMAttr `json:"a,omitempty"`
	Children []e2eDOMNode `json:"c,omitempty"`
}

func serializeHTMLForNode(rawHTML string) (e2eDOMNode, error) {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return e2eDOMNode{}, err
	}
	var conv func(*html.Node) (e2eDOMNode, bool)
	conv = func(n *html.Node) (e2eDOMNode, bool) {
		switch n.Type {
		case html.DocumentNode:
			out := e2eDOMNode{Type: 9}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if ch, ok := conv(c); ok {
					out.Children = append(out.Children, ch)
				}
			}
			return out, true
		case html.ElementNode:
			out := e2eDOMNode{Type: 1, Tag: strings.ToUpper(n.Data)}
			for _, a := range n.Attr {
				out.Attrs = append(out.Attrs, e2eDOMAttr{K: a.Key, V: a.Val})
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if ch, ok := conv(c); ok {
					out.Children = append(out.Children, ch)
				}
			}
			return out, true
		case html.TextNode:
			return e2eDOMNode{Type: 3, Text: n.Data}, true
		default:
			return e2eDOMNode{}, false
		}
	}
	root, _ := conv(doc)
	return root, nil
}

const e2eNodeDOMShimJS = `
class TextNode {
	constructor(val) { this.nodeType = 3; this.nodeValue = val || ""; this.parentElement = null; }
	get textContent() { return this.nodeValue; }
	get outerHTML() {
		return this.nodeValue.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
	}
}
class ShadowFrag {
	constructor(host, kids) {
		this.nodeType = 11;
		this.host = host;
		this.childNodes = [];
		this.children = [];
		for (const k of kids) this.appendChild(k);
	}
	appendChild(ch) {
		this.childNodes.push(ch);
		if (ch.nodeType === 1) { ch.parentElement = null; this.children.push(ch); }
	}
	querySelectorAll(sel) { return queryDesc(this, sel); }
}
class Elem {
	constructor(tag, attrs = []) {
		this.nodeType = 1;
		this.tagName = String(tag || "").toUpperCase();
		this._attrs = [];
		for (const { k, v } of attrs) this._attrs.push({ name: k, value: String(v) });
		this.childNodes = [];
		this.children = [];
		this.parentElement = null;
		this.shadowRoot = null;
	}
	get hidden() { return this.hasAttribute("hidden"); }
	get disabled() { return this.hasAttribute("disabled"); }
	get attributes() { return this._attrs.map(a => ({ name: a.name, value: a.value })); }
	getAttribute(k) {
		const kl = String(k).toLowerCase();
		const hit = this._attrs.find(a => a.name.toLowerCase() === kl);
		return hit ? hit.value : null;
	}
	hasAttribute(k) { return this.getAttribute(k) !== null; }
	setAttribute(k, v) {
		const kl = String(k).toLowerCase();
		const hit = this._attrs.find(a => a.name.toLowerCase() === kl);
		if (hit) hit.value = String(v);
		else this._attrs.push({ name: k, value: String(v) });
	}
	appendChild(ch) {
		this.childNodes.push(ch);
		if (ch.nodeType === 1) { ch.parentElement = this; this.children.push(ch); }
		return ch;
	}
	get textContent() { return this.childNodes.map(c => c.textContent || "").join(""); }
	get innerText() { return this.textContent; }
	get outerHTML() {
		const tag = this.tagName.toLowerCase();
		const aStr = this._attrs.map(a => ' ' + a.name + '="' + a.value.replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;") + '"').join("");
		const voidTags = new Set(["area","base","br","col","embed","hr","img","input","link","meta","param","source","track","wbr"]);
		if (voidTags.has(tag)) return "<" + tag + aStr + ">";
		return "<" + tag + aStr + ">" + this.childNodes.map(c => c.outerHTML || "").join("") + "</" + tag + ">";
	}
	querySelectorAll(sel) { return queryDesc(this, sel); }
	closest(sel) {
		let cur = this;
		while (cur && cur.nodeType === 1) {
			if (matchSelectorList(cur, sel)) return cur;
			cur = cur.parentElement;
		}
		return null;
	}
}
function splitTop(str, sep) {
	const out = [];
	let start = 0, inB = false, inP = 0, inQ = "";
	for (let i = 0; i < str.length; i++) {
		const c = str[i];
		if (inQ) { if (c === "\\" && i + 1 < str.length) { i++; continue; } if (c === inQ) inQ = ""; continue; }
		if (c === "'" || c === "\"") inQ = c;
		else if (c === "[") inB = true;
		else if (c === "]") inB = false;
		else if (c === "(") inP++;
		else if (c === ")" && inP > 0) inP--;
		else if (c === sep && !inB && inP === 0) { out.push(str.slice(start, i).trim()); start = i + 1; }
	}
	out.push(str.slice(start).trim());
	return out.filter(Boolean);
}
function tokenizeComplex(raw) {
	const s = raw.trim();
	const parts = [];
	let comb = " ", explicit = false, i = 0;
	while (i < s.length) {
		while (i < s.length && /\s/.test(s[i])) i++;
		if (i >= s.length) break;
		if (s[i] === ">" || s[i] === "+" || s[i] === "~") {
			comb = s[i]; explicit = true; i++; continue;
		}
		const start = i;
		let inB = false, inP = 0, inQ = "";
		while (i < s.length) {
			const c = s[i];
			if (inQ) { if (c === "\\" && i + 1 < s.length) { i += 2; continue; } if (c === inQ) inQ = ""; i++; continue; }
			if (c === "'" || c === "\"") inQ = c;
			else if (c === "[") inB = true;
			else if (c === "]") inB = false;
			else if (c === "(") inP++;
			else if (c === ")" && inP > 0) inP--;
			else if (!inB && inP === 0 && (/\s/.test(c) || c === ">" || c === "+" || c === "~")) break;
			i++;
		}
		parts.push({ comb: parts.length === 0 ? "" : (explicit ? comb : " "), tok: s.slice(start, i).trim() });
		comb = " "; explicit = false;
	}
	return parts;
}
function elemIndex(el) {
	if (!el.parentElement) return 0;
	return el.parentElement.children.indexOf(el) + 1;
}
function matchSimple(el, tok) {
	if (!el || el.nodeType !== 1 || !tok) return false;
	let i = 0;
	if (tok[0] === "*") i = 1;
	else if (/[a-zA-Z0-9_-]/.test(tok[0])) {
		while (i < tok.length && /[a-zA-Z0-9_-]/.test(tok[i])) i++;
		if (el.tagName !== tok.slice(0, i).toUpperCase()) return false;
	}
	while (i < tok.length) {
		const c = tok[i];
		if (c === "#") {
			i++; const st = i;
			while (i < tok.length && /[a-zA-Z0-9_-]/.test(tok[i])) i++;
			if ((el.getAttribute("id") || "") !== tok.slice(st, i)) return false;
		} else if (c === ".") {
			i++; const st = i;
			while (i < tok.length && /[a-zA-Z0-9_-]/.test(tok[i])) i++;
			const classes = (el.getAttribute("class") || "").trim().split(/\s+/).filter(Boolean);
			if (!classes.includes(tok.slice(st, i))) return false;
		} else if (c === "[") {
			i++; const st = i; let inQ = "";
			while (i < tok.length) {
				if (inQ) { if (tok[i] === inQ) inQ = ""; i++; continue; }
				if (tok[i] === "'" || tok[i] === "\"") inQ = tok[i];
				else if (tok[i] === "]") break;
				i++;
			}
			const expr = tok.slice(st, i).trim(); i++;
			const m = expr.match(/^([a-zA-Z0-9_:-]+)\s*(?:([*^$]?=)\s*(.*))?$/);
			if (!m) return false;
			const [, attrName, op, rhsRaw] = m;
			if (!el.hasAttribute(attrName)) return false;
			if (op) {
				let rhs = (rhsRaw || "").trim(), ci = false;
				if ((rhs.startsWith("\"") || rhs.startsWith("'"))) {
					const q = rhs[0], end = rhs.indexOf(q, 1);
					const rest = end >= 0 ? rhs.slice(end + 1).trim() : "";
					rhs = end >= 0 ? rhs.slice(1, end) : rhs.slice(1);
					ci = rest.toLowerCase() === "i";
				} else if (/\s+i$/i.test(rhs)) {
					rhs = rhs.replace(/\s+i$/i, "").trim(); ci = true;
				}
				let got = el.getAttribute(attrName) || "";
				if (ci) { got = got.toLowerCase(); rhs = rhs.toLowerCase(); }
				if (op === "=" && got !== rhs) return false;
				if (op === "*=" && !got.includes(rhs)) return false;
				if (op === "^=" && !got.startsWith(rhs)) return false;
				if (op === "$=" && !got.endsWith(rhs)) return false;
			}
		} else if (c === ":") {
			i++; const st = i; let inP = 0, inQ = "";
			while (i < tok.length) {
				if (inQ) { if (tok[i] === inQ) inQ = ""; i++; continue; }
				if (tok[i] === "'" || tok[i] === "\"") inQ = tok[i];
				else if (tok[i] === "(") inP++;
				else if (tok[i] === ")" && inP > 0) inP--;
				else if (inP === 0 && (tok[i] === "." || tok[i] === "#" || tok[i] === "[" || tok[i] === ":")) break;
				i++;
			}
			const ps = tok.slice(st, i);
			const psl = ps.toLowerCase();
			if (psl === "first-child") { if (elemIndex(el) !== 1) return false; }
			else if (psl === "last-child") { if (!el.parentElement || el.parentElement.children[el.parentElement.children.length - 1] !== el) return false; }
			else if (psl.startsWith("nth-child(") && psl.endsWith(")")) {
				const n = parseInt(psl.slice(10, -1).trim(), 10);
				if (elemIndex(el) !== n) return false;
			} else if (psl.startsWith("not(") && ps.endsWith(")")) {
				if (matchSelectorList(el, ps.slice(4, -1))) return false;
			} else return false;
		} else return false;
	}
	return true;
}
function matchChain(el, parts, idx) {
	if (!el || el.nodeType !== 1 || !matchSimple(el, parts[idx].tok)) return false;
	if (idx === 0) return true;
	const comb = parts[idx].comb;
	const p = el.parentElement;
	if (comb === ">") return !!p && matchChain(p, parts, idx - 1);
	if (comb === "+") {
		if (!p) return false;
		const pos = p.children.indexOf(el);
		return pos > 0 && matchChain(p.children[pos - 1], parts, idx - 1);
	}
	if (comb === "~") {
		if (!p) return false;
		const pos = p.children.indexOf(el);
		for (let k = pos - 1; k >= 0; k--) if (matchChain(p.children[k], parts, idx - 1)) return true;
		return false;
	}
	let anc = p;
	while (anc) { if (matchChain(anc, parts, idx - 1)) return true; anc = anc.parentElement; }
	return false;
}
function matchSelectorList(el, rawSel) {
	for (const g of splitTop(String(rawSel || ""), ",")) {
		const parts = tokenizeComplex(g);
		if (parts.length > 0 && matchChain(el, parts, parts.length - 1)) return true;
	}
	return false;
}
function queryDesc(root, sel) {
	const out = [];
	const walk = (n) => {
		for (const ch of (n.children || [])) {
			if (matchSelectorList(ch, sel)) out.push(ch);
			walk(ch);
		}
	};
	walk(root);
	return out;
}
function buildTree(raw) {
	if (raw.t === 3) return new TextNode(raw.v);
	const el = new Elem(raw.g, raw.a || []);
	const shadowWrapKids = [];
	for (const c of (raw.c || [])) {
		if (c.t === 1 && (c.a || []).some(a => a.k.toLowerCase() === "data-shadowroot" && a.v === "open")) {
			for (const sc of (c.c || [])) shadowWrapKids.push(buildTree(sc));
			continue;
		}
		el.appendChild(buildTree(c));
	}
	if (shadowWrapKids.length > 0) el.shadowRoot = new ShadowFrag(el, shadowWrapKids);
	return el;
}
function findFirstTag(n, tag) {
	if (n.nodeType === 1 && n.tagName === tag) return n;
	for (const c of (n.children || [])) { const hit = findFirstTag(c, tag); if (hit) return hit; }
	return null;
}
const rootDocChildren = (__DOM_JSON__.c || []).map(buildTree);
const htmlRoot = rootDocChildren.find(n => n.nodeType === 1 && n.tagName === "HTML") || new Elem("HTML");
const bodyEl = findFirstTag(htmlRoot, "BODY") || htmlRoot;
const titleEl = findFirstTag(htmlRoot, "TITLE");
global.location = { href: __PAGE_URL__ };
global.window = {
	location: global.location,
	getComputedStyle(el) {
		const st = (el && el.getAttribute && el.getAttribute("style")) || "";
		return {
			display: /display\s*:\s*none/i.test(st) ? "none" : "block",
			visibility: /visibility\s*:\s*hidden/i.test(st) ? "hidden" : "visible",
			opacity: /opacity\s*:\s*0(?:\b|;|$)/i.test(st) ? "0" : "1",
		};
	}
};
global.document = {
	nodeType: 9,
	title: titleEl ? titleEl.textContent.trim() : "",
	documentElement: htmlRoot,
	body: bodyEl,
	children: [htmlRoot],
	childNodes: [htmlRoot],
	createElement: (tag) => new Elem(tag),
	createTextNode: (v) => new TextNode(v),
	querySelectorAll: (sel) => htmlRoot.querySelectorAll(sel),
};
`

func evalExprInNodeDOM(nodeBin string, rawHTML string, pageURL string, expr string) (any, error) {
	domTree, err := serializeHTMLForNode(rawHTML)
	if err != nil {
		return nil, err
	}
	domBytes, _ := json.Marshal(domTree)
	urlBytes, _ := json.Marshal(pageURL)
	script := fmt.Sprintf(
		"const __DOM_JSON__ = %s;\nconst __PAGE_URL__ = %s;\n%s\nconst __OUT__ = (%s);\nprocess.stdout.write(JSON.stringify({ value: __OUT__ }));\n",
		string(domBytes),
		string(urlBytes),
		e2eNodeDOMShimJS,
		expr,
	)
	cmd := exec.Command(nodeBin, "-")
	cmd.Stdin = strings.NewReader(script)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return nil, fmt.Errorf("node eval failed: %w (%s)", runErr, string(out))
	}
	var env struct {
		Value any `json:"value"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("decode node eval output %q: %w", string(out), err)
	}
	return env.Value, nil
}

func readE2EWSFrame(br *bufio.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return nil, err
	}
	masked := (hdr[1] & 0x80) != 0
	length := uint64(hdr[1] & 0x7f)
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

func writeE2EWSFrame(conn net.Conn, payload []byte) error {
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

func reserveLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitHealthy(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/desktop/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("deskdrop-server at %s did not become healthy within 10s", baseURL)
}

func e2eAPIRequest(t *testing.T, method, urlStr, token string, body []byte) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, urlStr, r)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, urlStr, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func TestE2ECompiledCLIToRelayLoopback(t *testing.T) {
	repoRoot := findRepoRoot(t)
	tmp := t.TempDir()
	isolatedHome := filepath.Join(tmp, "home")
	if err := os.MkdirAll(isolatedHome, 0o700); err != nil {
		t.Fatal(err)
	}

	// 1. Build real deskdrop CLI and deskdrop-server binaries (or reuse prebuilt binaries in CI Step 2).
	serverBin := filepath.Join(tmp, "deskdrop-server")
	cliBin := filepath.Join(tmp, "deskdrop")
	if prebuiltDir := strings.TrimSpace(os.Getenv("DESKDROP_PREBUILT_BIN_DIR")); prebuiltDir != "" {
		serverBin = filepath.Join(prebuiltDir, "deskdrop-server")
		cliBin = filepath.Join(prebuiltDir, "deskdrop")
	} else {
		for _, target := range []struct {
			out string
			pkg string
		}{
			{serverBin, "./cmd/server"},
			{cliBin, "./cmd/deskdrop"},
		} {
			buildCmd := exec.Command("go", "build", "-o", target.out, target.pkg)
			buildCmd.Dir = repoRoot
			if out, err := buildCmd.CombinedOutput(); err != nil {
				t.Fatalf("go build %s failed: %v\n%s", target.pkg, err, string(out))
			}
		}
	}

	// 2. Load drift benchmark recipe & HTML fixtures (v1 healthy + v2 drifted).
	v1RecipeRaw, err := os.ReadFile(filepath.Join(repoRoot, "testdata", "drift", "status_v1_recipe.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rcp contract.Recipe
	if err := contract.ParseJSONStrict(v1RecipeRaw, &rcp); err != nil {
		t.Fatal(err)
	}
	// Clear wait_selector so the drift fixture immediately runs extraction and produces SELECTOR_DRIFT.
	rcp.Steps[0].WaitSelector = ""
	rcp.Steps[0].TimeoutMs = 5000

	recipesPath := filepath.Join(tmp, "recipes.json")
	rcpBytes, _ := json.MarshalIndent(rcp, "", "  ")
	if err := os.WriteFile(recipesPath, rcpBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	v1HTML, err := os.ReadFile(filepath.Join(repoRoot, "testdata", "drift", "status_v1.html"))
	if err != nil {
		t.Fatal(err)
	}
	v2HTML, err := os.ReadFile(filepath.Join(repoRoot, "testdata", "drift", "status_v2_drifted.html"))
	if err != nil {
		t.Fatal(err)
	}

	mockCDP := startMockCDPServer(t, rcp.Steps[0], "https://status.example.com/overview", string(v1HTML))

	statePath := filepath.Join(tmp, "state.json")
	blobDir := filepath.Join(tmp, "blobs")

	startRelay := func(addr string) (*exec.Cmd, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(ctx, serverBin)
		cmd.Env = append(os.Environ(),
			"HOME="+isolatedHome,
			"LISTEN_ADDR="+addr,
			"STORE_BACKEND=file",
			"STATE_FILE="+statePath,
			"STORAGE_BACKEND=local",
			"LOCAL_BLOB_DIR="+blobDir,
			"RECIPES_URI="+recipesPath,
			"HEALER_ENABLED=false",
			"DESKTOP_CLIENT_TOKEN=e2e-desk-tok",
			"AGENT_BUS_TOKEN=e2e-agent-tok",
			"ADMIN_TOKEN=e2e-admin-tok",
		)
		var logs bytes.Buffer
		cmd.Stdout = &logs
		cmd.Stderr = &logs
		if err := cmd.Start(); err != nil {
			cancel()
			t.Fatalf("start deskdrop-server: %v", err)
		}
		t.Cleanup(func() {
			cancel()
			_ = cmd.Wait()
		})
		waitHealthy(t, "http://"+addr)
		return cmd, cancel
	}

	addr := reserveLoopbackAddr(t)
	srvCmd, stopRelay := startRelay(addr)
	relayBase := "http://" + addr + "/desktop"

	cfgPath := filepath.Join(tmp, "deskdrop.json")
	cfgJSON, _ := json.Marshal(map[string]any{
		"from":          "e2e-worker",
		"service_url":   relayBase,
		"service_token": "e2e-desk-tok",
		"cdp_url":       mockCDP.server.URL,
		"debug_dir":     filepath.Join(tmp, "debug"),
	})
	if err := os.WriteFile(cfgPath, cfgJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	// 3. Happy-path E2E: enqueue -> compiled `deskdrop sync --once` -> claim -> drop -> bus message.
	code, enqRaw := e2eAPIRequest(t, http.MethodPost, relayBase+"/v1/tasks", "e2e-agent-tok",
		[]byte(`{"recipe":"service-status-board","requested_by":"e2e-agent","notify_to":"e2e-agent"}`))
	if code != http.StatusCreated {
		t.Fatalf("enqueue task 1 = %d (%s)", code, string(enqRaw))
	}
	var enq1 struct {
		Task contract.Task `json:"task"`
	}
	_ = json.Unmarshal(enqRaw, &enq1)

	syncCmd1 := exec.Command(cliBin, "sync", "--config", cfgPath)
	syncCmd1.Env = append(os.Environ(), "HOME="+isolatedHome)
	if out, err := syncCmd1.CombinedOutput(); err != nil {
		t.Fatalf("deskdrop sync (v1) failed: %v\n%s", err, string(out))
	}

	code, taskRaw := e2eAPIRequest(t, http.MethodGet, relayBase+"/v1/tasks/"+enq1.Task.ID, "e2e-agent-tok", nil)
	if code != http.StatusOK {
		t.Fatalf("GET task 1 = %d (%s)", code, string(taskRaw))
	}
	var task1 contract.Task
	_ = json.Unmarshal(taskRaw, &task1)
	if task1.Status != contract.TaskStatusSucceeded || task1.DropID == "" {
		t.Fatalf("expected task 1 succeeded with drop_id, got %+v", task1)
	}

	code, dropRaw := e2eAPIRequest(t, http.MethodGet, relayBase+"/v1/drops/"+task1.DropID, "e2e-agent-tok", nil)
	if code != http.StatusOK {
		t.Fatalf("GET drop 1 = %d (%s)", code, string(dropRaw))
	}
	var drop1 contract.DropRecord
	_ = json.Unmarshal(dropRaw, &drop1)
	if len(drop1.Report.Steps) != 1 || len(drop1.Report.Steps[0].Items) != 3 {
		t.Fatalf("expected 3 extracted items in drop 1, got %+v", drop1.Report.Steps)
	}
	if (mockCDP.nodeBin != "" || os.Getenv("DESKDROP_REQUIRE_NODE") == "1") && !mockCDP.usedNodeJS.Load() {
		t.Fatal("expected mockCDPServer to execute real generated JS runtime via node")
	}
	if drop1.Report.Steps[0].Fields["overall_status"] != "All Systems Operational" || drop1.Report.Steps[0].Fields["active_incidents"] != float64(0) {
		t.Fatalf("unexpected drop 1 JS-extracted fields: %+v", drop1.Report.Steps[0].Fields)
	}
	if drop1.Report.Steps[0].Items[0]["uptime_pct"] != 99.98 {
		t.Fatalf("unexpected drop 1 item[0] uptime_pct: %+v", drop1.Report.Steps[0].Items[0])
	}

	code, msgRaw := e2eAPIRequest(t, http.MethodGet, relayBase+"/v1/messages?for=e2e-agent", "e2e-agent-tok", nil)
	if code != http.StatusOK || !strings.Contains(string(msgRaw), "service-status-board") {
		t.Fatalf("expected bus drop message for e2e-agent, got %d %s", code, string(msgRaw))
	}

	// 4. Drift E2E: switch mock CDP to drifted v2 HTML -> compiled CLI uploads diagnostic bundle -> failed_diagnostic + bus alert.
	mockCDP.setHTML(string(v2HTML))
	code, enq2Raw := e2eAPIRequest(t, http.MethodPost, relayBase+"/v1/tasks", "e2e-agent-tok",
		[]byte(`{"recipe":"service-status-board","requested_by":"e2e-agent","notify_to":"e2e-agent"}`))
	if code != http.StatusCreated {
		t.Fatalf("enqueue task 2 = %d (%s)", code, string(enq2Raw))
	}
	var enq2 struct {
		Task contract.Task `json:"task"`
	}
	_ = json.Unmarshal(enq2Raw, &enq2)

	syncCmd2 := exec.Command(cliBin, "sync", "--config", cfgPath)
	syncCmd2.Env = append(os.Environ(), "HOME="+isolatedHome)
	if out, err := syncCmd2.CombinedOutput(); err != nil {
		t.Fatalf("deskdrop sync (v2 drift) failed: %v\n%s", err, string(out))
	}

	code, task2Raw := e2eAPIRequest(t, http.MethodGet, relayBase+"/v1/tasks/"+enq2.Task.ID, "e2e-agent-tok", nil)
	if code != http.StatusOK {
		t.Fatalf("GET task 2 = %d (%s)", code, string(task2Raw))
	}
	var task2 contract.Task
	_ = json.Unmarshal(task2Raw, &task2)
	if task2.Status != contract.TaskStatusFailedDiagnostic || task2.DiagnosticID == "" {
		t.Fatalf("expected task 2 failed_diagnostic with diagnostic_id, got %+v", task2)
	}

	code, zipBytes := e2eAPIRequest(t, http.MethodGet, relayBase+"/v1/diagnostics/"+task2.DiagnosticID+"/bundle.zip", "e2e-agent-tok", nil)
	if code != http.StatusOK || len(zipBytes) < 4 || string(zipBytes[:2]) != "PK" {
		t.Fatalf("GET diagnostic zip = %d len=%d, want 200 PK zip", code, len(zipBytes))
	}
	zr, zerr := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if zerr != nil {
		t.Fatalf("invalid diagnostic zip: %v", zerr)
	}
	foundV2DOM := false
	for _, zf := range zr.File {
		if zf.Name == "dom.html" {
			rc, _ := zf.Open()
			domRaw, _ := io.ReadAll(rc)
			_ = rc.Close()
			foundV2DOM = strings.Contains(string(domRaw), "service-grid-v2")
		}
	}
	if !foundV2DOM {
		t.Fatal("expected diagnostic zip dom.html to contain scrubbed service-grid-v2 markup")
	}

	// 5. FileStore restart persistence: stop relay, start fresh deskdrop-server on same state.json, verify drop + diagnostic + tasks survive.
	stopRelay()
	_ = srvCmd.Wait()

	addr2 := reserveLoopbackAddr(t)
	_, _ = startRelay(addr2)
	relayBase2 := "http://" + addr2 + "/desktop"

	code, persistedDrop := e2eAPIRequest(t, http.MethodGet, relayBase2+"/v1/drops/"+task1.DropID, "e2e-agent-tok", nil)
	if code != http.StatusOK || !strings.Contains(string(persistedDrop), "service-status-board") {
		t.Fatalf("persisted drop missing after restart: %d %s", code, string(persistedDrop))
	}
	code, persistedZip := e2eAPIRequest(t, http.MethodGet, relayBase2+"/v1/diagnostics/"+task2.DiagnosticID+"/bundle.zip", "e2e-agent-tok", nil)
	if code != http.StatusOK || len(persistedZip) != len(zipBytes) {
		t.Fatalf("persisted diagnostic zip missing after restart: %d len=%d (want %d)", code, len(persistedZip), len(zipBytes))
	}
}
