package verify

import (
	"strings"

	"golang.org/x/net/html"

	"github.com/tvaroska/deskdrop/internal/contract"
)

// QueryAll finds all descendant element nodes matching a CSS selector list.
// Supports tag, *, #id, .class, [attr], [attr=val], [attr*=val], [attr^=val], [attr$=val]
// (with optional 'i' flag), :first-child, :last-child, :nth-child(n), :not(...),
// descendant, direct-child (>), comma lists, and Shadow-DOM '>>>'.
func QueryAll(root *html.Node, rawSelector string) []*html.Node {
	if root == nil {
		return nil
	}
	parsed := contract.ParseCSSSelectorList(rawSelector)
	if len(parsed) == 0 {
		return nil
	}

	var out []*html.Node
	seen := make(map[*html.Node]bool)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n != root {
			for _, cs := range parsed {
				if matchesComplex(n, cs) {
					if !seen[n] {
						seen[n] = true
						out = append(out, n)
					}
					break
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

// QueryFirst returns the first matching element node under root.
func QueryFirst(root *html.Node, rawSelector string) *html.Node {
	all := QueryAll(root, rawSelector)
	if len(all) == 0 {
		return nil
	}
	return all[0]
}

// Matches reports whether element node n matches any selector in rawSelector.
func Matches(n *html.Node, rawSelector string) bool {
	if n == nil || n.Type != html.ElementNode {
		return false
	}
	for _, cs := range contract.ParseCSSSelectorList(rawSelector) {
		if matchesComplex(n, cs) {
			return true
		}
	}
	return false
}

// Closest walks upward from n (including n itself) to find the nearest ancestor matching rawSelector.
func Closest(n *html.Node, rawSelector string) *html.Node {
	parsed := contract.ParseCSSSelectorList(rawSelector)
	if len(parsed) == 0 {
		return nil
	}
	for cur := n; cur != nil; cur = cur.Parent {
		if cur.Type == html.ElementNode {
			for _, cs := range parsed {
				if matchesComplex(cur, cs) {
					return cur
				}
			}
		}
	}
	return nil
}

func matchesComplex(node *html.Node, parts contract.CSSComplexSelector) bool {
	return matchPart(node, parts, len(parts)-1)
}

func matchPart(node *html.Node, parts contract.CSSComplexSelector, idx int) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}
	if !matchesSimple(node, parts[idx].Simple) {
		return false
	}
	if idx == 0 {
		return true
	}
	switch parts[idx].Comb {
	case contract.CSSCombChild:
		parent := node.Parent
		if parent == nil || parent.Type != html.ElementNode {
			return false
		}
		return matchPart(parent, parts, idx-1)
	default:
		for anc := node.Parent; anc != nil; anc = anc.Parent {
			if anc.Type == html.ElementNode && matchPart(anc, parts, idx-1) {
				return true
			}
		}
		return false
	}
}

func matchesSimple(n *html.Node, s contract.CSSSimpleSelector) bool {
	if s.Tag != "" && s.Tag != "*" && !strings.EqualFold(n.Data, s.Tag) {
		return false
	}
	if s.ID != "" && getAttr(n, "id") != s.ID {
		return false
	}
	if len(s.Classes) > 0 {
		clsFields := strings.Fields(getAttr(n, "class"))
		for _, want := range s.Classes {
			found := false
			for _, got := range clsFields {
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
		val, exists := lookupAttr(n, ac.Name)
		if !exists {
			return false
		}
		gotVal, wantVal := val, ac.Value
		if ac.CaseInsensitive {
			gotVal = strings.ToLower(gotVal)
			wantVal = strings.ToLower(wantVal)
		}
		switch ac.Op {
		case contract.CSSAttrExists:
		case contract.CSSAttrEquals:
			if gotVal != wantVal {
				return false
			}
		case contract.CSSAttrContains:
			if !strings.Contains(gotVal, wantVal) {
				return false
			}
		case contract.CSSAttrPrefix:
			if !strings.HasPrefix(gotVal, wantVal) {
				return false
			}
		case contract.CSSAttrSuffix:
			if !strings.HasSuffix(gotVal, wantVal) {
				return false
			}
		}
	}
	if s.FirstChild && elementChildIndex(n) != 1 {
		return false
	}
	if s.LastChild && !isLastElementChild(n) {
		return false
	}
	if s.NthChild > 0 && elementChildIndex(n) != s.NthChild {
		return false
	}
	for _, neg := range s.NotSelectors {
		if len(neg) > 0 && matchesComplex(n, neg) {
			return false
		}
	}
	return true
}

func isLastElementChild(n *html.Node) bool {
	if n == nil || n.Parent == nil {
		return false
	}
	for cur := n.NextSibling; cur != nil; cur = cur.NextSibling {
		if cur.Type == html.ElementNode {
			return false
		}
	}
	return true
}

func elementChildIndex(n *html.Node) int {
	idx := 0
	for cur := n; cur != nil; cur = cur.PrevSibling {
		if cur.Type == html.ElementNode {
			idx++
		}
	}
	return idx
}

func lookupAttr(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val, true
		}
	}
	return "", false
}

func getAttr(n *html.Node, name string) string {
	v, _ := lookupAttr(n, name)
	return v
}
