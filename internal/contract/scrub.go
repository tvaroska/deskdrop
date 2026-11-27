package contract

import (
	"strings"
	"unicode/utf8"
)

const maxScrubbedHTMLBytes = 256 * 1024

// ScrubHTML removes active/visual noise (<script>, <style>, <svg>, <noscript>, comments)
// and redacts form field values (<input value=...>, <textarea>...</textarea>) using a pure-stdlib
// quote-aware HTML state-machine scanner before diagnostic packaging or exception verification.
func ScrubHTML(raw string) string {
	var out strings.Builder
	out.Grow(min(len(raw), maxScrubbedHTMLBytes))

	n := len(raw)
	i := 0
	for i < n {
		if raw[i] != '<' {
			out.WriteByte(raw[i])
			i++
			continue
		}

		// HTML comment: <!-- ... --> (unclosed strips to EOF).
		if strings.HasPrefix(raw[i:], "<!--") {
			if end := strings.Index(raw[i+4:], "-->"); end >= 0 {
				i += 4 + end + 3
			} else {
				i = n
			}
			continue
		}

		// CDATA section: <![CDATA[ ... ]]> (unclosed strips to EOF).
		if len(raw)-i >= 9 && strings.EqualFold(raw[i:i+9], "<![cdata[") {
			if end := strings.Index(raw[i+9:], "]]>"); end >= 0 {
				i += 9 + end + 3
			} else {
				i = n
			}
			continue
		}

		// Declarations (<!DOCTYPE ...>) or bogus comments / processing instructions (<!...>, <?...>).
		// Per HTML5 §13.2.5.41 & §13.2.5.58, '>' terminates immediately without quote-spanning.
		if i+1 < n && (raw[i+1] == '!' || raw[i+1] == '?') {
			end := scanRawGT(raw, i+2)
			if raw[i+1] == '!' && end <= n && raw[end-1] == '>' &&
				len(raw)-i >= 9 && strings.EqualFold(raw[i:i+9], "<!doctype") &&
				(i+9 == end-1 || isHTMLSpace(raw[i+9])) {
				out.WriteString("<!doctype html>")
			}
			i = end
			continue
		}

		// Closing tag (</tag ...>) or bogus closing comment (</>, </1...>).
		// Emit canonical </tag> without attributes and terminate on the first raw '>' so unclosed
		// quotes inside malformed closing tags can never swallow subsequent <script> or <input>.
		if i+1 < n && raw[i+1] == '/' {
			end := scanRawGT(raw, i+2)
			if end > n || end == i+2 || raw[end-1] != '>' {
				i = n
				continue
			}
			if i+2 < end-1 && isASCIILetter(raw[i+2]) {
				j := i + 2
				for j < end-1 && isTagNameChar(raw[j]) {
					j++
				}
				tagLower := strings.ToLower(raw[i+2 : j])
				switch tagLower {
				case "script", "style", "svg", "noscript":
				default:
					out.WriteString("</")
					out.WriteString(raw[i+2 : j])
					out.WriteByte('>')
				}
			}
			i = end
			continue
		}

		// Opening tag must start with ASCII letter; escape bare '<' so stripped comments/SVG cannot recombine into a tag.
		if i+1 >= n || !isASCIILetter(raw[i+1]) {
			out.WriteString("&lt;")
			i++
			continue
		}

		nameStart := i + 1
		j := nameStart
		for j < n && isTagNameChar(raw[j]) {
			j++
		}
		tagLower := strings.ToLower(raw[nameStart:j])
		tagEnd, attrs, selfClosing, closed := parseOpeningTag(raw, j)
		if !closed {
			// HTML5 eof-in-tag: drop unclosed tag at EOF so unclosed quotes cannot leak trailing tags.
			i = n
			continue
		}

		switch tagLower {
		case "script", "style", "noscript":
			// HTML5 raw-text elements are never self-closing; always skip through the matching end tag or EOF.
			i = skipUntilClosingTag(raw, tagEnd, tagLower)
		case "svg":
			i = tagEnd
			if !selfClosing {
				i = skipNestedSVG(raw, i)
			}
		case "textarea":
			// HTML5 RCDATA textarea is never self-closing; always redact body through </textarea> or EOF.
			writeSanitizedTag(&out, raw[nameStart:j], attrs, false, false)
			nextIdx, foundClose := findClosingTag(raw, tagEnd, "textarea")
			if foundClose {
				out.WriteString("</textarea>")
			}
			i = nextIdx
		case "input":
			writeSanitizedTag(&out, raw[nameStart:j], attrs, selfClosing, true)
			i = tagEnd
		default:
			writeSanitizedTag(&out, raw[nameStart:j], attrs, selfClosing, false)
			i = tagEnd
		}
	}

	s := strings.TrimSpace(out.String())
	if len(s) > maxScrubbedHTMLBytes {
		s = truncateUTF8Bytes(s, maxScrubbedHTMLBytes)
		if lastLT := strings.LastIndexByte(s, '<'); lastLT > strings.LastIndexByte(s, '>') {
			s = strings.TrimSpace(s[:lastLT])
		}
	}
	return s
}

type htmlAttrSpan struct {
	name      string
	value     string
	quote     byte
	hasValue  bool
	nameLower string
}

func parseOpeningTag(raw string, pos int) (end int, attrs []htmlAttrSpan, selfClosing bool, closed bool) {
	n := len(raw)
	i := pos
	for i < n {
		for i < n && isHTMLSpace(raw[i]) {
			i++
		}
		if i >= n {
			return n, attrs, selfClosing, false
		}
		if raw[i] == '>' {
			return i + 1, attrs, selfClosing, true
		}
		if raw[i] == '/' {
			if i+1 < n && raw[i+1] == '>' {
				return i + 2, attrs, true, true
			}
			i++
			continue
		}

		attrStart := i
		for i < n && !isHTMLSpace(raw[i]) && raw[i] != '=' && raw[i] != '>' && raw[i] != '/' {
			i++
		}
		if i == attrStart {
			i++
			continue
		}
		attrName := raw[attrStart:i]
		nameLower := strings.ToLower(attrName)

		for i < n && isHTMLSpace(raw[i]) {
			i++
		}
		hasVal := false
		valStr := ""
		quoteCh := byte(0)
		if i < n && raw[i] == '=' {
			hasVal = true
			i++
			for i < n && isHTMLSpace(raw[i]) {
				i++
			}
			if i >= n {
				return n, attrs, selfClosing, false
			}
			if raw[i] == '"' || raw[i] == '\'' {
				quoteCh = raw[i]
				i++
				valStart := i
				for i < n && raw[i] != quoteCh {
					i++
				}
				if i >= n {
					// Unclosed attribute quote reached EOF (HTML5 eof-in-tag).
					return n, attrs, selfClosing, false
				}
				valStr = raw[valStart:i]
				i++
			} else {
				valStart := i
				for i < n && !isHTMLSpace(raw[i]) && raw[i] != '>' {
					i++
				}
				valStr = raw[valStart:i]
			}
		}
		attrs = append(attrs, htmlAttrSpan{
			name:      attrName,
			value:     valStr,
			quote:     quoteCh,
			hasValue:  hasVal,
			nameLower: nameLower,
		})
	}
	return n, attrs, selfClosing, false
}

func writeSanitizedTag(out *strings.Builder, origTagName string, attrs []htmlAttrSpan, selfClosing bool, redactInputValue bool) {
	out.WriteByte('<')
	out.WriteString(origTagName)
	for _, a := range attrs {
		if strings.HasPrefix(a.nameLower, "on") || !isSafeAttrName(a.name) {
			continue
		}
		out.WriteByte(' ')
		if redactInputValue && a.nameLower == "value" {
			out.WriteString(`value=""`)
			continue
		}
		out.WriteString(a.name)
		if a.hasValue {
			q := a.quote
			if q == 0 {
				q = '"'
			}
			out.WriteByte('=')
			out.WriteByte(q)
			writeEscapedAttrValue(out, a.value, q)
			out.WriteByte(q)
		}
	}
	if selfClosing {
		out.WriteString(" />")
	} else {
		out.WriteByte('>')
	}
}

func isSafeAttrName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c <= 0x20 || c == '>' || c == '<' || c == '"' || c == '\'' || c == '/' || c == '=' {
			return false
		}
	}
	return true
}

func writeEscapedAttrValue(out *strings.Builder, val string, quote byte) {
	for i := 0; i < len(val); i++ {
		switch val[i] {
		case '<':
			out.WriteString("&lt;")
		case '>':
			out.WriteString("&gt;")
		case '"':
			if quote == '"' {
				out.WriteString("&quot;")
			} else {
				out.WriteByte('"')
			}
		case '\'':
			if quote == '\'' {
				out.WriteString("&#39;")
			} else {
				out.WriteByte('\'')
			}
		default:
			out.WriteByte(val[i])
		}
	}
}

// scanRawGT returns the index immediately after the next raw '>' byte, or len(raw) if none exists.
func scanRawGT(raw string, pos int) int {
	if pos >= len(raw) {
		return len(raw)
	}
	if idx := strings.IndexByte(raw[pos:], '>'); idx >= 0 {
		return pos + idx + 1
	}
	return len(raw)
}

func skipUntilClosingTag(raw string, pos int, tagLower string) int {
	nextIdx, _ := findClosingTag(raw, pos, tagLower)
	return nextIdx
}

func findClosingTag(raw string, pos int, tagLower string) (nextIdx int, found bool) {
	n := len(raw)
	tLen := len(tagLower)
	for i := pos; i < n; i++ {
		if raw[i] != '<' || i+2+tLen > n || raw[i+1] != '/' {
			continue
		}
		if !strings.EqualFold(raw[i+2:i+2+tLen], tagLower) {
			continue
		}
		after := i + 2 + tLen
		if after < n && !isHTMLSpace(raw[after]) && raw[after] != '>' && raw[after] != '/' {
			continue
		}
		end := scanRawGT(raw, after)
		if end <= n && end > after && raw[end-1] == '>' {
			return end, true
		}
		return n, false
	}
	return n, false
}

func skipNestedSVG(raw string, pos int) int {
	n := len(raw)
	depth := 1
	i := pos
	for i < n && depth > 0 {
		if raw[i] != '<' {
			i++
			continue
		}
		if strings.HasPrefix(raw[i:], "<!--") {
			if end := strings.Index(raw[i+4:], "-->"); end >= 0 {
				i += 4 + end + 3
			} else {
				return n
			}
			continue
		}
		if len(raw)-i >= 9 && strings.EqualFold(raw[i:i+9], "<![cdata[") {
			if end := strings.Index(raw[i+9:], "]]>"); end >= 0 {
				i += 9 + end + 3
			} else {
				return n
			}
			continue
		}
		if i+5 <= n && raw[i+1] == '/' && strings.EqualFold(raw[i+2:i+5], "svg") {
			after := i + 5
			if after >= n || isHTMLSpace(raw[after]) || raw[after] == '>' || raw[after] == '/' {
				end := scanRawGT(raw, after)
				if end > n || end == after || raw[end-1] != '>' {
					return n
				}
				i = end
				depth--
				continue
			}
		}
		if i+4 <= n && strings.EqualFold(raw[i+1:i+4], "svg") {
			after := i + 4
			if after >= n || isHTMLSpace(raw[after]) || raw[after] == '>' || raw[after] == '/' {
				end, _, selfClosing, closed := parseOpeningTag(raw, after)
				if !closed {
					return n
				}
				i = end
				if !selfClosing {
					depth++
				}
				continue
			}
		}
		if i+1 < n && (raw[i+1] == '/' || raw[i+1] == '!' || raw[i+1] == '?') {
			i = scanRawGT(raw, i+2)
			continue
		}
		if i+1 < n && isASCIILetter(raw[i+1]) {
			j := i + 1
			for j < n && isTagNameChar(raw[j]) {
				j++
			}
			end, _, _, closed := parseOpeningTag(raw, j)
			if !closed {
				return n
			}
			i = end
			continue
		}
		i++
	}
	return i
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isTagNameChar(c byte) bool {
	return isASCIILetter(c) || (c >= '0' && c <= '9') || c == '-' || c == ':' || c == '_'
}

func isHTMLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

func truncateUTF8Bytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
