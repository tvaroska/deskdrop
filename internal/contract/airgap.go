package contract

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type builtinAirGapRule struct {
	name      string
	re        *regexp.Regexp
	checkSafe bool
}

// Builtin patterns blocking accidental internal or corporate leaks on outbound payloads.
var builtinForbiddenRules = []builtinAirGapRule{
	{name: "depot_path", re: regexp.MustCompile("(?i)" + "//" + "depot/")},
	{name: "bug_tracker_id", re: regexp.MustCompile(`(?i)(?:^|https?://|[^a-zA-Z0-9./-])(` + "b" + `/\d{6,})(?:$|[^0-9])`)},
	{name: "internal_shortlink_url", re: regexp.MustCompile(`(?i)https?://(` + "go" + `/[a-zA-Z0-9_-]{3,})(?:$|[^a-zA-Z0-9-])`), checkSafe: true},
	{name: "internal_shortlink", re: regexp.MustCompile(`(?i)(?:^|[^a-zA-Z0-9./-])(` + "go" + `/[a-zA-Z0-9_-]{3,})(?:$|[^a-zA-Z0-9-])`), checkSafe: true},
	{name: "corp_hostname", re: regexp.MustCompile(`(?i)(?:^|[^a-zA-Z0-9.-])(?:[a-zA-Z0-9-]+\.)*` + "corp" + `\.google\.com(?:$|[^a-zA-Z0-9-])`)},
}

var jsonUnicodeBoundaryEscRe = regexp.MustCompile(`(?i)\\u00(?:0[0-9a-f]|1[0-9a-f]|2[0-9a-f]|3[a-f]|5[b-f]|7[b-f])`)

var safeGoRefs = map[string]bool{
	"go/mod":     true,
	"go/sum":     true,
	"go/work":    true,
	"go/version": true,
	"go/build":   true,
}

func normalizeJSONEscapedBoundaries(text string) string {
	if strings.IndexByte(text, '\\') < 0 {
		return ""
	}
	s := jsonUnicodeBoundaryEscRe.ReplaceAllString(text, " ")
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n', 'r', 't', 'b', 'f', '"', '\'', '/', '\\':
				b.WriteByte(' ')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func scanAirGapCandidate(s string, extraTerms []string) error {
	for _, rule := range builtinForbiddenRules {
		offset := 0
		for offset < len(s) {
			loc := rule.re.FindStringSubmatchIndex(s[offset:])
			if loc == nil {
				break
			}
			token := s[offset+loc[0] : offset+loc[1]]
			advance := loc[1]
			if len(loc) >= 4 && loc[2] >= 0 && loc[3] > loc[2] {
				token = s[offset+loc[2] : offset+loc[3]]
				// Advance only to the end of the captured token so a trailing delimiter
				// remains intact for evaluating immediately adjacent tokens.
				advance = loc[3]
			}
			if advance <= 0 {
				advance = 1
			}
			if rule.checkSafe {
				lowerTok := strings.ToLower(token)
				lowerTok = strings.TrimPrefix(lowerTok, "https://")
				lowerTok = strings.TrimPrefix(lowerTok, "http://")
				if safeGoRefs[lowerTok] {
					offset += advance
					continue
				}
			}
			return fmt.Errorf("air-gap guard blocked outbound payload (rule: %s)", rule.name)
		}
	}

	if len(extraTerms) == 0 {
		return nil
	}
	lowerText := strings.ToLower(s)
	for _, term := range extraTerms {
		t := strings.TrimSpace(term)
		if t == "" {
			continue
		}
		if strings.Contains(lowerText, strings.ToLower(t)) {
			return errors.New("air-gap guard blocked outbound payload matching configured forbidden term")
		}
	}
	return nil
}

// EnforceAirGap verifies text contains zero forbidden internal or caller-supplied terms.
// Extra terms are matched literally (case-insensitive) and matched values are never echoed in errors.
func EnforceAirGap(text string, extraTerms []string) error {
	if err := scanAirGapCandidate(text, extraTerms); err != nil {
		return err
	}
	if unescaped := normalizeJSONEscapedBoundaries(text); unescaped != "" {
		if err := scanAirGapCandidate(unescaped, extraTerms); err != nil {
			return err
		}
	}
	return nil
}

// LoadLocalGuardTerms reads optional workstation-local guard terms once at process startup.
// Keeping this outside EnforceAirGap ensures unit tests and library callers remain hermetic.
func LoadLocalGuardTerms() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	p := filepath.Join(home, ".config", "agent-guard", "customer_terms.txt")
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		out = append(out, t)
	}
	return out
}
