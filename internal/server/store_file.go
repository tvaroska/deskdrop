package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
)

// FileStore persists MemoryStore state atomically to a local JSON file (/data/state.json)
// via temp-file write + fsync + POSIX rename, eliminating PostgreSQL dependencies (ADR-004).
type FileStore struct {
	path string
	mem  *MemoryStore
	ioMu sync.Mutex
}

var _ Store = (*FileStore)(nil)

// OpenFileStore loads an existing JSON state file (or starts empty if absent) and verifies directory access.
func OpenFileStore(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("state file path is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", dir, err)
	}
	fs := &FileStore{
		path: path,
		mem:  NewMemoryStore(),
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fs, nil
		}
		return nil, fmt.Errorf("read state file %s: %w", path, err)
	}
	if len(raw) > 0 {
		var snap storeSnapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			return nil, fmt.Errorf("decode state file %s: %w", path, err)
		}
		fs.mem.importSnapshot(snap)
	}
	return fs, nil
}

func (f *FileStore) persist() error {
	f.ioMu.Lock()
	defer f.ioMu.Unlock()

	snap := f.mem.exportSnapshot()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, f.path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func (f *FileStore) Ping(_ context.Context) error {
	info, err := os.Stat(filepath.Dir(f.path))
	if err != nil {
		return fmt.Errorf("state directory unavailable: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("state directory %s is not a directory", filepath.Dir(f.path))
	}
	return nil
}

func (f *FileStore) Close() error { return nil }

func (f *FileStore) PostMessage(ctx context.Context, req contract.PostMessageRequest, idempotencyKey string) (contract.BusMessage, bool, error) {
	msg, created, err := f.mem.PostMessage(ctx, req, idempotencyKey)
	if err != nil || !created {
		return msg, created, err
	}
	if perr := f.persist(); perr != nil {
		return msg, created, perr
	}
	return msg, created, nil
}

func (f *FileStore) ListMessages(ctx context.Context, target string, statusFilter string, limit int) ([]contract.BusMessage, error) {
	return f.mem.ListMessages(ctx, target, statusFilter, limit)
}

func (f *FileStore) AckMessage(ctx context.Context, idOrShortID string, req contract.AckMessageRequest) (contract.BusMessage, error) {
	msg, err := f.mem.AckMessage(ctx, idOrShortID, req)
	if err != nil {
		return msg, err
	}
	if perr := f.persist(); perr != nil {
		return msg, perr
	}
	return msg, nil
}

func (f *FileStore) SaveDrop(ctx context.Context, req contract.SubmitDropRequest, idempotencyKey string) (contract.DropRecord, bool, error) {
	rec, created, err := f.mem.SaveDrop(ctx, req, idempotencyKey)
	if err != nil || !created {
		return rec, created, err
	}
	if perr := f.persist(); perr != nil {
		return rec, created, perr
	}
	return rec, created, nil
}

func (f *FileStore) GetDrop(ctx context.Context, idOrShortID string) (contract.DropRecord, error) {
	return f.mem.GetDrop(ctx, idOrShortID)
}

func (f *FileStore) ListDrops(ctx context.Context, topic string, recipe string, limit int) ([]contract.DropRecord, error) {
	return f.mem.ListDrops(ctx, topic, recipe, limit)
}

func (f *FileStore) SaveDiagnostic(ctx context.Context, diag contract.DiagnosticRecord) error {
	if err := f.mem.SaveDiagnostic(ctx, diag); err != nil {
		return err
	}
	return f.persist()
}

func (f *FileStore) GetDiagnostic(ctx context.Context, id string) (contract.DiagnosticRecord, error) {
	return f.mem.GetDiagnostic(ctx, id)
}

func (f *FileStore) ListDiagnostics(ctx context.Context, limit int) ([]contract.DiagnosticRecord, error) {
	return f.mem.ListDiagnostics(ctx, limit)
}

func (f *FileStore) TripDomainAuthBreaker(ctx context.Context, domain string, reason string) (contract.DomainBreaker, bool, error) {
	b, newly, err := f.mem.TripDomainAuthBreaker(ctx, domain, reason)
	if err != nil {
		return b, newly, err
	}
	if perr := f.persist(); perr != nil {
		return b, newly, perr
	}
	return b, newly, nil
}

func (f *FileStore) ClearDomainBreaker(ctx context.Context, domain string) error {
	if err := f.mem.ClearDomainBreaker(ctx, domain); err != nil {
		return err
	}
	return f.persist()
}

func (f *FileStore) ListDomainBreakers(ctx context.Context) ([]contract.DomainBreaker, error) {
	return f.mem.ListDomainBreakers(ctx)
}

func (f *FileStore) EnqueueTask(ctx context.Context, req contract.EnqueueTaskRequest) (contract.Task, bool, error) {
	task, deduped, err := f.mem.EnqueueTask(ctx, req)
	if err != nil || deduped {
		return task, deduped, err
	}
	if perr := f.persist(); perr != nil {
		return task, deduped, perr
	}
	return task, deduped, nil
}

func (f *FileStore) ClaimNextTask(ctx context.Context, host string, lease time.Duration) (*contract.Task, error) {
	task, err := f.mem.ClaimNextTask(ctx, host, lease)
	if err != nil || task == nil {
		return task, err
	}
	if perr := f.persist(); perr != nil {
		return task, perr
	}
	return task, nil
}

func (f *FileStore) HeartbeatTask(ctx context.Context, taskID string, host string, lease time.Duration) (contract.Task, error) {
	task, err := f.mem.HeartbeatTask(ctx, taskID, host, lease)
	if err != nil {
		return task, err
	}
	if perr := f.persist(); perr != nil {
		return task, perr
	}
	return task, nil
}

func (f *FileStore) CompleteTask(ctx context.Context, taskID string, dropID string) (contract.Task, error) {
	task, err := f.mem.CompleteTask(ctx, taskID, dropID)
	if err != nil {
		return task, err
	}
	if perr := f.persist(); perr != nil {
		return task, perr
	}
	return task, nil
}

func (f *FileStore) FailTask(ctx context.Context, taskID string, host string, triage contract.ErrorTriage, errMsg string, diagID string) (contract.Task, contract.DomainBreaker, bool, error) {
	task, brk, newly, err := f.mem.FailTask(ctx, taskID, host, triage, errMsg, diagID)
	if err != nil {
		return task, brk, newly, err
	}
	if perr := f.persist(); perr != nil {
		return task, brk, newly, perr
	}
	return task, brk, newly, nil
}

func (f *FileStore) SetTaskProposal(ctx context.Context, taskID string, proposalID string) error {
	if err := f.mem.SetTaskProposal(ctx, taskID, proposalID); err != nil {
		return err
	}
	return f.persist()
}

func (f *FileStore) GetTask(ctx context.Context, taskID string) (contract.Task, error) {
	return f.mem.GetTask(ctx, taskID)
}

func (f *FileStore) ListTasks(ctx context.Context, statusFilter string, limit int) ([]contract.Task, error) {
	return f.mem.ListTasks(ctx, statusFilter, limit)
}

func (f *FileStore) UpsertWorkerStatus(ctx context.Context, status contract.WorkerStatus) (contract.WorkerStatus, error) {
	ws, err := f.mem.UpsertWorkerStatus(ctx, status)
	if err != nil {
		return ws, err
	}
	if perr := f.persist(); perr != nil {
		return ws, perr
	}
	return ws, nil
}

func (f *FileStore) ListWorkerStatuses(ctx context.Context) ([]contract.WorkerStatus, error) {
	return f.mem.ListWorkerStatuses(ctx)
}

func (f *FileStore) SaveProposal(ctx context.Context, p contract.RecipePatchProposal) (contract.RecipePatchProposal, error) {
	saved, err := f.mem.SaveProposal(ctx, p)
	if err != nil {
		return saved, err
	}
	if perr := f.persist(); perr != nil {
		return saved, perr
	}
	return saved, nil
}

func (f *FileStore) GetProposal(ctx context.Context, id string) (contract.RecipePatchProposal, error) {
	return f.mem.GetProposal(ctx, id)
}

func (f *FileStore) ListProposals(ctx context.Context, statusFilter string, limit int) ([]contract.RecipePatchProposal, error) {
	return f.mem.ListProposals(ctx, statusFilter, limit)
}

func (f *FileStore) ReviewProposal(ctx context.Context, id string, status contract.ProposalStatus, reviewedBy string, note string) (contract.RecipePatchProposal, error) {
	rev, err := f.mem.ReviewProposal(ctx, id, status, reviewedBy, note)
	if err != nil {
		return rev, err
	}
	if perr := f.persist(); perr != nil {
		return rev, perr
	}
	return rev, nil
}
