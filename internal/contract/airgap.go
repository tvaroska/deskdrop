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
	{name: "bug_tracker_id", re: regexp.MustCompile(`(?i)\b` + "b" + `/\d{6,}\b`)},
	{name: "internal_shortlink_url", re: regexp.MustCompile(`(?i)https?://` + "go" + `/[a-zA-Z0-9_-]{3,}\b`), checkSafe: true},
	{name: "internal_shortlink", re: regexp.MustCompile(`(?i)(?:^|[^a-zA-Z0-9_./-])(` + "go" + `/[a-zA-Z0-9_-]{3,})\b`), checkSafe: true},
	{name: "corp_hostname", re: regexp.MustCompile(`(?i)\.` + "corp" + `\.google\.com`)},
}

var safeGoRefs = map[string]bool{
	"go/mod":     true,
	"go/sum":     true,
	"go/work":    true,
	"go/version": true,
	"go/build":   true,
}

// EnforceAirGap verifies text contains zero forbidden internal or caller-supplied terms.
// Extra terms are matched literally (case-insensitive) and matched values are never echoed in errors.
func EnforceAirGap(text string, extraTerms []string) error {
	for _, rule := range builtinForbiddenRules {
		matches := rule.re.FindAllStringSubmatch(text, -1)
		for _, m := range matches {
			token := m[0]
			if len(m) > 1 && m[1] != "" {
				token = m[1]
			}
			if rule.checkSafe {
				lowerTok := strings.ToLower(token)
				lowerTok = strings.TrimPrefix(lowerTok, "https://")
				lowerTok = strings.TrimPrefix(lowerTok, "http://")
				if safeGoRefs[lowerTok] {
					continue
				}
			}
			return fmt.Errorf("air-gap guard blocked outbound payload (rule: %s)", rule.name)
		}
	}

	if len(extraTerms) == 0 {
		return nil
	}
	lowerText := strings.ToLower(text)
	for _, term := range extraTerms {
		t := strings.TrimSpace(term)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.Contains(lowerText, strings.ToLower(t)) {
			return errors.New("air-gap guard blocked outbound payload matching configured forbidden term")
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
