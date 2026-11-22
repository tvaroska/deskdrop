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

type failStore struct {
	*MemoryStore
}

func (f *failStore) ListDrops(_ context.Context, _, _ string, _ int) ([]contract.DropRecord, error) {
	return nil, context.DeadlineExceeded
}

func TestWriteAPIErrorLogs5xxToStderrAndReleaseAssetReturns404WithoutBlobFallback(t *testing.T) {
	var errBuf bytes.Buffer
	restore := setErrorLogOutputForTest(&errBuf)
	defer restore()

	blobs := NewMemoryBlobStore()
	// Seed legacy blob path to prove handleDownloadRelease never falls back to blob storage.
	_ = blobs.Put(context.Background(), "tools/deskdrop/latest/deskdrop.exe", "application/octet-stream", []byte("legacy-blob"))

	svc := NewService(ServerConfig{
		Version:            "v1.1.1",
		DesktopClientToken: "desk-tok",
		AgentBusToken:      "agent-tok",
		ReleaseDir:         t.TempDir(), // empty release dir -> must return 404 directly
	}, &failStore{MemoryStore: NewMemoryStore()}, blobs, recipes.NewStaticCatalog(), nil)

	// 1. Missing release asset on disk returns 404 NOT_FOUND without touching blob storage or logging 5xx.
	relReq := httptest.NewRequest(http.MethodGet, "/v1/release/deskdrop.exe", nil)
	relReq.Header.Set("Authorization", "Bearer desk-tok")
	relW := httptest.NewRecorder()
	svc.ServeHTTP(relW, relReq)
	if relW.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/release/deskdrop.exe = %d, want 404 (%s)", relW.Code, relW.Body.String())
	}
	if errBuf.Len() != 0 {
		t.Fatalf("expected 0 stderr error logs for 404 response, got %q", errBuf.String())
	}

	// 2. 500 response logs method, path, status, code, and message to stderr.
	dropReq := httptest.NewRequest(http.MethodGet, "/v1/drops", nil)
	dropReq.Header.Set("Authorization", "Bearer agent-tok")
	dropW := httptest.NewRecorder()
	svc.ServeHTTP(dropW, dropReq)
	if dropW.Code != http.StatusInternalServerError {
		t.Fatalf("GET /v1/drops = %d, want 500", dropW.Code)
	}
	logged := errBuf.String()
	for _, want := range []string{"GET", "/v1/drops", "status=500", "code=STORE_ERROR", "context deadline exceeded"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("expected %q in 5xx stderr log, got %q", want, logged)
		}
	}

	// 3. GCSBlobStore.objectName strictly applies Prefix without legacy tools/ bypass.
	gcs := NewGCSBlobStore("my-bucket", "desktop")
	if got := gcs.objectName("tools/deskdrop/latest/deskdrop.exe"); got != "desktop/tools/deskdrop/latest/deskdrop.exe" {
		t.Fatalf("GCSBlobStore.objectName = %q, want desktop/tools/deskdrop/latest/deskdrop.exe", got)
	}
	if got := gcs.objectName("/diagnostics/diag_1.zip"); got != "desktop/diagnostics/diag_1.zip" {
		t.Fatalf("GCSBlobStore.objectName = %q, want desktop/diagnostics/diag_1.zip", got)
	}
}

func TestClaimTaskAllowedDriversSkipsCDPWhenOnlyUIARunnable(t *testing.T) {
	cat := recipes.NewStaticCatalog(
		contract.Recipe{
			Name:  "status-board",
			Topic: "ops",
			Steps: []contract.Step{{ID: "s1", URL: "https://status.example.com"}},
		},
		contract.Recipe{
			Name:           "desktop-windows",
			Topic:          "desktop-uia",
			Driver:         "uia",
			AllowedDomains: []string{"uia.local"},
			Steps:          []contract.Step{{ID: "open_windows", URL: "uia://desktop", WindowTitle: "*"}},
		},
	)
	store := NewMemoryStore()
	svc := NewService(ServerConfig{
		Version:            "v1.3.1",
		DesktopClientToken: "desk-tok",
		AgentBusToken:      "agent-tok",
	}, store, NewMemoryBlobStore(), cat, nil)

	// Enqueue high-priority CDP task + fallback task before UIA task.
	for _, body := range []string{
		`{"recipe":"status-board","priority":10,"requested_by":"agent-a"}`,
		`{"kind":"fallback_task","recipe":"status-board","priority":20,"fallback_plan":{"allowed_domain":"status.example.com","target_url":"https://status.example.com","actions":[{"action":"wait","wait_ms":100}]}}`,
		`{"recipe":"desktop-windows","priority":0,"requested_by":"agent-a"}`,
	} {
		rq := httptest.NewRequest(http.MethodPost, "/v1/tasks", strings.NewReader(body))
		rq.Header.Set("Authorization", "Bearer agent-tok")
		w := httptest.NewRecorder()
		svc.ServeHTTP(w, rq)
		if w.Code != http.StatusCreated {
			t.Fatalf("enqueue %s = %d (%s)", body, w.Code, w.Body.String())
		}
	}

	// 1. Worker with allowed_drivers=["uia"] skips CDP tasks and claims desktop-windows.
	claimUIA := httptest.NewRequest(http.MethodPost, "/v1/tasks/claim", strings.NewReader(`{"host":"win-pc","allowed_drivers":["uia"]}`))
	claimUIA.Header.Set("Authorization", "Bearer desk-tok")
	w1 := httptest.NewRecorder()
	svc.ServeHTTP(w1, claimUIA)
	var resp1 contract.ClaimTaskResponse
	_ = json.Unmarshal(w1.Body.Bytes(), &resp1)
	if resp1.Task == nil || resp1.Task.Recipe != "desktop-windows" {
		t.Fatalf("expected desktop-windows claimed, got %+v", resp1.Task)
	}

	// 2. Second UIA-only claim returns empty while leaving CDP tasks untouched (attempt=0).
	claimUIA2 := httptest.NewRequest(http.MethodPost, "/v1/tasks/claim", strings.NewReader(`{"host":"win-pc","allowed_drivers":["uia"]}`))
	claimUIA2.Header.Set("Authorization", "Bearer desk-tok")
	w2 := httptest.NewRecorder()
	svc.ServeHTTP(w2, claimUIA2)
	var resp2 contract.ClaimTaskResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
	if resp2.Task != nil {
		t.Fatalf("expected nil task on second UIA-only claim, got %+v", resp2.Task)
	}

	tasks, _ := store.ListTasks(context.Background(), "pending", 10)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 pending CDP tasks preserved, got %d", len(tasks))
	}
	for _, pt := range tasks {
		if pt.Attempt != 0 {
			t.Fatalf("pending CDP task %s had attempt=%d, want 0", pt.ID, pt.Attempt)
		}
	}

	// 3. Once Chrome connects (allowed_drivers=["cdp","uia"]), highest-priority CDP task is claimed.
	claimBoth := httptest.NewRequest(http.MethodPost, "/v1/tasks/claim", strings.NewReader(`{"host":"win-pc","allowed_drivers":["cdp","uia"]}`))
	claimBoth.Header.Set("Authorization", "Bearer desk-tok")
	w3 := httptest.NewRecorder()
	svc.ServeHTTP(w3, claimBoth)
	var resp3 contract.ClaimTaskResponse
	_ = json.Unmarshal(w3.Body.Bytes(), &resp3)
	if resp3.Task == nil || resp3.Task.Kind != contract.TaskKindFallback {
		t.Fatalf("expected priority-20 fallback_task claimed once CDP allowed, got %+v", resp3.Task)
	}
}
