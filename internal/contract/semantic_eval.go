package contract

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

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
			if val, ok := evalSemFieldRule(doc, rule, true); ok {
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

func evalSemFieldRule(scope *semNode, rule FieldRule, omitUnmatchedNumeric bool) (any, bool) {
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
			if omitUnmatchedNumeric {
				return nil, false
			}
			return int64(0), true
		}
		if rule.Parse == "float" {
			if omitUnmatchedNumeric {
				return nil, false
			}
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
		if omitUnmatchedNumeric {
			return nil, false
		}
		return int64(0), true
	case "float":
		if f, ok := ParseCompactFloat(raw); ok {
			return f, true
		}
		if omitUnmatchedNumeric {
			return nil, false
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
			if val, ok := evalSemFieldRule(scope, rule.Fields[k], false); ok {
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

func querySemAll(root *semNode, rawSelector string) []*semNode {
	if root == nil {
		return nil
	}
	groups := ParseCSSSelectorList(rawSelector)
	if len(groups) == 0 {
		return nil
	}

	var out []*semNode
	seen := make(map[*semNode]bool)
	var walk func(*semNode)
	walk = func(n *semNode) {
		if n != root {
			for _, cs := range groups {
				if matchSemChain(n, cs, len(cs)-1) {
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
	groups := ParseCSSSelectorList(rawSelector)
	if len(groups) == 0 {
		return nil
	}
	for cur := n; cur != nil && cur.role != "document"; cur = cur.parent {
		for _, cs := range groups {
			if matchSemChain(cur, cs, len(cs)-1) {
				return cur
			}
		}
	}
	return nil
}

func matchSemChain(n *semNode, parts CSSComplexSelector, idx int) bool {
	if n == nil || n.role == "document" {
		return false
	}
	if !matchSemSimple(n, parts[idx].Simple) {
		return false
	}
	if idx == 0 {
		return true
	}
	switch parts[idx].Comb {
	case CSSCombChild:
		if n.parent == nil || n.parent.role == "document" {
			return false
		}
		return matchSemChain(n.parent, parts, idx-1)
	case CSSCombNextSibling:
		pos := semChildIndex(n) - 1
		if pos <= 0 || n.parent == nil {
			return false
		}
		return matchSemChain(n.parent.children[pos-1], parts, idx-1)
	case CSSCombSubsequentSibling:
		pos := semChildIndex(n) - 1
		if pos <= 0 || n.parent == nil {
			return false
		}
		for i := pos - 1; i >= 0; i-- {
			if matchSemChain(n.parent.children[i], parts, idx-1) {
				return true
			}
		}
		return false
	default:
		for anc := n.parent; anc != nil && anc.role != "document"; anc = anc.parent {
			if matchSemChain(anc, parts, idx-1) {
				return true
			}
		}
		return false
	}
}

func matchSemSimple(n *semNode, s CSSSimpleSelector) bool {
	if s.Tag != "" && s.Tag != "*" && !semRolesMatch(n.role, s.Tag) {
		return false
	}
	if s.ID != "" && n.autoID != s.ID && n.raw.NodeID != s.ID {
		return false
	}
	if len(s.Classes) > 0 {
		tokens := strings.Fields(n.className)
		for _, want := range s.Classes {
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
	for _, ac := range s.Attrs {
		val, exists := lookupSemAttr(n, ac.Name)
		if !exists {
			return false
		}
		if strings.EqualFold(ac.Name, "role") && ac.Op == CSSAttrEquals {
			if !semRolesMatch(val, ac.Value) {
				return false
			}
			continue
		}
		gotVal, wantVal := val, ac.Value
		if ac.CaseInsensitive {
			gotVal = strings.ToLower(gotVal)
			wantVal = strings.ToLower(wantVal)
		}
		switch ac.Op {
		case CSSAttrExists:
		case CSSAttrEquals:
			if gotVal != wantVal {
				return false
			}
		case CSSAttrContains:
			if !strings.Contains(gotVal, wantVal) {
				return false
			}
		case CSSAttrPrefix:
			if !strings.HasPrefix(gotVal, wantVal) {
				return false
			}
		case CSSAttrSuffix:
			if !strings.HasSuffix(gotVal, wantVal) {
				return false
			}
		}
	}
	if s.FirstChild && semChildIndex(n) != 1 {
		return false
	}
	if s.LastChild && !semIsLastChild(n) {
		return false
	}
	if s.NthChild > 0 && semChildIndex(n) != s.NthChild {
		return false
	}
	for _, neg := range s.NotSelectors {
		if len(neg) > 0 && matchSemChain(n, neg, len(neg)-1) {
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
