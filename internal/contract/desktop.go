package contract

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TaskStatus enumerates states in the desktop extraction task queue.
type TaskStatus string

const (
	TaskStatusPending          TaskStatus = "pending"
	TaskStatusClaimed          TaskStatus = "claimed"
	TaskStatusSucceeded        TaskStatus = "succeeded"
	TaskStatusFailedDiagnostic TaskStatus = "failed_diagnostic"
	TaskStatusDeadLetter       TaskStatus = "dead_letter"
)

// TaskKind distinguishes deterministic recipe extraction from bounded Tier-2 fallback runs.
type TaskKind string

const (
	TaskKindExtract  TaskKind = "extract"
	TaskKindFallback TaskKind = "fallback_task"
)

// ErrorCode classifies extraction failures for automated triage and circuit breaking.
type ErrorCode string

const (
	ErrorCodeAuthExpired      ErrorCode = "AUTH_EXPIRED"
	ErrorCodeSelectorDrift    ErrorCode = "SELECTOR_DRIFT"
	ErrorCodeCDPTransient     ErrorCode = "CDP_TRANSIENT"
	ErrorCodeContractInvalid  ErrorCode = "CONTRACT_INVALID"
	ErrorCodeFallbackDeclined ErrorCode = "FALLBACK_DECLINED"
	ErrorCodeUnknown          ErrorCode = "EXTRACTION_FAILED"
)

// TriageAction specifies how the control plane handles a reported task failure.
type TriageAction string

const (
	TriageHoldDomainAuth       TriageAction = "hold_domain_auth"
	TriageDiagnosticEscalation TriageAction = "diagnostic_escalation"
	TriageRetryBackoff         TriageAction = "retry_backoff"
)

// ErrorTriage pairs a normalized ErrorCode with its deterministic recovery action.
type ErrorTriage struct {
	Code   ErrorCode    `json:"code"`
	Action TriageAction `json:"action"`
	Domain string       `json:"domain,omitempty"`
}

// ClassifyExtractionError inspects an extraction error message, landed URL, and bundle presence
// to choose between domain auth circuit breaking, selector repair escalation, or transient retry.
func ClassifyExtractionError(errMsg string, landedURL string, fallbackDomain string, hasBundle bool) ErrorTriage {
	lowerErr := strings.ToLower(errMsg)
	lowerURL := strings.ToLower(landedURL)
	domain := ExtractDomain(landedURL)
	if domain == "" || strings.Contains(domain, "accounts.google.") || strings.Contains(domain, "login.") {
		if fallbackDomain != "" {
			domain = fallbackDomain
		}
	}

	if strings.Contains(lowerErr, "fallback declined") || strings.Contains(lowerErr, "fallback_declined") {
		return ErrorTriage{
			Code:   ErrorCodeFallbackDeclined,
			Action: TriageDiagnosticEscalation,
			Domain: domain,
		}
	}

	if strings.Contains(lowerErr, "blocked/login url") ||
		strings.Contains(lowerErr, "session expired") ||
		strings.Contains(lowerErr, "sign in") ||
		strings.Contains(lowerErr, "unusual traffic") ||
		strings.Contains(lowerErr, "captcha") ||
		strings.Contains(lowerURL, "accounts.google.com") ||
		strings.Contains(lowerURL, "/servicelogin") ||
		strings.Contains(lowerURL, "/signin") ||
		strings.Contains(lowerURL, "/login") ||
		strings.Contains(lowerURL, "/sorry/") ||
		strings.Contains(lowerURL, "captcha") ||
		strings.Contains(lowerURL, "/challenge") {
		if fallbackDomain != "" && !DomainsMatch(domain, fallbackDomain) {
			domain = fallbackDomain
		}
		return ErrorTriage{
			Code:   ErrorCodeAuthExpired,
			Action: TriageHoldDomainAuth,
			Domain: domain,
		}
	}

	if strings.Contains(lowerErr, "extracted 0 table") ||
		strings.Contains(lowerErr, "extracted 0 metric") ||
		strings.Contains(lowerErr, "extracted 0 item") ||
		strings.Contains(lowerErr, "missing required field") ||
		strings.Contains(lowerErr, "matched 0 non-empty selector") ||
		strings.Contains(lowerErr, "missing expected text") ||
		strings.Contains(lowerErr, "expected >=") ||
		strings.Contains(lowerErr, "timed out waiting for selector") ||
		strings.Contains(lowerErr, "selector ") {
		return ErrorTriage{
			Code:   ErrorCodeSelectorDrift,
			Action: TriageDiagnosticEscalation,
			Domain: domain,
		}
	}

	if strings.Contains(lowerErr, "websocket") ||
		strings.Contains(lowerErr, "connection refused") ||
		strings.Contains(lowerErr, "context deadline exceeded") ||
		strings.Contains(lowerErr, "target closed") ||
		strings.Contains(lowerErr, "cdp ") ||
		strings.Contains(lowerErr, "eof") ||
		strings.Contains(lowerErr, "timeout") {
		return ErrorTriage{
			Code:   ErrorCodeCDPTransient,
			Action: TriageRetryBackoff,
			Domain: domain,
		}
	}

	if hasBundle {
		return ErrorTriage{
			Code:   ErrorCodeUnknown,
			Action: TriageDiagnosticEscalation,
			Domain: domain,
		}
	}
	return ErrorTriage{
		Code:   ErrorCodeUnknown,
		Action: TriageRetryBackoff,
		Domain: domain,
	}
}

// ComputeRetryBackoff returns exponential retry delay (15s, 60s, 240s, capped at 10m).
func ComputeRetryBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return 15 * time.Second
	}
	d := 15 * time.Second
	for i := 1; i < attempt; i++ {
		d *= 4
		if d > 10*time.Minute {
			return 10 * time.Minute
		}
	}
	return d
}

// ExtractDomain parses a normalized lowercase hostname from a URL or match pattern.
func ExtractDomain(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + strings.TrimPrefix(s, "//")
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// DomainsMatch reports whether host matches allowedDomain (exact or subdomain match).
func DomainsMatch(host string, allowedDomain string) bool {
	h := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(host)), "www.")
	a := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(allowedDomain)), "www.")
	if h == "" || a == "" {
		return false
	}
	return h == a || strings.HasSuffix(h, "."+a)
}

// InferRecipeDomain extracts the primary target domain of a Recipe from its steps.
func InferRecipeDomain(r Recipe) string {
	for _, d := range r.AllowedDomains {
		if norm := ExtractDomain(d); norm != "" {
			return norm
		}
	}
	for _, s := range r.Steps {
		if d := ExtractDomain(s.URL); d != "" && !strings.Contains(d, "{{") {
			return d
		}
		if d := ExtractDomain(s.MatchTabURL); d != "" && !strings.Contains(d, "{{") {
			return d
		}
	}
	if r.NormalizedDriver() == "uia" {
		return "uia.local"
	}
	return ""
}

// InferRecipeDomains returns all distinct hostnames referenced by a Recipe.
func InferRecipeDomains(r Recipe) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(raw string) {
		if strings.Contains(raw, "{{") {
			return
		}
		d := ExtractDomain(raw)
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, d := range r.AllowedDomains {
		add(d)
	}
	for _, s := range r.Steps {
		add(s.URL)
		add(s.MatchTabURL)
	}
	if len(out) == 0 && r.NormalizedDriver() == "uia" {
		out = append(out, "uia.local")
	}
	return out
}

// FallbackAction represents a single bounded Tier-2 recovery interaction (zero text input).
type FallbackAction struct {
	Action   string `json:"action"` // "scroll" | "wait" | "click"
	Selector string `json:"selector,omitempty"`
	DeltaY   int    `json:"delta_y,omitempty"`
	WaitMs   int    `json:"wait_ms,omitempty"`
}

// FallbackPlan defines at most 3 safe stateless actions on an allowlisted domain.
type FallbackPlan struct {
	AllowedDomain string           `json:"allowed_domain"`
	TargetURL     string           `json:"target_url"`
	StepID        string           `json:"step_id,omitempty"`
	Reason        string           `json:"reason,omitempty"`
	Actions       []FallbackAction `json:"actions"`
}

// ValidateFallbackPlan enforces the ADR-004 Tier-2 safety envelope (<=3 safe actions, same domain, no text input).
func ValidateFallbackPlan(plan FallbackPlan, allowedDomains []string) error {
	if len(plan.Actions) == 0 || len(plan.Actions) > 3 {
		return fmt.Errorf("fallback_plan must contain between 1 and 3 actions, got %d", len(plan.Actions))
	}
	u, err := url.Parse(strings.TrimSpace(plan.TargetURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("fallback_plan.target_url %q must be an http/https URL", plan.TargetURL)
	}
	targetDomain := strings.ToLower(u.Hostname())
	allowedDomain := ExtractDomain(plan.AllowedDomain)
	if allowedDomain == "" {
		return errors.New("fallback_plan.allowed_domain is required")
	}
	if !DomainsMatch(targetDomain, allowedDomain) {
		return fmt.Errorf("fallback_plan target domain %q violates allowed_domain %q", targetDomain, allowedDomain)
	}
	if len(allowedDomains) > 0 {
		matched := false
		for _, dom := range allowedDomains {
			if DomainsMatch(targetDomain, dom) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("fallback_plan target domain %q is not in approved recipe domains", targetDomain)
		}
	}
	for i, act := range plan.Actions {
		switch strings.ToLower(strings.TrimSpace(act.Action)) {
		case "scroll":
			if act.DeltaY == 0 {
				return fmt.Errorf("fallback_plan.actions[%d] scroll requires non-zero delta_y", i)
			}
		case "wait":
			if act.WaitMs <= 0 || act.WaitMs > 5000 {
				return fmt.Errorf("fallback_plan.actions[%d] wait_ms must be within 1..5000", i)
			}
		case "click":
			if strings.TrimSpace(act.Selector) == "" {
				return fmt.Errorf("fallback_plan.actions[%d] click requires selector", i)
			}
		default:
			return fmt.Errorf("fallback_plan.actions[%d] unsupported action %q (allowed: scroll, wait, click)", i, act.Action)
		}
	}
	return nil
}

// ValidateDropReport enforces structural invariants on a DropReport.
func ValidateDropReport(r DropReport, _ []string) error {
	if strings.TrimSpace(r.Recipe) == "" {
		return errors.New("drop_report.recipe is required")
	}
	if strings.TrimSpace(r.Topic) == "" {
		return errors.New("drop_report.topic is required")
	}
	if r.Mode != "" && r.Mode != DropModeDeterministic && r.Mode != DropModeComputerUseFallback {
		return fmt.Errorf("drop_report.mode %q is invalid", r.Mode)
	}
	if len(r.Steps) == 0 {
		return errors.New("drop_report.steps must contain at least 1 step")
	}
	for i, s := range r.Steps {
		if strings.TrimSpace(s.StepID) == "" {
			return fmt.Errorf("drop_report.steps[%d].step_id is required", i)
		}
	}
	return nil
}

// AttemptLog records a single execution attempt on a queued Task.
type AttemptLog struct {
	Attempt      int       `json:"attempt"`
	Host         string    `json:"host,omitempty"`
	ErrorCode    ErrorCode `json:"error_code,omitempty"`
	Error        string    `json:"error,omitempty"`
	DiagnosticID string    `json:"diagnostic_id,omitempty"`
	TimestampUTC string    `json:"timestamp_utc"`
}

// Task represents an agent-requested desktop extraction job or bounded Tier-2 fallback task.
type Task struct {
	ID           string            `json:"id"`
	Kind         TaskKind          `json:"kind,omitempty"`
	ParentTaskID string            `json:"parent_task_id,omitempty"`
	Recipe       string            `json:"recipe"`
	Vars         map[string]string `json:"vars,omitempty"`
	CustomRecipe *Recipe           `json:"custom_recipe,omitempty"`
	FallbackPlan *FallbackPlan     `json:"fallback_plan,omitempty"`
	Domain       string            `json:"domain,omitempty"`
	Priority     int               `json:"priority"`
	Status       TaskStatus        `json:"status"`
	Attempt      int               `json:"attempt"`
	MaxAttempts  int               `json:"max_attempts"`
	RequestedBy  string            `json:"requested_by"`
	NotifyTo     string            `json:"notify_to"`
	DedupeKey    string            `json:"dedupe_key,omitempty"`
	ClaimedBy    string            `json:"claimed_by,omitempty"`
	LeaseUntil   string            `json:"lease_until,omitempty"`
	NextRunAt    string            `json:"next_run_at"`
	DropID       string            `json:"drop_id,omitempty"`
	DiagnosticID string            `json:"diagnostic_id,omitempty"`
	ProposalID   string            `json:"proposal_id,omitempty"`
	LastError    string            `json:"last_error,omitempty"`
	LastCode     ErrorCode         `json:"last_code,omitempty"`
	Attempts     []AttemptLog      `json:"attempts,omitempty"`
	CreatedAtUTC string            `json:"created_at_utc"`
	UpdatedAtUTC string            `json:"updated_at_utc"`
}

// EnqueueTaskRequest is sent by an agent to request an extraction job.
type EnqueueTaskRequest struct {
	Kind         TaskKind          `json:"kind,omitempty"`
	ParentTaskID string            `json:"parent_task_id,omitempty"`
	Recipe       string            `json:"recipe"`
	Vars         map[string]string `json:"vars,omitempty"`
	CustomRecipe *Recipe           `json:"custom_recipe,omitempty"`
	FallbackPlan *FallbackPlan     `json:"fallback_plan,omitempty"`
	Domain       string            `json:"domain,omitempty"`
	Priority     int               `json:"priority,omitempty"`
	MaxAttempts  int               `json:"max_attempts,omitempty"`
	RequestedBy  string            `json:"requested_by,omitempty"`
	NotifyTo     string            `json:"notify_to,omitempty"`
	DedupeKey    string            `json:"dedupe_key,omitempty"`
}

// WorkerStatus reports live desktop client connectivity, version, browser attachment, and task readiness.
type WorkerStatus struct {
	Host              string   `json:"host"`
	Hostname          string   `json:"hostname,omitempty"`
	Version           string   `json:"version"`
	OS                string   `json:"os,omitempty"`
	Arch              string   `json:"arch,omitempty"`
	Connected         bool     `json:"connected"`
	AgeSeconds        int      `json:"age_seconds"`
	BrowserConnected  bool     `json:"browser_connected"`
	BrowserStatus     string   `json:"browser_status,omitempty"`
	ReadyToAccept     bool     `json:"ready_to_accept"`
	Busy              bool     `json:"busy,omitempty"`
	FallbackGate      string   `json:"fallback_gate,omitempty"`
	ChromeSession     string   `json:"chrome_session,omitempty"`
	AvailableSessions []string `json:"available_sessions,omitempty"`
	OpenTabsCount     int      `json:"open_tabs_count,omitempty"`
	LastAction        string   `json:"last_action,omitempty"`
	LastError         string   `json:"last_error,omitempty"`
	LastSeenUTC       string   `json:"last_seen_utc"`
}

// ClaimTaskRequest is sent by deskdrop sync to lease the next runnable job.
type ClaimTaskRequest struct {
	Host           string        `json:"host"`
	LeaseSeconds   int           `json:"lease_seconds,omitempty"`
	AllowedDrivers []string      `json:"allowed_drivers,omitempty"`
	Worker         *WorkerStatus `json:"worker,omitempty"`
}

// ClaimTaskResponse returns the leased Task (or Task == nil when queue is idle).
type ClaimTaskResponse struct {
	Task           *Task   `json:"task,omitempty"`
	ResolvedRecipe *Recipe `json:"resolved_recipe,omitempty"`
}

// SubmitDropRequest wraps a DropReport pushed by the desktop worker.
type SubmitDropRequest struct {
	From   string     `json:"from,omitempty"`
	To     string     `json:"to,omitempty"`
	TaskID string     `json:"task_id,omitempty"`
	Report DropReport `json:"report"`
}

// DropRecord is the persisted server representation of a DropReport.
type DropRecord struct {
	ID            string     `json:"id"`
	ShortID       string     `json:"short_id"`
	TaskID        string     `json:"task_id,omitempty"`
	MessageID     string     `json:"message_id,omitempty"`
	From          string     `json:"from"`
	To            string     `json:"to"`
	Idempotency   string     `json:"idempotency_key,omitempty"`
	Report        DropReport `json:"report"`
	ReceivedAtUTC string     `json:"received_at_utc"`
}

// FailReportMetadata carries error context in /errors or /tasks/{id}/fail requests.
type FailReportMetadata struct {
	From      string    `json:"from,omitempty"`
	To        string    `json:"to,omitempty"`
	Topic     string    `json:"topic,omitempty"`
	Recipe    string    `json:"recipe,omitempty"`
	StepID    string    `json:"step_id,omitempty"`
	LandedURL string    `json:"landed_url,omitempty"`
	Domain    string    `json:"domain,omitempty"`
	ErrorCode ErrorCode `json:"error_code,omitempty"`
	Error     string    `json:"error"`
}

// DiagnosticRecord tracks an uploaded failure diagnostic bundle in blob storage.
type DiagnosticRecord struct {
	ID           string    `json:"id"`
	TaskID       string    `json:"task_id,omitempty"`
	Topic        string    `json:"topic"`
	Recipe       string    `json:"recipe,omitempty"`
	Host         string    `json:"host,omitempty"`
	Domain       string    `json:"domain,omitempty"`
	ErrorCode    ErrorCode `json:"error_code"`
	Error        string    `json:"error"`
	BlobKey      string    `json:"blob_key,omitempty"`
	SizeBytes    int       `json:"size_bytes"`
	CreatedAtUTC string    `json:"created_at_utc"`
	DownloadPath string    `json:"download_path,omitempty"`
}

// DomainBreaker tracks per-domain circuit-breaker state (e.g. paused on expired auth).
type DomainBreaker struct {
	Domain       string `json:"domain"`
	State        string `json:"state"` // "closed" | "paused_auth"
	Reason       string `json:"reason,omitempty"`
	Alerted      bool   `json:"alerted"`
	TrippedAtUTC string `json:"tripped_at_utc,omitempty"`
	UpdatedAtUTC string `json:"updated_at_utc"`
}

// ProposalStatus enumerates the human-approval lifecycle of a RecipePatchProposal.
type ProposalStatus string

const (
	ProposalStatusPending  ProposalStatus = "pending"
	ProposalStatusApproved ProposalStatus = "approved"
	ProposalStatusRejected ProposalStatus = "rejected"
)

// RecipePatchProposal records a verified declarative recipe update drafted by the exception healer.
// It is never written back to RECIPES_URI until explicitly approved by a human operator.
type RecipePatchProposal struct {
	ID                  string         `json:"id"`
	RecipeName          string         `json:"recipe_name"`
	TaskID              string         `json:"task_id,omitempty"`
	DiagnosticID        string         `json:"diagnostic_id,omitempty"`
	Status              ProposalStatus `json:"status"`
	Rationale           string         `json:"rationale,omitempty"`
	ProposedRecipe      Recipe         `json:"proposed_recipe"`
	Verified            bool           `json:"verified"`
	VerificationSummary string         `json:"verification_summary,omitempty"`
	Iterations          int            `json:"iterations"`
	Model               string         `json:"model,omitempty"`
	CreatedAtUTC        string         `json:"created_at_utc"`
	UpdatedAtUTC        string         `json:"updated_at_utc"`
	ReviewedBy          string         `json:"reviewed_by,omitempty"`
	ReviewNote          string         `json:"review_note,omitempty"`
	ReviewedAtUTC       string         `json:"reviewed_at_utc,omitempty"`
}

// ProposalReviewRequest carries reviewer attribution on approve/reject endpoints.
type ProposalReviewRequest struct {
	ReviewedBy string `json:"reviewed_by,omitempty"`
	Note       string `json:"note,omitempty"`
}

// AckNote records a claim or completion annotation on a message board entry.
type AckNote struct {
	From         string `json:"from"`
	Status       string `json:"status"`
	Note         string `json:"note,omitempty"`
	TimestampUTC string `json:"timestamp_utc"`
}

// BusMessage represents a structured message on the relay message board.
type BusMessage struct {
	ID        string         `json:"id"`
	ShortID   string         `json:"short_id"`
	From      string         `json:"from"`
	To        []string       `json:"to"`
	Topic     string         `json:"topic,omitempty"`
	Status    string         `json:"status"` // "open" | "claimed" | "done"
	Body      string         `json:"body"`
	Notes     []string       `json:"notes,omitempty"`
	AckNotes  []AckNote      `json:"ack_notes,omitempty"`
	Meta      map[string]any `json:"meta,omitempty"`
	Timestamp string         `json:"timestamp"`
}

// PostMessageRequest creates a message on /v1/messages.
type PostMessageRequest struct {
	From  string         `json:"from"`
	To    []string       `json:"to"`
	Topic string         `json:"topic,omitempty"`
	Body  string         `json:"body"`
	Meta  map[string]any `json:"meta,omitempty"`
}

// AckMessageRequest claims or completes a message on /v1/messages/{id}/ack.
type AckMessageRequest struct {
	From   string `json:"from"`
	Status string `json:"status"` // "claim" | "claimed" | "done"
	Note   string `json:"note,omitempty"`
}
