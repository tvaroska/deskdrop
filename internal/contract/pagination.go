package contract

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	PaginationModeClick          = "click"
	PaginationModeScroll         = "scroll"
	DefaultPaginationMaxPages    = 3
	MaxPaginationPages           = 20
	DefaultPaginationWaitMs      = 600
	MaxPaginationWaitMs          = 10_000
	DefaultPaginationScrollDelta = 900
	MaxPaginationScrollDelta     = 5_000
	MaxMergedTableRows           = 500
	maxMergedTables              = 10
)

// IntPtr returns a pointer to v (useful for explicit PaginationRule.WaitMs values such as 0).
func IntPtr(v int) *int {
	return &v
}

// PaginationRule configures bounded declarative multi-page extraction ("click" Next control or "scroll" feed) within a Step.
type PaginationRule struct {
	Mode         string `json:"mode,omitempty"`
	NextSelector string `json:"next_selector,omitempty"`
	ScrollDeltaY int    `json:"scroll_delta_y,omitempty"`
	MaxPages     int    `json:"max_pages,omitempty"`
	WaitMs       *int   `json:"wait_ms,omitempty"`
	WaitSelector string `json:"wait_selector,omitempty"`
	StopOnNoNew  *bool  `json:"stop_on_no_new,omitempty"`
}

// NormalizedMode returns "scroll" when explicitly configured, defaulting to "click".
func (p PaginationRule) NormalizedMode() string {
	if strings.EqualFold(strings.TrimSpace(p.Mode), PaginationModeScroll) {
		return PaginationModeScroll
	}
	return PaginationModeClick
}

// EffectiveScrollDeltaY returns the configured scroll_delta_y in pixels or DefaultPaginationScrollDelta when unset.
func (p PaginationRule) EffectiveScrollDeltaY() int {
	if p.ScrollDeltaY <= 0 {
		return DefaultPaginationScrollDelta
	}
	return p.ScrollDeltaY
}

// EffectiveWaitMs returns the configured wait_ms (allowing explicit 0) or DefaultPaginationWaitMs when omitted.
func (p PaginationRule) EffectiveWaitMs() int {
	if p.WaitMs == nil {
		return DefaultPaginationWaitMs
	}
	return *p.WaitMs
}

// StopWhenNoNew defaults to true so pagination stops immediately when advancing produces zero new items/rows.
func (p PaginationRule) StopWhenNoNew() bool {
	if p.StopOnNoNew == nil {
		return true
	}
	return *p.StopOnNoNew
}

// ValidatePaginationRule resolves defaults and enforces bounds on p without variable substitution.
func ValidatePaginationRule(stepID string, in *PaginationRule) (*PaginationRule, error) {
	resolved, err := resolvePaginationRule(stepID, in, func(s string) string { return s })
	if err != nil || resolved == nil {
		return resolved, err
	}
	if !varPlaceholderRe.MatchString(resolved.NextSelector) {
		if err := validateOptionalSelector(fmt.Sprintf("step %q", stepID), "pagination.next_selector", resolved.NextSelector); err != nil {
			return nil, err
		}
	}
	if !varPlaceholderRe.MatchString(resolved.WaitSelector) {
		if err := validateOptionalSelector(fmt.Sprintf("step %q", stepID), "pagination.wait_selector", resolved.WaitSelector); err != nil {
			return nil, err
		}
	}
	return resolved, nil
}

func resolvePaginationRule(stepID string, in *PaginationRule, sub func(string) string) (*PaginationRule, error) {
	if in == nil {
		return nil, nil
	}
	p := *in
	mode := strings.ToLower(strings.TrimSpace(sub(p.Mode)))
	if mode == "" {
		mode = PaginationModeClick
	}
	p.NextSelector = strings.TrimSpace(sub(p.NextSelector))
	p.WaitSelector = strings.TrimSpace(sub(p.WaitSelector))

	switch mode {
	case PaginationModeClick:
		if p.NextSelector == "" {
			return nil, fmt.Errorf("step %q pagination.next_selector is required", stepID)
		}
		if p.ScrollDeltaY != 0 {
			return nil, fmt.Errorf("step %q pagination.scroll_delta_y requires mode %q", stepID, PaginationModeScroll)
		}
	case PaginationModeScroll:
		if p.NextSelector != "" {
			return nil, fmt.Errorf("step %q pagination.next_selector must be empty when mode is %q", stepID, PaginationModeScroll)
		}
		if p.ScrollDeltaY == 0 {
			p.ScrollDeltaY = DefaultPaginationScrollDelta
		}
		if p.ScrollDeltaY < 1 || p.ScrollDeltaY > MaxPaginationScrollDelta {
			return nil, fmt.Errorf("step %q pagination.scroll_delta_y must be within 1..%d, got %d", stepID, MaxPaginationScrollDelta, p.ScrollDeltaY)
		}
	default:
		return nil, fmt.Errorf("step %q pagination.mode %q is invalid (expected %q or %q)", stepID, mode, PaginationModeClick, PaginationModeScroll)
	}
	p.Mode = mode
	if p.MaxPages == 0 {
		p.MaxPages = DefaultPaginationMaxPages
	}
	if p.MaxPages < 1 || p.MaxPages > MaxPaginationPages {
		return nil, fmt.Errorf("step %q pagination.max_pages must be within 1..%d, got %d", stepID, MaxPaginationPages, p.MaxPages)
	}
	if p.WaitMs == nil {
		p.WaitMs = IntPtr(DefaultPaginationWaitMs)
	} else {
		w := *p.WaitMs
		p.WaitMs = &w
	}
	if *p.WaitMs < 0 || *p.WaitMs > MaxPaginationWaitMs {
		return nil, fmt.Errorf("step %q pagination.wait_ms must be within 0..%d, got %d", stepID, MaxPaginationWaitMs, *p.WaitMs)
	}
	return &p, nil
}

// MergePaginatedStepResult merges a subsequent page's StepResult into acc deterministically,
// deduplicating items (by ItemRule.DedupeBy or canonical JSON) and table rows (matching tables by header
// signature across all accumulated tables and capping rows at MaxMergedTableRows) while honoring ItemRule.Limit.
// It returns the merged result, the count of newly added items + table rows, and whether ItemRule.Limit was reached.
func MergePaginatedStepResult(acc StepResult, next StepResult, itemRule *ItemRule) (StepResult, int, bool) {
	added := 0
	itemLimit := 0
	dedupeField := ""
	if itemRule != nil {
		itemLimit = itemRule.Limit
		dedupeField = strings.TrimSpace(itemRule.DedupeBy)
	}

	seenItems := make(map[string]bool, len(acc.Items)+len(next.Items))
	for _, row := range acc.Items {
		seenItems[itemFingerprint(row, dedupeField)] = true
	}
	for _, row := range next.Items {
		if itemLimit > 0 && len(acc.Items) >= itemLimit {
			break
		}
		fp := itemFingerprint(row, dedupeField)
		if fp != "" && seenItems[fp] {
			continue
		}
		if fp != "" {
			seenItems[fp] = true
		}
		acc.Items = append(acc.Items, row)
		added++
	}

	// Merge table rows across pages by matching header signature across all acc.Tables (preventing
	// table-index shifts from duplicating tables) and enforcing MaxMergedTableRows.
	for ti, nextTbl := range next.Tables {
		matchIdx := -1
		if len(nextTbl.Headers) > 0 {
			for ai := range acc.Tables {
				if sameStringSlice(acc.Tables[ai].Headers, nextTbl.Headers) {
					matchIdx = ai
					break
				}
			}
		} else if ti < len(acc.Tables) && len(acc.Tables[ti].Headers) == 0 {
			matchIdx = ti
		}

		if matchIdx >= 0 {
			seenRows := make(map[string]bool, len(acc.Tables[matchIdx].Rows))
			for _, r := range acc.Tables[matchIdx].Rows {
				seenRows[strings.Join(r, "\x1f")] = true
			}
			for _, r := range nextTbl.Rows {
				if len(acc.Tables[matchIdx].Rows) >= MaxMergedTableRows {
					break
				}
				rk := strings.Join(r, "\x1f")
				if seenRows[rk] {
					continue
				}
				seenRows[rk] = true
				acc.Tables[matchIdx].Rows = append(acc.Tables[matchIdx].Rows, r)
				added++
			}
		} else if len(acc.Tables) < maxMergedTables {
			tbl := TableData{
				Title:   nextTbl.Title,
				Headers: append([]string(nil), nextTbl.Headers...),
			}
			seenRows := make(map[string]bool, len(nextTbl.Rows))
			for _, r := range nextTbl.Rows {
				if len(tbl.Rows) >= MaxMergedTableRows {
					break
				}
				rk := strings.Join(r, "\x1f")
				if seenRows[rk] {
					continue
				}
				seenRows[rk] = true
				tbl.Rows = append(tbl.Rows, r)
			}
			acc.Tables = append(acc.Tables, tbl)
			added += len(tbl.Rows)
		}
	}

	// Fill any scalar fields that were empty on earlier pages.
	if len(next.Fields) > 0 {
		if acc.Fields == nil {
			acc.Fields = make(map[string]any, len(next.Fields))
		}
		for k, v := range next.Fields {
			if cur, exists := acc.Fields[k]; !exists || isEmptyFieldValue(cur) {
				acc.Fields[k] = v
			}
		}
	}

	// Append unseen headings and metrics cleanly.
	seenHeadings := make(map[string]bool, len(acc.Headings))
	for _, h := range acc.Headings {
		seenHeadings[h] = true
	}
	for _, h := range next.Headings {
		if !seenHeadings[h] && len(acc.Headings) < 20 {
			seenHeadings[h] = true
			acc.Headings = append(acc.Headings, h)
		}
	}

	seenMetrics := make(map[string]bool, len(acc.Metrics))
	for _, m := range acc.Metrics {
		seenMetrics[m.Label+"="+m.Value] = true
	}
	for _, m := range next.Metrics {
		mk := m.Label + "=" + m.Value
		if !seenMetrics[mk] && len(acc.Metrics) < 30 {
			seenMetrics[mk] = true
			acc.Metrics = append(acc.Metrics, m)
		}
	}

	reachedLimit := itemLimit > 0 && len(acc.Items) >= itemLimit
	return acc, added, reachedLimit
}

func itemFingerprint(row map[string]any, dedupeField string) string {
	if dedupeField != "" {
		if v, ok := row[dedupeField]; ok && v != nil {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				return "k:" + s
			}
		}
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return ""
	}
	return "j:" + string(raw)
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
