package contract

import (
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
	CSSCombNone       CSSCombinator = 0
	CSSCombDescendant CSSCombinator = ' '
	CSSCombChild      CSSCombinator = '>'
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

// ParseCSSSelectorList parses a comma-separated CSS selector list, flattening Shadow-DOM '>>>' combinators.
func ParseCSSSelectorList(rawSelector string) []CSSComplexSelector {
	if strings.TrimSpace(rawSelector) == "" {
		return nil
	}
	normalized := strings.ReplaceAll(rawSelector, ">>>", " ")
	var groups []CSSComplexSelector
	for _, g := range SplitCSSTopLevel(normalized, ',') {
		if cs := ParseCSSComplexSelector(g); len(cs) > 0 {
			groups = append(groups, cs)
		}
	}
	return groups
}

// ParseCSSComplexSelector parses a single combinator-delimited complex selector.
func ParseCSSComplexSelector(raw string) CSSComplexSelector {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	var parts CSSComplexSelector
	comb := CSSCombNone
	i := 0
	for i < len(s) {
		for i < len(s) && isCSSSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == '>' {
			comb = CSSCombChild
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
			if !inBracket && inParen == 0 && (isCSSSpace(c) || c == '>') {
				break
			}
			i++
		}
		tok := strings.TrimSpace(s[start:i])
		if tok != "" {
			if len(parts) > 0 && comb == CSSCombNone {
				comb = CSSCombDescendant
			}
			parts = append(parts, CSSSelectorPart{
				Comb:   comb,
				Simple: ParseCSSSimpleSelector(tok),
			})
			comb = CSSCombNone
		}
	}
	return parts
}

// ParseCSSSimpleSelector parses a single compound selector token (e.g., button.primary[aria-label*="ok" i]:not(.skip)).
func ParseCSSSimpleSelector(tok string) CSSSimpleSelector {
	var ss CSSSimpleSelector
	i := 0
	for i < len(tok) {
		switch tok[i] {
		case '#':
			i++
			start := i
			for i < len(tok) && isCSSIdent(tok[i]) {
				i++
			}
			ss.ID = tok[start:i]
		case '.':
			i++
			start := i
			for i < len(tok) && isCSSIdent(tok[i]) {
				i++
			}
			if i > start {
				ss.Classes = append(ss.Classes, tok[start:i])
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
			if ac, ok := parseCSSAttrExpr(inner); ok {
				ss.Attrs = append(ss.Attrs, ac)
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
				ss.FirstChild = true
			case pseudo == "last-child":
				ss.LastChild = true
			case strings.HasPrefix(pseudo, "nth-child(") && strings.HasSuffix(pseudo, ")"):
				numStr := strings.TrimSpace(pseudo[len("nth-child(") : len(pseudo)-1])
				if n, err := strconv.Atoi(numStr); err == nil && n > 0 {
					ss.NthChild = n
				}
			case strings.HasPrefix(pseudo, "not(") && strings.HasSuffix(rawPseudo, ")"):
				inner := strings.TrimSpace(rawPseudo[4 : len(rawPseudo)-1])
				for _, g := range SplitCSSTopLevel(inner, ',') {
					if cs := ParseCSSComplexSelector(g); len(cs) > 0 {
						ss.NotSelectors = append(ss.NotSelectors, cs)
					}
				}
			}
		default:
			start := i
			for i < len(tok) && (isCSSIdent(tok[i]) || tok[i] == '*') {
				i++
			}
			if i > start {
				ss.Tag = tok[start:i]
			} else {
				i++
			}
		}
	}
	return ss
}

func parseCSSAttrExpr(expr string) (CSSAttrCheck, bool) {
	if expr == "" {
		return CSSAttrCheck{}, false
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
			if name == "" {
				return CSSAttrCheck{}, false
			}
			val, ci := parseCSSAttrValueAndFlag(expr[i+1:])
			return CSSAttrCheck{Name: name, Op: op, Value: val, CaseInsensitive: ci}, true
		}
	}
	return CSSAttrCheck{Name: strings.TrimSpace(expr), Op: CSSAttrExists}, true
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

// SplitCSSTopLevel splits a selector list on sep while ignoring separators inside quotes, brackets, or parens.
func SplitCSSTopLevel(s string, sep byte) []string {
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

func isCSSSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isCSSIdent(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
}
