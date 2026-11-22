package verify

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/net/html"

	"github.com/tvaroska/deskdrop/internal/contract"
)

// VerifyOutcome summarizes pure-Go declarative verification of a Recipe against a captured HTML snapshot.
type VerifyOutcome struct {
	Passed      bool                `json:"passed"`
	Error       string              `json:"error,omitempty"`
	StepID      string              `json:"step_id"`
	StepResult  contract.StepResult `json:"step_result"`
	ItemCount   int                 `json:"item_count"`
	FieldCount  int                 `json:"field_count"`
	TableCount  int                 `json:"table_count"`
	MetricCount int                 `json:"metric_count"`
}

// VerifyRecipeOnHTML resolves recipe variables, extracts step output purely in Go using golang.org/x/net/html,
// and runs contract.ValidateStepResult. Used both in offline contract tests and by the adk-go verifier tool.
func VerifyRecipeOnHTML(recipe contract.Recipe, vars map[string]string, stepID string, htmlDoc string, landedURL string) (VerifyOutcome, error) {
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

	res, err := ExtractStepFromHTML(*target, htmlDoc, landedURL)
	if err != nil {
		return VerifyOutcome{Passed: false, StepID: target.ID, Error: err.Error()}, err
	}
	valErr := contract.ValidateStepResult(*target, res)
	out := VerifyOutcome{
		Passed:      valErr == nil,
		StepID:      target.ID,
		StepResult:  res,
		ItemCount:   len(res.Items),
		FieldCount:  len(res.Fields),
		TableCount:  len(res.Tables),
		MetricCount: len(res.Metrics),
	}
	if valErr != nil {
		out.Error = valErr.Error()
	}
	return out, nil
}

// ExtractStepFromHTML evaluates a declarative contract.Step against a scrubbed HTML document in pure Go,
// mirroring internal/driver/cdp's in-browser declarative extractor.
func ExtractStepFromHTML(step contract.Step, htmlDoc string, landedURL string) (contract.StepResult, error) {
	doc, err := html.Parse(strings.NewReader(htmlDoc))
	if err != nil {
		return contract.StepResult{}, fmt.Errorf("parse html: %w", err)
	}
	body := QueryFirst(doc, "body")
	if body == nil {
		body = doc
	}

	title := ""
	if tNode := QueryFirst(doc, "title"); tNode != nil {
		title = nodeText(tNode)
	}

	var selectorsOut map[string]string
	if len(step.Selectors) > 0 {
		selectorsOut = make(map[string]string, len(step.Selectors))
		for k, sel := range step.Selectors {
			matches := QueryAll(doc, sel)
			var chunks []string
			for _, m := range matches {
				if txt := nodeText(m); txt != "" {
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
			if val, ok := evalFieldRule(body, rule); ok {
				fieldsOut[k] = val
			}
		}
	}

	var itemsOut []map[string]any
	if step.Items != nil {
		itemsOut = evalItemRule(doc, body, *step.Items)
	}

	headings := extractHeadings(doc)
	var metrics []contract.MetricPair
	if step.WantCards() {
		metrics = extractMetricCards(doc)
	}
	var tables []contract.TableData
	if step.WantTables() {
		tables = extractTables(doc)
	}

	maxText := step.MaxTextChars
	if maxText <= 0 {
		maxText = 1200
	}
	excerpt := truncateRunes(nodeText(body), maxText)

	res := contract.StepResult{
		StepID:      step.ID,
		Title:       title,
		URL:         landedURL,
		Headings:    headings,
		Metrics:     metrics,
		Selectors:   selectorsOut,
		Fields:      fieldsOut,
		Items:       itemsOut,
		Tables:      tables,
		TextExcerpt: excerpt,
	}
	if step.Pagination != nil {
		res.PagesVisited = 1
	}
	if len(res.Fields) == 0 {
		res.Fields = nil
	}
	if len(res.Items) == 0 {
		res.Items = nil
	}
	return res, nil
}

func evalFieldRule(scope *html.Node, rule contract.FieldRule) (any, bool) {
	if scope == nil {
		return nil, false
	}
	el := scope
	if rule.Selector != "" {
		el = QueryFirst(scope, rule.Selector)
	}
	if el != nil && rule.Closest != "" {
		el = Closest(el, rule.Closest)
	}

	raw := ""
	if el != nil {
		raw = readAttrOrText(el, rule.Attr)
	}
	if raw != "" && rule.Regex != "" {
		raw = applyRegex(raw, rule.Regex, rule.Group)
	}
	if raw == "" && rule.FallbackRegex != "" {
		raw = applyRegex(nodeText(scope), rule.FallbackRegex, rule.FallbackGroup)
	}
	raw = contract.NormalizeWhitespace(raw)
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
		raw = truncateRunes(raw, rule.MaxChars)
	}
	if rule.Prefix != "" {
		raw = rule.Prefix + raw
	}
	if rule.Suffix != "" {
		raw = raw + rule.Suffix
	}

	switch rule.Parse {
	case "number":
		if n, ok := contract.ParseCompactNumber(raw); ok {
			return n, true
		}
		return int64(0), true
	case "float":
		if f, ok := contract.ParseCompactFloat(raw); ok {
			return f, true
		}
		return float64(0), true
	default:
		return raw, true
	}
}

func evalItemRule(doc *html.Node, body *html.Node, rule contract.ItemRule) []map[string]any {
	matches := QueryAll(doc, rule.Container)
	limit := rule.Limit
	if limit <= 0 {
		limit = 50
	}

	keys := make([]string, 0, len(rule.Fields))
	for k := range rule.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	seenScopes := make(map[*html.Node]bool)
	seenKeys := make(map[string]bool)
	var out []map[string]any

	for _, match := range matches {
		scope := match
		if rule.AnchorClosest != "" {
			anc := Closest(match, rule.AnchorClosest)
			if anc == nil {
				cur := match
				for cur.Parent != nil && cur.Parent.Type == html.ElementNode &&
					cur.Parent != body && !strings.EqualFold(cur.Parent.Data, "main") {
					if len(QueryAll(cur.Parent, rule.Container)) > 1 {
						break
					}
					cur = cur.Parent
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
			if val, ok := evalFieldRule(scope, rule.Fields[k]); ok {
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

func readAttrOrText(el *html.Node, attrSpec string) string {
	spec := strings.TrimSpace(attrSpec)
	if spec == "" {
		spec = "text"
	}
	for _, part := range strings.Split(spec, "|") {
		p := strings.TrimSpace(part)
		var val string
		if p == "" || strings.EqualFold(p, "text") {
			val = nodeText(el)
		} else {
			val = contract.NormalizeWhitespace(getAttr(el, p))
		}
		if val != "" {
			return val
		}
	}
	return ""
}

func applyRegex(text string, pattern string, group int) string {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return ""
	}
	m := re.FindStringSubmatch(text)
	if len(m) == 0 {
		return ""
	}
	idx := group
	if idx <= 0 {
		if len(m) > 1 {
			idx = 1
		} else {
			idx = 0
		}
	}
	if idx >= len(m) {
		return ""
	}
	return m[idx]
}

func extractHeadings(root *html.Node) []string {
	var out []string
	seen := make(map[string]bool)
	for _, n := range QueryAll(root, "h1, h2, h3, [role='heading']") {
		t := nodeText(n)
		if t != "" && len([]rune(t)) <= 140 && !seen[t] {
			seen[t] = true
			out = append(out, t)
			if len(out) >= 20 {
				break
			}
		}
	}
	return out
}

var metricNumRe = regexp.MustCompile(`\d`)

func extractMetricCards(root *html.Node) []contract.MetricPair {
	var out []contract.MetricPair
	seen := make(map[string]bool)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(out) >= 30 {
			return
		}
		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)
			_, hasDataMetric := lookupAttr(n, "data-metric")
			isCard := strings.Contains(tag, "metric") || strings.Contains(tag, "kpi") || strings.Contains(tag, "stat") ||
				getAttr(n, "role") == "group" || hasDataMetric
			if isCard {
				lines := nodeTextLines(n)
				if len(lines) >= 2 && len(lines) <= 6 {
					lbl := lines[0]
					val := ""
					for _, ln := range lines[1:] {
						if metricNumRe.MatchString(ln) {
							val = ln
							break
						}
					}
					if lbl != "" && val != "" && len(lbl) <= 80 && len(val) <= 60 {
						k := lbl + "=" + val
						if !seen[k] {
							seen[k] = true
							out = append(out, contract.MetricPair{Label: lbl, Value: val})
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

func extractTables(root *html.Node) []contract.TableData {
	var tables []contract.TableData
	for _, tbl := range QueryAll(root, "table, [role='table'], [role='grid'], ytcp-table") {
		if len(tables) >= 10 {
			break
		}
		if strings.EqualFold(tbl.Data, "table") {
			var headers []string
			for _, th := range QueryAll(tbl, "thead th, tr:first-child th") {
				if t := nodeText(th); t != "" {
					headers = append(headers, t)
				}
			}
			var rows [][]string
			for _, tr := range QueryAll(tbl, "tbody tr, tr") {
				var cells []string
				nonEmpty := false
				for _, td := range QueryAll(tr, "td") {
					t := nodeText(td)
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
				tables = append(tables, contract.TableData{Headers: headers, Rows: rows})
			}
		} else {
			var headers []string
			for _, ch := range QueryAll(tbl, "[role='columnheader']") {
				if t := nodeText(ch); t != "" {
					headers = append(headers, t)
				}
			}
			var rows [][]string
			for _, r := range QueryAll(tbl, "[role='row']") {
				var cells []string
				nonEmpty := false
				for _, c := range QueryAll(r, "[role='cell'], [role='gridcell']") {
					t := nodeText(c)
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
				tables = append(tables, contract.TableData{Headers: headers, Rows: rows})
			}
		}
	}
	return tables
}

func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(cur *html.Node) {
		if cur == nil {
			return
		}
		if cur.Type == html.ElementNode {
			switch strings.ToLower(cur.Data) {
			case "script", "style", "svg", "noscript":
				return
			}
			if _, hidden := lookupAttr(cur, "hidden"); hidden {
				return
			}
			if strings.EqualFold(getAttr(cur, "aria-hidden"), "true") {
				return
			}
		}
		if cur.Type == html.TextNode {
			sb.WriteString(cur.Data)
			sb.WriteByte(' ')
			return
		}
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return contract.NormalizeWhitespace(sb.String())
}

func nodeTextLines(n *html.Node) []string {
	var lines []string
	var walk func(*html.Node)
	walk = func(cur *html.Node) {
		if cur == nil {
			return
		}
		if cur.Type == html.ElementNode {
			switch strings.ToLower(cur.Data) {
			case "script", "style", "svg", "noscript":
				return
			}
		}
		if cur.Type == html.TextNode {
			for _, ln := range strings.Split(cur.Data, "\n") {
				if t := contract.NormalizeWhitespace(ln); t != "" {
					lines = append(lines, t)
				}
			}
			return
		}
		for c := cur.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return lines
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max]))
}
