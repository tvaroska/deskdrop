package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/recipes"
)

type Scope int

const (
	ScopeDesktop Scope = 1
	ScopeAgent   Scope = 2
)

// ServerConfig configures the deskdrop HTTP relay and control plane.
type ServerConfig struct {
	Version            string
	PublicBaseURL      string
	DesktopClientToken string
	AgentBusToken      string
	AdminToken         string
	DiscordWebhookURL  string
	ReleaseDir         string
	ForbiddenTerms     []string
	DefaultLease       time.Duration
}

// Service implements HTTP handlers for both /v1/* and /desktop/v1/* (wire-compatible cutover).
type Service struct {
	cfg        ServerConfig
	store      Store
	blobs      BlobStore
	catalog    recipes.Catalog
	healer     ExceptionHealer
	httpClient *http.Client
	mux        *http.ServeMux
}

// NewService constructs a Service wired to Store, BlobStore, Catalog, and optional ExceptionHealer.
func NewService(cfg ServerConfig, store Store, blobs BlobStore, catalog recipes.Catalog, healer ExceptionHealer) *Service {
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if cfg.DefaultLease <= 0 {
		cfg.DefaultLease = 90 * time.Second
	}
	if blobs == nil {
		blobs = NewMemoryBlobStore()
	}
	if catalog == nil {
		catalog = recipes.NewStaticCatalog()
	}
	s := &Service{
		cfg:        cfg,
		store:      store,
		blobs:      blobs,
		catalog:    catalog,
		healer:     healer,
		httpClient: &http.Client{Timeout: 8 * time.Second},
		mux:        http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

type requestMetaWriter struct {
	http.ResponseWriter
	method string
	path   string
}

func (rw *requestMetaWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rw := &requestMetaWriter{
		ResponseWriter: w,
		method:         r.Method,
		path:           r.URL.Path,
	}
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("PANIC %s %s: %v", r.Method, r.URL.Path, rec)
			writeAPIError(rw, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error", nil)
		}
	}()
	s.mux.ServeHTTP(rw, r)
}

func (s *Service) handleDual(method string, suffix string, h http.HandlerFunc) {
	s.mux.HandleFunc(method+" /v1"+suffix, h)
	s.mux.HandleFunc(method+" /desktop/v1"+suffix, h)
}

func (s *Service) registerRoutes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /desktop/healthz", s.handleHealthz)

	// Desktop worker scope (desktop client + agent/admin tokens)
	s.handleDual("POST", "/drops", s.withScope(ScopeDesktop, s.handleSubmitDrop))
	s.handleDual("POST", "/errors", s.withScope(ScopeDesktop, s.handleSubmitError))
	s.handleDual("POST", "/tasks/claim", s.withScope(ScopeDesktop, s.handleClaimTask))
	s.handleDual("POST", "/tasks/{id}/heartbeat", s.withScope(ScopeDesktop, s.handleHeartbeatTask))
	s.handleDual("POST", "/tasks/{id}/complete", s.withScope(ScopeDesktop, s.handleCompleteTask))
	s.handleDual("POST", "/tasks/{id}/fail", s.withScope(ScopeDesktop, s.handleFailTask))
	s.handleDual("POST", "/tasks/{id}/fallback-result", s.withScope(ScopeDesktop, s.handleFallbackResult))
	s.handleDual("POST", "/workers/heartbeat", s.withScope(ScopeDesktop, s.handleWorkerHeartbeat))
	s.handleDual("GET", "/workers", s.withScope(ScopeDesktop, s.handleListWorkers))
	s.handleDual("GET", "/status", s.withScope(ScopeDesktop, s.handleDesktopStatus))
	s.handleDual("GET", "/recipes", s.withScope(ScopeDesktop, s.handleListRecipes))
	s.handleDual("GET", "/release/{asset}", s.withScope(ScopeDesktop, s.handleDownloadRelease))

	// Agent & control-plane scope
	s.handleDual("GET", "/drops", s.withScope(ScopeAgent, s.handleListDrops))
	s.handleDual("GET", "/drops/{id}", s.withScope(ScopeAgent, s.handleGetDrop))
	s.handleDual("POST", "/tasks", s.withScope(ScopeAgent, s.handleEnqueueTask))
	s.handleDual("GET", "/tasks", s.withScope(ScopeAgent, s.handleListTasks))
	s.handleDual("GET", "/tasks/{id}", s.withScope(ScopeAgent, s.handleGetTask))
	s.handleDual("GET", "/diagnostics", s.withScope(ScopeAgent, s.handleListDiagnostics))
	s.handleDual("GET", "/diagnostics/{id}/bundle.zip", s.withScope(ScopeAgent, s.handleDownloadDiagnostic))
	s.handleDual("GET", "/breakers", s.withScope(ScopeAgent, s.handleListBreakers))
	s.handleDual("POST", "/breakers/{domain}/resume", s.withScope(ScopeAgent, s.handleResumeBreaker))

	// Message board
	s.handleDual("GET", "/messages", s.withScope(ScopeAgent, s.handleListMessages))
	s.handleDual("POST", "/messages", s.withScope(ScopeAgent, s.handlePostMessage))
	s.handleDual("POST", "/messages/{id}/ack", s.withScope(ScopeAgent, s.handleAckMessage))

	// Recipe patch proposals (human-approval write-back to RECIPES_URI)
	s.handleDual("GET", "/proposals", s.withScope(ScopeAgent, s.handleListProposals))
	s.handleDual("GET", "/proposals/{id}", s.withScope(ScopeAgent, s.handleGetProposal))
	s.handleDual("POST", "/proposals/{id}/approve", s.withScope(ScopeAgent, s.handleApproveProposal))
	s.handleDual("POST", "/proposals/{id}/reject", s.withScope(ScopeAgent, s.handleRejectProposal))
}

func (s *Service) withScope(required Scope, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := extractBearer(r)
		if tok == "" {
			writeAPIError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing Authorization: Bearer token", nil)
			return
		}
		isAdmin := matchToken(tok, s.cfg.AdminToken)
		isAgent := matchToken(tok, s.cfg.AgentBusToken)
		isDesktop := matchToken(tok, s.cfg.DesktopClientToken)

		if !isAdmin && !isAgent && !isDesktop {
			writeAPIError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid bearer token", nil)
			return
		}
		if required == ScopeAgent && !isAdmin && !isAgent {
			writeAPIError(w, http.StatusForbidden, "FORBIDDEN", "desktop client token is write-scoped and cannot access agent control-plane routes", nil)
			return
		}
		next(w, r)
	}
}

func extractBearer(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func matchToken(provided, expected string) bool {
	if expected == "" || provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func (s *Service) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "STORE_UNAVAILABLE", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "deskdrop",
		"version": s.cfg.Version,
	})
}

func (s *Service) handleListMessages(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("for")
	statusFilter := r.URL.Query().Get("status")
	limit := parseIntQuery(r, "limit", 30)
	msgs, err := s.store.ListMessages(r.Context(), target, statusFilter, limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	if msgs == nil {
		msgs = []contract.BusMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"target":   target,
		"messages": msgs,
	})
}

func (s *Service) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	var req contract.PostMessageRequest
	if err := decodeJSONBody(r, 512*1024, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), nil)
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, string(contract.ErrorCodeContractInvalid), "message body is required", nil)
		return
	}
	if err := contract.EnforceAirGap(req.Topic+" "+req.Body, s.cfg.ForbiddenTerms); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "AIRGAP_VIOLATION", err.Error(), nil)
		return
	}
	if len(req.Meta) > 0 {
		metaBytes, _ := json.Marshal(req.Meta)
		if err := contract.EnforceAirGap(string(metaBytes), s.cfg.ForbiddenTerms); err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "AIRGAP_VIOLATION", err.Error(), nil)
			return
		}
	}

	idemp := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	msg, created, err := s.store.PostMessage(r.Context(), req, idemp)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	if created {
		s.mirrorWebhookAsync(formatDiscordBusLine(msg.ShortID, msg.From, strings.Join(msg.To, ","), msg.Topic, msg.Body))
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"ok":           true,
		"deduplicated": !created,
		"id":           msg.ID,
		"short_id":     msg.ShortID,
		"message":      msg,
	})
}

func (s *Service) handleAckMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req contract.AckMessageRequest
	if err := decodeJSONBody(r, 64*1024, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), nil)
		return
	}
	if req.Status != "claim" && req.Status != "claimed" && req.Status != "done" && req.Status != "" {
		writeAPIError(w, http.StatusUnprocessableEntity, string(contract.ErrorCodeContractInvalid), "status must be 'claim' or 'done'", nil)
		return
	}
	if req.Note != "" {
		if err := contract.EnforceAirGap(req.Note, s.cfg.ForbiddenTerms); err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, "AIRGAP_VIOLATION", err.Error(), nil)
			return
		}
	}

	msg, err := s.store.AckMessage(r.Context(), id, req)
	if errors.Is(err, ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("message %q not found", id), nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	if req.Note != "" {
		s.mirrorWebhookAsync(fmt.Sprintf("`[bus:%s]` **%s** (%s): %s", msg.ShortID, req.From, msg.Status, req.Note))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"id":       msg.ID,
		"short_id": msg.ShortID,
		"status":   msg.Status,
		"message":  msg,
	})
}

func (s *Service) handleSubmitDrop(w http.ResponseWriter, r *http.Request) {
	var req contract.SubmitDropRequest
	if err := decodeJSONBody(r, 4*1024*1024, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), nil)
		return
	}
	rec, created, busMsg, err := s.ingestDrop(r.Context(), req, strings.TrimSpace(r.Header.Get("Idempotency-Key")))
	if err != nil {
		s.writeIngestError(w, err)
		return
	}
	code := http.StatusCreated
	if !created {
		code = http.StatusOK
	}
	writeJSON(w, code, map[string]any{
		"ok":           true,
		"deduplicated": !created,
		"drop_id":      rec.ID,
		"short_id":     rec.ShortID,
		"message_id":   busMsg.ID,
	})
}

func (s *Service) ingestDrop(ctx context.Context, req contract.SubmitDropRequest, idemp string) (contract.DropRecord, bool, contract.BusMessage, error) {
	if req.Report.Mode == "" {
		req.Report.Mode = contract.DropModeDeterministic
	}
	if err := contract.ValidateDropReport(req.Report, s.cfg.ForbiddenTerms); err != nil {
		return contract.DropRecord{}, false, contract.BusMessage{}, &validationErr{Code: string(contract.ErrorCodeContractInvalid), Msg: err.Error()}
	}
	rawBytes, _ := json.Marshal(req.Report)
	if err := contract.EnforceAirGap(string(rawBytes)+" "+req.From+" "+req.To, s.cfg.ForbiddenTerms); err != nil {
		return contract.DropRecord{}, false, contract.BusMessage{}, &validationErr{Code: "AIRGAP_VIOLATION", Msg: err.Error()}
	}

	rec, created, err := s.store.SaveDrop(ctx, req, idemp)
	if err != nil {
		return contract.DropRecord{}, false, contract.BusMessage{}, err
	}

	var busMsg contract.BusMessage
	if created {
		body := formatDropBusBody(rec, s.cfg.PublicBaseURL)
		busMsg, _, _ = s.store.PostMessage(ctx, contract.PostMessageRequest{
			From:  rec.From,
			To:    strings.Split(rec.To, ","),
			Topic: rec.Report.Topic,
			Body:  body,
			Meta: map[string]any{
				"kind":    "drop",
				"mode":    rec.Report.Mode,
				"drop_id": rec.ID,
				"recipe":  rec.Report.Recipe,
				"task_id": rec.TaskID,
			},
		}, "drop-msg:"+rec.ID)
		s.mirrorWebhookAsync(formatDiscordBusLine(busMsg.ShortID, busMsg.From, rec.To, rec.Report.Topic, summarizeDropShort(rec, s.cfg.PublicBaseURL)))
	}
	return rec, created, busMsg, nil
}

func (s *Service) handleListDrops(w http.ResponseWriter, r *http.Request) {
	topic := r.URL.Query().Get("topic")
	recipe := r.URL.Query().Get("recipe")
	limit := parseIntQuery(r, "limit", 20)
	drops, err := s.store.ListDrops(r.Context(), topic, recipe, limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	if drops == nil {
		drops = []contract.DropRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "drops": drops})
}

func (s *Service) handleGetDrop(w http.ResponseWriter, r *http.Request) {
	rec, err := s.store.GetDrop(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "drop not found", nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Service) handleEnqueueTask(w http.ResponseWriter, r *http.Request) {
	var req contract.EnqueueTaskRequest
	if err := decodeJSONBody(r, 512*1024, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), nil)
		return
	}
	if strings.TrimSpace(req.Recipe) == "" && req.CustomRecipe == nil {
		writeAPIError(w, http.StatusUnprocessableEntity, string(contract.ErrorCodeContractInvalid), "recipe or custom_recipe is required", nil)
		return
	}

	resolved, err := s.catalog.ValidateAndResolveTask(r.Context(), req.Recipe, req.CustomRecipe, req.Vars)
	if errors.Is(err, recipes.ErrUnapprovedUseCase) {
		writeAPIError(w, http.StatusForbidden, "UNAPPROVED_USE_CASE", err.Error(), nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, string(contract.ErrorCodeContractInvalid), err.Error(), nil)
		return
	}
	if req.Recipe == "" {
		req.Recipe = resolved.Name
	}

	rawCheck, _ := json.Marshal(resolved)
	if err := contract.EnforceAirGap(string(rawCheck)+" "+req.RequestedBy+" "+req.NotifyTo, s.cfg.ForbiddenTerms); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "AIRGAP_VIOLATION", err.Error(), nil)
		return
	}
	if req.Domain == "" {
		req.Domain = contract.InferRecipeDomain(resolved)
	}
	if req.FallbackPlan != nil {
		if err := contract.ValidateFallbackPlan(*req.FallbackPlan, contract.InferRecipeDomains(resolved)); err != nil {
			writeAPIError(w, http.StatusUnprocessableEntity, string(contract.ErrorCodeContractInvalid), err.Error(), nil)
			return
		}
	}

	task, deduped, err := s.store.EnqueueTask(r.Context(), req)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	code := http.StatusCreated
	if deduped {
		code = http.StatusOK
	}
	writeJSON(w, code, map[string]any{
		"ok":           true,
		"deduplicated": deduped,
		"task":         task,
	})
}

func (s *Service) handleClaimTask(w http.ResponseWriter, r *http.Request) {
	var req contract.ClaimTaskRequest
	if err := decodeJSONBody(r, 64*1024, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), nil)
		return
	}
	lease := s.cfg.DefaultLease
	if req.LeaseSeconds > 0 && req.LeaseSeconds <= 600 {
		lease = time.Duration(req.LeaseSeconds) * time.Second
	}
	host := strings.TrimSpace(req.Host)
	if host == "" {
		host = "desktop-worker"
	}

	verHdr := headerClientVersion(r)
	if req.Worker != nil {
		ws := *req.Worker
		if ws.Host == "" {
			ws.Host = host
		}
		if ws.Version == "" {
			ws.Version = verHdr
		}
		_, _ = s.store.UpsertWorkerStatus(r.Context(), ws)
	} else {
		_, _ = s.store.UpsertWorkerStatus(r.Context(), contract.WorkerStatus{
			Host:             host,
			Version:          verHdr,
			BrowserConnected: true,
			BrowserStatus:    "polling tasks",
			ReadyToAccept:    true,
			LastAction:       "tasks/claim",
		})
	}

	var allow func(contract.Task) bool
	if len(req.AllowedDrivers) > 0 {
		allowedSet := make(map[string]bool, len(req.AllowedDrivers))
		for _, d := range req.AllowedDrivers {
			if norm := strings.ToLower(strings.TrimSpace(d)); norm != "" {
				allowedSet[norm] = true
			}
		}
		if len(allowedSet) > 0 {
			// Resolve all catalog drivers BEFORE entering MemoryStore.ClaimNextTask's lock
			// so GCS/disk catalog reloads never run while holding the store mutex.
			recipeDrivers := make(map[string]string)
			if s.catalog != nil {
				if list, lerr := s.catalog.List(r.Context()); lerr == nil {
					for _, rcp := range list {
						recipeDrivers[strings.ToLower(strings.TrimSpace(rcp.Name))] = rcp.NormalizedDriver()
					}
				}
			}
			allow = func(t contract.Task) bool {
				return allowedSet[taskDriverFromMap(t, recipeDrivers)]
			}
		}
	}

	task, err := s.store.ClaimNextTask(r.Context(), host, lease, allow)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	if task == nil {
		writeJSON(w, http.StatusOK, contract.ClaimTaskResponse{})
		return
	}

	var resolved *contract.Recipe
	if rcp, rerr := s.catalog.ValidateAndResolveTask(r.Context(), task.Recipe, task.CustomRecipe, task.Vars); rerr == nil {
		resolved = &rcp
	}

	writeJSON(w, http.StatusOK, contract.ClaimTaskResponse{
		Task:           task,
		ResolvedRecipe: resolved,
	})
}

func taskDriverFromMap(t contract.Task, recipeDrivers map[string]string) string {
	if t.Kind == contract.TaskKindFallback {
		return "cdp"
	}
	if t.CustomRecipe != nil {
		return t.CustomRecipe.NormalizedDriver()
	}
	if t.Recipe != "" {
		if drv, ok := recipeDrivers[strings.ToLower(strings.TrimSpace(t.Recipe))]; ok {
			return drv
		}
	}
	if strings.EqualFold(strings.TrimSpace(t.Domain), "uia.local") {
		return "uia"
	}
	return "cdp"
}

func (s *Service) handleHeartbeatTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req contract.ClaimTaskRequest
	_ = decodeJSONBody(r, 32*1024, &req)
	lease := s.cfg.DefaultLease
	if req.LeaseSeconds > 0 && req.LeaseSeconds <= 600 {
		lease = time.Duration(req.LeaseSeconds) * time.Second
	}
	task, err := s.store.HeartbeatTask(r.Context(), id, req.Host, lease)
	if errors.Is(err, ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "task not found", nil)
		return
	}
	if errors.Is(err, ErrConflict) {
		writeAPIError(w, http.StatusConflict, "TASK_NOT_CLAIMED", "task is no longer in claimed state", nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task": task})
}

func (s *Service) handleCompleteTask(w http.ResponseWriter, r *http.Request) {
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

	var req contract.SubmitDropRequest
	if err := decodeJSONBody(r, 4*1024*1024, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), nil)
		return
	}
	req.TaskID = id
	if req.To == "" {
		req.To = task.NotifyTo
	}

	rec, _, busMsg, err := s.ingestDrop(r.Context(), req, "task-complete:"+id)
	if err != nil {
		s.writeIngestError(w, err)
		return
	}

	updated, err := s.store.CompleteTask(r.Context(), id, rec.ID)
	if errors.Is(err, ErrConflict) {
		writeAPIError(w, http.StatusConflict, "TASK_NOT_CLAIMED", "task is no longer in claimed state", nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	if task.ParentTaskID != "" {
		_, _ = s.store.CompleteTask(r.Context(), task.ParentTaskID, rec.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"task":       updated,
		"drop_id":    rec.ID,
		"short_id":   rec.ShortID,
		"message_id": busMsg.ID,
	})
}

func (s *Service) handleListTasks(w http.ResponseWriter, r *http.Request) {
	statusFilter := r.URL.Query().Get("status")
	limit := parseIntQuery(r, "limit", 30)
	tasks, err := s.store.ListTasks(r.Context(), statusFilter, limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	if tasks == nil {
		tasks = []contract.Task{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tasks": tasks})
}

func (s *Service) handleGetTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.store.GetTask(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "task not found", nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Service) handleListDiagnostics(w http.ResponseWriter, r *http.Request) {
	limit := parseIntQuery(r, "limit", 20)
	diags, err := s.store.ListDiagnostics(r.Context(), limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	for i := range diags {
		if diags[i].BlobKey != "" {
			diags[i].DownloadPath = fmt.Sprintf("/v1/diagnostics/%s/bundle.zip", diags[i].ID)
		}
	}
	if diags == nil {
		diags = []contract.DiagnosticRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "diagnostics": diags})
}

func (s *Service) handleDownloadDiagnostic(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	diag, err := s.store.GetDiagnostic(r.Context(), id)
	if errors.Is(err, ErrNotFound) || diag.BlobKey == "" {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "diagnostic bundle not found", nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	data, err := s.blobs.Get(r.Context(), diag.BlobKey)
	if errors.Is(err, ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "diagnostic blob missing from storage", nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "BLOB_ERROR", err.Error(), nil)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="debug_%s.zip"`, diag.ID))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Service) handleListBreakers(w http.ResponseWriter, r *http.Request) {
	breakers, err := s.store.ListDomainBreakers(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	if breakers == nil {
		breakers = []contract.DomainBreaker{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "breakers": breakers})
}

func (s *Service) handleResumeBreaker(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	if err := s.store.ClearDomainBreaker(r.Context(), domain); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "domain": domain, "state": "closed"})
}

func (s *Service) handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	var ws contract.WorkerStatus
	if err := decodeJSONBody(r, 64*1024, &ws); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_JSON", err.Error(), nil)
		return
	}
	if ws.Version == "" {
		ws.Version = headerClientVersion(r)
	}
	updated, err := s.store.UpsertWorkerStatus(r.Context(), ws)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"worker": updated,
	})
}

func (s *Service) handleListWorkers(w http.ResponseWriter, r *http.Request) {
	workers, err := s.store.ListWorkerStatuses(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	if workers == nil {
		workers = []contract.WorkerStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"server_version": s.cfg.Version,
		"workers":        workers,
	})
}

func (s *Service) handleDesktopStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workers, err := s.store.ListWorkerStatuses(ctx)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	if workers == nil {
		workers = []contract.WorkerStatus{}
	}
	breakers, _ := s.store.ListDomainBreakers(ctx)
	if breakers == nil {
		breakers = []contract.DomainBreaker{}
	}
	tasks, _ := s.store.ListTasks(ctx, "all", 50)
	proposals, _ := s.store.ListProposals(ctx, "pending", 50)
	recipeList, _ := s.catalog.List(ctx)

	connectedCount, readyCount := 0, 0
	for _, wk := range workers {
		if wk.Connected {
			connectedCount++
			if wk.BrowserConnected && wk.ReadyToAccept {
				readyCount++
			}
		}
	}
	pendingCount, claimedCount, failedDiagCount := 0, 0, 0
	for _, t := range tasks {
		switch t.Status {
		case contract.TaskStatusPending:
			pendingCount++
		case contract.TaskStatusClaimed:
			claimedCount++
		case contract.TaskStatusFailedDiagnostic:
			failedDiagCount++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"server_version": s.cfg.Version,
		"summary": map[string]any{
			"any_client_connected":    connectedCount > 0,
			"any_browser_ready":       readyCount > 0,
			"connected_workers":       connectedCount,
			"ready_workers":           readyCount,
			"pending_tasks":           pendingCount,
			"claimed_tasks":           claimedCount,
			"failed_diagnostic_tasks": failedDiagCount,
			"open_breakers":           len(breakers),
			"pending_proposals":       len(proposals),
			"approved_recipes":        len(recipeList),
		},
		"workers":  workers,
		"breakers": breakers,
	})
}

func (s *Service) handleDownloadRelease(w http.ResponseWriter, r *http.Request) {
	asset := strings.TrimSpace(r.PathValue("asset"))
	switch asset {
	case "deskdrop.exe", "deskdrop", "SHA256SUMS.txt":
	default:
		writeAPIError(w, http.StatusBadRequest, "INVALID_ASSET", "asset must be deskdrop.exe, deskdrop, or SHA256SUMS.txt", nil)
		return
	}
	relDir := s.cfg.ReleaseDir
	if relDir == "" {
		relDir = "/release"
	}
	data, err := os.ReadFile(filepath.Join(relDir, asset))
	if errors.Is(err, os.ErrNotExist) {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "release asset not found", nil)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "RELEASE_READ_ERROR", err.Error(), nil)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-DeskDrop-Version", s.cfg.Version)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func headerClientVersion(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-DeskDrop-Version")); v != "" {
		return v
	}
	return strings.TrimSpace(r.Header.Get("X-Browser-Drop-Version"))
}
