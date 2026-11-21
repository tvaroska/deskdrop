package server

import (
	"context"

	"github.com/tvaroska/deskdrop/internal/contract"
)

// HealRequest carries scrubbed SELECTOR_DRIFT snapshot artifacts into the exception-only healer.
type HealRequest struct {
	Task          contract.Task
	Recipe        contract.Recipe
	StepID        string
	LandedURL     string
	ErrorMsg      string
	DiagnosticID  string
	DOMHTML       string
	ScreenshotPNG []byte
	AllowTier2    bool
}

// HealResult holds the exception healer's verified outputs:
// - FallbackReport: Tier-1 offline extraction (mode="computer_use_fallback")
// - FallbackPlan: bounded Tier-2 stateless follow-up plan (<=3 actions) when blocked
// - Proposal: pure-Go verified RecipePatchProposal (never auto-applied without approval)
type HealResult struct {
	FallbackReport *contract.DropReport
	FallbackPlan   *contract.FallbackPlan
	Proposal       *contract.RecipePatchProposal
	Summary        string
}

// ExceptionHealer is the Driven Port invoked strictly on SELECTOR_DRIFT when allowed by the recipe.
type ExceptionHealer interface {
	HealDrift(ctx context.Context, req HealRequest) (HealResult, error)
}
