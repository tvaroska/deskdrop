package contract

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type semNode struct {
	raw        AXNode
	role       string
	name       string
	autoID     string
	className  string
	desc       string
	value      string
	isPassword bool
	props      map[string]string
	parent     *semNode
	children   []*semNode
}

// ExtractStepFromAXNodes evaluates a declarative Step over a serialized CDP or Windows UIA
// semantic accessibility tree in pure Go stdlib (zero external dependencies), enforcing
// password and editable-control value redaction before evaluation.
func ExtractStepFromAXNodes(step Step, rawNodes []AXNode, landedURI string) (StepResult, error) {
	scrubbed := ScrubAXNodes(rawNodes)
	doc := buildSemanticTree(scrubbed)

	title := ""
	for _, ch := range doc.children {
		if ch.name != "" {
			title = ch.name
			break
		}
	}

	var selectorsOut map[string]string
	if len(step.Selectors) > 0 {
		selectorsOut = make(map[string]string, len(step.Selectors))
		for k, sel := range step.Selectors {
			matches := querySemAll(doc, sel)
			var chunks []string
			for _, m := range matches {
				if txt := semNodeText(m); txt != "" {
					chunks = append(chunks, txt)
				}
			}
			selectorsOut[k] = strings.Join(chunks, " | ")
		}
	}

	var fieldsOut map[string]any
	if len(step.Fields) > 0 {
		fieldsOut = make(map[string]any, len(step.Fields))
		for k, rule := range step.Fields {
			if val, ok := evalSemFieldRule(doc, rule); ok {
				fieldsOut[k] = val
			}
		}
	}

	var itemsOut []map[string]any
	if step.Items != nil {
		itemsOut = evalSemItemRule(doc, *step.Items)
	}

	headings := extractSemHeadings(doc)
	var metrics []MetricPair
	if step.WantCards() {
		metrics = extractSemMetricCards(doc)
	}
	var tables []TableData
	if step.WantTables() {
		tables = extractSemTables(doc)
	}

	maxText := step.MaxTextChars
	if maxText <= 0 {
		maxText = 1200
	}
	excerpt := truncateSemRunes(semNodeText(doc), maxText)

	res := StepResult{
		StepID:      step.ID,
		Title:       title,
		URL:         landedURI,
		Headings:    headings,
		Metrics:     metrics,
		Selectors:   selectorsOut,
		Fields:      fieldsOut,
		Items:       itemsOut,
		Tables:      tables,
		TextExcerpt: excerpt,
	}
	if len(res.Fields) == 0 {
		res.Fields = nil
	}
	if len(res.Items) == 0 {
		res.Items = nil
	}
	return res, nil
}

func buildSemanticTree(nodes []AXNode) *semNode {
	doc := &semNode{role: "document", props: make(map[string]string)}
	if len(nodes) == 0 {
		return doc
	}

	byID := make(map[string]*AXNode, len(nodes))
	hasParent := make(map[string]bool, len(nodes))
	order := make([]string, 0, len(nodes))

	for i := range nodes {
		n := &nodes[i]
		if n.NodeID == "" {
			continue
		}
		if _, exists := byID[n.NodeID]; !exists {
			order = append(order, n.NodeID)
		}
		byID[n.NodeID] = n
	}
	for _, id := range order {
		n := byID[id]
		for _, cid := range n.ChildIDs {
			if _, ok := byID[cid]; ok {
				hasParent[cid] = true
			}
		}
		if n.ParentID != "" {
			if _, ok := byID[n.ParentID]; ok {
				hasParent[id] = true
			}
		}
	}

	childrenByParent := make(map[string][]string, len(nodes))
	for _, id := range order {
		n := byID[id]
		if len(n.ChildIDs) > 0 {
			for _, cid := range n.ChildIDs {
				if _, ok := byID[cid]; ok {
					childrenByParent[id] = append(childrenByParent[id], cid)
				}
			}
		}
	}
	for _, id := range order {
		n := byID[id]
		if n.ParentID != "" {
			if _, ok := byID[n.ParentID]; ok && !containsString(childrenByParent[n.ParentID], id) {
				childrenByParent[n.ParentID] = append(childrenByParent[n.ParentID], id)
			}
		}
	}

	var roots []string
	for _, id := range order {
		if !hasParent[id] {
			roots = append(roots, id)
		}
	}
	if len(roots) == 0 && len(order) > 0 {
		roots = append(roots, order[0])
	}

	visited := make(map[string]bool, len(nodes))
	var attach func(id string, parent *semNode)
	attach = func(id string, parent *semNode) {
		if visited[id] {
			return
		}
		visited[id] = true
		raw, ok := byID[id]
		if !ok {
			return
		}
		roleLower := NormalizeAXRole(raw.Role.StringValue())
		if roleLower == "inlinetextbox" {
			return
		}
		if raw.Ignored {
			for _, cid := range childrenByParent[id] {
				attach(cid, parent)
			}
			return
		}

		props := make(map[string]string, len(raw.Properties))
		for _, p := range raw.Properties {
			pk := strings.ToLower(strings.TrimSpace(p.Name))
			if pk != "" {
				props[pk] = p.Value.StringValue()
			}
		}
		autoID := NormalizeWhitespace(raw.AutomationID)
		if autoID == "" {
			if v := props["automation_id"]; v != "" {
				autoID = v
			} else if v := props["automationid"]; v != "" {
				autoID = v
			}
		}
		cls := NormalizeWhitespace(raw.ClassName)
		if cls == "" {
			if v := props["class_name"]; v != "" {
				cls = v
			} else if v := props["classname"]; v != "" {
				cls = v
			}
		}

		sn := &semNode{
			raw:        *raw,
			role:       roleLower,
			name:       raw.Name.StringValue(),
			autoID:     autoID,
			className:  cls,
			desc:       raw.Description.StringValue(),
			value:      raw.Value.StringValue(),
			isPassword: raw.IsPassword || roleLower == "password",
			props:      props,
			parent:     parent,
		}
		parent.children = append(parent.children, sn)
		for _, cid := range childrenByParent[id] {
			attach(cid, sn)
		}
	}

	for _, rid := range roots {
		attach(rid, doc)
	}
	return doc
}

func evalSemFieldRule(scope *semNode, rule FieldRule) (any, bool) {
	if scope == nil {
		return nil, false
	}
	el := scope
	if rule.Selector != "" {
		el = querySemFirst(scope, rule.Selector)
	}
	if el != nil && rule.Closest != "" {
		el = closestSem(el, rule.Closest)
	}

	raw := ""
	if el != nil {
		raw = readSemAttrOrText(el, rule.Attr)
	}
	if raw != "" && rule.Regex != "" {
		raw = applySemRegex(raw, rule.Regex, rule.Group)
	}
	raw = NormalizeWhitespace(raw)
	if rule.FallbackRegex != "" {
		needFallback := raw == ""
		if !needFallback && (rule.Parse == "number" || rule.Parse == "float") {
			_, ok := ParseCompactFloat(raw)
			needFallback = !ok
		}
		if needFallback {
			raw = NormalizeWhitespace(applySemRegex(semNodeText(scope), rule.FallbackRegex, rule.FallbackGroup))
		}
	}
	if raw == "" {
		if rule.Optional {
			return nil, false
		}
		if rule.Parse == "number" {
			return int64(0), true
		}
		if rule.Parse == "float" {
			return float64(0), true
		}
		return "", true
	}
	if rule.MaxChars > 0 {
		raw = truncateSemRunes(raw, rule.MaxChars)
	}
	if rule.Prefix != "" {
		raw = rule.Prefix + raw
	}
	if rule.Suffix != "" {
		raw = raw + rule.Suffix
	}

	switch rule.Parse {
	case "number":
		if n, ok := ParseCompactNumber(raw); ok {
			return n, true
		}
		return int64(0), true
	case "float":
		if f, ok := ParseCompactFloat(raw); ok {
			return f, true
		}
		return float64(0), true
	default:
		return raw, true
	}
}

func evalSemItemRule(doc *semNode, rule ItemRule) []map[string]any {
	matches := querySemAll(doc, rule.Container)
	limit := rule.Limit
	if limit <= 0 {
		limit = 50
	}

	keys := make([]string, 0, len(rule.Fields))
	for k := range rule.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	seenScopes := make(map[*semNode]bool)
	seenKeys := make(map[string]bool)
	var out []map[string]any

	for _, match := range matches {
		scope := match
		if rule.AnchorClosest != "" {
			anc := closestSem(match, rule.AnchorClosest)
			if anc == nil {
				cur := match
				for cur.parent != nil && cur.parent != doc {
					if len(querySemAll(cur.parent, rule.Container)) > 1 {
						break
					}
					cur = cur.parent
				}
				anc = cur
			}
			scope = anc
		}
		if scope == nil || seenScopes[scope] {
			continue
		}
		seenScopes[scope] = true

		row := make(map[string]any, len(rule.Fields))
		for _, k := range keys {
			if val, ok := evalSemFieldRule(scope, rule.Fields[k]); ok {
				row[k] = val
			}
		}
		if rule.DedupeBy != "" {
			if rawKey, ok := row[rule.DedupeBy]; ok {
				dk := strings.TrimSpace(fmt.Sprint(rawKey))
				if dk != "" {
					if seenKeys[dk] {
						continue
					}
					seenKeys[dk] = true
				}
			}
		}
		if len(row) > 0 {
			out = append(out, row)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

func readSemAttrOrText(n *semNode, attrSpec string) string {
	spec := strings.TrimSpace(attrSpec)
	if spec == "" {
		spec = "text"
	}
	for _, part := range strings.Split(spec, "|") {
		p := strings.TrimSpace(part)
		var val string
		if p == "" || strings.EqualFold(p, "text") {
			val = semNodeText(n)
		} else {
			v, _ := lookupSemAttr(n, p)
			val = NormalizeWhitespace(v)
		}
		if val != "" {
			return val
		}
	}
	return ""
}

func lookupSemAttr(n *semNode, rawName string) (string, bool) {
	if n == nil {
		return "", false
	}
	k := strings.ToLower(strings.TrimSpace(rawName))
	switch k {
	case "role":
		return n.role, n.role != ""
	case "name", "aria-label", "label", "title":
		return n.name, n.name != ""
	case "automation_id", "automationid", "id":
		if n.autoID != "" {
			return n.autoID, true
		}
		return n.raw.NodeID, n.raw.NodeID != ""
	case "class_name", "classname", "class":
		return n.className, n.className != ""
	case "description", "help_text":
		return n.desc, n.desc != ""
	case "value":
		return n.value, n.value != ""
	default:
		v, ok := n.props[k]
		return v, ok
	}
}

func semNodeText(n *semNode) string {
	if n == nil {
		return ""
	}
	var chunks []string
	var walk func(cur *semNode, parentName string)
	walk = func(cur *semNode, parentName string) {
		if cur == nil || cur.isPassword {
			return
		}
		if isSensitiveInputRole(cur.role) {
			// Expose accessible label if present, never editable input Value.
			if cur.name != "" && !strings.EqualFold(cur.name, parentName) {
				chunks = append(chunks, cur.name)
			}
			return
		}
		nextParent := parentName
		if cur.name != "" && cur.name != "[REDACTED]" && cur.role != "document" && cur.role != "rootwebarea" && cur.role != "webarea" {
			if !strings.EqualFold(cur.name, parentName) {
				chunks = append(chunks, cur.name)
			}
			nextParent = cur.name
		}
		if cur.value != "" && cur.value != "[REDACTED]" && !strings.EqualFold(cur.value, cur.name) {
			chunks = append(chunks, cur.value)
		}
		for _, ch := range cur.children {
			walk(ch, nextParent)
		}
	}
	walk(n, "")
	return NormalizeWhitespace(strings.Join(chunks, " "))
}

func extractSemHeadings(doc *semNode) []string {
	var out []string
	seen := make(map[string]bool)
	var walk func(*semNode)
	walk = func(n *semNode) {
		if len(out) >= 20 {
			return
		}
		if n.role == "heading" || n.role == "titlebar" {
			t := semNodeText(n)
			if t != "" && len([]rune(t)) <= 140 && !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
		for _, ch := range n.children {
			walk(ch)
		}
	}
	walk(doc)
	return out
}

var semMetricDigitRe = regexp.MustCompile(`\d`)

func extractSemMetricCards(doc *semNode) []MetricPair {
	var out []MetricPair
	seen := make(map[string]bool)
	var walk func(*semNode)
	walk = func(n *semNode) {
		if len(out) >= 30 {
			return
		}
		if n.role == "group" && len(n.children) >= 2 && len(n.children) <= 6 {
			lbl := semNodeText(n.children[0])
			val := ""
			for _, ch := range n.children[1:] {
				t := semNodeText(ch)
				if semMetricDigitRe.MatchString(t) {
					val = t
					break
				}
			}
			if lbl != "" && val != "" && len(lbl) <= 80 && len(val) <= 60 {
				k := lbl + "=" + val
				if !seen[k] {
					seen[k] = true
					out = append(out, MetricPair{Label: lbl, Value: val})
				}
			}
		}
		for _, ch := range n.children {
			walk(ch)
		}
	}
	walk(doc)
	return out
}

func extractSemTables(doc *semNode) []TableData {
	var tables []TableData
	for _, tbl := range querySemAll(doc, "table, grid, datagrid") {
		if len(tables) >= 10 {
			break
		}
		var headers []string
		for _, h := range querySemAll(tbl, "columnheader, headeritem") {
			if t := semNodeText(h); t != "" {
				headers = append(headers, t)
			}
		}
		var rows [][]string
		for _, r := range querySemAll(tbl, "row, dataitem") {
			cellsNodes := querySemAll(r, "cell, gridcell")
			if len(cellsNodes) == 0 {
				cellsNodes = r.children
			}
			var cells []string
			nonEmpty := false
			for _, cn := range cellsNodes {
				t := semNodeText(cn)
				if t != "" {
					nonEmpty = true
				}
				cells = append(cells, t)
			}
			if nonEmpty {
				rows = append(rows, cells)
				if len(rows) >= 100 {
					break
				}
			}
		}
		if len(rows) > 0 {
			tables = append(tables, TableData{Headers: headers, Rows: rows})
		}
	}
	return tables
}

// Semantic selector parser (mirrors internal/verify/selector.go in pure Go stdlib).

type semAttrOp string

const (
	semAttrExists   semAttrOp = ""
	semAttrEquals   semAttrOp = "="
	semAttrContains semAttrOp = "*="
	semAttrPrefix   semAttrOp = "^="
	semAttrSuffix   semAttrOp = "$="
)

type semAttrCheck struct {
	name            string
	op              semAttrOp
	value           string
	caseInsensitive bool
}

type semSimpleSelector struct {
	role         string
	autoID       string
	classes      []string
	attrs        []semAttrCheck
	firstChild   bool
	lastChild    bool
	nthChild     int
	notSelectors [][]semPart
}

type semPart struct {
	childOnly bool
	simple    semSimpleSelector
}

func querySemAll(root *semNode, rawSelector string) []*semNode {
	if root == nil || strings.TrimSpace(rawSelector) == "" {
		return nil
	}
	normalized := strings.ReplaceAll(rawSelector, ">>>", " ")
	var groups [][]semPart
	for _, g := range splitSemTopLevel(normalized, ',') {
		if cs := parseSemComplex(g); len(cs) > 0 {
			groups = append(groups, cs)
		}
	}
	if len(groups) == 0 {
		return nil
	}

	var out []*semNode
	seen := make(map[*semNode]bool)
	var walk func(*semNode)
	walk = func(n *semNode) {
		if n != root {
			for _, cs := range groups {
				if matchSemChain(n, root, cs, len(cs)-1) {
					if !seen[n] {
						seen[n] = true
						out = append(out, n)
					}
					break
				}
			}
		}
		for _, ch := range n.children {
			walk(ch)
		}
	}
	walk(root)
	return out
}

func querySemFirst(root *semNode, rawSelector string) *semNode {
	all := querySemAll(root, rawSelector)
	if len(all) == 0 {
		return nil
	}
	return all[0]
}

func closestSem(n *semNode, rawSelector string) *semNode {
	normalized := strings.ReplaceAll(rawSelector, ">>>", " ")
	var groups [][]semPart
	for _, g := range splitSemTopLevel(normalized, ',') {
		if cs := parseSemComplex(g); len(cs) > 0 {
			groups = append(groups, cs)
		}
	}
	for cur := n; cur != nil && cur.role != "document"; cur = cur.parent {
		for _, cs := range groups {
			if matchSemChain(cur, nil, cs, len(cs)-1) {
				return cur
			}
		}
	}
	return nil
}

func matchSemChain(n *semNode, _ *semNode, parts []semPart, idx int) bool {
	if n == nil || n.role == "document" {
		return false
	}
	if !matchSemSimple(n, parts[idx].simple) {
		return false
	}
	if idx == 0 {
		return true
	}
	if parts[idx].childOnly {
		if n.parent == nil || n.parent.role == "document" {
			return false
		}
		return matchSemChain(n.parent, nil, parts, idx-1)
	}
	for anc := n.parent; anc != nil && anc.role != "document"; anc = anc.parent {
		if matchSemChain(anc, nil, parts, idx-1) {
			return true
		}
	}
	return false
}

func matchSemSimple(n *semNode, s semSimpleSelector) bool {
	if s.role != "" && s.role != "*" && !semRolesMatch(n.role, s.role) {
		return false
	}
	if s.autoID != "" && n.autoID != s.autoID && n.raw.NodeID != s.autoID {
		return false
	}
	if len(s.classes) > 0 {
		tokens := strings.Fields(n.className)
		for _, want := range s.classes {
			found := n.className == want
			for _, got := range tokens {
				if got == want {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
	}
	for _, ac := range s.attrs {
		val, exists := lookupSemAttr(n, ac.name)
		if !exists {
			return false
		}
		if strings.EqualFold(ac.name, "role") && ac.op == semAttrEquals {
			if !semRolesMatch(val, ac.value) {
				return false
			}
			continue
		}
		gotVal, wantVal := val, ac.value
		if ac.caseInsensitive {
			gotVal = strings.ToLower(gotVal)
			wantVal = strings.ToLower(wantVal)
		}
		switch ac.op {
		case semAttrExists:
		case semAttrEquals:
			if gotVal != wantVal {
				return false
			}
		case semAttrContains:
			if !strings.Contains(gotVal, wantVal) {
				return false
			}
		case semAttrPrefix:
			if !strings.HasPrefix(gotVal, wantVal) {
				return false
			}
		case semAttrSuffix:
			if !strings.HasSuffix(gotVal, wantVal) {
				return false
			}
		}
	}
	if s.firstChild && semChildIndex(n) != 1 {
		return false
	}
	if s.lastChild && !semIsLastChild(n) {
		return false
	}
	if s.nthChild > 0 && semChildIndex(n) != s.nthChild {
		return false
	}
	for _, neg := range s.notSelectors {
		if len(neg) > 0 && matchSemChain(n, nil, neg, len(neg)-1) {
			return false
		}
	}
	return true
}

func semIsLastChild(n *semNode) bool {
	if n == nil || n.parent == nil || len(n.parent.children) == 0 {
		return false
	}
	return n.parent.children[len(n.parent.children)-1] == n
}

func semRolesMatch(nodeRole string, wantRole string) bool {
	nr := NormalizeAXRole(nodeRole)
	wr := NormalizeAXRole(wantRole)
	if nr == wr {
		return true
	}
	if (nr == "statictext" && wr == "text") || (nr == "text" && wr == "statictext") {
		return true
	}
	if (nr == "textbox" && wr == "edit") || (nr == "edit" && wr == "textbox") {
		return true
	}
	return false
}

func semChildIndex(n *semNode) int {
	if n == nil || n.parent == nil {
		return 0
	}
	for i, ch := range n.parent.children {
		if ch == n {
			return i + 1
		}
	}
	return 0
}

func parseSemComplex(raw string) []semPart {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	var parts []semPart
	childOnly := false
	i := 0
	for i < len(s) {
		for i < len(s) && isSemSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == '>' {
			childOnly = true
			i++
			continue
		}
		start := i
		inBracket := false
		inQuote := byte(0)
		inParen := 0
		for i < len(s) {
			c := s[i]
			if inQuote != 0 {
				if c == inQuote {
					inQuote = 0
				}
				i++
				continue
			}
			if c == '\'' || c == '"' {
				inQuote = c
				i++
				continue
			}
			if c == '[' {
				inBracket = true
				i++
				continue
			}
			if c == ']' {
				inBracket = false
				i++
				continue
			}
			if c == '(' {
				inParen++
				i++
				continue
			}
			if c == ')' && inParen > 0 {
				inParen--
				i++
				continue
			}
			if !inBracket && inParen == 0 && (isSemSpace(c) || c == '>') {
				break
			}
			i++
		}
		tok := strings.TrimSpace(s[start:i])
		if tok != "" {
			parts = append(parts, semPart{
				childOnly: childOnly && len(parts) > 0,
				simple:    parseSemSimple(tok),
			})
			childOnly = false
		}
	}
	return parts
}

func parseSemSimple(tok string) semSimpleSelector {
	var ss semSimpleSelector
	i := 0
	for i < len(tok) {
		switch tok[i] {
		case '#':
			i++
			start := i
			for i < len(tok) && isSemIdent(tok[i]) {
				i++
			}
			ss.autoID = tok[start:i]
		case '.':
			i++
			start := i
			for i < len(tok) && isSemIdent(tok[i]) {
				i++
			}
			if i > start {
				ss.classes = append(ss.classes, tok[start:i])
			}
		case '[':
			i++
			start := i
			inQ := byte(0)
			for i < len(tok) {
				if inQ != 0 {
					if tok[i] == inQ {
						inQ = 0
					}
					i++
					continue
				}
				if tok[i] == '\'' || tok[i] == '"' {
					inQ = tok[i]
					i++
					continue
				}
				if tok[i] == ']' {
					break
				}
				i++
			}
			inner := strings.TrimSpace(tok[start:i])
			if i < len(tok) && tok[i] == ']' {
				i++
			}
			if ac, ok := parseSemAttrExpr(inner); ok {
				ss.attrs = append(ss.attrs, ac)
			}
		case ':':
			i++
			start := i
			inParen := 0
			inQ := byte(0)
			for i < len(tok) {
				if inQ != 0 {
					if tok[i] == inQ {
						inQ = 0
					}
					i++
					continue
				}
				if tok[i] == '\'' || tok[i] == '"' {
					inQ = tok[i]
				} else if tok[i] == '(' {
					inParen++
				} else if tok[i] == ')' && inParen > 0 {
					inParen--
				} else if inParen == 0 && (tok[i] == '.' || tok[i] == '#' || tok[i] == '[' || tok[i] == ':') {
					break
				}
				i++
			}
			rawPseudo := strings.TrimSpace(tok[start:i])
			pseudo := strings.ToLower(rawPseudo)
			switch {
			case pseudo == "first-child":
				ss.firstChild = true
			case pseudo == "last-child":
				ss.lastChild = true
			case strings.HasPrefix(pseudo, "nth-child(") && strings.HasSuffix(pseudo, ")"):
				numStr := strings.TrimSpace(pseudo[len("nth-child(") : len(pseudo)-1])
				if n, err := strconv.Atoi(numStr); err == nil && n > 0 {
					ss.nthChild = n
				}
			case strings.HasPrefix(pseudo, "not(") && strings.HasSuffix(rawPseudo, ")"):
				inner := strings.TrimSpace(rawPseudo[4 : len(rawPseudo)-1])
				for _, g := range splitSemTopLevel(inner, ',') {
					if cs := parseSemComplex(g); len(cs) > 0 {
						ss.notSelectors = append(ss.notSelectors, cs)
					}
				}
			}
		default:
			start := i
			for i < len(tok) && (isSemIdent(tok[i]) || tok[i] == '*') {
				i++
			}
			if i > start {
				ss.role = NormalizeAXRole(tok[start:i])
			} else {
				i++
			}
		}
	}
	return ss
}

func parseSemAttrExpr(expr string) (semAttrCheck, bool) {
	if expr == "" {
		return semAttrCheck{}, false
	}
	inQ := byte(0)
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if inQ != 0 {
			if c == inQ {
				inQ = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			inQ = c
			continue
		}
		if c == '=' && i > 0 {
			op := semAttrEquals
			nameEnd := i
			if i >= 2 {
				switch expr[i-1] {
				case '*':
					op = semAttrContains
					nameEnd = i - 1
				case '^':
					op = semAttrPrefix
					nameEnd = i - 1
				case '$':
					op = semAttrSuffix
					nameEnd = i - 1
				}
			}
			name := strings.TrimSpace(expr[:nameEnd])
			if name == "" {
				return semAttrCheck{}, false
			}
			val, ci := parseCSSAttrValueAndFlag(expr[i+1:])
			return semAttrCheck{name: name, op: op, value: val, caseInsensitive: ci}, true
		}
	}
	return semAttrCheck{name: strings.TrimSpace(expr), op: semAttrExists}, true
}

func parseCSSAttrValueAndFlag(raw string) (val string, caseInsensitive bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	if s[0] == '"' || s[0] == '\'' {
		q := s[0]
		end := strings.IndexByte(s[1:], q)
		if end >= 0 {
			val = s[1 : 1+end]
			rest := strings.TrimSpace(s[2+end:])
			return val, strings.EqualFold(rest, "i")
		}
		return strings.Trim(s, `"'`), false
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", false
	}
	if len(fields) >= 2 && strings.EqualFold(fields[len(fields)-1], "i") {
		return strings.Join(fields[:len(fields)-1], " "), true
	}
	return s, false
}

func splitSemTopLevel(s string, sep byte) []string {
	var out []string
	start := 0
	inBracket := false
	inParen := 0
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			inQuote = c
		case '[':
			inBracket = true
		case ']':
			inBracket = false
		case '(':
			inParen++
		case ')':
			if inParen > 0 {
				inParen--
			}
		default:
			if c == sep && !inBracket && inParen == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(s[start:]))
	return out
}

func applySemRegex(text string, pattern string, group int) string {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return ""
	}
	m := re.FindStringSubmatch(text)
	if len(m) == 0 {
		return ""
	}
	if group > 0 {
		if group >= len(m) {
			return ""
		}
		return m[group]
	}
	if len(m) == 1 {
		return m[0]
	}
	for i := 1; i < len(m); i++ {
		if m[i] != "" {
			return m[i]
		}
	}
	return ""
}

func truncateSemRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max]))
}

func containsString(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

func isSemSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isSemIdent(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
}
