package contract

import (
	"fmt"
	"strings"
)

// DefaultItemLimit matches the default per-step ItemRule cap in JS and Go extractors when Limit <= 0.
const DefaultItemLimit = 50

// StepItemDiff reports deterministic item additions and removals for a single recipe step.
type StepItemDiff struct {
	StepID             string           `json:"step_id"`
	DedupeBy           string           `json:"dedupe_by,omitempty"`
	Limit              int              `json:"limit,omitempty"`
	WindowTruncated    bool             `json:"window_truncated"`
	Added              []map[string]any `json:"added"`
	Removed            []map[string]any `json:"removed"`
	UnchangedCount     int              `json:"unchanged_count"`
	WindowShiftedCount int              `json:"window_shifted_count"`
}

// DropDiff compares a DropRecord against its baseline DropRecord for the same (recipe, topic).
type DropDiff struct {
	OK                 bool             `json:"ok"`
	DropID             string           `json:"drop_id"`
	BaselineDropID     string           `json:"baseline_drop_id,omitempty"`
	HasBaseline        bool             `json:"has_baseline"`
	Recipe             string           `json:"recipe"`
	Topic              string           `json:"topic"`
	Added              []map[string]any `json:"added"`
	Removed            []map[string]any `json:"removed"`
	AddedCount         int              `json:"added_count"`
	RemovedCount       int              `json:"removed_count"`
	UnchangedCount     int              `json:"unchanged_count"`
	WindowShiftedCount int              `json:"window_shifted_count"`
	Steps              []StepItemDiff   `json:"steps"`
}

// DiffStepItems computes deterministic set differences between currItems and prevItems keyed by dedupeBy.
// To prevent false-positive removals on top-N (e.g. 10..25) chronological windows when new items push
// older tail items past #N, an absent baseline item is classified as Removed only when:
//  1. len(currDeduped) < effectiveLimit (the current capture reached the true end of the feed), OR
//  2. its position in prevItems is strictly above the oldest baseline item still present in currItems.
func DiffStepItems(currItems []map[string]any, prevItems []map[string]any, dedupeBy string, limit int) StepItemDiff {
	dedupeField := strings.TrimSpace(dedupeBy)
	effectiveLimit := limit
	if effectiveLimit <= 0 {
		effectiveLimit = DefaultItemLimit
	}

	currRows, currKeys, currSet := dedupeOrderedItems(currItems, dedupeField)
	prevRows, prevKeys, prevSet := dedupeOrderedItems(prevItems, dedupeField)

	out := StepItemDiff{
		DedupeBy:        dedupeField,
		Limit:           limit,
		WindowTruncated: len(currRows) >= effectiveLimit,
		Added:           make([]map[string]any, 0),
		Removed:         make([]map[string]any, 0),
	}

	for i, k := range currKeys {
		if prevSet[k] {
			out.UnchangedCount++
		} else {
			out.Added = append(out.Added, currRows[i])
		}
	}

	if len(prevRows) == 0 {
		return out
	}

	if !out.WindowTruncated {
		for i, pk := range prevKeys {
			if !currSet[pk] {
				out.Removed = append(out.Removed, prevRows[i])
			}
		}
		return out
	}

	cutoffPrevIdx := -1
	for i, pk := range prevKeys {
		if currSet[pk] {
			cutoffPrevIdx = i
		}
	}

	for i, pk := range prevKeys {
		if currSet[pk] {
			continue
		}
		if i < cutoffPrevIdx {
			out.Removed = append(out.Removed, prevRows[i])
		} else {
			out.WindowShiftedCount++
		}
	}
	return out
}

// ComputeDropDiff diffs curr against prev (nil when curr is the first drop for (recipe, topic)),
// resolving per-step DedupeBy and Limit from recipe when available and applying optional overrides.
func ComputeDropDiff(curr DropRecord, prev *DropRecord, recipe *Recipe, overrideDedupeBy string, overrideLimit int) DropDiff {
	prevSteps := make(map[string]StepResult)
	baselineID := ""
	if prev != nil {
		baselineID = prev.ID
		for _, ps := range prev.Report.Steps {
			prevSteps[ps.StepID] = ps
		}
	}

	rcpSteps := make(map[string]Step)
	if recipe != nil {
		for _, rs := range recipe.Steps {
			rcpSteps[rs.ID] = rs
		}
	}

	diff := DropDiff{
		OK:             true,
		DropID:         curr.ID,
		BaselineDropID: baselineID,
		HasBaseline:    prev != nil,
		Recipe:         curr.Report.Recipe,
		Topic:          curr.Report.Topic,
		Added:          make([]map[string]any, 0),
		Removed:        make([]map[string]any, 0),
		Steps:          make([]StepItemDiff, 0, len(curr.Report.Steps)),
	}

	overrideKey := strings.TrimSpace(overrideDedupeBy)
	seenSteps := make(map[string]bool, len(curr.Report.Steps))
	for _, cs := range curr.Report.Steps {
		seenSteps[cs.StepID] = true
		dedupeBy := ""
		limit := 0
		if rs, ok := rcpSteps[cs.StepID]; ok && rs.Items != nil {
			dedupeBy = rs.Items.DedupeBy
			limit = rs.Items.Limit
		}
		if overrideKey != "" {
			dedupeBy = overrideKey
		}
		if overrideLimit > 0 {
			limit = overrideLimit
		}

		var prevItems []map[string]any
		if prev != nil {
			if ps, ok := prevSteps[cs.StepID]; ok {
				prevItems = ps.Items
			}
		}

		sd := DiffStepItems(cs.Items, prevItems, dedupeBy, limit)
		sd.StepID = cs.StepID
		diff.Steps = append(diff.Steps, sd)
		diff.Added = append(diff.Added, sd.Added...)
		diff.Removed = append(diff.Removed, sd.Removed...)
		diff.UnchangedCount += sd.UnchangedCount
		diff.WindowShiftedCount += sd.WindowShiftedCount
	}
	if prev != nil {
		for _, ps := range prev.Report.Steps {
			if seenSteps[ps.StepID] || len(ps.Items) == 0 {
				continue
			}
			dedupeBy := ""
			limit := 0
			if rs, ok := rcpSteps[ps.StepID]; ok && rs.Items != nil {
				dedupeBy = rs.Items.DedupeBy
				limit = rs.Items.Limit
			}
			if overrideKey != "" {
				dedupeBy = overrideKey
			}
			if overrideLimit > 0 {
				limit = overrideLimit
			}
			sd := DiffStepItems(nil, ps.Items, dedupeBy, limit)
			sd.StepID = ps.StepID
			diff.Steps = append(diff.Steps, sd)
			diff.Removed = append(diff.Removed, sd.Removed...)
		}
	}
	diff.AddedCount = len(diff.Added)
	diff.RemovedCount = len(diff.Removed)
	return diff
}

func dedupeOrderedItems(items []map[string]any, dedupeField string) ([]map[string]any, []string, map[string]bool) {
	rows := make([]map[string]any, 0, len(items))
	keys := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, row := range items {
		k := itemFingerprint(row, dedupeField)
		if k == "" {
			k = fmt.Sprintf("idx:%d", len(rows))
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		rows = append(rows, row)
		keys = append(keys, k)
	}
	return rows, keys, seen
}
