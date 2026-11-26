package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
)

func TestFileStoreAtomicPersistenceAcrossRestarts(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	fs, err := OpenFileStore(statePath)
	if err != nil {
		t.Fatalf("OpenFileStore failed: %v", err)
	}

	ctx := context.Background()
	msg, created, err := fs.PostMessage(ctx, contract.PostMessageRequest{
		From:  "agent-a",
		To:    []string{"agent-b"},
		Topic: "ops",
		Body:  "hello atomic file store",
	}, "idemp-1")
	if err != nil || !created {
		t.Fatalf("PostMessage failed: created=%v err=%v", created, err)
	}

	task, _, err := fs.EnqueueTask(ctx, contract.EnqueueTaskRequest{
		Recipe:      "status-board",
		Domain:      "status.example.com",
		RequestedBy: "agent-a",
	})
	if err != nil {
		t.Fatalf("EnqueueTask failed: %v", err)
	}
	claimed, err := fs.ClaimNextTask(ctx, "worker-1", 60*time.Second, nil)
	if err != nil || claimed == nil || claimed.ID != task.ID {
		t.Fatalf("ClaimNextTask mismatch: %+v err=%v", claimed, err)
	}

	prop, err := fs.SaveProposal(ctx, contract.RecipePatchProposal{
		RecipeName: "status-board",
		TaskID:     task.ID,
		Verified:   true,
		ProposedRecipe: contract.Recipe{
			Name:  "status-board",
			Topic: "ops",
			Steps: []contract.Step{{ID: "s1", URL: "https://status.example.com"}},
		},
	})
	if err != nil {
		t.Fatalf("SaveProposal failed: %v", err)
	}

	// Re-open FileStore from disk to verify atomic persistence.
	fs2, err := OpenFileStore(statePath)
	if err != nil {
		t.Fatalf("reopen FileStore failed: %v", err)
	}
	msgs, err := fs2.ListMessages(ctx, "agent-b", "open", 10)
	if err != nil || len(msgs) != 1 || msgs[0].ID != msg.ID {
		t.Fatalf("reloaded messages mismatch: %+v err=%v", msgs, err)
	}
	gotTask, err := fs2.GetTask(ctx, task.ID)
	if err != nil || gotTask.Status != contract.TaskStatusClaimed {
		t.Fatalf("reloaded task mismatch: %+v err=%v", gotTask, err)
	}
	gotProp, err := fs2.GetProposal(ctx, prop.ID)
	if err != nil || !gotProp.Verified || gotProp.Status != contract.ProposalStatusPending {
		t.Fatalf("reloaded proposal mismatch: %+v err=%v", gotProp, err)
	}
}

func TestM52LegacyStateJSONCompatibilityAndCorruptRecovery(t *testing.T) {
	ctx := context.Background()

	// 1. Legacy v1.0 state.json fixture (pre-M4/v1.3 fields omitted) loads cleanly without dropping state.
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	legacyJSON := `{
  "schema_version": "1.0",
  "saved_at_utc": "2026-01-15T10:00:00Z",
  "messages": [
    {
      "id": "msg_legacy1",
      "short_id": "leg001",
      "from": "agent-legacy",
      "to": ["all"],
      "topic": "ops",
      "status": "open",
      "body": "legacy bus alert",
      "timestamp": "2026-01-15T10:00:00Z"
    }
  ],
  "msg_idemp": {
    "legacy-k1": "msg_legacy1"
  },
  "drops": [
    {
      "id": "drop_legacy1",
      "short_id": "drp001",
      "from": "win-pc",
      "to": "all",
      "received_at_utc": "2026-01-15T10:01:00Z",
      "report": {
        "schema_version": "2.0",
        "mode": "deterministic",
        "recipe": "status-board",
        "topic": "ops",
        "host": "win-pc",
        "captured_at_utc": "2026-01-15T10:01:00Z",
        "steps": [{"step_id": "s1", "url": "https://status.example.com", "fields": {"status": "OK"}}]
      }
    }
  ],
  "diagnostics": [
    {
      "id": "diag_legacy1",
      "topic": "ops",
      "recipe": "status-board",
      "domain": "status.example.com",
      "error_code": "SELECTOR_DRIFT",
      "error": "missing selector",
      "blob_key": "diagnostics/diag_legacy1.zip",
      "created_at_utc": "2026-01-15T10:02:00Z"
    }
  ],
  "breakers": {
    "paused.example.com": {
      "domain": "paused.example.com",
      "state": "paused_auth",
      "reason": "login redirect",
      "alerted": true,
      "tripped_at_utc": "2026-01-15T10:03:00Z"
    }
  },
  "tasks": [
    {
      "id": "task_legacy1",
      "kind": "extract_recipe",
      "recipe": "status-board",
      "domain": "status.example.com",
      "priority": 5,
      "status": "pending",
      "attempt": 0,
      "max_attempts": 3,
      "requested_by": "agent-legacy",
      "notify_to": "all",
      "next_run_at": "2026-01-15T10:00:00Z",
      "created_at_utc": "2026-01-15T10:00:00Z",
      "updated_at_utc": "2026-01-15T10:00:00Z"
    }
  ],
  "workers": {
    "win-pc": {
      "host": "win-pc",
      "version": "v1.0.0",
      "browser_connected": true,
      "ready_to_accept": true,
      "last_seen_utc": "2026-01-15T10:04:00Z"
    }
  },
  "proposals": [
    {
      "id": "prop_legacy1",
      "recipe_name": "status-board",
      "task_id": "task_legacy1",
      "status": "pending",
      "verified": true,
      "rationale": "legacy selector update",
      "proposed_recipe": {
        "name": "status-board",
        "topic": "ops",
        "steps": [{"id": "s1", "url": "https://status.example.com"}]
      },
      "created_at_utc": "2026-01-15T10:05:00Z"
    }
  ]
}
`
	if err := os.WriteFile(statePath, []byte(legacyJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	// Plant leftover corrupt temp files beside good state.json to verify OpenFileStore ignores them.
	leftoverTmp := filepath.Join(dir, ".state-12345.tmp")
	if err := os.WriteFile(leftoverTmp, []byte(`{"schema_version":"2.0","truncated":`), 0o600); err != nil {
		t.Fatal(err)
	}

	fs, err := OpenFileStore(statePath)
	if err != nil {
		t.Fatalf("OpenFileStore(legacy v1.0) failed: %v", err)
	}
	msgs, _ := fs.ListMessages(ctx, "all", "open", 10)
	if len(msgs) != 1 || msgs[0].ID != "msg_legacy1" {
		t.Fatalf("legacy messages lost: %+v", msgs)
	}
	if _, created, err := fs.PostMessage(ctx, contract.PostMessageRequest{From: "a", Body: "dup"}, "legacy-k1"); err != nil || created {
		t.Fatalf("legacy idempotency key lost: created=%v err=%v", created, err)
	}
	if d, err := fs.GetDrop(ctx, "drop_legacy1"); err != nil || d.Report.Recipe != "status-board" {
		t.Fatalf("legacy drop lost: %+v err=%v", d, err)
	}
	if diag, err := fs.GetDiagnostic(ctx, "diag_legacy1"); err != nil || diag.BlobKey == "" {
		t.Fatalf("legacy diagnostic lost: %+v err=%v", diag, err)
	}
	if brks, _ := fs.ListDomainBreakers(ctx); len(brks) != 1 || brks[0].Domain != "paused.example.com" {
		t.Fatalf("legacy breaker lost: %+v", brks)
	}
	if task, err := fs.GetTask(ctx, "task_legacy1"); err != nil || task.Status != contract.TaskStatusPending {
		t.Fatalf("legacy task lost: %+v err=%v", task, err)
	}
	if prop, err := fs.GetProposal(ctx, "prop_legacy1"); err != nil || !prop.Verified {
		t.Fatalf("legacy proposal lost: %+v err=%v", prop, err)
	}
	if workers, _ := fs.ListWorkerStatuses(ctx); len(workers) != 1 || workers[0].Host != "win-pc" {
		t.Fatalf("legacy worker lost: %+v", workers)
	}

	// Mutating and reopening preserves both legacy and newly written records.
	newTask, _, err := fs.EnqueueTask(ctx, contract.EnqueueTaskRequest{Recipe: "new-recipe", Domain: "new.example.com"})
	if err != nil {
		t.Fatalf("EnqueueTask after legacy load failed: %v", err)
	}
	fsReopen, err := OpenFileStore(statePath)
	if err != nil {
		t.Fatalf("reopen upgraded state.json failed: %v", err)
	}
	if _, err := fsReopen.GetTask(ctx, "task_legacy1"); err != nil {
		t.Fatalf("legacy task lost after upgrade persist: %v", err)
	}
	if _, err := fsReopen.GetTask(ctx, newTask.ID); err != nil {
		t.Fatalf("new task lost after upgrade persist: %v", err)
	}

	// 2. Truncated/corrupt state.json returns decode error without clobbering the file on disk.
	corruptPath := filepath.Join(t.TempDir(), "state.json")
	corruptContent := `{"schema_version":"2.0","tasks":[{"id":"task_partial"`
	if err := os.WriteFile(corruptPath, []byte(corruptContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileStore(corruptPath); err == nil || !strings.Contains(err.Error(), "decode state file") {
		t.Fatalf("OpenFileStore(corrupt) = %v, want decode state file error", err)
	}
	preserved, _ := os.ReadFile(corruptPath)
	if string(preserved) != corruptContent {
		t.Fatalf("OpenFileStore clobbered corrupt state file: got %q", string(preserved))
	}

	// 3. Empty 0-byte state.json loads cleanly as empty store and supports writes.
	emptyPath := filepath.Join(t.TempDir(), "empty-state.json")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fsEmpty, err := OpenFileStore(emptyPath)
	if err != nil {
		t.Fatalf("OpenFileStore(empty) failed: %v", err)
	}
	if _, _, err := fsEmpty.PostMessage(ctx, contract.PostMessageRequest{From: "a", Body: "ok"}, ""); err != nil {
		t.Fatalf("PostMessage on empty-loaded store failed: %v", err)
	}
}
