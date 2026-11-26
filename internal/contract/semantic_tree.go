package contract

import (
	"regexp"
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
