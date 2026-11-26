package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	defer svc.Close()

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
		HealingQueued bool `json:"healing_queued"`
	}
	_ = json.Unmarshal(failW.Body.Bytes(), &failResp)
	if !failResp.HealingQueued {
		t.Fatalf("expected healing_queued=true on SELECTOR_DRIFT fail response: %s", failW.Body.String())
	}
	if err := svc.WaitHealerIdle(context.Background()); err != nil {
		t.Fatalf("WaitHealerIdle failed: %v", err)
	}

	healedTask, err := store.GetTask(context.Background(), enqResp.Task.ID)
	if err != nil || healedTask.ProposalID == "" || healedTask.DropID == "" || healedTask.Status != contract.TaskStatusSucceeded {
		t.Fatalf("expected async healer to set ProposalID, DropID, and succeeded status, got %+v err=%v", healedTask, err)
	}

	// Verify RECIPES_URI has NOT been mutated before human approval!
	before, _, _ := cat.Get(context.Background(), "status-board")
	if before.Steps[0].Fields["status"].Selector != ".old-status" {
		t.Fatalf("RECIPES_URI was mutated prior to approval: %+v", before.Steps[0].Fields)
	}

	// Approve proposal -> writes back atomically to RECIPES_URI!
	appReq := httptest.NewRequest(http.MethodPost, "/v1/proposals/"+healedTask.ProposalID+"/approve", strings.NewReader(`{"reviewed_by":"operator"}`))
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

type failSaveCatalog struct {
	recipes.Catalog
	failSave atomic.Bool
	getCalls atomic.Int32
}

func (f *failSaveCatalog) Get(ctx context.Context, name string) (contract.Recipe, bool, error) {
	f.getCalls.Add(1)
	return f.Catalog.Get(ctx, name)
}

func (f *failSaveCatalog) SaveRecipe(ctx context.Context, r contract.Recipe) error {
	if f.failSave.Load() {
		return errors.New("simulated catalog write failure")
	}
	return f.Catalog.SaveRecipe(ctx, r)
}

func TestS2ProposalApprovalFailClosedAndRollback(t *testing.T) {
	baseCat := recipes.NewStaticCatalog(contract.Recipe{
		Name:     "release-feed",
		Topic:    "releases",
		Defaults: map[string]string{"org": "example-org", "repo": "example-repo"},
		Steps:    []contract.Step{{ID: "s1", URL: "https://releases.example.com/{{org}}/{{repo}}"}},
	})
	cat := &failSaveCatalog{Catalog: baseCat}
	store := NewMemoryStore()
	svc := NewService(ServerConfig{
		Version:            "v1.3.2",
		DesktopClientToken: "desk-tok",
		AgentBusToken:      "agent-tok",
	}, store, NewMemoryBlobStore(), cat, nil)

	approve := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/proposals/"+id+"/approve", strings.NewReader(`{"reviewed_by":"op"}`))
		req.Header.Set("Authorization", "Bearer agent-tok")
		w := httptest.NewRecorder()
		svc.ServeHTTP(w, req)
		return w
	}

	// 1. Unregistered recipe (!exists) must fail closed (403).
	pMissing, _ := store.SaveProposal(context.Background(), contract.RecipePatchProposal{
		RecipeName:     "unregistered",
		Verified:       true,
		ProposedRecipe: contract.Recipe{Name: "unregistered", Topic: "t", Steps: []contract.Step{{ID: "s1", URL: "https://evil.invalid"}}},
	})
	if w := approve(pMissing.ID); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not registered") {
		t.Fatalf("approve unregistered recipe = %d, want 403 not registered (%s)", w.Code, w.Body.String())
	}

	// 2. ProposedRecipe.Name mismatch must fail (422).
	pMismatch, _ := store.SaveProposal(context.Background(), contract.RecipePatchProposal{
		RecipeName:     "release-feed",
		Verified:       true,
		ProposedRecipe: contract.Recipe{Name: "../state", Topic: "releases", Steps: []contract.Step{{ID: "s1", URL: "https://releases.example.com/a/b"}}},
	})
	if w := approve(pMismatch.ID); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("approve name mismatch = %d, want 422 (%s)", w.Code, w.Body.String())
	}

	// 3. Defaults host-template pivot must fail closed (403).
	pPivot, _ := store.SaveProposal(context.Background(), contract.RecipePatchProposal{
		RecipeName: "release-feed",
		Verified:   true,
		ProposedRecipe: contract.Recipe{
			Name:     "release-feed",
			Topic:    "releases",
			Defaults: map[string]string{"h": "evil.invalid"},
			Steps:    []contract.Step{{ID: "s1", URL: "https://{{h}}/x"}},
		},
	})
	if w := approve(pPivot.ID); w.Code != http.StatusForbidden {
		t.Fatalf("approve defaults host pivot = %d, want 403 (%s)", w.Code, w.Body.String())
	}

	// 4. Valid path-templated release-feed proposal: failed SaveRecipe rolls proposal back to pending, retry succeeds (200).
	pValid, _ := store.SaveProposal(context.Background(), contract.RecipePatchProposal{
		RecipeName: "release-feed",
		Verified:   true,
		ProposedRecipe: contract.Recipe{
			Name:        "release-feed",
			Topic:       "releases",
			Description: "healed",
			Defaults:    map[string]string{"org": "example-org", "repo": "example-repo"},
			Steps:       []contract.Step{{ID: "s1", URL: "https://releases.example.com/{{org}}/{{repo}}"}},
		},
	})
	cat.failSave.Store(true)
	if w := approve(pValid.ID); w.Code != http.StatusInternalServerError {
		t.Fatalf("approve with failing SaveRecipe = %d, want 500", w.Code)
	}
	afterFail, _ := store.GetProposal(context.Background(), pValid.ID)
	if afterFail.Status != contract.ProposalStatusPending {
		t.Fatalf("expected proposal rolled back to pending after SaveRecipe failure, got %q", afterFail.Status)
	}
	cat.failSave.Store(false)
	if w := approve(pValid.ID); w.Code != http.StatusOK {
		t.Fatalf("approve valid path-templated release-feed = %d, want 200 (%s)", w.Code, w.Body.String())
	}

	// 5. Concurrent /approve vs /reject TOCTOU: if /reject wins, RECIPES_URI must NEVER be overwritten.
	pRace, _ := store.SaveProposal(context.Background(), contract.RecipePatchProposal{
		RecipeName: "release-feed",
		Verified:   true,
		ProposedRecipe: contract.Recipe{
			Name:        "release-feed",
			Topic:       "releases",
			Description: "raced-description",
			Defaults:    map[string]string{"org": "example-org", "repo": "example-repo"},
			Steps:       []contract.Step{{ID: "s1", URL: "https://releases.example.com/{{org}}/{{repo}}"}},
		},
	})
	var wg sync.WaitGroup
	var appCode, rejCode atomic.Int32
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		appCode.Store(int32(approve(pRace.ID).Code))
	}()
	go func() {
		defer wg.Done()
		<-start
		req := httptest.NewRequest(http.MethodPost, "/v1/proposals/"+pRace.ID+"/reject", strings.NewReader(`{"reviewed_by":"op"}`))
		req.Header.Set("Authorization", "Bearer agent-tok")
		w := httptest.NewRecorder()
		svc.ServeHTTP(w, req)
		rejCode.Store(int32(w.Code))
	}()
	close(start)
	wg.Wait()
	if !((appCode.Load() == http.StatusOK && rejCode.Load() == http.StatusConflict) ||
		(rejCode.Load() == http.StatusOK && appCode.Load() == http.StatusConflict)) {
		t.Fatalf("concurrent approve/reject codes = (%d, %d), want one 200 and one 409", appCode.Load(), rejCode.Load())
	}
	if rejCode.Load() == http.StatusOK {
		cur, _, _ := baseCat.Get(context.Background(), "release-feed")
		if cur.Description == "raced-description" {
			t.Fatal("TOCTOU bug: rejected proposal still overwrote RECIPES_URI")
		}
	}
}

func TestS3DiagnosticZipBoundsDuplicatesAndAirGap(t *testing.T) {
	svc := NewService(ServerConfig{
		Version:            "v1.3.2",
		DesktopClientToken: "desk-tok",
		AgentBusToken:      "agent-tok",
		ForbiddenTerms:     []string{"top-secret-term"},
	}, NewMemoryStore(), NewMemoryBlobStore(), recipes.NewStaticCatalog(), nil)

	postErrBundle := func(metaJSON string, bundle []byte) *httptest.ResponseRecorder {
		var mp bytes.Buffer
		mw := multipart.NewWriter(&mp)
		_ = mw.WriteField("payload_json", metaJSON)
		if bundle != nil {
			fw, _ := mw.CreateFormFile("bundle", "debug.zip")
			_, _ = fw.Write(bundle)
		}
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/v1/errors", &mp)
		req.Header.Set("Authorization", "Bearer desk-tok")
		req.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		svc.ServeHTTP(w, req)
		return w
	}

	// 1. Non-zip body rejected with 422.
	if w := postErrBundle(`{"error":"boom"}`, []byte("not-a-zip-file")); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("non-zip bundle = %d, want 422", w.Code)
	}

	// 2. Duplicate entries (zip bomb vector) rejected with 422.
	var dupBuf bytes.Buffer
	zw := zip.NewWriter(&dupBuf)
	w1, _ := zw.Create("screenshot.png")
	_, _ = w1.Write([]byte("a"))
	w2, _ := zw.Create("screenshot.png")
	_, _ = w2.Write([]byte("b"))
	_ = zw.Close()
	if w := postErrBundle(`{"error":"boom"}`, dupBuf.Bytes()); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate zip entry = %d, want 422 (%s)", w.Code, w.Body.String())
	}

	// 3. Oversized dom.html (>512 KiB) rejected with 422.
	var bigBuf bytes.Buffer
	zwBig := zip.NewWriter(&bigBuf)
	wb, _ := zwBig.Create("dom.html")
	_, _ = wb.Write(bytes.Repeat([]byte("a"), 513*1024))
	_ = zwBig.Close()
	if w := postErrBundle(`{"error":"boom"}`, bigBuf.Bytes()); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized dom.html = %d, want 422 (%s)", w.Code, w.Body.String())
	}

	// 4. Secret inside error.json or meta.Recipe blocked with 422.
	var errJSONBuf bytes.Buffer
	zwErr := zip.NewWriter(&errJSONBuf)
	we, _ := zwErr.Create("error.json")
	_, _ = we.Write([]byte(`{"detail":"contains top-secret-term"}`))
	_ = zwErr.Close()
	if w := postErrBundle(`{"error":"boom"}`, errJSONBuf.Bytes()); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("secret in error.json = %d, want 422 (%s)", w.Code, w.Body.String())
	}
	if w := postErrBundle(`{"error":"boom","recipe":"top-secret-term"}`, nil); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("secret in meta.Recipe = %d, want 422 (%s)", w.Code, w.Body.String())
	}

	// 5. Non-PNG payload disguised as .png and non-canonical ./dom.html entry rejected with 422.
	var fakePNGBuf bytes.Buffer
	zwPNG := zip.NewWriter(&fakePNGBuf)
	wp, _ := zwPNG.Create("leak.png")
	_, _ = wp.Write([]byte("top-secret-term plain text masquerading as png"))
	_ = zwPNG.Close()
	if w := postErrBundle(`{"error":"boom"}`, fakePNGBuf.Bytes()); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("fake .png entry = %d, want 422 (%s)", w.Code, w.Body.String())
	}

	var dotSlashBuf bytes.Buffer
	zwDot := zip.NewWriter(&dotSlashBuf)
	wd, _ := zwDot.Create("./dom.html")
	_, _ = wd.Write([]byte("<html></html>"))
	_ = zwDot.Close()
	if w := postErrBundle(`{"error":"boom"}`, dotSlashBuf.Bytes()); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("./dom.html entry = %d, want 422 (%s)", w.Code, w.Body.String())
	}
}

func TestS4FileStoreRollbackIdempPruningTaskProtectionAndTransitionGuards(t *testing.T) {
	ctx := context.Background()

	// 1. FileStore rollback on persist failure (works identically under non-root and uid=0 CI containers).
	stateDir := filepath.Join(t.TempDir(), "state-dir")
	statePath := filepath.Join(stateDir, "state.json")
	fs, err := OpenFileStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fs.PostMessage(ctx, contract.PostMessageRequest{From: "a", Body: "seed"}, "k0"); err != nil {
		t.Fatal(err)
	}
	// Remove stateDir so atomic temp-file creation fails with ENOENT even when running as root.
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}

	if _, _, err := fs.PostMessage(ctx, contract.PostMessageRequest{From: "a", Body: "must-rollback"}, "k1"); err == nil {
		t.Fatal("expected PostMessage to fail when state directory is missing")
	}
	// Restore directory: retrying with same idempotency key k1 must create (not deduplicate a rolled-back entry).
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, created, err := fs.PostMessage(ctx, contract.PostMessageRequest{From: "a", Body: "must-rollback"}, "k1")
	if err != nil || !created {
		t.Fatalf("retry after rollback: created=%v err=%v, want created=true err=nil", created, err)
	}

	// 2. Identical idle UpsertWorkerStatus skips fsync (state.json mtime unchanged).
	ws := contract.WorkerStatus{Host: "win-pc", Version: "v1.3.2", BrowserConnected: true, ReadyToAccept: true, LastAction: "sync"}
	if _, err := fs.UpsertWorkerStatus(ctx, ws); err != nil {
		t.Fatal(err)
	}
	st1, _ := os.Stat(statePath)
	past := time.Now().Add(-10 * time.Second)
	_ = os.Chtimes(statePath, past, past)
	if _, err := fs.UpsertWorkerStatus(ctx, ws); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(statePath)
	if !st2.ModTime().Equal(past.Truncate(time.Second)) && st2.ModTime().After(past.Add(time.Second)) {
		t.Fatalf("expected no-op UpsertWorkerStatus to skip persist, mtime changed from %v to %v (initial %v)", past, st2.ModTime(), st1.ModTime())
	}

	// 3. MemoryStore prunes evicted msgIdemp/dropIdemp keys and protects pending tasks from eviction.
	ms := NewMemoryStore()
	for i := 0; i < maxRetainedMessages+25; i++ {
		_, _, _ = ms.PostMessage(ctx, contract.PostMessageRequest{From: "a", Body: "msg"}, "idemp-"+string(rune('a'+(i%26)))+strings.Repeat("x", i%10)+time.Now().Format("150405.000000000"))
	}
	if len(ms.msgIdemp) > maxRetainedMessages {
		t.Fatalf("msgIdemp leaked evicted keys: len=%d > %d", len(ms.msgIdemp), maxRetainedMessages)
	}

	// Enqueue one pending task, then churn > maxRetainedTasks completed tasks: pending task must survive.
	pinned, _, _ := ms.EnqueueTask(ctx, contract.EnqueueTaskRequest{Recipe: "pinned-pending", Domain: "paused.example.com"})
	_, _, _ = ms.TripDomainAuthBreaker(ctx, "paused.example.com", "paused")
	for i := 0; i < maxRetainedTasks+15; i++ {
		t2, _, _ := ms.EnqueueTask(ctx, contract.EnqueueTaskRequest{Recipe: "churn", Domain: "ok.example.com"})
		claimed, _ := ms.ClaimNextTask(ctx, "w1", time.Minute, nil)
		if claimed != nil && claimed.ID == t2.ID {
			_, _ = ms.CompleteTask(ctx, t2.ID, "drop_x")
		}
	}
	if _, err := ms.GetTask(ctx, pinned.ID); err != nil {
		t.Fatalf("live pending task %s was evicted: %v", pinned.ID, err)
	}

	// 4. FailTask cannot revert succeeded task or fail a task owned by another worker.
	t3, _, _ := ms.EnqueueTask(ctx, contract.EnqueueTaskRequest{Recipe: "r1", Domain: "ok.example.com"})
	claimed3, _ := ms.ClaimNextTask(ctx, "worker-a", time.Minute, nil)
	if _, _, _, err := ms.FailTask(ctx, claimed3.ID, "worker-b", contract.ErrorTriage{Code: contract.ErrorCodeUnknown, Action: contract.TriageRetryBackoff}, "wrong owner", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("FailTask by non-owner = %v, want ErrConflict", err)
	}
	if _, _, _, err := ms.FailTask(ctx, claimed3.ID, "", contract.ErrorTriage{Code: contract.ErrorCodeUnknown, Action: contract.TriageRetryBackoff}, "empty owner", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("FailTask with empty host = %v, want ErrConflict", err)
	}
	if _, err := ms.HeartbeatTask(ctx, claimed3.ID, "", time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("HeartbeatTask with empty host = %v, want ErrConflict", err)
	}
	if _, err := ms.CompleteTask(ctx, claimed3.ID, "drop_done"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ms.FailTask(ctx, t3.ID, "worker-a", contract.ErrorTriage{Code: contract.ErrorCodeUnknown, Action: contract.TriageRetryBackoff}, "late fail", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("FailTask on succeeded task = %v, want ErrConflict", err)
	}
}

func TestS5ErrorCodeOverrideGuardAndS6PreLockDriverResolution(t *testing.T) {
	var healerCalls atomic.Int32
	cat := &failSaveCatalog{
		Catalog: recipes.NewStaticCatalog(contract.Recipe{
			Name:  "status-board",
			Topic: "ops",
			Steps: []contract.Step{{ID: "s1", URL: "https://status.example.com"}},
		}),
	}
	store := NewMemoryStore()
	svc := NewService(ServerConfig{
		Version:            "v1.3.2",
		DesktopClientToken: "desk-tok",
		AgentBusToken:      "agent-tok",
	}, store, NewMemoryBlobStore(), cat, &stubHealer{
		fn: func(_ context.Context, _ HealRequest) (HealResult, error) {
			healerCalls.Add(1)
			return HealResult{}, nil
		},
	})

	// S-5: Caller passing error_code=SELECTOR_DRIFT on /login URL must stay AUTH_EXPIRED and NEVER run healer.
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	wh, _ := zw.Create("dom.html")
	_, _ = wh.Write([]byte(`<html><body>Sign in</body></html>`))
	_ = zw.Close()

	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	_ = mw.WriteField("payload_json", `{"recipe":"status-board","landed_url":"https://status.example.com/login","error_code":"SELECTOR_DRIFT","error":"redirected to login"}`)
	fw, _ := mw.CreateFormFile("bundle", "debug.zip")
	_, _ = fw.Write(zbuf.Bytes())
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/errors", &mp)
	req.Header.Set("Authorization", "Bearer desk-tok")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /v1/errors = %d (%s)", w.Code, w.Body.String())
	}
	if healerCalls.Load() != 0 {
		t.Fatalf("expected 0 healer calls on /login override attempt, got %d", healerCalls.Load())
	}
	if !strings.Contains(w.Body.String(), `"AUTH_EXPIRED"`) {
		t.Fatalf("expected AUTH_EXPIRED triage, got %s", w.Body.String())
	}

	// Clear the tripped auth breaker before testing CONTRACT_INVALID task failure.
	if err := store.ClearDomainBreaker(context.Background(), "status.example.com"); err != nil {
		t.Fatal(err)
	}

	// S-5: CONTRACT_INVALID sets diagnostic_escalation (0 retries).
	t1, _, _ := store.EnqueueTask(context.Background(), contract.EnqueueTaskRequest{Recipe: "status-board", Domain: "status.example.com", MaxAttempts: 3})
	claimed, err := store.ClaimNextTask(context.Background(), "win-pc", time.Minute, nil)
	if err != nil || claimed == nil || claimed.ID != t1.ID {
		t.Fatalf("expected t1 claimed, got %+v err=%v", claimed, err)
	}
	failReq := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+t1.ID+"/fail", strings.NewReader(`{"from":"win-pc","error_code":"CONTRACT_INVALID","error":"missing resolved recipe"}`))
	failReq.Header.Set("Authorization", "Bearer desk-tok")
	fw2 := httptest.NewRecorder()
	svc.ServeHTTP(fw2, failReq)
	if fw2.Code != http.StatusOK {
		t.Fatalf("fail CONTRACT_INVALID = %d (%s)", fw2.Code, fw2.Body.String())
	}
	t1After, _ := store.GetTask(context.Background(), t1.ID)
	if t1After.Status != contract.TaskStatusFailedDiagnostic {
		t.Fatalf("CONTRACT_INVALID status = %q, want failed_diagnostic (no retry)", t1After.Status)
	}

	// S-6: handleClaimTask resolves drivers from catalog.List BEFORE ClaimNextTask (0 catalog.Get calls during claim filter).
	_, _, _ = store.EnqueueTask(context.Background(), contract.EnqueueTaskRequest{Recipe: "status-board", Domain: "status.example.com"})
	cat.getCalls.Store(0)
	claimReq := httptest.NewRequest(http.MethodPost, "/v1/tasks/claim", strings.NewReader(`{"host":"win-pc","allowed_drivers":["uia"]}`))
	claimReq.Header.Set("Authorization", "Bearer desk-tok")
	cw := httptest.NewRecorder()
	svc.ServeHTTP(cw, claimReq)
	if cw.Code != http.StatusOK {
		t.Fatalf("claim = %d", cw.Code)
	}
	if cat.getCalls.Load() != 0 {
		t.Fatalf("expected 0 catalog.Get calls during ClaimNextTask filter, got %d", cat.getCalls.Load())
	}
}

func TestT1AuthMatrixProposalSecurityBreakerHoldAndLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	baseRecipe := contract.Recipe{
		Name:  "status-board",
		Topic: "ops",
		Steps: []contract.Step{{ID: "s1", URL: "https://status.example.com/overview"}},
	}
	cat := recipes.NewStaticCatalog(baseRecipe)
	store := NewMemoryStore()
	svc := NewService(ServerConfig{
		Version:            "v1.4.0",
		DesktopClientToken: "desk-tok",
		AgentBusToken:      "agent-tok",
		AdminToken:         "admin-tok",
		ForbiddenTerms:     []string{"forbidden-secret"},
	}, store, NewMemoryBlobStore(), cat, nil)

	doReq := func(method, path, token, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		svc.ServeHTTP(w, r)
		return w
	}

	// 1. Auth matrix across ScopeDesktop, ScopeAgent, and ScopeAdmin (401/403/200).
	for _, ep := range []struct{ method, path string }{
		{http.MethodGet, "/v1/status"},
		{http.MethodGet, "/v1/tasks"},
		{http.MethodPost, "/v1/breakers/status.example.com/resume"},
		{http.MethodPost, "/v1/proposals/prop_x/approve"},
	} {
		if w := doReq(ep.method, ep.path, "", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("no token %s %s = %d, want 401", ep.method, ep.path, w.Code)
		}
		if w := doReq(ep.method, ep.path, "wrong-token", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("invalid token %s %s = %d, want 401", ep.method, ep.path, w.Code)
		}
	}
	if w := doReq(http.MethodGet, "/v1/status", "desk-tok", ""); w.Code != http.StatusOK {
		t.Fatalf("desktop token GET /v1/status = %d, want 200", w.Code)
	}
	if w := doReq(http.MethodGet, "/v1/tasks", "desk-tok", ""); w.Code != http.StatusForbidden {
		t.Fatalf("desktop token GET /v1/tasks = %d, want 403", w.Code)
	}
	if w := doReq(http.MethodGet, "/v1/tasks", "agent-tok", ""); w.Code != http.StatusOK {
		t.Fatalf("agent token GET /v1/tasks = %d, want 200", w.Code)
	}
	// S-12 ScopeAdmin: when AdminToken is configured, agent-tok MUST receive 403 on admin routes.
	for _, ep := range []string{
		"/v1/breakers/status.example.com/resume",
		"/v1/proposals/prop_x/approve",
		"/v1/proposals/prop_x/reject",
	} {
		if w := doReq(http.MethodPost, ep, "desk-tok", `{}`); w.Code != http.StatusForbidden {
			t.Fatalf("desktop token POST %s = %d, want 403", ep, w.Code)
		}
		if w := doReq(http.MethodPost, ep, "agent-tok", `{}`); w.Code != http.StatusForbidden {
			t.Fatalf("agent token POST %s = %d, want 403 when AdminToken configured", ep, w.Code)
		}
	}

	// 2. Proposal approval security: unverified 422, domain pivot 403, air-gap 422, reject + conflict 409.
	pUnverified, _ := store.SaveProposal(ctx, contract.RecipePatchProposal{
		RecipeName:     "status-board",
		Verified:       false,
		ProposedRecipe: baseRecipe,
	})
	if w := doReq(http.MethodPost, "/v1/proposals/"+pUnverified.ID+"/approve", "admin-tok", `{}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("approve unverified proposal = %d, want 422 (%s)", w.Code, w.Body.String())
	}

	pivotRecipe := baseRecipe
	pivotRecipe.Steps = []contract.Step{{ID: "s1", URL: "https://evil.invalid/steal"}}
	pPivot, _ := store.SaveProposal(ctx, contract.RecipePatchProposal{
		RecipeName:     "status-board",
		Verified:       true,
		ProposedRecipe: pivotRecipe,
	})
	if w := doReq(http.MethodPost, "/v1/proposals/"+pPivot.ID+"/approve", "admin-tok", `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("approve domain pivot proposal = %d, want 403 (%s)", w.Code, w.Body.String())
	}

	pAirGap, _ := store.SaveProposal(ctx, contract.RecipePatchProposal{
		RecipeName:     "status-board",
		Verified:       true,
		ProposedRecipe: baseRecipe,
	})
	if w := doReq(http.MethodPost, "/v1/proposals/"+pAirGap.ID+"/approve", "admin-tok", `{"note":"leaks forbidden-secret"}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("approve air-gap note = %d, want 422 (%s)", w.Code, w.Body.String())
	}

	// Reject succeeds (200) without mutating RECIPES_URI; second review returns 409.
	pReject, _ := store.SaveProposal(ctx, contract.RecipePatchProposal{
		RecipeName: "status-board",
		Verified:   true,
		ProposedRecipe: contract.Recipe{
			Name:        "status-board",
			Topic:       "ops",
			Description: "must-not-be-written",
			Steps:       []contract.Step{{ID: "s1", URL: "https://status.example.com/overview"}},
		},
	})
	if w := doReq(http.MethodPost, "/v1/proposals/"+pReject.ID+"/reject", "admin-tok", `{"reviewed_by":"sec-op"}`); w.Code != http.StatusOK {
		t.Fatalf("reject proposal = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if cur, _, _ := cat.Get(ctx, "status-board"); cur.Description == "must-not-be-written" {
		t.Fatal("rejected proposal mutated catalog")
	}
	if w := doReq(http.MethodPost, "/v1/proposals/"+pReject.ID+"/approve", "admin-tok", `{}`); w.Code != http.StatusConflict {
		t.Fatalf("approve after reject = %d, want 409", w.Code)
	}
	if w := doReq(http.MethodPost, "/v1/proposals/"+pReject.ID+"/reject", "admin-tok", `{}`); w.Code != http.StatusConflict {
		t.Fatalf("double reject = %d, want 409", w.Code)
	}

	// 3. AUTH_EXPIRED breaker hold, req.Domain spoof prevention (S-12), and admin resume.
	enqW := doReq(http.MethodPost, "/v1/tasks", "agent-tok", `{"recipe":"status-board","domain":"spoofed-bypass.example.com"}`)
	if enqW.Code != http.StatusCreated {
		t.Fatalf("enqueue status-board = %d (%s)", enqW.Code, enqW.Body.String())
	}
	var enqResp struct {
		Task contract.Task `json:"task"`
	}
	_ = json.Unmarshal(enqW.Body.Bytes(), &enqResp)
	if enqResp.Task.Domain != "status.example.com" {
		t.Fatalf("S-12 spoofed req.Domain accepted: got %q, want status.example.com", enqResp.Task.Domain)
	}

	claimW := doReq(http.MethodPost, "/v1/tasks/claim", "desk-tok", `{"host":"win-pc"}`)
	if claimW.Code != http.StatusOK || !strings.Contains(claimW.Body.String(), enqResp.Task.ID) {
		t.Fatalf("initial claim = %d (%s)", claimW.Code, claimW.Body.String())
	}
	failW := doReq(http.MethodPost, "/v1/tasks/"+enqResp.Task.ID+"/fail", "desk-tok", `{"from":"win-pc","landed_url":"https://status.example.com/login","error":"redirected to login"}`)
	if failW.Code != http.StatusOK {
		t.Fatalf("fail AUTH_EXPIRED = %d (%s)", failW.Code, failW.Body.String())
	}
	// Enqueue a second task with spoofed domain while breaker is open: must still be held!
	_ = doReq(http.MethodPost, "/v1/tasks", "agent-tok", `{"recipe":"status-board","domain":"other-clean.example.com"}`)
	claimHeld := doReq(http.MethodPost, "/v1/tasks/claim", "desk-tok", `{"host":"win-pc"}`)
	var heldResp contract.ClaimTaskResponse
	_ = json.Unmarshal(claimHeld.Body.Bytes(), &heldResp)
	if heldResp.Task != nil {
		t.Fatalf("expected tasks held while AUTH_EXPIRED breaker open, got %+v", heldResp.Task)
	}

	// Admin resumes breaker -> waiting task immediately claimable.
	resW := doReq(http.MethodPost, "/v1/breakers/status.example.com/resume", "admin-tok", `{}`)
	if resW.Code != http.StatusOK {
		t.Fatalf("admin resume breaker = %d (%s)", resW.Code, resW.Body.String())
	}
	claimAfter := doReq(http.MethodPost, "/v1/tasks/claim", "desk-tok", `{"host":"win-pc"}`)
	var afterResp contract.ClaimTaskResponse
	_ = json.Unmarshal(claimAfter.Body.Bytes(), &afterResp)
	if afterResp.Task == nil || afterResp.Task.Domain != "status.example.com" {
		t.Fatalf("expected task claimable after breaker resume, got %+v", afterResp.Task)
	}

	// 4. Claim lease expiry: attempt < max_attempts requeues to pending; attempt >= max_attempts dead-letters.
	leaseStore := NewMemoryStore()
	lt, _, _ := leaseStore.EnqueueTask(ctx, contract.EnqueueTaskRequest{
		Recipe:      "status-board",
		Domain:      "status.example.com",
		MaxAttempts: 2,
	})
	c1, err := leaseStore.ClaimNextTask(ctx, "win-pc", time.Minute, nil)
	if err != nil || c1 == nil || c1.Attempt != 1 {
		t.Fatalf("lease claim 1 = (%+v, %v)", c1, err)
	}
	// Force lease expiry in the past.
	leaseStore.mu.Lock()
	leaseStore.tasks[0].LeaseUntil = "2020-01-01T00:00:00Z"
	leaseStore.mu.Unlock()

	requeued, err := leaseStore.GetTask(ctx, lt.ID)
	if err != nil || requeued.Status != contract.TaskStatusPending {
		t.Fatalf("expired attempt 1 status = %q err=%v, want pending", requeued.Status, err)
	}
	c2, err := leaseStore.ClaimNextTask(ctx, "win-pc", time.Minute, nil)
	if err != nil || c2 == nil || c2.Attempt != 2 {
		t.Fatalf("lease claim 2 = (%+v, %v)", c2, err)
	}
	leaseStore.mu.Lock()
	leaseStore.tasks[0].LeaseUntil = "2020-01-01T00:00:00Z"
	leaseStore.mu.Unlock()

	dead, err := leaseStore.GetTask(ctx, lt.ID)
	if err != nil || dead.Status != contract.TaskStatusDeadLetter || dead.LastCode != contract.ErrorCodeCDPTransient {
		t.Fatalf("expired attempt 2 status = (%q, %q, %v), want (dead_letter, CDP_TRANSIENT, nil)", dead.Status, dead.LastCode, err)
	}
}

func TestS12S13HealerErrorLoggingFallbackCloseAndExactStatusCounts(t *testing.T) {
	ctx := context.Background()
	var errBuf bytes.Buffer
	restore := setErrorLogOutputForTest(&errBuf)
	defer restore()

	cat := recipes.NewStaticCatalog(contract.Recipe{
		Name:  "status-board",
		Topic: "ops",
		Steps: []contract.Step{{ID: "s1", URL: "https://status.example.com/overview"}},
	})
	store := NewMemoryStore()
	failingHealer := &stubHealer{
		fn: func(_ context.Context, _ HealRequest) (HealResult, error) {
			return HealResult{}, errors.New("vertex 429 quota exceeded")
		},
	}
	svc := NewService(ServerConfig{
		Version:            "v1.4.0",
		DesktopClientToken: "desk-tok",
		AgentBusToken:      "agent-tok",
		AdminToken:         "admin-tok",
	}, store, NewMemoryBlobStore(), cat, failingHealer)
	defer svc.Close()

	makeDOMZip := func() []byte {
		var zb bytes.Buffer
		zw := zip.NewWriter(&zb)
		wh, _ := zw.Create("dom.html")
		_, _ = wh.Write([]byte(`<html><body><div>drifted</div></body></html>`))
		_ = zw.Close()
		return zb.Bytes()
	}

	// 1. S-12: runDriftHealer logs errors to stderr via errLogger instead of dropping them silently.
	tFail, _, _ := store.EnqueueTask(ctx, contract.EnqueueTaskRequest{Recipe: "status-board", Domain: "status.example.com"})
	_, _ = store.ClaimNextTask(ctx, "win-pc", time.Minute, nil)
	var mp1 bytes.Buffer
	mw1 := multipart.NewWriter(&mp1)
	_ = mw1.WriteField("payload_json", `{"from":"win-pc","step_id":"s1","landed_url":"https://status.example.com/overview","error":"step \"s1\" missing required field \"status\""}`)
	fw1, _ := mw1.CreateFormFile("bundle", "debug.zip")
	_, _ = fw1.Write(makeDOMZip())
	_ = mw1.Close()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+tFail.ID+"/fail", &mp1)
	req1.Header.Set("Authorization", "Bearer desk-tok")
	req1.Header.Set("Content-Type", mw1.FormDataContentType())
	w1 := httptest.NewRecorder()
	svc.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("fail task with failing healer = %d (%s)", w1.Code, w1.Body.String())
	}
	if err := svc.WaitHealerIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errBuf.String(), "DRIFT_HEALER") || !strings.Contains(errBuf.String(), "vertex 429 quota exceeded") {
		t.Fatalf("expected healer error logged to stderr, got %q", errBuf.String())
	}

	// 2. S-12: handleFallbackResult closes unhealed fallback task to failed_diagnostic (never stuck in claimed).
	fbTask, _, _ := store.EnqueueTask(ctx, contract.EnqueueTaskRequest{
		Kind:   contract.TaskKindFallback,
		Recipe: "status-board",
		Domain: "status.example.com",
		FallbackPlan: &contract.FallbackPlan{
			AllowedDomain: "status.example.com",
			TargetURL:     "https://status.example.com/overview",
			Actions:       []contract.FallbackAction{{Action: "wait", WaitMs: 100}},
		},
	})
	_, _ = store.ClaimNextTask(ctx, "win-pc", time.Minute, nil)
	var mp2 bytes.Buffer
	mw2 := multipart.NewWriter(&mp2)
	_ = mw2.WriteField("payload_json", `{"from":"win-pc","step_id":"s1","landed_url":"https://status.example.com/overview"}`)
	fw2, _ := mw2.CreateFormFile("bundle", "debug.zip")
	_, _ = fw2.Write(makeDOMZip())
	_ = mw2.Close()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+fbTask.ID+"/fallback-result", &mp2)
	req2.Header.Set("Authorization", "Bearer desk-tok")
	req2.Header.Set("Content-Type", mw2.FormDataContentType())
	w2 := httptest.NewRecorder()
	svc.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("fallback-result = %d (%s)", w2.Code, w2.Body.String())
	}
	if err := svc.WaitHealerIdle(ctx); err != nil {
		t.Fatal(err)
	}
	fbAfter, _ := store.GetTask(ctx, fbTask.ID)
	if fbAfter.Status != contract.TaskStatusFailedDiagnostic || !strings.Contains(fbAfter.LastError, "vertex 429 quota exceeded") {
		t.Fatalf("fallback task after healer error = (%q, %q), want failed_diagnostic with healer error", fbAfter.Status, fbAfter.LastError)
	}

	// 3. S-13: GET /v1/status reports exact pending_tasks count beyond the old 50-item list cap.
	for i := 0; i < 65; i++ {
		_, _, _ = store.EnqueueTask(ctx, contract.EnqueueTaskRequest{Recipe: "status-board", Domain: "status.example.com"})
	}
	stReq := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	stReq.Header.Set("Authorization", "Bearer desk-tok")
	stW := httptest.NewRecorder()
	svc.ServeHTTP(stW, stReq)
	if stW.Code != http.StatusOK {
		t.Fatalf("GET /v1/status = %d (%s)", stW.Code, stW.Body.String())
	}
	var statusResp struct {
		Summary struct {
			PendingTasks          int `json:"pending_tasks"`
			FailedDiagnosticTasks int `json:"failed_diagnostic_tasks"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(stW.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("decode /v1/status: %v", err)
	}
	if statusResp.Summary.PendingTasks != 65 {
		t.Fatalf("/v1/status pending_tasks = %d, want 65", statusResp.Summary.PendingTasks)
	}
	if statusResp.Summary.FailedDiagnosticTasks != 2 {
		t.Fatalf("/v1/status failed_diagnostic_tasks = %d, want 2", statusResp.Summary.FailedDiagnosticTasks)
	}
}

func TestM61AsyncDriftHealerSurvivesCancelledRequestDeduplicatesAndDrains(t *testing.T) {
	ctx := context.Background()
	baseRecipe := contract.Recipe{
		Name:  "youtube-overview",
		Topic: "creator",
		Steps: []contract.Step{{
			ID:     "s1",
			URL:    "https://studio.example.com/analytics",
			Fields: map[string]contract.FieldRule{"views": {Selector: ".old-views", Parse: "number"}},
			Expect: contract.Expectation{RequiredFields: []string{"views"}},
		}},
	}
	healedRecipe := baseRecipe
	healedRecipe.Steps = []contract.Step{{
		ID:     "s1",
		URL:    "https://studio.example.com/analytics",
		Fields: map[string]contract.FieldRule{"views": {Selector: ".new-views", Parse: "number"}},
		Expect: contract.Expectation{RequiredFields: []string{"views"}},
	}}

	var (
		healerCalls     atomic.Int32
		healerStartedCh = make(chan struct{}, 4)
		releaseHealerCh = make(chan struct{})
		sawCancelledCtx atomic.Bool
	)
	slowHealer := &stubHealer{
		fn: func(hctx context.Context, req HealRequest) (HealResult, error) {
			healerCalls.Add(1)
			healerStartedCh <- struct{}{}
			<-releaseHealerCh
			if hctx.Err() != nil {
				sawCancelledCtx.Store(true)
				return HealResult{}, hctx.Err()
			}
			return HealResult{
				FallbackPlan: &contract.FallbackPlan{
					AllowedDomain: "studio.example.com",
					TargetURL:     "https://studio.example.com/analytics",
					StepID:        "s1",
					Reason:        "Click Shorts tab",
					Actions:       []contract.FallbackAction{{Action: "click", Selector: "button.shorts"}},
				},
				Proposal: &contract.RecipePatchProposal{
					RecipeName:     req.Recipe.Name,
					ProposedRecipe: healedRecipe,
					Verified:       true,
					Iterations:     1,
				},
			}, nil
		},
	}

	store := NewMemoryStore()
	svc := NewService(ServerConfig{
		Version:            "v1.5.0",
		DesktopClientToken: "desk-tok",
		AgentBusToken:      "agent-tok",
		HealerTimeout:      5 * time.Second,
		HealerConcurrency:  1,
		HealerQueueSize:    8,
	}, store, NewMemoryBlobStore(), recipes.NewStaticCatalog(baseRecipe), slowHealer)
	defer svc.Close()

	enq, _, _ := store.EnqueueTask(ctx, contract.EnqueueTaskRequest{Recipe: "youtube-overview", Domain: "studio.example.com"})
	_, _ = store.ClaimNextTask(ctx, "win-pc", 90*time.Second, nil)

	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	wh, _ := zw.Create("dom.html")
	_, _ = wh.Write([]byte(`<html><body><div class="new-views">1200</div></body></html>`))
	_ = zw.Close()

	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	_ = mw.WriteField("payload_json", `{"from":"win-pc","step_id":"s1","landed_url":"https://studio.example.com/analytics","error":"step \"s1\" missing required field \"views\""}`)
	fw, _ := mw.CreateFormFile("bundle", "debug.zip")
	_, _ = fw.Write(zb.Bytes())
	_ = mw.Close()

	// Cancel r.Context() IMMEDIATELY after /fail returns (simulating DesktopClient's 30s HTTP timeout).
	reqCtx, cancelReq := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+enq.ID+"/fail", &mp).WithContext(reqCtx)
	req.Header.Set("Authorization", "Bearer desk-tok")
	req.Header.Set("Content-Type", mw.FormDataContentType())

	startedAt := time.Now()
	w := httptest.NewRecorder()
	svc.ServeHTTP(w, req)
	elapsed := time.Since(startedAt)
	cancelReq() // HTTP request context is now dead while slowHealer is still blocked!

	if w.Code != http.StatusOK || elapsed > 500*time.Millisecond {
		t.Fatalf("/fail status=%d elapsed=%s, want 200 immediately (<500ms)", w.Code, elapsed)
	}
	var failOut struct {
		DiagnosticID  string `json:"diagnostic_id"`
		HealingQueued bool   `json:"healing_queued"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &failOut)
	if !failOut.HealingQueued || failOut.DiagnosticID == "" {
		t.Fatalf("expected healing_queued=true and diagnostic_id, got %+v", failOut)
	}

	// Wait until background worker enters HealDrift, then test (task.ID, diagID) deduplication.
	<-healerStartedCh
	queuedDup, deduped := svc.enqueueDriftHeal(driftHealJob{
		Task:    contract.Task{ID: enq.ID},
		Recipe:  baseRecipe,
		DiagID:  failOut.DiagnosticID,
		DOMHTML: "<html></html>",
	})
	if queuedDup || !deduped {
		t.Fatalf("duplicate (task.ID, diagID) enqueue = (queued=%v, deduped=%v), want (false, true)", queuedDup, deduped)
	}

	// Release slow healer after r.Context() was already cancelled; Shutdown drains cleanly.
	close(releaseHealerCh)
	shutCtx, shutCancel := context.WithTimeout(ctx, 3*time.Second)
	defer shutCancel()
	if err := svc.Shutdown(shutCtx); err != nil {
		t.Fatalf("Shutdown drain failed: %v", err)
	}
	if sawCancelledCtx.Load() {
		t.Fatal("HealDrift context was cancelled by HTTP r.Context()!")
	}
	if healerCalls.Load() != 1 {
		t.Fatalf("healerCalls = %d, want 1 (deduplicated)", healerCalls.Load())
	}

	afterTask, _ := store.GetTask(ctx, enq.ID)
	if afterTask.ProposalID == "" {
		t.Fatalf("expected ProposalID persisted after detached heal, got %+v", afterTask)
	}
	pendingTasks, _ := store.ListTasks(ctx, "pending", 10)
	if len(pendingTasks) != 1 || pendingTasks[0].Kind != contract.TaskKindFallback || pendingTasks[0].ParentTaskID != enq.ID {
		t.Fatalf("expected Tier-2 fallback_task enqueued asynchronously, got %+v", pendingTasks)
	}
}

func TestM72HandleDiffDropEndpointAndQueryOverrides(t *testing.T) {
	cat := recipes.NewStaticCatalog(contract.Recipe{
		Name:  "bookmarks-feed",
		Topic: "bookmarks",
		Steps: []contract.Step{{
			ID:  "list",
			URL: "https://bookmarks.example.com/list",
			Items: &contract.ItemRule{
				Container: "article",
				DedupeBy:  "id",
				Limit:     4,
				Fields:    map[string]contract.FieldRule{"id": {Selector: "a"}},
			},
		}},
	})
	store := NewMemoryStore()
	svc := NewService(ServerConfig{
		Version:            "v1.5.0",
		DesktopClientToken: "desk-tok",
		AgentBusToken:      "agent-tok",
	}, store, NewMemoryBlobStore(), cat, nil)
	defer svc.Close()

	saveDrop := func(recipe, topic string, ids ...string) contract.DropRecord {
		items := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			items = append(items, map[string]any{"id": id})
		}
		rec, _, err := store.SaveDrop(context.Background(), contract.SubmitDropRequest{
			From: "win-pc",
			To:   "agent",
			Report: contract.DropReport{
				SchemaVersion: "2.0",
				Recipe:        recipe,
				Topic:         topic,
				Steps:         []contract.StepResult{{StepID: "list", URL: "https://bookmarks.example.com/list", Items: items}},
			},
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}

	d1 := saveDrop("bookmarks-feed", "bookmarks", "b4", "b3", "b2", "b1")
	_ = saveDrop("other-recipe", "other-topic", "x1")
	// b3 was unbookmarked; b5 & b6 added at top -> top-4 window is [b6, b5, b4, b2] (b1 shifted past #4).
	d2 := saveDrop("bookmarks-feed", "bookmarks", "b6", "b5", "b4", "b2")
	d3 := saveDrop("bookmarks-feed", "bookmarks", "b7", "b6", "b5", "b4")

	// 1. Desktop token rejected (403 ScopeAgent).
	forbReq := httptest.NewRequest(http.MethodGet, "/v1/drops/"+d2.ID+"/diff", nil)
	forbReq.Header.Set("Authorization", "Bearer desk-tok")
	forbW := httptest.NewRecorder()
	svc.ServeHTTP(forbW, forbReq)
	if forbW.Code != http.StatusForbidden {
		t.Fatalf("desk-tok GET /diff = %d, want 403", forbW.Code)
	}

	// 2. Historical drop d2 diffs against its own predecessor d1 (NOT newer d3!), catching b3 removal while ignoring shifted b1.
	for _, prefix := range []string{"/v1", "/desktop/v1"} {
		req := httptest.NewRequest(http.MethodGet, prefix+"/drops/"+d2.ID+"/diff", nil)
		req.Header.Set("Authorization", "Bearer agent-tok")
		w := httptest.NewRecorder()
		svc.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s/drops/%s/diff = %d (%s)", prefix, d2.ID, w.Code, w.Body.String())
		}
		var diff contract.DropDiff
		if err := json.Unmarshal(w.Body.Bytes(), &diff); err != nil {
			t.Fatal(err)
		}
		if !diff.HasBaseline || diff.BaselineDropID != d1.ID {
			t.Fatalf("baseline = %q (has=%v), want %q", diff.BaselineDropID, diff.HasBaseline, d1.ID)
		}
		if diff.AddedCount != 2 || diff.RemovedCount != 1 || diff.Removed[0]["id"] != "b3" || diff.WindowShiftedCount != 1 {
			t.Fatalf("unexpected d2 diff: %+v", diff)
		}
	}

	// 3. First drop d1 has HasBaseline=false; invalid limit returns 422; missing drop returns 404.
	reqFirst := httptest.NewRequest(http.MethodGet, "/v1/drops/"+d1.ShortID+"/diff", nil)
	reqFirst.Header.Set("Authorization", "Bearer agent-tok")
	wFirst := httptest.NewRecorder()
	svc.ServeHTTP(wFirst, reqFirst)
	var firstDiff contract.DropDiff
	_ = json.Unmarshal(wFirst.Body.Bytes(), &firstDiff)
	if wFirst.Code != http.StatusOK || firstDiff.HasBaseline {
		t.Fatalf("first drop diff = %d %+v, want 200 HasBaseline=false", wFirst.Code, firstDiff)
	}

	reqBadLim := httptest.NewRequest(http.MethodGet, "/v1/drops/"+d3.ID+"/diff?limit=0", nil)
	reqBadLim.Header.Set("Authorization", "Bearer agent-tok")
	wBadLim := httptest.NewRecorder()
	svc.ServeHTTP(wBadLim, reqBadLim)
	if wBadLim.Code != http.StatusUnprocessableEntity {
		t.Fatalf("limit=0 = %d, want 422", wBadLim.Code)
	}

	reqMissing := httptest.NewRequest(http.MethodGet, "/v1/drops/drop_missing/diff", nil)
	reqMissing.Header.Set("Authorization", "Bearer agent-tok")
	wMissing := httptest.NewRecorder()
	svc.ServeHTTP(wMissing, reqMissing)
	if wMissing.Code != http.StatusNotFound {
		t.Fatalf("missing drop = %d, want 404", wMissing.Code)
	}
}
