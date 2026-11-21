package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Builtin patterns blocking accidental internal or corporate leaks on outbound payloads.
var builtinForbiddenPatterns = []*regexp.Regexp{
	regexp.MustCompile("(?i)" + "//" + "depot/"),
	regexp.MustCompile(`(?i)\b` + "b" + `/\d{6,}\b`),
	regexp.MustCompile(`(?i)\b` + "go" + `/[a-zA-Z0-9_-]{3,}\b`),
	regexp.MustCompile(`(?i)\.` + "corp" + `\.google\.com`),
}

var safeGoRefs = map[string]bool{
	"go/mod":     true,
	"go/sum":     true,
	"go/work":    true,
	"go/version": true,
	"go/build":   true,
}

// EnforceAirGap verifies text contains zero forbidden internal or user-configured terms.
func EnforceAirGap(text string, extraTerms []string) error {
	for _, re := range builtinForbiddenPatterns {
		matches := re.FindAllString(text, -1)
		for _, m := range matches {
			if safeGoRefs[strings.ToLower(m)] {
				continue
			}
			return fmt.Errorf("air-gap guard blocked outbound payload matching pattern %q", m)
		}
	}

	allTerms := append([]string{}, extraTerms...)
	allTerms = append(allTerms, loadLocalGuardTerms()...)
	for _, term := range allTerms {
		t := strings.TrimSpace(term)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		re, err := regexp.Compile("(?i)" + t)
		if err != nil {
			if strings.Contains(strings.ToLower(text), strings.ToLower(t)) {
				return fmt.Errorf("air-gap guard blocked outbound payload matching term %q", t)
			}
			continue
		}
		if m := re.FindString(text); m != "" {
			return fmt.Errorf("air-gap guard blocked outbound payload matching term %q", m)
		}
	}
	return nil
}

func loadLocalGuardTerms() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	p := filepath.Join(home, ".config", "agent-guard", "customer_terms.txt")
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	return strings.Split(string(raw), "\n")
}
