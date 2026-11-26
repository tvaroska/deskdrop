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

	// 1. FileStore rollback on persist failure (read-only directory).
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	fs, err := OpenFileStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fs.PostMessage(ctx, contract.PostMessageRequest{From: "a", Body: "seed"}, "k0"); err != nil {
		t.Fatal(err)
	}
	// Make directory read-only so atomic temp file creation fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()

	if _, _, err := fs.PostMessage(ctx, contract.PostMessageRequest{From: "a", Body: "must-rollback"}, "k1"); err == nil {
		t.Fatal("expected PostMessage to fail in read-only directory")
	}
	// Restore permissions: retrying with same idempotency key k1 must create (not deduplicate a rolled-back entry).
	if err := os.Chmod(dir, 0o755); err != nil {
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
