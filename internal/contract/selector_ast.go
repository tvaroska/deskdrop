package contract

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// CSSAttrOp enumerates supported CSS attribute comparison operators.
type CSSAttrOp string

const (
	CSSAttrExists   CSSAttrOp = ""
	CSSAttrEquals   CSSAttrOp = "="
	CSSAttrContains CSSAttrOp = "*="
	CSSAttrPrefix   CSSAttrOp = "^="
	CSSAttrSuffix   CSSAttrOp = "$="
)

// CSSAttrCheck represents a single [attr], [attr=val], [attr*=val i], [attr^=val], or [attr$=val] predicate.
type CSSAttrCheck struct {
	Name            string
	Op              CSSAttrOp
	Value           string
	CaseInsensitive bool
}

// CSSCombinator separates adjacent parts of a complex selector.
type CSSCombinator byte

const (
	CSSCombNone              CSSCombinator = 0
	CSSCombDescendant        CSSCombinator = ' '
	CSSCombChild             CSSCombinator = '>'
	CSSCombNextSibling       CSSCombinator = '+'
	CSSCombSubsequentSibling CSSCombinator = '~'
)

// CSSSimpleSelector is a parsed compound simple selector (tag/role, #id, .classes, [attrs], pseudos).
type CSSSimpleSelector struct {
	Tag          string
	ID           string
	Classes      []string
	Attrs        []CSSAttrCheck
	FirstChild   bool
	LastChild    bool
	NthChild     int
	NotSelectors []CSSComplexSelector
}

// CSSSelectorPart pairs a combinator with the simple selector that follows it.
type CSSSelectorPart struct {
	Comb   CSSCombinator
	Simple CSSSimpleSelector
}

// CSSComplexSelector is an ordered chain of simple selectors joined by combinators.
type CSSComplexSelector []CSSSelectorPart

// ValidateCSSSelector checks that rawSelector is syntactically valid for declarative JS, Go HTML, and semantic evaluation.
func ValidateCSSSelector(rawSelector string) error {
	_, err := parseCSSSelectorListChecked(rawSelector)
	return err
}

// ParseCSSSelectorList parses a comma-separated CSS selector list, flattening Shadow-DOM '>>>' combinators.
// Malformed selectors fail closed (returning nil) so unsupported syntax never silently widens a match.
func ParseCSSSelectorList(rawSelector string) []CSSComplexSelector {
	groups, err := parseCSSSelectorListChecked(rawSelector)
	if err != nil {
		return nil
	}
	return groups
}

func parseCSSSelectorListChecked(rawSelector string) ([]CSSComplexSelector, error) {
	if strings.TrimSpace(rawSelector) == "" {
		return nil, errors.New("selector must not be empty")
	}
	rawGroups, err := splitCSSTopLevelChecked(rawSelector, ',')
	if err != nil {
		return nil, err
	}
	var groups []CSSComplexSelector
	for _, g := range rawGroups {
		if g == "" {
			return nil, fmt.Errorf("empty selector group in %q", rawSelector)
		}
		flattened, ferr := flattenShadowCombinators(g)
		if ferr != nil {
			return nil, ferr
		}
		cs, cerr := parseCSSComplexSelectorChecked(flattened)
		if cerr != nil {
			return nil, cerr
		}
		groups = append(groups, cs)
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("selector %q produced zero valid groups", rawSelector)
	}
	return groups, nil
}

func flattenShadowCombinators(group string) (string, error) {
	var b strings.Builder
	inBracket := false
	inParen := 0
	inQuote := byte(0)
	segStart := 0
	sawHop := false
	for i := 0; i < len(group); i++ {
		c := group[i]
		if inQuote != 0 {
			if c == '\\' && i+1 < len(group) {
				i++
				continue
			}
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
		case '>':
			if !inBracket && inParen == 0 && i+2 < len(group) && group[i+1] == '>' && group[i+2] == '>' {
				seg := strings.TrimSpace(group[segStart:i])
				if seg == "" {
					return "", fmt.Errorf("empty Shadow-DOM '>>>' hop in %q", group)
				}
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(seg)
				sawHop = true
				i += 2
				segStart = i + 1
			}
		}
	}
	tail := strings.TrimSpace(group[segStart:])
	if sawHop && tail == "" {
		return "", fmt.Errorf("trailing Shadow-DOM '>>>' hop in %q", group)
	}
	if !sawHop {
		return group, nil
	}
	b.WriteByte(' ')
	b.WriteString(tail)
	return b.String(), nil
}

// ParseCSSComplexSelector parses a single combinator-delimited complex selector, failing closed on syntax errors.
func ParseCSSComplexSelector(raw string) CSSComplexSelector {
	cs, err := parseCSSComplexSelectorChecked(raw)
	if err != nil {
		return nil
	}
	return cs
}

func isExplicitCSSCombinator(c byte) bool {
	return c == '>' || c == '+' || c == '~'
}

func parseCSSComplexSelectorChecked(raw string) (CSSComplexSelector, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errors.New("empty selector")
	}
	var parts CSSComplexSelector
	comb := CSSCombNone
	hasExplicitComb := false
	i := 0
	for i < len(s) {
		for i < len(s) && isCSSSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}
		if isExplicitCSSCombinator(s[i]) {
			if len(parts) == 0 || hasExplicitComb {
				return nil, fmt.Errorf("unexpected combinator %q in selector %q", s[i], raw)
			}
			comb = CSSCombinator(s[i])
			hasExplicitComb = true
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
				if c == '\\' && i+1 < len(s) {
					i += 2
					continue
				}
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
				if !inBracket {
					return nil, fmt.Errorf("unmatched ']' in selector %q", raw)
				}
				inBracket = false
				i++
				continue
			}
			if c == '(' {
				inParen++
				i++
				continue
			}
			if c == ')' {
				if inParen == 0 {
					return nil, fmt.Errorf("unmatched ')' in selector %q", raw)
				}
				inParen--
				i++
				continue
			}
			if !inBracket && inParen == 0 && (isCSSSpace(c) || isExplicitCSSCombinator(c)) {
				break
			}
			i++
		}
		if inQuote != 0 || inBracket || inParen != 0 {
			return nil, fmt.Errorf("unclosed delimiter in selector %q", raw)
		}
		tok := strings.TrimSpace(s[start:i])
		if tok == "" {
			return nil, fmt.Errorf("empty compound token in selector %q", raw)
		}
		simple, err := parseCSSSimpleSelectorChecked(tok)
		if err != nil {
			return nil, err
		}
		if len(parts) > 0 && comb == CSSCombNone {
			comb = CSSCombDescendant
		}
		parts = append(parts, CSSSelectorPart{
			Comb:   comb,
			Simple: simple,
		})
		comb = CSSCombNone
		hasExplicitComb = false
	}
	if hasExplicitComb || len(parts) == 0 {
		return nil, fmt.Errorf("dangling combinator at end of selector %q", raw)
	}
	return parts, nil
}

// ParseCSSSimpleSelector parses a single compound selector token, returning zero value if invalid.
func ParseCSSSimpleSelector(tok string) CSSSimpleSelector {
	ss, err := parseCSSSimpleSelectorChecked(tok)
	if err != nil {
		return CSSSimpleSelector{}
	}
	return ss
}

func parseCSSSimpleSelectorChecked(tok string) (CSSSimpleSelector, error) {
	if tok == "" {
		return CSSSimpleSelector{}, errors.New("empty simple selector")
	}
	var ss CSSSimpleSelector
	i := 0
	// Tag or universal '*' is valid only at index 0 of a compound selector.
	if tok[0] == '*' {
		ss.Tag = "*"
		i = 1
	} else if isCSSIdent(tok[0]) {
		for i < len(tok) && isCSSIdent(tok[i]) {
			i++
		}
		ss.Tag = tok[:i]
	}

	for i < len(tok) {
		switch tok[i] {
		case '#':
			i++
			start := i
			for i < len(tok) && isCSSIdent(tok[i]) {
				i++
			}
			if i == start {
				return CSSSimpleSelector{}, fmt.Errorf("empty #id in selector %q", tok)
			}
			ss.ID = tok[start:i]
		case '.':
			i++
			start := i
			for i < len(tok) && isCSSIdent(tok[i]) {
				i++
			}
			if i == start {
				return CSSSimpleSelector{}, fmt.Errorf("empty .class in selector %q", tok)
			}
			ss.Classes = append(ss.Classes, tok[start:i])
		case '[':
			i++
			start := i
			inQ := byte(0)
			for i < len(tok) {
				if inQ != 0 {
					if tok[i] == '\\' && i+1 < len(tok) {
						i += 2
						continue
					}
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
			if i >= len(tok) || tok[i] != ']' {
				return CSSSimpleSelector{}, fmt.Errorf("unclosed '[' in selector %q", tok)
			}
			inner := strings.TrimSpace(tok[start:i])
			i++
			ac, ok := parseCSSAttrExpr(inner)
			if !ok {
				return CSSSimpleSelector{}, fmt.Errorf("invalid attribute selector [%s] in %q", inner, tok)
			}
			ss.Attrs = append(ss.Attrs, ac)
		case ':':
			i++
			if i < len(tok) && tok[i] == ':' {
				return CSSSimpleSelector{}, fmt.Errorf("unsupported pseudo-element in selector %q", tok)
			}
			start := i
			inParen := 0
			inQ := byte(0)
			for i < len(tok) {
				if inQ != 0 {
					if tok[i] == '\\' && i+1 < len(tok) {
						i += 2
						continue
					}
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
				ss.FirstChild = true
			case pseudo == "last-child":
				ss.LastChild = true
			case strings.HasPrefix(pseudo, "nth-child(") && strings.HasSuffix(pseudo, ")"):
				numStr := strings.TrimSpace(pseudo[len("nth-child(") : len(pseudo)-1])
				n, err := strconv.Atoi(numStr)
				if err != nil || n <= 0 {
					return CSSSimpleSelector{}, fmt.Errorf("invalid :nth-child(%s) in selector %q", numStr, tok)
				}
				ss.NthChild = n
			case strings.HasPrefix(pseudo, "not(") && strings.HasSuffix(rawPseudo, ")"):
				inner := strings.TrimSpace(rawPseudo[4 : len(rawPseudo)-1])
				if strings.Contains(inner, ">>>") {
					return CSSSimpleSelector{}, fmt.Errorf("Shadow-DOM '>>>' is not allowed inside :not(%s) in selector %q", inner, tok)
				}
				negGroups, err := parseCSSSelectorListChecked(inner)
				if err != nil {
					return CSSSimpleSelector{}, fmt.Errorf("invalid :not(%s) in selector %q: %w", inner, tok, err)
				}
				ss.NotSelectors = append(ss.NotSelectors, negGroups...)
			default:
				return CSSSimpleSelector{}, fmt.Errorf("unsupported pseudo-class :%s in selector %q", rawPseudo, tok)
			}
		default:
			return CSSSimpleSelector{}, fmt.Errorf("unexpected character %q in selector %q", tok[i], tok)
		}
	}
	return ss, nil
}

func isValidCSSAttrName(name string) bool {
	if name == "" {
		return false
	}
	c0 := name[0]
	if !((c0 >= 'a' && c0 <= 'z') || (c0 >= 'A' && c0 <= 'Z') || c0 == '_' || c0 == '-') {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if !isCSSIdent(c) && c != ':' {
			return false
		}
	}
	return true
}

func parseCSSAttrExpr(expr string) (CSSAttrCheck, bool) {
	if expr == "" {
		return CSSAttrCheck{}, false
	}
	inQ := byte(0)
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if inQ != 0 {
			if c == '\\' && i+1 < len(expr) {
				i++
				continue
			}
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
			op := CSSAttrEquals
			nameEnd := i
			if i >= 2 {
				switch expr[i-1] {
				case '*':
					op = CSSAttrContains
					nameEnd = i - 1
				case '^':
					op = CSSAttrPrefix
					nameEnd = i - 1
				case '$':
					op = CSSAttrSuffix
					nameEnd = i - 1
				}
			}
			name := strings.TrimSpace(expr[:nameEnd])
			if !isValidCSSAttrName(name) {
				return CSSAttrCheck{}, false
			}
			val, ci, ok := parseCSSAttrValueAndFlag(expr[i+1:])
			if !ok {
				return CSSAttrCheck{}, false
			}
			return CSSAttrCheck{Name: name, Op: op, Value: val, CaseInsensitive: ci}, true
		}
	}
	if inQ != 0 {
		return CSSAttrCheck{}, false
	}
	name := strings.TrimSpace(expr)
	if !isValidCSSAttrName(name) {
		return CSSAttrCheck{}, false
	}
	return CSSAttrCheck{Name: name, Op: CSSAttrExists}, true
}

func parseCSSAttrValueAndFlag(raw string) (val string, caseInsensitive bool, ok bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false, false
	}
	if s[0] == '"' || s[0] == '\'' {
		q := s[0]
		var b strings.Builder
		i := 1
		closed := false
		for i < len(s) {
			if s[i] == '\\' && i+1 < len(s) {
				b.WriteByte(s[i+1])
				i += 2
				continue
			}
			if s[i] == q {
				closed = true
				i++
				break
			}
			b.WriteByte(s[i])
			i++
		}
		if !closed {
			return "", false, false
		}
		rest := strings.TrimSpace(s[i:])
		if rest != "" && !strings.EqualFold(rest, "i") {
			return "", false, false
		}
		return b.String(), strings.EqualFold(rest, "i"), true
	}
	fields := strings.Fields(s)
	if len(fields) == 0 || len(fields) > 2 {
		return "", false, false
	}
	if len(fields) == 2 {
		if !strings.EqualFold(fields[1], "i") {
			return "", false, false
		}
	}
	tok := fields[0]
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if !isCSSIdent(c) && c != ':' && c != '.' && c != '/' {
			return "", false, false
		}
	}
	return tok, len(fields) == 2, true
}

// SplitCSSTopLevel splits a selector list on sep while ignoring separators inside quotes, brackets, or parens.
func SplitCSSTopLevel(s string, sep byte) []string {
	out, _ := splitCSSTopLevelChecked(s, sep)
	return out
}

func splitCSSTopLevelChecked(s string, sep byte) ([]string, error) {
	var out []string
	start := 0
	inBracket := false
	inParen := 0
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote != 0 {
			if c == '\\' && i+1 < len(s) {
				i++
				continue
			}
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
			if !inBracket {
				return nil, fmt.Errorf("unmatched ']' in selector %q", s)
			}
			inBracket = false
		case '(':
			inParen++
		case ')':
			if inParen == 0 {
				return nil, fmt.Errorf("unmatched ')' in selector %q", s)
			}
			inParen--
		default:
			if c == sep && !inBracket && inParen == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(s[start:]))
	if inQuote != 0 || inBracket || inParen != 0 {
		return out, fmt.Errorf("unclosed quote, bracket, or parenthesis in selector %q", s)
	}
	return out, nil
}

func isCSSSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isCSSIdent(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
}
