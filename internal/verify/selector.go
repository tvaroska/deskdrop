package verify

import (
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

type attrOp string

const (
	attrExists   attrOp = ""
	attrEquals   attrOp = "="
	attrContains attrOp = "*="
	attrPrefix   attrOp = "^="
	attrSuffix   attrOp = "$="
)

type attrCheck struct {
	name            string
	op              attrOp
	value           string
	caseInsensitive bool
}

type simpleSelector struct {
	tag          string
	id           string
	classes      []string
	attrs        []attrCheck
	firstChild   bool
	lastChild    bool
	nthChild     int
	notSelectors []complexSelector
}

type combinator byte

const (
	combNone       combinator = 0
	combDescendant combinator = ' '
	combChild      combinator = '>'
)

type selectorPart struct {
	comb   combinator
	simple simpleSelector
}

type complexSelector []selectorPart

// QueryAll finds all descendant (or self-matched) element nodes matching a CSS selector list.
// Supports tag, *, #id, .class, [attr], [attr=val], [attr*=val], [attr^=val], [attr$=val],
// :first-child, :nth-child(n), descendant, direct-child (>), comma lists, and Shadow-DOM '>>>'.
func QueryAll(root *html.Node, rawSelector string) []*html.Node {
	if root == nil || strings.TrimSpace(rawSelector) == "" {
		return nil
	}
	// Flatten Shadow-DOM '>>>' combinator to descendant traversal because captured dom.html
	// inlines open shadow roots under <div data-shadowroot="open">.
	normalized := strings.ReplaceAll(rawSelector, ">>>", " ")
	groups := splitTopLevel(normalized, ',')
	var parsed []complexSelector
	for _, g := range groups {
		if cs := parseComplexSelector(g); len(cs) > 0 {
			parsed = append(parsed, cs)
		}
	}
	if len(parsed) == 0 {
		return nil
	}

	var out []*html.Node
	seen := make(map[*html.Node]bool)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n != root {
			for _, cs := range parsed {
				if matchesComplex(n, root, cs) {
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
	normalized := strings.ReplaceAll(rawSelector, ">>>", " ")
	for _, g := range splitTopLevel(normalized, ',') {
		if cs := parseComplexSelector(g); len(cs) > 0 && matchesComplex(n, nil, cs) {
			return true
		}
	}
	return false
}

// Closest walks upward from n (including n itself) to find the nearest ancestor matching rawSelector.
func Closest(n *html.Node, rawSelector string) *html.Node {
	for cur := n; cur != nil; cur = cur.Parent {
		if cur.Type == html.ElementNode && Matches(cur, rawSelector) {
			return cur
		}
	}
	return nil
}

func matchesComplex(node *html.Node, scopeRoot *html.Node, parts complexSelector) bool {
	return matchPart(node, scopeRoot, parts, len(parts)-1)
}

func matchPart(node *html.Node, _ *html.Node, parts complexSelector, idx int) bool {
	if node == nil || node.Type != html.ElementNode {
		return false
	}
	if !matchesSimple(node, parts[idx].simple) {
		return false
	}
	if idx == 0 {
		return true
	}
	comb := parts[idx].comb
	switch comb {
	case combChild:
		parent := node.Parent
		if parent == nil || parent.Type != html.ElementNode {
			return false
		}
		return matchPart(parent, nil, parts, idx-1)
	default: // combDescendant
		for anc := node.Parent; anc != nil; anc = anc.Parent {
			if anc.Type == html.ElementNode && matchPart(anc, nil, parts, idx-1) {
				return true
			}
		}
		return false
	}
}

func matchesSimple(n *html.Node, s simpleSelector) bool {
	if s.tag != "" && s.tag != "*" && !strings.EqualFold(n.Data, s.tag) {
		return false
	}
	if s.id != "" && getAttr(n, "id") != s.id {
		return false
	}
	if len(s.classes) > 0 {
		clsFields := strings.Fields(getAttr(n, "class"))
		for _, want := range s.classes {
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
	for _, ac := range s.attrs {
		val, exists := lookupAttr(n, ac.name)
		if !exists {
			return false
		}
		gotVal, wantVal := val, ac.value
		if ac.caseInsensitive {
			gotVal = strings.ToLower(gotVal)
			wantVal = strings.ToLower(wantVal)
		}
		switch ac.op {
		case attrExists:
		case attrEquals:
			if gotVal != wantVal {
				return false
			}
		case attrContains:
			if !strings.Contains(gotVal, wantVal) {
				return false
			}
		case attrPrefix:
			if !strings.HasPrefix(gotVal, wantVal) {
				return false
			}
		case attrSuffix:
			if !strings.HasSuffix(gotVal, wantVal) {
				return false
			}
		}
	}
	if s.firstChild && elementChildIndex(n) != 1 {
		return false
	}
	if s.lastChild && !isLastElementChild(n) {
		return false
	}
	if s.nthChild > 0 && elementChildIndex(n) != s.nthChild {
		return false
	}
	for _, neg := range s.notSelectors {
		if len(neg) > 0 && matchesComplex(n, nil, neg) {
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

func parseComplexSelector(raw string) complexSelector {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	var parts complexSelector
	comb := combNone
	i := 0
	for i < len(s) {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == '>' {
			comb = combChild
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
			if !inBracket && inParen == 0 && (isSpace(c) || c == '>') {
				break
			}
			i++
		}
		tok := strings.TrimSpace(s[start:i])
		if tok != "" {
			if len(parts) > 0 && comb == combNone {
				comb = combDescendant
			}
			parts = append(parts, selectorPart{
				comb:   comb,
				simple: parseSimpleSelector(tok),
			})
			comb = combNone
		}
	}
	return parts
}

func parseSimpleSelector(tok string) simpleSelector {
	var ss simpleSelector
	i := 0
	for i < len(tok) {
		switch tok[i] {
		case '#':
			i++
			start := i
			for i < len(tok) && isIdentChar(tok[i]) {
				i++
			}
			ss.id = tok[start:i]
		case '.':
			i++
			start := i
			for i < len(tok) && isIdentChar(tok[i]) {
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
			if ac, ok := parseAttrExpr(inner); ok {
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
				for _, g := range splitTopLevel(inner, ',') {
					if cs := parseComplexSelector(g); len(cs) > 0 {
						ss.notSelectors = append(ss.notSelectors, cs)
					}
				}
			}
		default:
			start := i
			for i < len(tok) && (isIdentChar(tok[i]) || tok[i] == '*') {
				i++
			}
			if i > start {
				ss.tag = tok[start:i]
			} else {
				i++
			}
		}
	}
	return ss
}

func parseAttrExpr(expr string) (attrCheck, bool) {
	if expr == "" {
		return attrCheck{}, false
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
			op := attrEquals
			nameEnd := i
			if i >= 2 {
				switch expr[i-1] {
				case '*':
					op = attrContains
					nameEnd = i - 1
				case '^':
					op = attrPrefix
					nameEnd = i - 1
				case '$':
					op = attrSuffix
					nameEnd = i - 1
				}
			}
			name := strings.TrimSpace(expr[:nameEnd])
			if name == "" {
				return attrCheck{}, false
			}
			val, ci := parseAttrValueAndFlag(expr[i+1:])
			return attrCheck{name: name, op: op, value: val, caseInsensitive: ci}, true
		}
	}
	return attrCheck{name: strings.TrimSpace(expr), op: attrExists}, true
}

func parseAttrValueAndFlag(raw string) (val string, caseInsensitive bool) {
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

func splitTopLevel(s string, sep byte) []string {
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

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
}
