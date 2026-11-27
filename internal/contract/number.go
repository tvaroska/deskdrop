package contract

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

var (
	invisibleCharsRe = regexp.MustCompile("[\u200b\u00a0\ufeff]+")
	whitespaceRunRe  = regexp.MustCompile(`\s+`)
	compactNumRe     = regexp.MustCompile(`([+-]?(?:\d+(?:\.\d+)?|\.\d+))(?:\s*([KkMmBb])\b)?`)
)

// NormalizeWhitespace removes invisible Unicode separators and collapses whitespace runs.
func NormalizeWhitespace(raw string) string {
	s := invisibleCharsRe.ReplaceAllString(raw, " ")
	s = whitespaceRunRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// ParseCompactFloat parses formatted human numbers such as "1,234", "1.2K", "3.5M", ".5%", or "42.7%".
func ParseCompactFloat(raw string) (float64, bool) {
	cleaned := strings.ReplaceAll(NormalizeWhitespace(raw), ",", "")
	m := compactNumRe.FindStringSubmatch(cleaned)
	if len(m) < 2 {
		return 0, false
	}
	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil || math.IsNaN(val) || math.IsInf(val, 0) {
		return 0, false
	}
	if len(m) >= 3 {
		switch strings.ToUpper(m[2]) {
		case "K":
			val *= 1_000
		case "M":
			val *= 1_000_000
		case "B":
			val *= 1_000_000_000
		}
	}
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return 0, false
	}
	return val, true
}

// ParseCompactNumber parses formatted human counts ("1,234", "1.2K", "3.5M") into a rounded int64,
// failing closed if the rounded magnitude overflows signed 64-bit integer bounds.
func ParseCompactNumber(raw string) (int64, bool) {
	val, ok := ParseCompactFloat(raw)
	if !ok {
		return 0, false
	}
	rounded := math.Round(val)
	if math.IsNaN(rounded) || math.IsInf(rounded, 0) || rounded >= math.Ldexp(1, 63) || rounded < -math.Ldexp(1, 63) {
		return 0, false
	}
	return int64(rounded), true
}
