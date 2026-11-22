package server

import (
	"context"
	"path/filepath"
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
