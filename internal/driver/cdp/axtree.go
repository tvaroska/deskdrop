package cdp

import (
	"context"
	"encoding/json"

	"github.com/tvaroska/deskdrop/internal/contract"
)

// CaptureCompactAXTree fetches Chrome's full accessibility tree via CDP Accessibility.getFullAXTree
// and formats it into a compact, privacy-scrubbed semantic snapshot. Invoked strictly on exception
// and Tier-2 fallback paths (zero happy-path overhead).
func (c *Client) CaptureCompactAXTree(ctx context.Context, sessionID string) (string, contract.AXGroundingStats, error) {
	var raw json.RawMessage
	if err := c.Call(ctx, sessionID, "Accessibility.getFullAXTree", nil, &raw); err != nil {
		return "", contract.AXGroundingStats{}, err
	}
	nodes, err := contract.ParseAXTreeJSON(raw)
	if err != nil {
		return "", contract.AXGroundingStats{}, err
	}
	compact, stats := contract.FormatCompactAXTree(nodes, len(raw))
	return compact, stats, nil
}
