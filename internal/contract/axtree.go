package contract

import (
	"encoding/json"
	"fmt"
	"strings"
)

const MaxCompactAXTreeBytes = 64 * 1024

// AXValue represents a typed scalar property returned by CDP Accessibility.getFullAXTree.
type AXValue struct {
	Type  string `json:"type,omitempty"`
	Value any    `json:"value,omitempty"`
}

func (v *AXValue) StringValue() string {
	if v == nil || v.Value == nil {
		return ""
	}
	switch t := v.Value.(type) {
	case string:
		return NormalizeWhitespace(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	default:
		return NormalizeWhitespace(fmt.Sprint(t))
	}
}

// AXProperty represents a named state or attribute on an AXNode.
type AXProperty struct {
	Name  string  `json:"name"`
	Value AXValue `json:"value"`
}

// AXNode mirrors a node from Chrome DevTools Protocol Accessibility.getFullAXTree
// or a native desktop accessibility tree such as Windows UI Automation (UIA).
type AXNode struct {
	NodeID           string       `json:"nodeId"`
	Ignored          bool         `json:"ignored,omitempty"`
	ParentID         string       `json:"parentId,omitempty"`
	ChildIDs         []string     `json:"childIds,omitempty"`
	BackendDOMNodeID int64        `json:"backendDOMNodeId,omitempty"`
	AutomationID     string       `json:"automationId,omitempty"`
	ClassName        string       `json:"className,omitempty"`
	IsPassword       bool         `json:"isPassword,omitempty"`
	Role             *AXValue     `json:"role,omitempty"`
	Name             *AXValue     `json:"name,omitempty"`
	Description      *AXValue     `json:"description,omitempty"`
	Value            *AXValue     `json:"value,omitempty"`
	Properties       []AXProperty `json:"properties,omitempty"`
}

type axTreeIndex struct {
	byID             map[string]*AXNode
	childrenByParent map[string][]string
	roots            []string
	order            []string
}

func buildAXTreeIndex(nodes []AXNode) axTreeIndex {
	idx := axTreeIndex{
		byID:             make(map[string]*AXNode, len(nodes)),
		childrenByParent: make(map[string][]string, len(nodes)),
		order:            make([]string, 0, len(nodes)),
	}
	hasParent := make(map[string]bool, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if n.NodeID == "" {
			continue
		}
		if _, exists := idx.byID[n.NodeID]; !exists {
			idx.order = append(idx.order, n.NodeID)
		}
		idx.byID[n.NodeID] = n
	}
	for _, id := range idx.order {
		n := idx.byID[id]
		for _, cid := range n.ChildIDs {
			if _, ok := idx.byID[cid]; ok {
				hasParent[cid] = true
				if !containsString(idx.childrenByParent[id], cid) {
					idx.childrenByParent[id] = append(idx.childrenByParent[id], cid)
				}
			}
		}
		if n.ParentID != "" {
			if _, ok := idx.byID[n.ParentID]; ok {
				hasParent[id] = true
			}
		}
	}
	for _, id := range idx.order {
		n := idx.byID[id]
		if n.ParentID != "" {
			if _, ok := idx.byID[n.ParentID]; ok && !containsString(idx.childrenByParent[n.ParentID], id) {
				idx.childrenByParent[n.ParentID] = append(idx.childrenByParent[n.ParentID], id)
			}
		}
	}
	for _, id := range idx.order {
		if !hasParent[id] {
			idx.roots = append(idx.roots, id)
		}
	}
	if len(idx.roots) == 0 && len(idx.order) > 0 {
		idx.roots = append(idx.roots, idx.order[0])
	}
	return idx
}

func isNodeSensitive(n *AXNode) (isPwd bool, isSens bool) {
	roleLower := NormalizeAXRole(n.Role.StringValue())
	isPwd = n.IsPassword || roleLower == "password" || hasTruthyAXProp(n.Properties, "ispassword") || hasTruthyAXProp(n.Properties, "password")
	return isPwd, isPwd || isSensitiveInputRole(roleLower)
}

// ScrubAXNodes returns a copy of nodes with password/editable control values AND all
// descendants (e.g., child StaticText) inside sensitive inputs redacted to "[REDACTED]".
func ScrubAXNodes(nodes []AXNode) []AXNode {
	if len(nodes) == 0 {
		return nil
	}
	out := make([]AXNode, len(nodes))
	for i, n := range nodes {
		cp := n
		if len(n.ChildIDs) > 0 {
			cp.ChildIDs = append([]string(nil), n.ChildIDs...)
		}
		if len(n.Properties) > 0 {
			cp.Properties = make([]AXProperty, len(n.Properties))
			copy(cp.Properties, n.Properties)
		}
		out[i] = cp
	}

	idx := buildAXTreeIndex(out)
	sensitiveDescendant := make(map[string]bool)
	passwordDescendant := make(map[string]bool)

	var markDesc func(id string, pwd bool)
	markDesc = func(id string, pwd bool) {
		for _, cid := range idx.childrenByParent[id] {
			alreadySens := sensitiveDescendant[cid]
			alreadyPwd := passwordDescendant[cid]
			if alreadySens && (!pwd || alreadyPwd) {
				continue
			}
			sensitiveDescendant[cid] = true
			if pwd {
				passwordDescendant[cid] = true
			}
			markDesc(cid, pwd)
		}
	}

	for i := range out {
		n := &out[i]
		isPwd, isSens := isNodeSensitive(n)
		if isSens && n.NodeID != "" {
			markDesc(n.NodeID, isPwd)
		}
	}

	for i := range out {
		cp := &out[i]
		isPwd, isSens := isNodeSensitive(cp)
		inSens := cp.NodeID != "" && sensitiveDescendant[cp.NodeID]
		inPwd := cp.NodeID != "" && passwordDescendant[cp.NodeID]
		if isPwd || inPwd {
			cp.IsPassword = true
		}
		if inSens {
			if cp.Name != nil && cp.Name.StringValue() != "" {
				cp.Name = &AXValue{Type: "string", Value: "[REDACTED]"}
			}
			if cp.Description != nil && cp.Description.StringValue() != "" {
				cp.Description = &AXValue{Type: "string", Value: "[REDACTED]"}
			}
		}
		if (isSens || inSens) && cp.Value != nil && cp.Value.StringValue() != "" {
			cp.Value = &AXValue{Type: "string", Value: "[REDACTED]"}
		}
		if isSens || inSens {
			for pi := range cp.Properties {
				pk := strings.ToLower(strings.TrimSpace(cp.Properties[pi].Name))
				if pk == "value" && cp.Properties[pi].Value.StringValue() != "" {
					cp.Properties[pi].Value = AXValue{Type: "string", Value: "[REDACTED]"}
				}
			}
		}
	}
	return out
}

// NormalizeAXRole maps CDP and Windows UIA ControlType names to lowercase canonical role tokens.
func NormalizeAXRole(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, "controltype.")
	s = strings.TrimPrefix(s, "uia_")
	s = strings.TrimSuffix(s, "controltypeid")
	return s
}

func hasTruthyAXProp(props []AXProperty, keyLower string) bool {
	for _, p := range props {
		if strings.ToLower(strings.TrimSpace(p.Name)) == keyLower {
			v := strings.ToLower(p.Value.StringValue())
			return v == "true" || v == "1"
		}
	}
	return false
}

// AXGroundingStats summarizes semantic density and canvas/WebGL blind spots in an AXTree snapshot.
type AXGroundingStats struct {
	TotalNodes          int  `json:"total_nodes"`
	KeptNodes           int  `json:"kept_nodes"`
	SemanticTextNodes   int  `json:"semantic_text_nodes"`
	InteractiveControls int  `json:"interactive_controls"`
	CanvasNodes         int  `json:"canvas_nodes"`
	UnlabelledCanvases  int  `json:"unlabelled_canvases"`
	CanvasBlindSpot     bool `json:"canvas_blind_spot"`
	RawBytes            int  `json:"raw_bytes"`
	CompactBytes        int  `json:"compact_bytes"`
}

// ParseAXTreeJSON decodes either `{ "nodes": [...] }` or a bare `[...]` AXNode array.
func ParseAXTreeJSON(raw []byte) ([]AXNode, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var nodes []AXNode
		if err := json.Unmarshal(raw, &nodes); err != nil {
			return nil, err
		}
		return nodes, nil
	}
	var env struct {
		Nodes []AXNode `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	return env.Nodes, nil
}

// FormatCompactAXTree converts raw CDP Accessibility.getFullAXTree nodes into a compact,
// privacy-scrubbed semantic outline and computes grounding diagnostics (including <canvas>/WebGL blind spots).
func FormatCompactAXTree(nodes []AXNode, rawByteLen int) (string, AXGroundingStats) {
	stats := AXGroundingStats{
		TotalNodes: len(nodes),
		RawBytes:   rawByteLen,
	}
	if len(nodes) == 0 {
		return "", stats
	}

	scrubbed := ScrubAXNodes(nodes)
	idx := buildAXTreeIndex(scrubbed)

	var b strings.Builder
	visited := make(map[string]bool, len(scrubbed))

	var walk func(id string, depth int, parentName string)
	walk = func(id string, depth int, parentName string) {
		if visited[id] || b.Len() >= MaxCompactAXTreeBytes {
			return
		}
		visited[id] = true
		n, ok := idx.byID[id]
		if !ok {
			return
		}

		role := strings.TrimSpace(n.Role.StringValue())
		roleLower := NormalizeAXRole(role)

		// InlineTextBox duplicates glyph runs already present in parent StaticText.
		if roleLower == "inlinetextbox" {
			return
		}

		// Ignored wrappers may still own non-ignored semantic descendants in flat CDP trees.
		if n.Ignored {
			for _, cid := range idx.childrenByParent[id] {
				walk(cid, depth, parentName)
			}
			return
		}

		name := n.Name.StringValue()
		val := n.Value.StringValue()
		_, isSens := isNodeSensitive(n)
		if isSens && val == "" && hasAccessibleDescendantText(id, idx, 0) {
			val = "[REDACTED]"
		}

		if isCanvasLikeRole(roleLower, n) {
			stats.CanvasNodes++
			if name == "" && !hasAccessibleDescendantText(id, idx, 0) {
				stats.UnlabelledCanvases++
			}
		}
		if isInteractiveAXRole(roleLower) {
			stats.InteractiveControls++
		}

		// Skip StaticText when identical to its immediate parent's accessible name.
		if roleLower == "statictext" {
			if name == "" || (parentName != "" && strings.EqualFold(name, parentName)) {
				return
			}
			stats.SemanticTextNodes++
		} else if name != "" && roleLower != "rootwebarea" && roleLower != "webarea" {
			stats.SemanticTextNodes++
		}

		props := formatSalientAXProps(n.Properties)
		isStructuralNoise := isUnnamedWrapperRole(roleLower) && name == "" && val == "" && props == ""
		if isStructuralNoise {
			for _, cid := range idx.childrenByParent[id] {
				walk(cid, depth, parentName)
			}
			return
		}

		stats.KeptNodes++
		indent := strings.Repeat("  ", minInt(depth, 12))
		b.WriteString(indent)
		b.WriteString("[")
		if role == "" {
			b.WriteString("node")
		} else {
			b.WriteString(role)
		}
		b.WriteString("]")
		if name != "" {
			fmt.Fprintf(&b, " %q", truncateAXStr(name, 180))
		}
		if autoID := strings.TrimSpace(n.AutomationID); autoID != "" {
			fmt.Fprintf(&b, " id=%q", truncateAXStr(autoID, 80))
		}
		if val != "" && val != name {
			fmt.Fprintf(&b, " value=%q", truncateAXStr(val, 120))
		}
		if props != "" {
			b.WriteString(" ")
			b.WriteString(props)
		}
		if isCanvasLikeRole(roleLower, n) && name == "" && !hasAccessibleDescendantText(id, idx, 0) {
			b.WriteString(" [UNGROUNDED_PIXEL_SURFACE]")
		}
		b.WriteByte('\n')

		// Do not descend into sensitive input subtrees once the control itself is emitted redacted.
		if isSens {
			return
		}

		nextParentName := parentName
		if name != "" {
			nextParentName = name
		}
		for _, cid := range idx.childrenByParent[id] {
			walk(cid, depth+1, nextParentName)
		}
	}

	for _, rid := range idx.roots {
		walk(rid, 0, "")
	}

	out := strings.TrimSpace(b.String())
	if len(out) > MaxCompactAXTreeBytes {
		out = strings.TrimSpace(truncateUTF8Bytes(out, MaxCompactAXTreeBytes))
	}
	stats.CompactBytes = len(out)
	// Canvas blind spot triggers when unlabelled <canvas>/WebGL surfaces exist and little or no semantic text is exposed.
	if stats.UnlabelledCanvases > 0 && stats.SemanticTextNodes <= 3 {
		stats.CanvasBlindSpot = true
	}
	return out, stats
}

func isSensitiveInputRole(roleLower string) bool {
	switch roleLower {
	case "textbox", "edit", "searchbox", "combobox", "spinbutton", "password":
		return true
	default:
		return false
	}
}

func isUnnamedWrapperRole(roleLower string) bool {
	switch roleLower {
	case "generic", "none", "presentation", "group", "section", "paragraph", "div":
		return true
	default:
		return false
	}
}

func isInteractiveAXRole(roleLower string) bool {
	switch roleLower {
	case "button", "link", "tab", "menuitem", "checkbox", "radio", "switch", "combobox", "textbox", "searchbox":
		return true
	default:
		return false
	}
}

func isCanvasLikeRole(roleLower string, n *AXNode) bool {
	if roleLower == "canvas" || roleLower == "graphics-document" {
		return true
	}
	if roleLower == "img" || roleLower == "image" {
		desc := strings.ToLower(n.Description.StringValue())
		if strings.Contains(desc, "canvas") || strings.Contains(desc, "webgl") {
			return true
		}
	}
	return false
}

func hasAccessibleDescendantText(id string, idx axTreeIndex, depth int) bool {
	if depth > 8 {
		return false
	}
	for _, cid := range idx.childrenByParent[id] {
		child, ok := idx.byID[cid]
		if !ok {
			continue
		}
		if !child.Ignored && child.Name.StringValue() != "" {
			return true
		}
		if hasAccessibleDescendantText(cid, idx, depth+1) {
			return true
		}
	}
	return false
}

func formatSalientAXProps(props []AXProperty) string {
	if len(props) == 0 {
		return ""
	}
	var parts []string
	for _, p := range props {
		k := strings.ToLower(strings.TrimSpace(p.Name))
		val := p.Value.StringValue()
		if val == "" || val == "false" {
			continue
		}
		switch k {
		case "level", "disabled", "expanded", "checked", "selected", "modal", "invalid":
			parts = append(parts, fmt.Sprintf("(%s=%s)", k, val))
		case "url":
			parts = append(parts, fmt.Sprintf("url=%q", truncateAXStr(val, 100)))
		}
	}
	return strings.Join(parts, " ")
}

func truncateAXStr(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "..."
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
