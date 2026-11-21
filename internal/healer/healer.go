package healer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagents/loopagent"
	"google.golang.org/adk/v2/agent/workflowagents/sequentialagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/exitlooptool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/server"
	"github.com/tvaroska/deskdrop/internal/verify"
)

const (
	DefaultModelName     = "gemini-3.8-flash"
	DefaultLocation      = "us-central1"
	DefaultMaxIterations = 3
	maxPromptHTMLRunes   = 40_000
)

// Config configures the exception-only adk-go healer (Vertex AI or injected model.LLM for offline tests).
type Config struct {
	ModelName          string
	Project            string
	Location           string
	APIKey             string
	MaxPatchIterations uint
	ForbiddenTerms     []string
	LLM                model.LLM
}

// Healer implements server.ExceptionHealer using google.golang.org/adk/v2:
// SequentialAgent(Tier-1/Tier-2 fallback agent -> LoopAgent(MaxIterations=3) + pure-Go verifier tool).
type Healer struct {
	llm                model.LLM
	modelName          string
	maxPatchIterations uint
	forbiddenTerms     []string
}

var _ server.ExceptionHealer = (*Healer)(nil)

// New initializes a Vertex AI (or Gemini API / injected test LLM) exception healer.
func New(ctx context.Context, cfg Config) (*Healer, error) {
	modelName := strings.TrimSpace(cfg.ModelName)
	if modelName == "" {
		modelName = DefaultModelName
	}
	maxIter := cfg.MaxPatchIterations
	if maxIter == 0 {
		maxIter = DefaultMaxIterations
	}

	llm := cfg.LLM
	if llm == nil {
		clientCfg := &genai.ClientConfig{}
		switch {
		case strings.TrimSpace(cfg.Project) != "":
			loc := strings.TrimSpace(cfg.Location)
			if loc == "" {
				loc = DefaultLocation
			}
			clientCfg.Backend = genai.BackendVertexAI
			clientCfg.Project = strings.TrimSpace(cfg.Project)
			clientCfg.Location = loc
		case strings.TrimSpace(cfg.APIKey) != "":
			clientCfg.Backend = genai.BackendGeminiAPI
			clientCfg.APIKey = strings.TrimSpace(cfg.APIKey)
		default:
			return nil, errors.New("healer requires GOOGLE_CLOUD_PROJECT (Vertex AI) or GEMINI_API_KEY")
		}
		var err error
		llm, err = gemini.NewModel(ctx, modelName, clientCfg)
		if err != nil {
			return nil, fmt.Errorf("init adk gemini model %q: %w", modelName, err)
		}
	}

	return &Healer{
		llm:                llm,
		modelName:          modelName,
		maxPatchIterations: maxIter,
		forbiddenTerms:     cfg.ForbiddenTerms,
	}, nil
}

// HealDrift runs strictly on SELECTOR_DRIFT snapshots (never on happy paths or AUTH_EXPIRED).
func (h *Healer) HealDrift(ctx context.Context, req server.HealRequest) (server.HealResult, error) {
	if !req.Recipe.AllowsComputerUseFallback() {
		return server.HealResult{Summary: "computer_use_fallback disabled by recipe"}, nil
	}
	scrubbedHTML := contract.ScrubHTML(req.DOMHTML)
	if err := contract.EnforceAirGap(scrubbedHTML, h.forbiddenTerms); err != nil {
		return server.HealResult{}, fmt.Errorf("air-gap blocked healer input: %w", err)
	}

	allowedDomains := contract.InferRecipeDomains(req.Recipe)

	var (
		mu          sync.Mutex
		result      server.HealResult
		iterations  int
		stopPatcher bool
	)

	// Tool 1: Tier-1 offline fallback extraction submission.
	type submitReportArgs struct {
		Fields      map[string]any   `json:"fields,omitempty"`
		Items       []map[string]any `json:"items,omitempty"`
		SummaryText string           `json:"summary_text,omitempty"`
	}
	type submitReportResult struct {
		Accepted bool   `json:"accepted"`
		Error    string `json:"error,omitempty"`
	}
	submitFallbackTool, err := functiontool.New(functiontool.Config{
		Name:        "submit_fallback_report",
		Description: "Submit extracted structured fields and/or items recovered directly from the offline screenshot and scrubbed dom.html.",
	}, func(_ agent.Context, args submitReportArgs) (submitReportResult, error) {
		stepID := req.StepID
		if stepID == "" && len(req.Recipe.Steps) > 0 {
			stepID = req.Recipe.Steps[0].ID
		}
		rep := contract.DropReport{
			SchemaVersion: "2.0",
			Mode:          contract.DropModeComputerUseFallback,
			Recipe:        req.Recipe.Name,
			Topic:         req.Recipe.Topic,
			Host:          "deskdrop-healer",
			CapturedAtUTC: time.Now().UTC().Format(time.RFC3339),
			Steps: []contract.StepResult{
				{
					StepID:      stepID,
					URL:         req.LandedURL,
					Fields:      args.Fields,
					Items:       args.Items,
					TextExcerpt: args.SummaryText,
				},
			},
		}
		if err := contract.ValidateDropReport(rep, h.forbiddenTerms); err != nil {
			return submitReportResult{Accepted: false, Error: err.Error()}, nil
		}
		raw, _ := json.Marshal(rep)
		if err := contract.EnforceAirGap(string(raw), h.forbiddenTerms); err != nil {
			return submitReportResult{Accepted: false, Error: err.Error()}, nil
		}
		mu.Lock()
		result.FallbackReport = &rep
		mu.Unlock()
		return submitReportResult{Accepted: true}, nil
	})
	if err != nil {
		return server.HealResult{}, err
	}

	// Tool 2: Bounded Tier-2 stateless follow-up plan (<= 3 safe actions, same domain, zero text input).
	type tier2Args struct {
		Reason  string                    `json:"reason"`
		Actions []contract.FallbackAction `json:"actions"`
	}
	type tier2Result struct {
		Scheduled bool   `json:"scheduled"`
		Error     string `json:"error,omitempty"`
	}
	tier2Tool, err := functiontool.New(functiontool.Config{
		Name:        "request_tier2_fallback",
		Description: "Request 1 bounded stateless re-open with at most 3 safe actions (scroll, wait, click) ONLY when content is blocked by a modal, overlay, or collapsed section.",
	}, func(_ agent.Context, args tier2Args) (tier2Result, error) {
		if !req.AllowTier2 {
			return tier2Result{Scheduled: false, Error: "tier-2 fallback already used; extract offline only"}, nil
		}
		targetURL := req.LandedURL
		if targetURL == "" && len(req.Recipe.Steps) > 0 {
			targetURL = req.Recipe.Steps[0].URL
		}
		allowedDomain := req.Task.Domain
		if allowedDomain == "" && len(allowedDomains) > 0 {
			allowedDomain = allowedDomains[0]
		}
		plan := contract.FallbackPlan{
			AllowedDomain: allowedDomain,
			TargetURL:     targetURL,
			StepID:        req.StepID,
			Reason:        args.Reason,
			Actions:       args.Actions,
		}
		if err := contract.ValidateFallbackPlan(plan, allowedDomains); err != nil {
			return tier2Result{Scheduled: false, Error: err.Error()}, nil
		}
		mu.Lock()
		result.FallbackPlan = &plan
		stopPatcher = true
		mu.Unlock()
		return tier2Result{Scheduled: true}, nil
	})
	if err != nil {
		return server.HealResult{}, err
	}

	// Tool 3: Pure-Go HTML verifier tool used inside the LoopAgent (MaxIterations: 3).
	type verifyRecipeArgs struct {
		RecipeJSON string `json:"recipe_json"`
		Rationale  string `json:"rationale"`
	}
	type verifyRecipeResult struct {
		Passed     bool   `json:"passed"`
		Error      string `json:"error,omitempty"`
		ItemCount  int    `json:"item_count"`
		FieldCount int    `json:"field_count"`
		TableCount int    `json:"table_count"`
		NextAction string `json:"next_action"`
	}
	verifyTool, err := functiontool.New(functiontool.Config{
		Name:        "verify_recipe_on_snapshot",
		Description: "Deterministically verify a proposed declarative Recipe JSON (zero arbitrary JS) in pure Go against the captured scrubbed dom.html snapshot.",
	}, func(_ agent.Context, args verifyRecipeArgs) (verifyRecipeResult, error) {
		mu.Lock()
		if stopPatcher {
			mu.Unlock()
			return verifyRecipeResult{Passed: false, Error: "page blocked pending tier-2 snapshot", NextAction: "call exit_loop"}, nil
		}
		iterations++
		iterNum := iterations
		mu.Unlock()

		var candidate contract.Recipe
		if err := contract.ParseJSONStrict([]byte(args.RecipeJSON), &candidate); err != nil {
			return verifyRecipeResult{Passed: false, Error: "strict recipe schema error: " + err.Error(), NextAction: "fix JSON and retry"}, nil
		}
		if candidate.Name == "" {
			candidate.Name = req.Recipe.Name
		}
		if candidate.Topic == "" {
			candidate.Topic = req.Recipe.Topic
		}
		// Enforce domain lock so a drifted DOM cannot pivot the recipe to an unapproved host.
		for _, dom := range contract.InferRecipeDomains(candidate) {
			matched := false
			for _, allowed := range allowedDomains {
				if contract.DomainsMatch(dom, allowed) {
					matched = true
					break
				}
			}
			if !matched {
				return verifyRecipeResult{
					Passed:     false,
					Error:      fmt.Sprintf("domain pivot blocked: %q is outside approved domains %v", dom, allowedDomains),
					NextAction: "keep original recipe URL/domain and retry",
				}, nil
			}
		}
		if err := contract.EnforceAirGap(args.RecipeJSON, h.forbiddenTerms); err != nil {
			return verifyRecipeResult{Passed: false, Error: err.Error(), NextAction: "remove forbidden terms"}, nil
		}

		outcome, verr := verify.VerifyRecipeOnHTML(candidate, req.Task.Vars, req.StepID, scrubbedHTML, req.LandedURL)
		if verr != nil {
			return verifyRecipeResult{Passed: false, Error: verr.Error(), NextAction: "fix recipe contract error and retry"}, nil
		}
		if !outcome.Passed {
			return verifyRecipeResult{
				Passed:     false,
				Error:      outcome.Error,
				ItemCount:  outcome.ItemCount,
				FieldCount: outcome.FieldCount,
				TableCount: outcome.TableCount,
				NextAction: "adjust CSS selectors or field rules and retry",
			}, nil
		}

		mu.Lock()
		result.Proposal = &contract.RecipePatchProposal{
			RecipeName:          req.Recipe.Name,
			TaskID:              req.Task.ID,
			DiagnosticID:        req.DiagnosticID,
			Status:              contract.ProposalStatusPending,
			Rationale:           strings.TrimSpace(args.Rationale),
			ProposedRecipe:      candidate,
			Verified:            true,
			VerificationSummary: fmt.Sprintf("verified on dom.html: %d items, %d fields, %d tables", outcome.ItemCount, outcome.FieldCount, outcome.TableCount),
			Iterations:          iterNum,
			Model:               h.modelName,
		}
		mu.Unlock()
		return verifyRecipeResult{
			Passed:     true,
			ItemCount:  outcome.ItemCount,
			FieldCount: outcome.FieldCount,
			TableCount: outcome.TableCount,
			NextAction: "verification passed; call exit_loop now",
		}, nil
	})
	if err != nil {
		return server.HealResult{}, err
	}

	exitTool, err := exitlooptool.New()
	if err != nil {
		return server.HealResult{}, err
	}

	fallbackTools := []tool.Tool{submitFallbackTool}
	if req.AllowTier2 {
		fallbackTools = append(fallbackTools, tier2Tool)
	}
	fallbackAgent, err := llmagent.New(llmagent.Config{
		Name:        "offline_snapshot_fallback",
		Model:       h.llm,
		Description: "Extracts structured output offline from screenshot + scrubbed dom.html or requests 1 bounded Tier-2 fallback.",
		Instruction: "You are DeskDrop's Tier-1 exception fallback extractor. Inspect the provided screenshot and scrubbed dom.html. " +
			"If the target metrics/items are visible in the snapshot, call submit_fallback_report with clean structured fields/items. " +
			"ONLY if the page is blocked by a modal, cookie banner, or collapsed section and data is missing, call request_tier2_fallback with 1..3 safe actions (scroll, wait, click — never type text or navigate cross-domain).",
		Tools: fallbackTools,
	})
	if err != nil {
		return server.HealResult{}, err
	}

	patchDrafter, err := llmagent.New(llmagent.Config{
		Name:        "recipe_patch_drafter",
		Model:       h.llm,
		Description: "Drafts and Go-verifies an updated declarative Recipe (zero arbitrary JS) against dom.html.",
		Instruction: "You are DeskDrop's declarative recipe repair drafter. If a Tier-2 fallback was requested because the page is blocked, immediately call exit_loop. " +
			"Otherwise inspect the failed recipe and scrubbed dom.html, update ONLY the broken CSS selectors / field rules / item rules (zero arbitrary JS, same approved URL/domain), " +
			"call verify_recipe_on_snapshot to verify in pure Go, and once passed==true immediately call exit_loop.",
		Tools: []tool.Tool{verifyTool, exitTool},
	})
	if err != nil {
		return server.HealResult{}, err
	}

	patchLoop, err := loopagent.New(loopagent.Config{
		AgentConfig: agent.Config{
			Name:        "recipe_patch_loop",
			Description: "Iteratively refines and verifies declarative recipe patches against dom.html.",
			SubAgents:   []agent.Agent{patchDrafter},
		},
		MaxIterations: h.maxPatchIterations,
	})
	if err != nil {
		return server.HealResult{}, err
	}

	seqAgent, err := sequentialagent.New(sequentialagent.Config{
		AgentConfig: agent.Config{
			Name:        "deskdrop_drift_healer",
			Description: "Two-stage SELECTOR_DRIFT exception route: offline/Tier-2 fallback followed by Go-verified recipe patch loop.",
			SubAgents:   []agent.Agent{fallbackAgent, patchLoop},
		},
	})
	if err != nil {
		return server.HealResult{}, err
	}

	r, err := runner.New(runner.Config{
		AppName:           "deskdrop-healer",
		Agent:             seqAgent,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		return server.HealResult{}, err
	}

	recipeBytes, _ := json.MarshalIndent(req.Recipe, "", "  ")
	prompt := fmt.Sprintf(
		"SELECTOR_DRIFT Exception Context\nRecipe: %s\nStep ID: %s\nLanded URL: %s\nError: %s\n\nFailed Recipe JSON:\n%s\n\nScrubbed dom.html:\n%s",
		req.Recipe.Name,
		req.StepID,
		req.LandedURL,
		req.ErrorMsg,
		string(recipeBytes),
		truncateRunes(scrubbedHTML, maxPromptHTMLRunes),
	)
	parts := []*genai.Part{genai.NewPartFromText(prompt)}
	if len(req.ScreenshotPNG) > 0 {
		parts = append(parts, genai.NewPartFromBytes(req.ScreenshotPNG, "image/png"))
	}
	userMsg := genai.NewContentFromParts(parts, genai.RoleUser)

	sessID := newSessionID()
	for _, runErr := range r.Run(ctx, "healer", sessID, userMsg, agent.RunConfig{}) {
		if runErr != nil {
			return server.HealResult{}, fmt.Errorf("adk healer run failed: %w", runErr)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if result.Proposal != nil {
		result.Summary = fmt.Sprintf("healed with verified proposal (%d iteration(s))", result.Proposal.Iterations)
	} else if result.FallbackPlan != nil {
		result.Summary = fmt.Sprintf("scheduled tier-2 fallback (%d action(s))", len(result.FallbackPlan.Actions))
	} else if result.FallbackReport != nil {
		result.Summary = "extracted tier-1 offline fallback report"
	} else {
		result.Summary = "no fallback report or verified patch produced"
	}
	return result, nil
}

func newSessionID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "heal-" + hex.EncodeToString(b)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
