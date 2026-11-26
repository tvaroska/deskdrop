package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
)

var (
	ErrConflict = errors.New("state conflict")
)

// Store is the Driven Port for control-plane persistence (MemoryStore or atomic JSON FileStore).
type Store interface {
	Ping(ctx context.Context) error
	Close() error

	// Message board (/v1/messages)
	PostMessage(ctx context.Context, req contract.PostMessageRequest, idempotencyKey string) (contract.BusMessage, bool, error)
	ListMessages(ctx context.Context, target string, statusFilter string, limit int) ([]contract.BusMessage, error)
	AckMessage(ctx context.Context, idOrShortID string, req contract.AckMessageRequest) (contract.BusMessage, error)

	// Structured drops (/v1/drops)
	SaveDrop(ctx context.Context, req contract.SubmitDropRequest, idempotencyKey string) (contract.DropRecord, bool, error)
	GetDrop(ctx context.Context, idOrShortID string) (contract.DropRecord, error)
	ListDrops(ctx context.Context, topic string, recipe string, limit int) ([]contract.DropRecord, error)

	// Diagnostic bundles & per-domain auth circuit breakers
	SaveDiagnostic(ctx context.Context, diag contract.DiagnosticRecord) error
	GetDiagnostic(ctx context.Context, id string) (contract.DiagnosticRecord, error)
	ListDiagnostics(ctx context.Context, limit int) ([]contract.DiagnosticRecord, error)

	TripDomainAuthBreaker(ctx context.Context, domain string, reason string) (contract.DomainBreaker, bool, error)
	ClearDomainBreaker(ctx context.Context, domain string) error
	ListDomainBreakers(ctx context.Context) ([]contract.DomainBreaker, error)

	// Bidirectional extraction & fallback task queue
	EnqueueTask(ctx context.Context, req contract.EnqueueTaskRequest) (contract.Task, bool, error)
	ClaimNextTask(ctx context.Context, host string, lease time.Duration, allow func(contract.Task) bool) (*contract.Task, error)
	HeartbeatTask(ctx context.Context, taskID string, host string, lease time.Duration) (contract.Task, error)
	CompleteTask(ctx context.Context, taskID string, dropID string) (contract.Task, error)
	FailTask(ctx context.Context, taskID string, host string, triage contract.ErrorTriage, errMsg string, diagID string) (contract.Task, contract.DomainBreaker, bool, error)
	SetTaskProposal(ctx context.Context, taskID string, proposalID string) error
	GetTask(ctx context.Context, taskID string) (contract.Task, error)
	ListTasks(ctx context.Context, statusFilter string, limit int) ([]contract.Task, error)

	// Desktop worker readiness telemetry
	UpsertWorkerStatus(ctx context.Context, status contract.WorkerStatus) (contract.WorkerStatus, error)
	ListWorkerStatuses(ctx context.Context) ([]contract.WorkerStatus, error)

	// Verified recipe patch proposals (human-approval gate)
	SaveProposal(ctx context.Context, p contract.RecipePatchProposal) (contract.RecipePatchProposal, error)
	GetProposal(ctx context.Context, id string) (contract.RecipePatchProposal, error)
	ListProposals(ctx context.Context, statusFilter string, limit int) ([]contract.RecipePatchProposal, error)
	ReviewProposal(ctx context.Context, id string, status contract.ProposalStatus, reviewedBy string, note string) (contract.RecipePatchProposal, error)
	RevertProposalToPending(ctx context.Context, id string) error
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(b))
}

func newShortID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
}

func formatISO(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000Z")
}

func normalizeRecipients(in []string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, raw := range in {
		for _, part := range strings.Split(raw, ",") {
			s := strings.ToLower(strings.TrimSpace(part))
			if s == "" {
				continue
			}
			if _, ok := seen[s]; !ok {
				seen[s] = struct{}{}
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		return []string{"all"}
	}
	return out
}

func formatAckStrings(notes []contract.AckNote) []string {
	if len(notes) == 0 {
		return nil
	}
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		if n.Note != "" {
			out = append(out, fmt.Sprintf("%s (%s): %s", n.From, n.Status, n.Note))
		}
	}
	return out
}
