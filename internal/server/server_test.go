package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/recipes"
)

type stubHealer struct {
	fn func(ctx context.Context, req HealRequest) (HealResult, error)
}

func (s *stubHealer) HealDrift(ctx context.Context, req HealRequest) (HealResult, error) {
	return s.fn(ctx, req)
}

func TestDualPrefixRoutesAuthAndUnapprovedURL403(t *testing.T) {
	cat := recipes.NewStaticCatalog(contract.Recipe{
		Name:  "status-board",
		Topic: "ops",
		Steps: []contract.Step{{ID: "s1", URL: "https://status.example.com"}},
	})
	store := NewMemoryStore()
	svc := NewService(ServerConfig{
		Version:            "v1.0.0",
		DesktopClientToken: "desk-tok",
		AgentBusToken:      "agent-tok",
		AdminToken:         "admin-tok",
	}, store, NewMemoryBlobStore(), cat, nil)

	// Both /healthz and /desktop/healthz return 200.
	for _, p := range []string{"/healthz", "/desktop/healthz"} {
		rr := httptest.NewRecorder()
		svc.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", p, rr.Code)
		}
	}

	// Desktop token cannot read /v1/messages (403).
	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer desk-tok")
	rr := httptest.NewRecorder()
	svc.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("desktop token GET /v1/messages = %d, want 403", rr.Code)
	}

	// Unapproved ad-hoc recipe returns HTTP 403 on both /v1/tasks and /desktop/v1/tasks.
	for _, ep := range []string{"/v1/tasks", "/desktop/v1/tasks"} {
		body := `{"recipe":"unknown-recipe"}`
		rq := httptest.NewRequest(http.MethodPost, ep, strings.NewReader(body))
		rq.Header.Set("Authorization", "Bearer agent-tok")
		w := httptest.NewRecorder()
		svc.ServeHTTP(w, rq)
		if w.Code != http.StatusForbidden {
			t.Fatalf("POST %s unknown recipe = %d, want 403 (body=%s)", ep, w.Code, w.Body.String())
		}
	}

	// Approved recipe succeeds on /desktop/v1/tasks and can be listed via /v1/tasks.
	rq := httptest.NewRequest(http.MethodPost, "/desktop/v1/tasks", strings.NewReader(`{"recipe":"status-board","requested_by":"agent-a"}`))
	rq.Header.Set("Authorization", "Bearer agent-tok")
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, rq)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /desktop/v1/tasks approved = %d, want 201 (%s)", w.Code, w.Body.String())
	}
}

func TestDriftHealerProposalRequiresHumanApprovalBeforeWriteBack(t *testing.T) {
	dir := t.TempDir()
	recipesPath := filepath.Join(dir, "recipes.json")
	cat := recipes.NewReloadableCatalog(recipesPath)
	baseRecipe := contract.Recipe{
		Name:  "status-board",
		Topic: "ops",
		Steps: []contract.Step{
			{
				ID:  "s1",
				URL: "https://status.example.com",
				Fields: map[string]contract.FieldRule{
					"status": {Selector: ".old-status"},
				},
				Expect: contract.Expectation{RequiredFields: []string{"status"}},
			},
		},
	}
	if err := cat.SaveRecipe(context.Background(), baseRecipe); err != nil {
		t.Fatal(err)
	}

	healedRecipe := baseRecipe
	healedRecipe.Steps = []contract.Step{
		{
			ID:  "s1",
			URL: "https://status.example.com",
			Fields: map[string]contract.FieldRule{
				"status": {Selector: ".v2-status"},
			},
			Expect: contract.Expectation{RequiredFields: []string{"status"}},
		},
	}

	healer := &stubHealer{
		fn: func(_ context.Context, req HealRequest) (HealResult, error) {
			return HealResult{
				FallbackReport: &contract.DropReport{
					SchemaVersion: "2.0",
					Mode:          contract.DropModeComputerUseFallback,
					Recipe:        req.Recipe.Name,
					Topic:         req.Recipe.Topic,
					Host:          "healer",
					CapturedAtUTC: nowISO(),
					Steps: []contract.StepResult{
						{StepID: "s1", URL: req.LandedURL, Fields: map[string]any{"status": "Operational"}},
					},
				},
				Proposal: &contract.RecipePatchProposal{
					RecipeName:     req.Recipe.Name,
					Rationale:      "Selector migrated from .old-status to .v2-status",
					ProposedRecipe: healedRecipe,
					Verified:       true,
					Iterations:     1,
				},
			}, nil
		},
	}

	store, err := OpenFileStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(ServerConfig{
		Version:            "v1.0.0",
		DesktopClientToken: "desk-tok",
		AgentBusToken:      "agent-tok",
	}, store, NewMemoryBlobStore(), cat, healer)

	// Enqueue & claim task.
	enqReq := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(`{"recipe":"status-board"}`))
	enqReq.Header.Set("Authorization", "Bearer agent-tok")
	enqW := httptest.NewRecorder()
	svc.ServeHTTP(enqW, enqReq)
	var enqResp struct {
		Task contract.Task `json:"task"`
	}
	_ = json.Unmarshal(enqW.Body.Bytes(), &enqResp)

	claimReq := httptest.NewRequest(http.MethodPost, "/v1/tasks/claim", strings.NewReader(`{"host":"win-pc"}`))
	claimReq.Header.Set("Authorization", "Bearer desk-tok")
	svc.ServeHTTP(httptest.NewRecorder(), claimReq)

	// Upload SELECTOR_DRIFT bundle with scrubbed dom.html.
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	wHTML, _ := zw.Create("dom.html")
	_, _ = wHTML.Write([]byte(`<html><body><div class="v2-status">Operational</div></body></html>`))
	_ = zw.Close()

	var mpBuf bytes.Buffer
	mw := multipart.NewWriter(&mpBuf)
	_ = mw.WriteField("payload_json", `{"from":"win-pc","step_id":"s1","landed_url":"https://status.example.com","error":"step \"s1\" missing required field \"status\""}`)
	fw, _ := mw.CreateFormFile("bundle", "debug.zip")
	_, _ = fw.Write(zipBuf.Bytes())
	_ = mw.Close()

	failReq := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+enqResp.Task.ID+"/fail", &mpBuf)
	failReq.Header.Set("Authorization", "Bearer desk-tok")
	failReq.Header.Set("Content-Type", mw.FormDataContentType())
	failW := httptest.NewRecorder()
	svc.ServeHTTP(failW, failReq)
	if failW.Code != http.StatusOK {
		t.Fatalf("fail task = %d: %s", failW.Code, failW.Body.String())
	}

	var failResp struct {
		ProposalID     string `json:"proposal_id"`
		FallbackDropID string `json:"fallback_drop_id"`
	}
	_ = json.Unmarshal(failW.Body.Bytes(), &failResp)
	if failResp.ProposalID == "" || failResp.FallbackDropID == "" {
		t.Fatalf("expected proposal_id and fallback_drop_id, got %+v", failResp)
	}

	// Verify RECIPES_URI has NOT been mutated before human approval!
	before, _, _ := cat.Get(context.Background(), "status-board")
	if before.Steps[0].Fields["status"].Selector != ".old-status" {
		t.Fatalf("RECIPES_URI was mutated prior to approval: %+v", before.Steps[0].Fields)
	}

	// Approve proposal -> writes back atomically to RECIPES_URI!
	appReq := httptest.NewRequest(http.MethodPost, "/v1/proposals/"+failResp.ProposalID+"/approve", strings.NewReader(`{"reviewed_by":"operator"}`))
	appReq.Header.Set("Authorization", "Bearer agent-tok")
	appW := httptest.NewRecorder()
	svc.ServeHTTP(appW, appReq)
	if appW.Code != http.StatusOK {
		t.Fatalf("approve proposal = %d: %s", appW.Code, appW.Body.String())
	}

	after, _, _ := cat.Get(context.Background(), "status-board")
	if after.Steps[0].Fields["status"].Selector != ".v2-status" {
		t.Fatalf("expected RECIPES_URI to update to .v2-status after approval, got %+v", after.Steps[0].Fields)
	}
}
