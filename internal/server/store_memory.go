package server

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
)

const (
	maxRetainedMessages    = 500
	maxRetainedDrops       = 500
	maxRetainedDiagnostics = 200
	maxRetainedTasks       = 500
	maxRetainedProposals   = 200
)

type storeSnapshot struct {
	SchemaVersion string                            `json:"schema_version"`
	SavedAtUTC    string                            `json:"saved_at_utc"`
	Messages      []contract.BusMessage             `json:"messages,omitempty"`
	MsgIdemp      map[string]string                 `json:"msg_idemp,omitempty"`
	Drops         []contract.DropRecord             `json:"drops,omitempty"`
	DropIdemp     map[string]string                 `json:"drop_idemp,omitempty"`
	Diagnostics   []contract.DiagnosticRecord       `json:"diagnostics,omitempty"`
	Breakers      map[string]contract.DomainBreaker `json:"breakers,omitempty"`
	Tasks         []contract.Task                   `json:"tasks,omitempty"`
	Workers       map[string]contract.WorkerStatus  `json:"workers,omitempty"`
	Proposals     []contract.RecipePatchProposal    `json:"proposals,omitempty"`
}

// MemoryStore is a mutex-guarded in-memory Store used directly in tests and wrapped by FileStore.
type MemoryStore struct {
	mu          sync.Mutex
	messages    []contract.BusMessage
	msgIdemp    map[string]string
	drops       []contract.DropRecord
	dropIdemp   map[string]string
	diagnostics []contract.DiagnosticRecord
	breakers    map[string]contract.DomainBreaker
	tasks       []contract.Task
	workers     map[string]contract.WorkerStatus
	proposals   []contract.RecipePatchProposal
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		msgIdemp:  make(map[string]string),
		dropIdemp: make(map[string]string),
		breakers:  make(map[string]contract.DomainBreaker),
		workers:   make(map[string]contract.WorkerStatus),
	}
}

func (m *MemoryStore) Ping(_ context.Context) error { return nil }
func (m *MemoryStore) Close() error                 { return nil }

func (m *MemoryStore) exportSnapshot() storeSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	snap := storeSnapshot{
		SchemaVersion: "2.0",
		SavedAtUTC:    nowISO(),
		Messages:      slices.Clone(m.messages),
		MsgIdemp:      cloneStringMap(m.msgIdemp),
		Drops:         slices.Clone(m.drops),
		DropIdemp:     cloneStringMap(m.dropIdemp),
		Diagnostics:   slices.Clone(m.diagnostics),
		Breakers:      make(map[string]contract.DomainBreaker, len(m.breakers)),
		Tasks:         slices.Clone(m.tasks),
		Workers:       make(map[string]contract.WorkerStatus, len(m.workers)),
		Proposals:     slices.Clone(m.proposals),
	}
	for k, v := range m.breakers {
		snap.Breakers[k] = v
	}
	for k, v := range m.workers {
		snap.Workers[k] = v
	}
	return snap
}

func (m *MemoryStore) importSnapshot(snap storeSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = slices.Clone(snap.Messages)
	m.msgIdemp = cloneStringMap(snap.MsgIdemp)
	m.drops = slices.Clone(snap.Drops)
	m.dropIdemp = cloneStringMap(snap.DropIdemp)
	m.diagnostics = slices.Clone(snap.Diagnostics)
	m.breakers = make(map[string]contract.DomainBreaker, len(snap.Breakers))
	for k, v := range snap.Breakers {
		m.breakers[k] = v
	}
	m.tasks = slices.Clone(snap.Tasks)
	m.workers = make(map[string]contract.WorkerStatus, len(snap.Workers))
	for k, v := range snap.Workers {
		m.workers[k] = v
	}
	m.proposals = slices.Clone(snap.Proposals)
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (m *MemoryStore) PostMessage(_ context.Context, req contract.PostMessageRequest, idempotencyKey string) (contract.BusMessage, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if idempotencyKey != "" {
		if existingID, ok := m.msgIdemp[idempotencyKey]; ok {
			for _, msg := range m.messages {
				if msg.ID == existingID {
					return msg, false, nil
				}
			}
		}
	}

	msg := contract.BusMessage{
		ID:        newID("msg"),
		ShortID:   newShortID(),
		From:      strings.ToLower(strings.TrimSpace(req.From)),
		To:        normalizeRecipients(req.To),
		Topic:     strings.TrimSpace(req.Topic),
		Status:    "open",
		Body:      strings.TrimSpace(req.Body),
		Meta:      req.Meta,
		Timestamp: nowISO(),
	}
	if msg.From == "" {
		msg.From = "agent"
	}
	m.messages = append(m.messages, msg)
	if len(m.messages) > maxRetainedMessages {
		m.messages = m.messages[len(m.messages)-maxRetainedMessages:]
	}
	if idempotencyKey != "" {
		m.msgIdemp[idempotencyKey] = msg.ID
	}
	return msg, true, nil
}

func (m *MemoryStore) ListMessages(_ context.Context, target string, statusFilter string, limit int) ([]contract.BusMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if limit <= 0 || limit > 200 {
		limit = 30
	}
	target = strings.ToLower(strings.TrimSpace(target))
	statusFilter = strings.ToLower(strings.TrimSpace(statusFilter))

	var matched []contract.BusMessage
	for i := len(m.messages) - 1; i >= 0 && len(matched) < limit; i-- {
		msg := m.messages[i]
		switch statusFilter {
		case "all":
		case "done":
			if msg.Status != "done" {
				continue
			}
		default:
			if msg.Status == "done" {
				continue
			}
		}
		if target != "" && target != "all" {
			if !slices.Contains(msg.To, target) && !slices.Contains(msg.To, "all") {
				continue
			}
		}
		msg.Notes = formatAckStrings(msg.AckNotes)
		matched = append(matched, msg)
	}
	slices.Reverse(matched)
	return matched, nil
}

func (m *MemoryStore) AckMessage(_ context.Context, idOrShortID string, req contract.AckMessageRequest) (contract.BusMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	needle := strings.TrimSpace(idOrShortID)
	for i := len(m.messages) - 1; i >= 0; i-- {
		if m.messages[i].ID == needle || m.messages[i].ShortID == needle {
			normStatus := "done"
			if req.Status == "claim" || req.Status == "claimed" {
				normStatus = "claimed"
			}
			if m.messages[i].Status != "done" || normStatus == "done" {
				m.messages[i].Status = normStatus
			}
			frm := strings.ToLower(strings.TrimSpace(req.From))
			if frm == "" {
				frm = "agent"
			}
			note := contract.AckNote{
				From:         frm,
				Status:       normStatus,
				Note:         strings.TrimSpace(req.Note),
				TimestampUTC: nowISO(),
			}
			m.messages[i].AckNotes = append(m.messages[i].AckNotes, note)
			m.messages[i].Notes = formatAckStrings(m.messages[i].AckNotes)
			return m.messages[i], nil
		}
	}
	return contract.BusMessage{}, ErrNotFound
}

func (m *MemoryStore) SaveDrop(_ context.Context, req contract.SubmitDropRequest, idempotencyKey string) (contract.DropRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if idempotencyKey != "" {
		if existingID, ok := m.dropIdemp[idempotencyKey]; ok {
			for _, d := range m.drops {
				if d.ID == existingID {
					return d, false, nil
				}
			}
		}
	}

	frm := strings.ToLower(strings.TrimSpace(req.From))
	if frm == "" {
		frm = strings.ToLower(strings.TrimSpace(req.Report.Host))
		if frm == "" {
			frm = "desktop-worker"
		}
	}
	to := strings.Join(normalizeRecipients([]string{req.To}), ",")
	if req.To == "" {
		to = "all"
	}

	rec := contract.DropRecord{
		ID:            newID("drop"),
		ShortID:       newShortID(),
		TaskID:        strings.TrimSpace(req.TaskID),
		From:          frm,
		To:            to,
		Idempotency:   idempotencyKey,
		Report:        req.Report,
		ReceivedAtUTC: nowISO(),
	}

	for _, s := range req.Report.Steps {
		if dom := contract.ExtractDomain(s.URL); dom != "" {
			delete(m.breakers, dom)
		}
	}

	m.drops = append(m.drops, rec)
	if len(m.drops) > maxRetainedDrops {
		m.drops = m.drops[len(m.drops)-maxRetainedDrops:]
	}
	if idempotencyKey != "" {
		m.dropIdemp[idempotencyKey] = rec.ID
	}
	return rec, true, nil
}

func (m *MemoryStore) GetDrop(_ context.Context, idOrShortID string) (contract.DropRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	needle := strings.TrimSpace(idOrShortID)
	for i := len(m.drops) - 1; i >= 0; i-- {
		if m.drops[i].ID == needle || m.drops[i].ShortID == needle {
			return m.drops[i], nil
		}
	}
	return contract.DropRecord{}, ErrNotFound
}

func (m *MemoryStore) ListDrops(_ context.Context, topic string, recipe string, limit int) ([]contract.DropRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var out []contract.DropRecord
	for i := len(m.drops) - 1; i >= 0 && len(out) < limit; i-- {
		d := m.drops[i]
		if topic != "" && !strings.EqualFold(d.Report.Topic, topic) {
			continue
		}
		if recipe != "" && !strings.EqualFold(d.Report.Recipe, recipe) {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func (m *MemoryStore) SaveDiagnostic(_ context.Context, diag contract.DiagnosticRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.diagnostics = append(m.diagnostics, diag)
	if len(m.diagnostics) > maxRetainedDiagnostics {
		m.diagnostics = m.diagnostics[len(m.diagnostics)-maxRetainedDiagnostics:]
	}
	return nil
}

func (m *MemoryStore) GetDiagnostic(_ context.Context, id string) (contract.DiagnosticRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.diagnostics) - 1; i >= 0; i-- {
		if m.diagnostics[i].ID == id {
			return m.diagnostics[i], nil
		}
	}
	return contract.DiagnosticRecord{}, ErrNotFound
}

func (m *MemoryStore) ListDiagnostics(_ context.Context, limit int) ([]contract.DiagnosticRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var out []contract.DiagnosticRecord
	for i := len(m.diagnostics) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, m.diagnostics[i])
	}
	return out, nil
}

func (m *MemoryStore) TripDomainAuthBreaker(_ context.Context, domain string, reason string) (contract.DomainBreaker, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	dom := strings.ToLower(strings.TrimSpace(domain))
	if dom == "" {
		dom = "unknown"
	}
	now := nowISO()
	existing, exists := m.breakers[dom]
	newlyAlerted := !exists || existing.State != "paused_auth" || !existing.Alerted
	b := contract.DomainBreaker{
		Domain:       dom,
		State:        "paused_auth",
		Reason:       reason,
		Alerted:      true,
		TrippedAtUTC: existing.TrippedAtUTC,
		UpdatedAtUTC: now,
	}
	if b.TrippedAtUTC == "" || newlyAlerted {
		b.TrippedAtUTC = now
	}
	m.breakers[dom] = b
	return b, newlyAlerted, nil
}

func (m *MemoryStore) ClearDomainBreaker(_ context.Context, domain string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.breakers, strings.ToLower(strings.TrimSpace(domain)))
	return nil
}

func (m *MemoryStore) ListDomainBreakers(_ context.Context) ([]contract.DomainBreaker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]contract.DomainBreaker, 0, len(m.breakers))
	for _, b := range m.breakers {
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b contract.DomainBreaker) int {
		return strings.Compare(a.Domain, b.Domain)
	})
	return out, nil
}

func (m *MemoryStore) EnqueueTask(_ context.Context, req contract.EnqueueTaskRequest) (contract.Task, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	dedupe := strings.TrimSpace(req.DedupeKey)
	if dedupe != "" {
		for _, t := range m.tasks {
			if t.DedupeKey == dedupe && (t.Status == contract.TaskStatusPending || t.Status == contract.TaskStatusClaimed) {
				return t, true, nil
			}
		}
	}

	maxAtt := req.MaxAttempts
	if maxAtt <= 0 {
		maxAtt = 3
	}
	kind := req.Kind
	if kind == "" {
		kind = contract.TaskKindExtract
	}
	reqBy := strings.ToLower(strings.TrimSpace(req.RequestedBy))
	if reqBy == "" {
		reqBy = "agent"
	}
	notifyTo := strings.TrimSpace(req.NotifyTo)
	if notifyTo == "" {
		notifyTo = reqBy
	}
	now := nowISO()
	task := contract.Task{
		ID:           newID("task"),
		Kind:         kind,
		ParentTaskID: strings.TrimSpace(req.ParentTaskID),
		Recipe:       strings.TrimSpace(req.Recipe),
		Vars:         req.Vars,
		CustomRecipe: req.CustomRecipe,
		FallbackPlan: req.FallbackPlan,
		Domain:       strings.ToLower(strings.TrimSpace(req.Domain)),
		Priority:     req.Priority,
		Status:       contract.TaskStatusPending,
		Attempt:      0,
		MaxAttempts:  maxAtt,
		RequestedBy:  reqBy,
		NotifyTo:     notifyTo,
		DedupeKey:    dedupe,
		NextRunAt:    now,
		CreatedAtUTC: now,
		UpdatedAtUTC: now,
	}
	m.tasks = append(m.tasks, task)
	if len(m.tasks) > maxRetainedTasks {
		m.tasks = m.tasks[len(m.tasks)-maxRetainedTasks:]
	}
	return task, false, nil
}

func (m *MemoryStore) sweepExpiredLeasesLocked(now time.Time) {
	nowStr := formatISO(now)
	for i := range m.tasks {
		t := &m.tasks[i]
		if t.Status == contract.TaskStatusClaimed && t.LeaseUntil != "" && t.LeaseUntil <= nowStr {
			if t.Attempt >= t.MaxAttempts {
				t.Status = contract.TaskStatusDeadLetter
				t.LastCode = contract.ErrorCodeCDPTransient
				t.LastError = "claim lease expired after max_attempts"
			} else {
				t.Status = contract.TaskStatusPending
				t.NextRunAt = nowStr
			}
			t.LeaseUntil = ""
			t.UpdatedAtUTC = nowStr
		}
	}
}

func (m *MemoryStore) ClaimNextTask(_ context.Context, host string, lease time.Duration, allow func(contract.Task) bool) (*contract.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().UTC()
	m.sweepExpiredLeasesLocked(now)
	nowStr := formatISO(now)

	bestIdx := -1
	for i := range m.tasks {
		t := &m.tasks[i]
		if t.Status != contract.TaskStatusPending || t.NextRunAt > nowStr {
			continue
		}
		if t.Domain != "" {
			if b, paused := m.breakers[t.Domain]; paused && b.State == "paused_auth" {
				continue
			}
		}
		if allow != nil && !allow(*t) {
			continue
		}
		if bestIdx == -1 || t.Priority > m.tasks[bestIdx].Priority {
			bestIdx = i
		}
	}
	if bestIdx == -1 {
		return nil, nil
	}

	t := &m.tasks[bestIdx]
	t.Status = contract.TaskStatusClaimed
	t.Attempt++
	t.ClaimedBy = strings.TrimSpace(host)
	t.LeaseUntil = formatISO(now.Add(lease))
	t.UpdatedAtUTC = nowStr
	cp := *t
	return &cp, nil
}

func (m *MemoryStore) HeartbeatTask(_ context.Context, taskID string, host string, lease time.Duration) (contract.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.tasks {
		t := &m.tasks[i]
		if t.ID == taskID {
			if t.Status != contract.TaskStatusClaimed {
				return contract.Task{}, ErrConflict
			}
			now := time.Now().UTC()
			t.LeaseUntil = formatISO(now.Add(lease))
			t.UpdatedAtUTC = formatISO(now)
			if host != "" {
				t.ClaimedBy = host
			}
			return *t, nil
		}
	}
	return contract.Task{}, ErrNotFound
}

func (m *MemoryStore) CompleteTask(_ context.Context, taskID string, dropID string) (contract.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.tasks {
		t := &m.tasks[i]
		if t.ID == taskID {
			t.Status = contract.TaskStatusSucceeded
			t.DropID = dropID
			t.LeaseUntil = ""
			t.UpdatedAtUTC = nowISO()
			if t.Domain != "" {
				delete(m.breakers, t.Domain)
			}
			return *t, nil
		}
	}
	return contract.Task{}, ErrNotFound
}

func (m *MemoryStore) FailTask(_ context.Context, taskID string, host string, triage contract.ErrorTriage, errMsg string, diagID string) (contract.Task, contract.DomainBreaker, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	idx := -1
	for i := range m.tasks {
		if m.tasks[i].ID == taskID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return contract.Task{}, contract.DomainBreaker{}, false, ErrNotFound
	}

	now := time.Now().UTC()
	nowStr := formatISO(now)
	t := &m.tasks[idx]
	t.LastError = errMsg
	t.LastCode = triage.Code
	t.LeaseUntil = ""
	t.UpdatedAtUTC = nowStr
	if diagID != "" {
		t.DiagnosticID = diagID
	}
	t.Attempts = append(t.Attempts, contract.AttemptLog{
		Attempt:      t.Attempt,
		Host:         host,
		ErrorCode:    triage.Code,
		Error:        errMsg,
		DiagnosticID: diagID,
		TimestampUTC: nowStr,
	})

	var breaker contract.DomainBreaker
	newlyAlerted := false

	switch triage.Action {
	case contract.TriageHoldDomainAuth:
		if t.Attempt > 0 {
			t.Attempt--
		}
		t.Status = contract.TaskStatusPending
		t.NextRunAt = nowStr
		dom := triage.Domain
		if dom == "" {
			dom = t.Domain
		}
		if dom == "" {
			dom = "unknown"
		}
		t.Domain = dom
		existing, exists := m.breakers[dom]
		newlyAlerted = !exists || existing.State != "paused_auth" || !existing.Alerted
		breaker = contract.DomainBreaker{
			Domain:       dom,
			State:        "paused_auth",
			Reason:       errMsg,
			Alerted:      true,
			TrippedAtUTC: existing.TrippedAtUTC,
			UpdatedAtUTC: nowStr,
		}
		if breaker.TrippedAtUTC == "" || newlyAlerted {
			breaker.TrippedAtUTC = nowStr
		}
		m.breakers[dom] = breaker

	case contract.TriageDiagnosticEscalation:
		t.Status = contract.TaskStatusFailedDiagnostic

	default:
		if t.Attempt >= t.MaxAttempts {
			t.Status = contract.TaskStatusDeadLetter
		} else {
			t.Status = contract.TaskStatusPending
			t.NextRunAt = formatISO(now.Add(contract.ComputeRetryBackoff(t.Attempt)))
		}
	}

	return *t, breaker, newlyAlerted, nil
}

func (m *MemoryStore) SetTaskProposal(_ context.Context, taskID string, proposalID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.tasks {
		if m.tasks[i].ID == taskID {
			m.tasks[i].ProposalID = proposalID
			m.tasks[i].UpdatedAtUTC = nowISO()
			return nil
		}
	}
	return ErrNotFound
}

func (m *MemoryStore) GetTask(_ context.Context, taskID string) (contract.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepExpiredLeasesLocked(time.Now().UTC())
	for _, t := range m.tasks {
		if t.ID == taskID {
			return t, nil
		}
	}
	return contract.Task{}, ErrNotFound
}

func (m *MemoryStore) ListTasks(_ context.Context, statusFilter string, limit int) ([]contract.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepExpiredLeasesLocked(time.Now().UTC())
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	statusFilter = strings.ToLower(strings.TrimSpace(statusFilter))
	var out []contract.Task
	for i := len(m.tasks) - 1; i >= 0 && len(out) < limit; i-- {
		t := m.tasks[i]
		if statusFilter != "" && statusFilter != "all" && string(t.Status) != statusFilter {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func (m *MemoryStore) UpsertWorkerStatus(_ context.Context, ws contract.WorkerStatus) (contract.WorkerStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	host := strings.ToLower(strings.TrimSpace(ws.Host))
	if host == "" {
		host = "desktop-worker"
	}
	existing := m.workers[host]
	ws.Host = host
	if ws.Version == "" {
		ws.Version = existing.Version
	}
	if ws.Hostname == "" {
		ws.Hostname = existing.Hostname
	}
	if ws.OS == "" {
		ws.OS = existing.OS
	}
	if ws.Arch == "" {
		ws.Arch = existing.Arch
	}
	if ws.ChromeSession == "" {
		ws.ChromeSession = existing.ChromeSession
	}
	if ws.FallbackGate == "" {
		ws.FallbackGate = existing.FallbackGate
	}
	if len(ws.AvailableSessions) == 0 {
		ws.AvailableSessions = existing.AvailableSessions
	}
	ws.LastSeenUTC = nowISO()
	ws.AgeSeconds = 0
	ws.Connected = true
	m.workers[host] = ws
	return ws, nil
}

func (m *MemoryStore) ListWorkerStatuses(_ context.Context) ([]contract.WorkerStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().UTC()
	out := make([]contract.WorkerStatus, 0, len(m.workers))
	for _, ws := range m.workers {
		if ts, err := time.Parse("2006-01-02T15:04:05.000000Z", ws.LastSeenUTC); err == nil {
			age := int(now.Sub(ts).Seconds())
			if age < 0 {
				age = 0
			}
			ws.AgeSeconds = age
			ws.Connected = age <= 60
		}
		out = append(out, ws)
	}
	slices.SortFunc(out, func(a, b contract.WorkerStatus) int {
		return strings.Compare(a.Host, b.Host)
	})
	return out, nil
}

func (m *MemoryStore) SaveProposal(_ context.Context, p contract.RecipePatchProposal) (contract.RecipePatchProposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := nowISO()
	if strings.TrimSpace(p.ID) == "" {
		p.ID = newID("prop")
	}
	if p.Status == "" {
		p.Status = contract.ProposalStatusPending
	}
	if p.CreatedAtUTC == "" {
		p.CreatedAtUTC = now
	}
	p.UpdatedAtUTC = now
	m.proposals = append(m.proposals, p)
	if len(m.proposals) > maxRetainedProposals {
		m.proposals = m.proposals[len(m.proposals)-maxRetainedProposals:]
	}
	return p, nil
}

func (m *MemoryStore) GetProposal(_ context.Context, id string) (contract.RecipePatchProposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	needle := strings.TrimSpace(id)
	for i := len(m.proposals) - 1; i >= 0; i-- {
		if m.proposals[i].ID == needle {
			return m.proposals[i], nil
		}
	}
	return contract.RecipePatchProposal{}, ErrNotFound
}

func (m *MemoryStore) ListProposals(_ context.Context, statusFilter string, limit int) ([]contract.RecipePatchProposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	sf := strings.ToLower(strings.TrimSpace(statusFilter))
	var out []contract.RecipePatchProposal
	for i := len(m.proposals) - 1; i >= 0 && len(out) < limit; i-- {
		p := m.proposals[i]
		if sf != "" && sf != "all" && string(p.Status) != sf {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (m *MemoryStore) ReviewProposal(_ context.Context, id string, status contract.ProposalStatus, reviewedBy string, note string) (contract.RecipePatchProposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	needle := strings.TrimSpace(id)
	for i := range m.proposals {
		if m.proposals[i].ID == needle {
			if m.proposals[i].Status != contract.ProposalStatusPending {
				return contract.RecipePatchProposal{}, ErrConflict
			}
			now := nowISO()
			m.proposals[i].Status = status
			m.proposals[i].ReviewedBy = strings.TrimSpace(reviewedBy)
			m.proposals[i].ReviewNote = strings.TrimSpace(note)
			m.proposals[i].ReviewedAtUTC = now
			m.proposals[i].UpdatedAtUTC = now
			return m.proposals[i], nil
		}
	}
	return contract.RecipePatchProposal{}, ErrNotFound
}
