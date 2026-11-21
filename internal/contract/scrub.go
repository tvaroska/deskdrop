package contract

import (
	"regexp"
	"strings"
)

const maxScrubbedHTMLBytes = 256 * 1024

var (
	stripScriptRe   = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
	stripStyleRe    = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
	stripSVGRe      = regexp.MustCompile(`(?is)<svg\b[^>]*>.*?</svg>`)
	stripNoScriptRe = regexp.MustCompile(`(?is)<noscript\b[^>]*>.*?</noscript>`)
	stripCommentRe  = regexp.MustCompile(`(?s)<!--.*?-->`)
	stripTextareaRe = regexp.MustCompile(`(?is)(<textarea\b[^>]*>).*?(</textarea>)`)
	inputTagRe      = regexp.MustCompile(`(?is)<input\b[^>]*>`)
	inputValueRe    = regexp.MustCompile(`(?is)\bvalue\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+)`)
)

// ScrubHTML removes active/visual noise (<script>, <style>, <svg>, <noscript>, comments)
// and redacts form field values (<input value=...>, <textarea>...</textarea>) before
// diagnostic bundle packaging or exception-route verification.
func ScrubHTML(raw string) string {
	s := stripScriptRe.ReplaceAllString(raw, "")
	s = stripStyleRe.ReplaceAllString(s, "")
	s = stripSVGRe.ReplaceAllString(s, "")
	s = stripNoScriptRe.ReplaceAllString(s, "")
	s = stripCommentRe.ReplaceAllString(s, "")
	s = stripTextareaRe.ReplaceAllString(s, "$1$2")
	s = inputTagRe.ReplaceAllStringFunc(s, func(tag string) string {
		return inputValueRe.ReplaceAllString(tag, `value=""`)
	})
	s = strings.TrimSpace(s)
	if len(s) > maxScrubbedHTMLBytes {
		s = s[:maxScrubbedHTMLBytes]
	}
	return s
}
