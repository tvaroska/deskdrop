package contract

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	DefaultPaginationMaxPages = 3
	MaxPaginationPages        = 20
	DefaultPaginationWaitMs   = 600
	MaxPaginationWaitMs       = 10_000
)

// PaginationRule configures bounded declarative multi-page extraction ("Next page" loop) within a Step.
type PaginationRule struct {
	NextSelector string `json:"next_selector"`
	MaxPages     int    `json:"max_pages,omitempty"`
	WaitMs       int    `json:"wait_ms,omitempty"`
	WaitSelector string `json:"wait_selector,omitempty"`
	StopOnNoNew  *bool  `json:"stop_on_no_new,omitempty"`
}

// StopWhenNoNew defaults to true so pagination stops immediately when clicking Next produces zero new items/rows.
func (p PaginationRule) StopWhenNoNew() bool {
	if p.StopOnNoNew == nil {
		return true
	}
	return *p.StopOnNoNew
}

func resolvePaginationRule(stepID string, in *PaginationRule, sub func(string) string) (*PaginationRule, error) {
	if in == nil {
		return nil, nil
	}
	p := *in
	p.NextSelector = strings.TrimSpace(sub(p.NextSelector))
	p.WaitSelector = strings.TrimSpace(sub(p.WaitSelector))
	if p.NextSelector == "" {
		return nil, fmt.Errorf("step %q pagination.next_selector is required", stepID)
	}
	if p.MaxPages == 0 {
		p.MaxPages = DefaultPaginationMaxPages
	}
	if p.MaxPages < 1 || p.MaxPages > MaxPaginationPages {
		return nil, fmt.Errorf("step %q pagination.max_pages must be within 1..%d, got %d", stepID, MaxPaginationPages, p.MaxPages)
	}
	if p.WaitMs == 0 {
		p.WaitMs = DefaultPaginationWaitMs
	}
	if p.WaitMs < 0 || p.WaitMs > MaxPaginationWaitMs {
		return nil, fmt.Errorf("step %q pagination.wait_ms must be within 0..%d, got %d", stepID, MaxPaginationWaitMs, p.WaitMs)
	}
	return &p, nil
}

// MergePaginatedStepResult merges a subsequent page's StepResult into acc deterministically,
// deduplicating items (by ItemRule.DedupeBy or canonical JSON) and table rows while honoring ItemRule.Limit.
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

	// Merge table rows across pages by matching table headers (or index when headers match).
	for ti, nextTbl := range next.Tables {
		if ti < len(acc.Tables) && sameStringSlice(acc.Tables[ti].Headers, nextTbl.Headers) {
			seenRows := make(map[string]bool, len(acc.Tables[ti].Rows))
			for _, r := range acc.Tables[ti].Rows {
				seenRows[strings.Join(r, "\x1f")] = true
			}
			for _, r := range nextTbl.Rows {
				rk := strings.Join(r, "\x1f")
				if seenRows[rk] {
					continue
				}
				seenRows[rk] = true
				acc.Tables[ti].Rows = append(acc.Tables[ti].Rows, r)
				added++
			}
		} else if len(acc.Tables) < 10 {
			acc.Tables = append(acc.Tables, nextTbl)
			added += len(nextTbl.Rows)
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
		if v, ok := row[dedupeField]; ok {
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
