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

// ScrubAXNodes returns a copy of nodes with password and editable control values redacted to "[REDACTED]".
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
		roleLower := NormalizeAXRole(n.Role.StringValue())
		isPwd := n.IsPassword || roleLower == "password" || hasTruthyAXProp(n.Properties, "ispassword") || hasTruthyAXProp(n.Properties, "password")
		if isPwd {
			cp.IsPassword = true
		}
		if cp.Value != nil && cp.Value.StringValue() != "" && (isPwd || isSensitiveInputRole(roleLower)) {
			cp.Value = &AXValue{Type: "string", Value: "[REDACTED]"}
		}
		for pi := range cp.Properties {
			pk := strings.ToLower(strings.TrimSpace(cp.Properties[pi].Name))
			if pk == "value" && cp.Properties[pi].Value.StringValue() != "" && (isPwd || isSensitiveInputRole(roleLower)) {
				cp.Properties[pi].Value = AXValue{Type: "string", Value: "[REDACTED]"}
			}
		}
		out[i] = cp
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

	var roots []string
	for _, id := range order {
		if !hasParent[id] {
			roots = append(roots, id)
		}
	}
	if len(roots) == 0 && len(order) > 0 {
		roots = append(roots, order[0])
	}

	var b strings.Builder
	visited := make(map[string]bool, len(nodes))

	var walk func(id string, depth int, parentName string)
	walk = func(id string, depth int, parentName string) {
		if visited[id] || b.Len() >= MaxCompactAXTreeBytes {
			return
		}
		visited[id] = true
		n, ok := byID[id]
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
			for _, cid := range n.ChildIDs {
				walk(cid, depth, parentName)
			}
			return
		}

		name := n.Name.StringValue()
		val := n.Value.StringValue()
		isPwd := n.IsPassword || roleLower == "password" || hasTruthyAXProp(n.Properties, "ispassword") || hasTruthyAXProp(n.Properties, "password")
		if (isPwd || isSensitiveInputRole(roleLower)) && val != "" {
			val = "[REDACTED]"
		}

		if isCanvasLikeRole(roleLower, n) {
			stats.CanvasNodes++
			if name == "" && !hasAccessibleDescendantText(n, byID, 0) {
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
			for _, cid := range n.ChildIDs {
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
		if isCanvasLikeRole(roleLower, n) && name == "" && !hasAccessibleDescendantText(n, byID, 0) {
			b.WriteString(" [UNGROUNDED_PIXEL_SURFACE]")
		}
		b.WriteByte('\n')

		nextParentName := parentName
		if name != "" {
			nextParentName = name
		}
		for _, cid := range n.ChildIDs {
			walk(cid, depth+1, nextParentName)
		}
	}

	for _, rid := range roots {
		walk(rid, 0, "")
	}

	out := strings.TrimSpace(b.String())
	if len(out) > MaxCompactAXTreeBytes {
		out = strings.TrimSpace(out[:MaxCompactAXTreeBytes])
	}
	stats.CompactBytes = len(out)
	// Canvas blind spot triggers when unlabelled <canvas>/WebGL surfaces exist and little or no semantic text is exposed.
	if stats.UnlabelledCanvases > 0 && stats.SemanticTextNodes <= 3 {
		stats.CanvasBlindSpot = true
	}
	return out, stats
}

// InspectHTMLCanvasBlindSpot checks whether scrubbed HTML relies on <canvas> elements without fallback text.
func InspectHTMLCanvasBlindSpot(scrubbedHTML string, axStats AXGroundingStats) bool {
	if axStats.CanvasBlindSpot {
		return true
	}
	lower := strings.ToLower(scrubbedHTML)
	if !strings.Contains(lower, "<canvas") {
		return false
	}
	return axStats.UnlabelledCanvases > 0 || axStats.TotalNodes == 0
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

func hasAccessibleDescendantText(n *AXNode, byID map[string]*AXNode, depth int) bool {
	if n == nil || depth > 8 {
		return false
	}
	for _, cid := range n.ChildIDs {
		child, ok := byID[cid]
		if !ok {
			continue
		}
		if !child.Ignored && child.Name.StringValue() != "" {
			return true
		}
		if hasAccessibleDescendantText(child, byID, depth+1) {
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
