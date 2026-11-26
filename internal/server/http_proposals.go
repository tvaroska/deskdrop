package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tvaroska/deskdrop/internal/contract"
)

func (s *Service) handleListRecipes(w http.ResponseWriter, r *http.Request) {
	list, err := s.catalog.List(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "CATALOG_ERROR", err.Error(), nil)
		return
	}
	if list == nil {
		list = []contract.Recipe{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"recipes": list,
	})
}

func failMetadataAirGapText(meta contract.FailReportMetadata) string {
	return strings.Join([]string{
		meta.From,
		meta.To,
		meta.Topic,
		meta.Recipe,
		meta.StepID,
		meta.LandedURL,
		meta.Domain,
		string(meta.ErrorCode),
		meta.Error,
	}, " ")
}

// applySafeErrorCodeOverride restricts caller-supplied meta.ErrorCode overrides to an explicit safe
// allowlist, never allowing a caller to downgrade AUTH_EXPIRED into SELECTOR_DRIFT or trigger the healer.
func applySafeErrorCodeOverride(triage contract.ErrorTriage, requested contract.ErrorCode) (contract.ErrorTriage, error) {
	code := contract.ErrorCode(strings.TrimSpace(string(requested)))
	if code == "" {
		return triage, nil
	}
	if triage.Action == contract.TriageHoldDomainAuth {
		// Auth/login/CAPTCHA holds are fail-closed and cannot be overridden by caller error_code.
		return triage, nil
	}
	switch code {
	case contract.ErrorCodeContractInvalid:
		triage.Code = contract.ErrorCodeContractInvalid
		triage.Action = contract.TriageDiagnosticEscalation
	case contract.ErrorCodeFallbackDeclined:
		triage.Code = contract.ErrorCodeFallbackDeclined
		triage.Action = contract.TriageDiagnosticEscalation
	case contract.ErrorCodeAuthExpired:
		triage.Code = contract.ErrorCodeAuthExpired
		triage.Action = contract.TriageHoldDomainAuth
	case contract.ErrorCodeCDPTransient:
		triage.Code = contract.ErrorCodeCDPTransient
		triage.Action = contract.TriageRetryBackoff
	case contract.ErrorCodeUnknown:
		triage.Code = contract.ErrorCodeUnknown
	case contract.ErrorCodeSelectorDrift:
		// Never allow callers to force SELECTOR_DRIFT and invoke the LLM healer unless
		// deterministic classification already identified SELECTOR_DRIFT.
		if triage.Code != contract.ErrorCodeSelectorDrift {
			return triage, nil
		}
	default:
		return triage, fmt.Errorf("unsupported error_code %q", code)
	}
	return triage, nil
}

func (s *Service) handleSubmitError(w http.ResponseWriter, r *http.Request) {
	meta, zipBytes, err := parseFailRequest(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}
	if strings.TrimSpace(meta.Error) == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, string(contract.ErrorCodeContractInvalid), "error field is required", nil)
		return
	}
	if err := contract.EnforceAirGap(failMetadataAirGapText(meta), s.cfg.ForbiddenTerms); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "AIRGAP_VIOLATION", err.Error(), nil)
		return
	}
	zdata, zerr := inspectDiagnosticZip(zipBytes)
	if zerr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, string(contract.ErrorCodeContractInvalid), zerr.Error(), nil)
		return
	}
	if zdata.AllText != "" {
		if err := contract.EnforceAirGap(zdata.AllText, s.cfg.ForbiddenTerms); err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "AIRGAP_VIOLATION", err.Error(), nil)
			return
		}
	}

	triage := contract.ClassifyExtractionError(meta.Error, meta.LandedURL, meta.Domain, len(zipBytes) > 0)
	triage, oerr := applySafeErrorCodeOverride(triage, meta.ErrorCode)
	if oerr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, string(contract.ErrorCodeContractInvalid), oerr.Error(), nil)
		return
	}

	diagID, err := s.persistDiagnosticIfPresent(r.Context(), "", meta, triage, zipBytes)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "BLOB_ERROR", err.Error(), nil)
		return
	}

	from := meta.From
	if from == "" {
		from = "desktop-worker"
	}
	to := meta.To
	if to == "" {
		to = "all"
	}

	alertSent := false
	if triage.Action == contract.TriageHoldDomainAuth && triage.Domain != "" {
		_, newlyAlerted, berr := s.store.TripDomainAuthBreaker(r.Context(), triage.Domain, meta.Error)
		if berr != nil {
			writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", berr.Error(), nil)
			return
		}
		if newlyAlerted {
			s.emitTriageBusAlert(r.Context(), from, to, meta.Topic, triage, meta.Error, diagID, "")
			alertSent = true
		}
	} else {
		s.emitTriageBusAlert(r.Context(), from, to, meta.Topic, triage, meta.Error, diagID, "")
		alertSent = true
	}

	var proposalID string
	var fallbackDropID string
	if triage.Code == contract.ErrorCodeSelectorDrift && triage.Action == contract.TriageDiagnosticEscalation && s.healer != nil && zdata.DOMHTML != "" && meta.Recipe != "" {
		if rcp, ok, _ := s.catalog.Get(r.Context(), meta.Recipe); ok && rcp.AllowsComputerUseFallback() {
			outcome := s.runDriftHealer(r.Context(), contract.Task{Recipe: rcp.Name, NotifyTo: to, Domain: triage.Domain}, rcp, meta.StepID, meta.LandedURL, meta.Error, diagID, zdata.DOMHTML, zdata.AXTreeText, zdata.ScreenshotPNG, false)
			proposalID = outcome.ProposalID
			fallbackDropID = outcome.DropID
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"ok":               true,
		"triage":           triage,
		"diagnostic_id":    diagID,
		"alert_sent":       alertSent,
		"proposal_id":      proposalID,
		"fallback_drop_id": fallbackDropID,
	})
}

func (s *Service) handleFailTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, err := s.store.GetTask(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "task not found", nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	if task.Status != contract.TaskStatusClaimed {
		writeAPIError(w, http.StatusConflict, "TASK_NOT_CLAIMED", fmt.Sprintf("task %q is in status %q, expected claimed", id, task.Status), nil)
		return
	}

	meta, zipBytes, err := parseFailRequest(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}
	if strings.TrimSpace(meta.Error) == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, string(contract.ErrorCodeContractInvalid), "error field is required", nil)
		return
	}
	if meta.From != "" && task.ClaimedBy != "" && !strings.EqualFold(strings.TrimSpace(meta.From), strings.TrimSpace(task.ClaimedBy)) {
		writeAPIError(w, http.StatusConflict, "TASK_NOT_CLAIMED", fmt.Sprintf("task %q is claimed by %q", id, task.ClaimedBy), nil)
		return
	}
	if err := contract.EnforceAirGap(failMetadataAirGapText(meta), s.cfg.ForbiddenTerms); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "AIRGAP_VIOLATION", err.Error(), nil)
		return
	}
	zdata, zerr := inspectDiagnosticZip(zipBytes)
	if zerr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, string(contract.ErrorCodeContractInvalid), zerr.Error(), nil)
		return
	}
	if zdata.AllText != "" {
		if err := contract.EnforceAirGap(zdata.AllText, s.cfg.ForbiddenTerms); err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "AIRGAP_VIOLATION", err.Error(), nil)
			return
		}
	}

	if meta.Recipe == "" {
		meta.Recipe = task.Recipe
	}
	if meta.Topic == "" {
		meta.Topic = task.Recipe
	}
	fallbackDomain := meta.Domain
	if fallbackDomain == "" {
		fallbackDomain = task.Domain
	}
	triage := contract.ClassifyExtractionError(meta.Error, meta.LandedURL, fallbackDomain, len(zipBytes) > 0)
	triage, oerr := applySafeErrorCodeOverride(triage, meta.ErrorCode)
	if oerr != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, string(contract.ErrorCodeContractInvalid), oerr.Error(), nil)
		return
	}

	diagID, err := s.persistDiagnosticIfPresent(r.Context(), id, meta, triage, zipBytes)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "BLOB_ERROR", err.Error(), nil)
		return
	}

	host := meta.From
	if host == "" {
		host = task.ClaimedBy
	}
	updated, _, newlyAlerted, err := s.store.FailTask(r.Context(), id, host, triage, meta.Error, diagID)
	if errors.Is(err, ErrConflict) {
		writeAPIError(w, http.StatusConflict, "TASK_NOT_CLAIMED", "task is no longer in claimed state", nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}

	notify := task.NotifyTo
	if notify == "" {
		notify = "all"
	}
	switch {
	case triage.Action == contract.TriageHoldDomainAuth && newlyAlerted:
		s.emitTriageBusAlert(r.Context(), host, notify, meta.Topic, triage, meta.Error, diagID, id)
	case updated.Status == contract.TaskStatusFailedDiagnostic:
		s.emitTriageBusAlert(r.Context(), host, notify, meta.Topic, triage, meta.Error, diagID, id)
	case updated.Status == contract.TaskStatusDeadLetter:
		s.emitTriageBusAlert(r.Context(), host, notify, meta.Topic, triage, meta.Error, diagID, id)
	}

	var healOut driftHealSummary
	if triage.Code == contract.ErrorCodeSelectorDrift && triage.Action == contract.TriageDiagnosticEscalation && s.healer != nil && zdata.DOMHTML != "" {
		rcp, rerr := s.catalog.ValidateAndResolveTask(r.Context(), task.Recipe, task.CustomRecipe, task.Vars)
		if rerr == nil && rcp.AllowsComputerUseFallback() {
			allowTier2 := task.Kind != contract.TaskKindFallback
			healOut = s.runDriftHealer(r.Context(), updated, rcp, meta.StepID, meta.LandedURL, meta.Error, diagID, zdata.DOMHTML, zdata.AXTreeText, zdata.ScreenshotPNG, allowTier2)
			if refreshed, gerr := s.store.GetTask(r.Context(), id); gerr == nil {
				updated = refreshed
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"task":             updated,
		"triage":           triage,
		"diagnostic_id":    diagID,
		"proposal_id":      healOut.ProposalID,
		"fallback_task_id": healOut.FallbackTaskID,
		"fallback_drop_id": healOut.DropID,
	})
}

// handleFallbackResult ingests fresh scrubbed dom.html + screenshot.png returned by a Tier-2
// bounded fallback task and triggers Tier-1 offline extraction + verified proposal drafting.
func (s *Service) handleFallbackResult(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, err := s.store.GetTask(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "task not found", nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	if task.Kind != contract.TaskKindFallback {
		writeAPIError(w, http.StatusUnprocessableEntity, string(contract.ErrorCodeContractInvalid), "task is not a fallback_task", nil)
		return
	}
	if task.Status != contract.TaskStatusClaimed {
		writeAPIError(w, http.StatusConflict, "TASK_NOT_CLAIMED", fmt.Sprintf("task %q is in status %q, expected claimed", id, task.Status), nil)
		return
	}

	meta, zipBytes, err := parseFailRequest(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}
	if err := contract.EnforceAirGap(failMetadataAirGapText(meta), s.cfg.ForbiddenTerms); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "AIRGAP_VIOLATION", err.Error(), nil)
		return
	}
	zdata, err := inspectDiagnosticZip(zipBytes)
	if err != nil || zdata.DOMHTML == "" {
		msg := "fallback result bundle must include dom.html"
		if err != nil {
			msg = err.Error()
		}
		writeAPIError(w, http.StatusUnprocessableEntity, string(contract.ErrorCodeContractInvalid), msg, nil)
		return
	}
	if err := contract.EnforceAirGap(zdata.AllText, s.cfg.ForbiddenTerms); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "AIRGAP_VIOLATION", err.Error(), nil)
		return
	}

	triage := contract.ErrorTriage{
		Code:   contract.ErrorCodeSelectorDrift,
		Action: contract.TriageDiagnosticEscalation,
		Domain: task.Domain,
	}
	diagID, err := s.persistDiagnosticIfPresent(r.Context(), id, meta, triage, zipBytes)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "BLOB_ERROR", err.Error(), nil)
		return
	}

	var healOut driftHealSummary
	if s.healer != nil {
		rcp, rerr := s.catalog.ValidateAndResolveTask(r.Context(), task.Recipe, task.CustomRecipe, task.Vars)
		if rerr == nil && rcp.AllowsComputerUseFallback() {
			healOut = s.runDriftHealer(r.Context(), task, rcp, meta.StepID, meta.LandedURL, meta.Error, diagID, zdata.DOMHTML, zdata.AXTreeText, zdata.ScreenshotPNG, false)
		}
	}

	updated, _ := s.store.GetTask(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"task":             updated,
		"diagnostic_id":    diagID,
		"proposal_id":      healOut.ProposalID,
		"fallback_drop_id": healOut.DropID,
	})
}

type driftHealSummary struct {
	DropID         string
	FallbackTaskID string
	ProposalID     string
}

func (s *Service) runDriftHealer(
	ctx context.Context,
	task contract.Task,
	rcp contract.Recipe,
	stepID string,
	landedURL string,
	errMsg string,
	diagID string,
	domHTML string,
	axTreeText string,
	screenshotPNG []byte,
	allowTier2 bool,
) driftHealSummary {
	var out driftHealSummary
	res, err := s.healer.HealDrift(ctx, HealRequest{
		Task:          task,
		Recipe:        rcp,
		StepID:        stepID,
		LandedURL:     landedURL,
		ErrorMsg:      errMsg,
		DiagnosticID:  diagID,
		DOMHTML:       domHTML,
		AXTreeText:    axTreeText,
		ScreenshotPNG: screenshotPNG,
		AllowTier2:    allowTier2,
	})
	if err != nil {
		return out
	}

	notify := task.NotifyTo
	if notify == "" {
		notify = "all"
	}

	if res.FallbackReport != nil {
		rep := *res.FallbackReport
		rep.Mode = contract.DropModeComputerUseFallback
		if rep.Recipe == "" {
			rep.Recipe = rcp.Name
		}
		if rep.Topic == "" {
			rep.Topic = rcp.Topic
		}
		if rep.SchemaVersion == "" {
			rep.SchemaVersion = "2.0"
		}
		if rec, _, _, ierr := s.ingestDrop(ctx, contract.SubmitDropRequest{
			From:   "deskdrop-healer",
			To:     notify,
			TaskID: task.ID,
			Report: rep,
		}, "heal-drop:"+task.ID+":"+diagID); ierr == nil {
			out.DropID = rec.ID
			if task.ID != "" {
				_, _ = s.store.CompleteTask(ctx, task.ID, rec.ID)
			}
			if task.ParentTaskID != "" {
				_, _ = s.store.CompleteTask(ctx, task.ParentTaskID, rec.ID)
			}
		}
	} else if res.FallbackPlan != nil && allowTier2 {
		allowed := contract.InferRecipeDomains(rcp)
		if contract.ValidateFallbackPlan(*res.FallbackPlan, allowed) == nil {
			fbTask, _, eerr := s.store.EnqueueTask(ctx, contract.EnqueueTaskRequest{
				Kind:         contract.TaskKindFallback,
				ParentTaskID: task.ID,
				Recipe:       rcp.Name,
				Vars:         task.Vars,
				FallbackPlan: res.FallbackPlan,
				Domain:       task.Domain,
				Priority:     task.Priority + 1,
				MaxAttempts:  1,
				RequestedBy:  "deskdrop-healer",
				NotifyTo:     notify,
				DedupeKey:    "fallback:" + task.ID,
			})
			if eerr == nil {
				out.FallbackTaskID = fbTask.ID
			}
		}
	}

	if res.Proposal != nil {
		prop := *res.Proposal
		prop.Status = contract.ProposalStatusPending
		if prop.RecipeName == "" {
			prop.RecipeName = rcp.Name
		}
		prop.TaskID = task.ID
		prop.DiagnosticID = diagID
		if saved, serr := s.store.SaveProposal(ctx, prop); serr == nil {
			out.ProposalID = saved.ID
			if task.ID != "" {
				_ = s.store.SetTaskProposal(ctx, task.ID, saved.ID)
			}
			if task.ParentTaskID != "" {
				_ = s.store.SetTaskProposal(ctx, task.ParentTaskID, saved.ID)
			}
			_, _, _ = s.store.PostMessage(ctx, contract.PostMessageRequest{
				From:  "deskdrop-healer",
				To:    strings.Split(notify, ","),
				Topic: rcp.Topic + ":proposal",
				Body: fmt.Sprintf(
					"Verified recipe patch proposal `%s` for `%s` (verified=%v, iterations=%d). Approve with POST /v1/proposals/%s/approve",
					saved.ID, saved.RecipeName, saved.Verified, saved.Iterations, saved.ID,
				),
				Meta: map[string]any{
					"kind":        "recipe_patch_proposal",
					"proposal_id": saved.ID,
					"recipe":      saved.RecipeName,
					"verified":    saved.Verified,
				},
			}, "proposal-msg:"+saved.ID)
		}
	}
	return out
}

func (s *Service) handleListProposals(w http.ResponseWriter, r *http.Request) {
	statusFilter := r.URL.Query().Get("status")
	limit := parseIntQuery(r, "limit", 30)
	list, err := s.store.ListProposals(r.Context(), statusFilter, limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	if list == nil {
		list = []contract.RecipePatchProposal{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proposals": list})
}

func (s *Service) handleGetProposal(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetProposal(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "proposal not found", nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Service) handleApproveProposal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req contract.ProposalReviewRequest
	if err := decodeJSONBody(r, 32*1024, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), nil)
		return
	}

	prop, err := s.store.GetProposal(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "proposal not found", nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	if prop.Status != contract.ProposalStatusPending {
		writeAPIError(w, http.StatusConflict, "PROPOSAL_NOT_PENDING", fmt.Sprintf("proposal %q is already %s", id, prop.Status), nil)
		return
	}
	if !prop.Verified {
		writeAPIError(w, http.StatusUnprocessableEntity, "PROPOSAL_UNVERIFIED", "cannot approve an unverified recipe patch proposal", nil)
		return
	}
	if err := contract.ValidateRecipeName(prop.ProposedRecipe.Name); err != nil || strings.TrimSpace(prop.ProposedRecipe.Name) != strings.TrimSpace(prop.RecipeName) {
		writeAPIError(w, http.StatusUnprocessableEntity, string(contract.ErrorCodeContractInvalid), fmt.Sprintf("proposed_recipe.name %q must strictly match proposal recipe_name %q", prop.ProposedRecipe.Name, prop.RecipeName), nil)
		return
	}

	// Fail closed: recipe must exist in catalog and proposed domains must remain inside approved domains.
	existing, exists, gerr := s.catalog.Get(r.Context(), prop.RecipeName)
	if gerr != nil {
		writeAPIError(w, http.StatusInternalServerError, "CATALOG_ERROR", gerr.Error(), nil)
		return
	}
	if !exists {
		writeAPIError(w, http.StatusForbidden, "DOMAIN_PIVOT_BLOCKED", fmt.Sprintf("recipe %q is not registered in RECIPES_URI", prop.RecipeName), nil)
		return
	}
	allowed := contract.InferRecipeDomains(existing)
	if err := contract.ValidateRecipeDomains(prop.ProposedRecipe, allowed, nil); err != nil {
		writeAPIError(w, http.StatusForbidden, "DOMAIN_PIVOT_BLOCKED", fmt.Sprintf("proposed recipe domain check failed: %v", err), nil)
		return
	}

	rawProposal, _ := json.Marshal(prop.ProposedRecipe)
	if err := contract.EnforceAirGap(string(rawProposal)+" "+req.Note+" "+req.ReviewedBy, s.cfg.ForbiddenTerms); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "AIRGAP_VIOLATION", err.Error(), nil)
		return
	}

	reviewer := strings.TrimSpace(req.ReviewedBy)
	if reviewer == "" {
		reviewer = "operator"
	}
	// Reserve approval state atomically BEFORE writing RECIPES_URI so a concurrent /reject
	// cannot race with SaveRecipe and leave RECIPES_URI overwritten on a 409 conflict.
	approved, err := s.store.ReviewProposal(r.Context(), id, contract.ProposalStatusApproved, reviewer, req.Note)
	if errors.Is(err, ErrConflict) {
		writeAPIError(w, http.StatusConflict, "PROPOSAL_NOT_PENDING", "proposal already reviewed", nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}

	if err := s.catalog.SaveRecipe(r.Context(), prop.ProposedRecipe); err != nil {
		_ = s.store.RevertProposalToPending(r.Context(), id)
		writeAPIError(w, http.StatusInternalServerError, "CATALOG_WRITE_ERROR", err.Error(), nil)
		return
	}

	_, _, _ = s.store.PostMessage(r.Context(), contract.PostMessageRequest{
		From:  reviewer,
		To:    []string{"all"},
		Topic: prop.RecipeName + ":approved",
		Body:  fmt.Sprintf("Approved recipe patch proposal `%s` for `%s` and wrote back to RECIPES_URI.", approved.ID, approved.RecipeName),
	}, "approve-msg:"+approved.ID)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"proposal": approved,
	})
}

func (s *Service) handleRejectProposal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req contract.ProposalReviewRequest
	if err := decodeJSONBody(r, 32*1024, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), nil)
		return
	}
	reviewer := strings.TrimSpace(req.ReviewedBy)
	if reviewer == "" {
		reviewer = "operator"
	}
	rejected, err := s.store.ReviewProposal(r.Context(), id, contract.ProposalStatusRejected, reviewer, req.Note)
	if errors.Is(err, ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "proposal not found", nil)
		return
	}
	if errors.Is(err, ErrConflict) {
		writeAPIError(w, http.StatusConflict, "PROPOSAL_NOT_PENDING", "proposal already reviewed", nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"proposal": rejected,
	})
}
