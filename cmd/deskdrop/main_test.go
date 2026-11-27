package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tvaroska/deskdrop/internal/bus"
	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/driver"
	"github.com/tvaroska/deskdrop/internal/driver/cdp"
	"github.com/tvaroska/deskdrop/internal/driver/uia"
)

type fakeDriver struct {
	mu            sync.Mutex
	name          string
	connected     *bool
	browserStatus string
	fallbackRuns  int
	fbRes         driver.FallbackExecResult
}

func (f *fakeDriver) Name() string {
	if f.name != "" {
		return f.name
	}
	return "fake"
}
func (f *fakeDriver) ExecuteStep(_ context.Context, _ contract.Step) (contract.StepResult, error) {
	return contract.StepResult{StepID: "s1", URL: "https://example.com"}, nil
}
func (f *fakeDriver) ExecuteFallback(_ context.Context, _ contract.Step, _ contract.FallbackPlan) (driver.FallbackExecResult, error) {
	f.mu.Lock()
	f.fallbackRuns++
	f.mu.Unlock()
	return f.fbRes, nil
}
func (f *fakeDriver) ProbeStatus(_ context.Context) driver.DriverStatus {
	conn := true
	if f.connected != nil {
		conn = *f.connected
	}
	bs := f.browserStatus
	if bs == "" {
		bs = "Ready"
	}
	return driver.DriverStatus{Connected: conn, Label: bs, BrowserStatus: bs}
}
func (f *fakeDriver) Close() error { return nil }

func TestFallbackGateTrayDeclinesAndAutoExecutes(t *testing.T) {
	if _, err := validateFallbackGate("auto"); err != nil {
		t.Fatalf("auto rejected: %v", err)
	}
	if _, err := validateFallbackGate("tray"); err != nil {
		t.Fatalf("tray rejected: %v", err)
	}
	if _, err := validateFallbackGate("yolo"); err == nil {
		t.Fatal("expected invalid gate to fail")
	}

	var (
		mu              sync.Mutex
		failedCode      contract.ErrorCode
		completedDrop   bool
		fallbackUploads int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/fail"):
			var meta contract.FailReportMetadata
			_ = json.NewDecoder(r.Body).Decode(&meta)
			failedCode = meta.ErrorCode
			_, _ = w.Write([]byte(`{"ok":true,"triage":{"code":"FALLBACK_DECLINED","action":"diagnostic_escalation"}}`))
		case strings.HasSuffix(r.URL.Path, "/complete"):
			completedDrop = true
			_, _ = w.Write([]byte(`{"ok":true,"drop_id":"drop_1","short_id":"abc123","message_id":"msg_1"}`))
		case strings.HasSuffix(r.URL.Path, "/fallback-result"):
			fallbackUploads++
			_, _ = w.Write([]byte(`{"ok":true,"fallback_drop_id":"drop_fb","proposal_id":"prop_1"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	cfg := contract.Config{
		From:         "win-pc",
		ServiceURL:   srv.URL,
		ServiceToken: "desk-tok",
		FallbackGate: "tray",
	}
	client := bus.NewDesktopClient(cfg)
	drv := &fakeDriver{
		fbRes: driver.FallbackExecResult{
			StepResult:    contract.StepResult{StepID: "s1", URL: "https://status.example.com", Fields: map[string]any{"status": "OK"}},
			ScreenshotPNG: []byte{0x89, 0x50, 0x4E, 0x47},
			DOMHTML:       `<html><body><div class="v2">OK</div></body></html>`,
			ActionsRun:    1,
		},
	}
	rcp := &contract.Recipe{
		Name:  "status-board",
		Topic: "ops",
		Steps: []contract.Step{
			{
				ID:     "s1",
				URL:    "https://status.example.com",
				Expect: contract.Expectation{RequiredFields: []string{"status"}},
			},
		},
	}
	task := contract.Task{
		ID:     "task_fb1",
		Kind:   contract.TaskKindFallback,
		Recipe: "status-board",
		Domain: "status.example.com",
		FallbackPlan: &contract.FallbackPlan{
			AllowedDomain: "status.example.com",
			TargetURL:     "https://status.example.com",
			StepID:        "s1",
			Reason:        "Dismiss release banner",
			Actions:       []contract.FallbackAction{{Action: "click", Selector: "button.close"}},
		},
	}

	// Gate denies -> driver must NOT run and server receives FALLBACK_DECLINED.
	execFallbackTask(context.Background(), drv, cfg, client, "win-pc", task, rcp, func(_ contract.FallbackPlan) (bool, error) {
		return false, nil
	})
	if drv.fallbackRuns != 0 {
		t.Fatalf("expected 0 fallback runs when declined, got %d", drv.fallbackRuns)
	}
	if failedCode != contract.ErrorCodeFallbackDeclined {
		t.Fatalf("failedCode = %q, want FALLBACK_DECLINED", failedCode)
	}

	// Gate approves (auto) -> driver runs bounded actions and completes task.
	execFallbackTask(context.Background(), drv, cfg, client, "win-pc", task, rcp, makeFallbackApprover("auto"))
	if drv.fallbackRuns != 1 || !completedDrop {
		t.Fatalf("expected 1 fallback run and completed drop, got runs=%d completed=%v uploads=%d", drv.fallbackRuns, completedDrop, fallbackUploads)
	}
}

func TestIsNewerVersion(t *testing.T) {
	if !isNewerVersion("v1.0.1", "v1.0.0") {
		t.Fatal("expected v1.0.1 > v1.0.0")
	}
	if isNewerVersion("v1.0.0", "v1.0.0") {
		t.Fatal("expected v1.0.0 not newer than v1.0.0")
	}
}

func TestValidateDriverAndSelection(t *testing.T) {
	for _, okName := range []string{"", "cdp", "uia", "UIA"} {
		if _, err := validateDriverName(okName); err != nil {
			t.Fatalf("validateDriverName(%q) failed: %v", okName, err)
		}
	}
	if _, err := validateDriverName("playwright"); err == nil {
		t.Fatal("expected unsupported driver name to fail")
	}
	uiaDrv := newTargetDriver("uia", nil, contract.Config{})
	defer uiaDrv.Close()
	if uiaDrv.Name() != "uia" {
		t.Fatalf("uiaDrv.Name() = %q, want uia", uiaDrv.Name())
	}
	cdpDrv := newTargetDriver("cdp", nil, contract.Config{})
	defer cdpDrv.Close()
	if cdpDrv.Name() != "cdp" {
		t.Fatalf("cdpDrv.Name() = %q, want cdp", cdpDrv.Name())
	}
}

func TestDrainQueueExecutesUIATasksWhileBrowserCDPDisconnected(t *testing.T) {
	origMake := makeTargetDriver
	defer func() { makeTargetDriver = origMake }()

	staticUIA := &uia.StaticTreeProvider{
		Windows: []uia.StaticWindowFixture{
			{
				Title: "Task Manager",
				Nodes: []contract.AXNode{
					{
						NodeID: "1",
						Role:   &contract.AXValue{Value: "window"},
						Name:   &contract.AXValue{Value: "Task Manager"},
					},
				},
			},
		},
	}
	makeTargetDriver = func(driverName string, pool *cdp.SessionPool, cfg contract.Config) driver.TargetDriver {
		if strings.EqualFold(driverName, "uia") {
			return uia.NewDriverWithProvider(staticUIA)
		}
		return origMake(driverName, pool, cfg)
	}

	var (
		mu             sync.Mutex
		claimedUIA     bool
		completedItems int
		seenAllowed    []string
	)

	uiaRecipe := contract.Recipe{
		Name:           "desktop-windows",
		Topic:          "desktop-uia",
		Driver:         "uia",
		AllowedDomains: []string{"uia.local"},
		Steps: []contract.Step{
			{
				ID:          "open_windows",
				URL:         "uia://desktop",
				WindowTitle: "*",
				Items: &contract.ItemRule{
					Container: "window",
					Fields: map[string]contract.FieldRule{
						"title": {Attr: "name"},
					},
				},
				Expect: contract.Expectation{MinItems: 1},
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/tasks/claim"):
			var req contract.ClaimTaskRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			seenAllowed = append([]string(nil), req.AllowedDrivers...)
			if len(req.AllowedDrivers) == 1 && req.AllowedDrivers[0] == "uia" && !claimedUIA {
				claimedUIA = true
				_ = json.NewEncoder(w).Encode(contract.ClaimTaskResponse{
					Task: &contract.Task{
						ID:          "task_uia_1",
						Kind:        contract.TaskKindExtract,
						Recipe:      "desktop-windows",
						Domain:      "uia.local",
						Attempt:     1,
						MaxAttempts: 2,
					},
					ResolvedRecipe: &uiaRecipe,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(contract.ClaimTaskResponse{})
		case strings.HasSuffix(r.URL.Path, "/complete"):
			var sub contract.SubmitDropRequest
			_ = json.NewDecoder(r.Body).Decode(&sub)
			if len(sub.Report.Steps) > 0 {
				completedItems = len(sub.Report.Steps[0].Items)
			}
			_, _ = w.Write([]byte(`{"ok":true,"drop_id":"drop_uia_1","short_id":"uia123","message_id":"msg_uia_1"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	disconn := false
	cdpPrimary := &fakeDriver{
		name:          "cdp",
		connected:     &disconn,
		browserStatus: "Disconnected (click Connect to retry)",
	}
	cfg := contract.Config{
		From:         "win-pc",
		ServiceURL:   srv.URL,
		ServiceToken: "desk-tok",
	}
	client := bus.NewDesktopClient(cfg)

	// 1. First pass: Chrome CDP is disconnected, but UIA is available -> claims & completes UIA task!
	drained, err := drainQueue(context.Background(), cdpPrimary, cfg, client, "win-pc", 0, false, makeFallbackApprover("tray"))
	if err != nil || drained != 1 {
		t.Fatalf("drainQueue first pass: drained=%d err=%v, want drained=1 err=nil", drained, err)
	}
	if len(seenAllowed) != 1 || seenAllowed[0] != "uia" {
		t.Fatalf("AllowedDrivers = %v, want [uia]", seenAllowed)
	}
	if completedItems != 1 {
		t.Fatalf("completedItems = %d, want 1", completedItems)
	}

	// 2. Second pass: no more UIA tasks runnable while Chrome remains disconnected -> returns browser error.
	drained2, err2 := drainQueue(context.Background(), cdpPrimary, cfg, client, "win-pc", 0, false, makeFallbackApprover("tray"))
	if drained2 != 0 || err2 == nil || !strings.Contains(err2.Error(), "Disconnected") {
		t.Fatalf("drainQueue second pass: drained=%d err=%v, want 0 + Disconnected error", drained2, err2)
	}
}

func TestS1PerformSelfUpdateVerifiesSHA256BeforeSwap(t *testing.T) {
	goodBin := []byte("MZ-valid-updated-binary-payload-1234567890")
	goodSum := sha256.Sum256(goodBin)
	goodHex := hex.EncodeToString(goodSum[:])

	var serveBadHash atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/release/SHA256SUMS.txt":
			h := goodHex
			if serveBadHash.Load() {
				h = strings.Repeat("0", 64)
			}
			_, _ = w.Write([]byte(h + "  deskdrop.exe\n"))
		case "/v1/release/deskdrop.exe":
			w.Header().Set("X-DeskDrop-Version", "v1.3.2")
			_, _ = w.Write(goodBin)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	exePath := filepath.Join(dir, "deskdrop.exe")
	origBin := []byte("MZ-original-binary")
	if err := os.WriteFile(exePath, origBin, 0o755); err != nil {
		t.Fatal(err)
	}

	client := bus.NewDesktopClient(contract.Config{ServiceURL: srv.URL, ServiceToken: "desk-tok"})
	client.RetryBaseDelay = 1 * time.Millisecond

	// Mismatched SHA-256 must fail and leave existing binary intact.
	serveBadHash.Store(true)
	if _, err := performSelfUpdateTo(context.Background(), client, exePath, "windows", 10); err == nil {
		t.Fatal("expected SHA-256 mismatch to fail self-update")
	}
	afterBad, _ := os.ReadFile(exePath)
	if string(afterBad) != string(origBin) {
		t.Fatalf("existing binary was overwritten on SHA-256 mismatch: got %q", string(afterBad))
	}

	// Matching SHA-256 swaps atomically.
	serveBadHash.Store(false)
	ver, err := performSelfUpdateTo(context.Background(), client, exePath, "windows", 10)
	if err != nil || ver != "v1.3.2" {
		t.Fatalf("performSelfUpdateTo valid SHA-256 = (%q, %v), want (v1.3.2, nil)", ver, err)
	}
	afterGood, _ := os.ReadFile(exePath)
	if string(afterGood) != string(goodBin) {
		t.Fatalf("updated binary mismatch: got %q", string(afterGood))
	}
}

func TestM25RunTraySyncStepSkipsDrainOnRelay503(t *testing.T) {
	var (
		statusHits    atomic.Int32
		heartbeatHits atomic.Int32
		claimHits     atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status":
			statusHits.Add(1)
			http.Error(w, "recreating pod", http.StatusServiceUnavailable)
		case "/v1/workers/heartbeat":
			heartbeatHits.Add(1)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/tasks/claim":
			claimHits.Add(1)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := contract.Config{From: "win-pc", ServiceURL: srv.URL, ServiceToken: "desk-tok"}
	client := bus.NewDesktopClient(cfg)
	client.RetryBaseDelay = 1 * time.Millisecond

	drv := &fakeDriver{name: "cdp"}
	drained, updated, err := runTraySyncStep(context.Background(), drv, cfg, client, "win-pc", makeFallbackApprover("tray"), nil)
	if !errors.Is(err, errRelayRestarting) || drained != 0 || updated {
		t.Fatalf("runTraySyncStep on 503 = (drained=%d, updated=%v, err=%v), want (0, false, errRelayRestarting)", drained, updated, err)
	}
	if heartbeatHits.Load() != 0 || claimHits.Load() != 0 {
		t.Fatalf("expected 0 heartbeat and 0 claim calls on 503 status, got heartbeat=%d claim=%d", heartbeatHits.Load(), claimHits.Load())
	}
	if statusHits.Load() > 4 {
		t.Fatalf("expected <=4 /v1/status calls per tick, got %d", statusHits.Load())
	}
}

func TestS11S13T7ConfigCommentsAtomicSaveAndCLINegativePaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// S-11: saveConfigChromeSession handles // comments without shadowing into CWD deskdrop.json.
	cfgDir := t.TempDir()
	cfgFile := filepath.Join(cfgDir, "deskdrop.json")
	commented := "// User deskdrop config\n{\n  \"service_url\": \"https://relay.example.com\",\n  \"chrome-session\": \"OldProfile\",\n  \"chrome_profile\": \"OldProfile\"\n}\n"
	if err := os.WriteFile(cfgFile, []byte(commented), 0o600); err != nil {
		t.Fatal(err)
	}
	cwdDir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(cwdDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	if err := saveConfigChromeSession(cfgFile, "Profile 2"); err != nil {
		t.Fatalf("saveConfigChromeSession on commented config failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwdDir, "deskdrop.json")); err == nil {
		t.Fatal("saveConfigChromeSession shadowed config into CWD deskdrop.json")
	}
	updated, err := loadConfig(cfgFile)
	if err != nil || updated.ChromeSession != "Profile 2" || updated.ServiceURL != "https://relay.example.com" {
		t.Fatalf("reloaded config mismatch: cfg=%+v err=%v", updated, err)
	}

	// S-13: cmdOpenBrowser propagates --config errors instead of ignoring them.
	if err := cmdOpenBrowser([]string{"--config", filepath.Join(cfgDir, "missing.json")}); err == nil {
		t.Fatal("expected cmdOpenBrowser to return error on missing explicit --config")
	}

	// T-7: CLI argument parsing, unknown commands, recipe loading, and local recipes listing.
	if err := run([]string{"help"}); err != nil {
		t.Fatalf("run(help) failed: %v", err)
	}
	if err := run([]string{"version"}); err != nil {
		t.Fatalf("run(version) failed: %v", err)
	}
	if err := run([]string{"unknown-subcmd"}); err == nil {
		t.Fatal("expected unknown command error")
	}
	if err := cmdRun([]string{"--dry-run"}); err == nil {
		t.Fatal("expected cmdRun without recipe arg to fail")
	}
	if err := cmdRun([]string{"--driver", "invalid", "foo.json"}); err == nil {
		t.Fatal("expected cmdRun with invalid --driver to fail")
	}
	if err := cmdSync([]string{"--config", cfgFile, "--fallback-gate", "invalid"}); err == nil {
		t.Fatal("expected cmdSync with invalid --fallback-gate to fail")
	}

	// loadLocalRecipesFile handles bundle, slice, single recipe, and rejects invalid JSON.
	singlePath := filepath.Join(cfgDir, "single.json")
	_ = os.WriteFile(singlePath, []byte(`{"name":"s1","topic":"t","steps":[{"id":"st1","url":"https://example.com"}]}`), 0o600)
	list, err := loadLocalRecipesFile(singlePath)
	if err != nil || len(list) != 1 || list[0].Name != "s1" {
		t.Fatalf("loadLocalRecipesFile single = (%+v, %v)", list, err)
	}
	bundlePath := filepath.Join(cfgDir, "bundle.json")
	_ = os.WriteFile(bundlePath, []byte(`{"schema_version":"2.0","recipes":[{"name":"b1","topic":"t","steps":[{"id":"st1","url":"https://example.com"}]}]}`), 0o600)
	bList, err := loadLocalRecipesFile(bundlePath)
	if err != nil || len(bList) != 1 || bList[0].Name != "b1" {
		t.Fatalf("loadLocalRecipesFile bundle = (%+v, %v)", bList, err)
	}

	// cmdRecipes against local recipes_uri succeeds.
	localCfg := filepath.Join(cfgDir, "recipes-only.json")
	_ = os.WriteFile(localCfg, []byte(`{"recipes_uri":"`+filepath.ToSlash(bundlePath)+`"}`), 0o600)
	if err := cmdRecipes([]string{"--config", localCfg}); err != nil {
		t.Fatalf("cmdRecipes(local recipes_uri) failed: %v", err)
	}
	if err := cmdRecipes([]string{"--config", filepath.Join(cfgDir, "missing.json")}); err == nil {
		t.Fatal("expected cmdRecipes missing config to fail")
	}

	// splitPositionalFirst handles flags before and after positional target.
	pos, rest, err := splitPositionalFirst([]string{"--dry-run", "--var", "a=1", "my-recipe"})
	if err != nil || pos != "my-recipe" || len(rest) != 3 {
		t.Fatalf("splitPositionalFirst = (%q, %v, %v)", pos, rest, err)
	}

	// executeRecipeAndDeliver sends X-DeskDrop-Version header on deskdrop run uploads (S-13).
	var gotDropVer atomic.Value
	var gotRecipesVer atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/drops":
			gotDropVer.Store(r.Header.Get("X-DeskDrop-Version"))
			_, _ = w.Write([]byte(`{"ok":true,"drop_id":"d1","short_id":"s1","message_id":"m1"}`))
		default:
			gotRecipesVer.Store(r.Header.Get("X-DeskDrop-Version"))
			_, _ = w.Write([]byte(`{"ok":true,"recipes":[]}`))
		}
	}))
	defer srv.Close()
	origMake := makeTargetDriver
	makeTargetDriver = func(_ string, _ *cdp.SessionPool, _ contract.Config) driver.TargetDriver {
		return &fakeDriver{name: "cdp"}
	}
	defer func() { makeTargetDriver = origMake }()

	runCfg := contract.Config{From: "win-pc", ServiceURL: srv.URL, ServiceToken: "tok"}
	runRecipe := contract.Recipe{
		Name:  "demo",
		Topic: "ops",
		Steps: []contract.Step{{ID: "s1", URL: "https://example.com"}},
	}
	if err := executeRecipeAndDeliver(runRecipe, "cdp", runCfg, "all", false); err != nil {
		t.Fatalf("executeRecipeAndDeliver failed: %v", err)
	}
	if v, _ := gotDropVer.Load().(string); v != version {
		t.Fatalf("deskdrop run X-DeskDrop-Version = %q, want %q", v, version)
	}
	verCfg := filepath.Join(cfgDir, "http-ver.json")
	_ = os.WriteFile(verCfg, []byte(`{"service_url":"`+srv.URL+`","service_token":"tok"}`), 0o600)
	if err := cmdRecipes([]string{"--config", verCfg}); err != nil {
		t.Fatalf("cmdRecipes HTTP failed: %v", err)
	}
	if v, _ := gotRecipesVer.Load().(string); v != version {
		t.Fatalf("cmdRecipes X-DeskDrop-Version = %q, want %q", v, version)
	}
}

func TestM62FallbackTaskHeartbeatsBeforeSlowApprovalAndAutoDeclinesOnTimeout(t *testing.T) {
	var (
		heartbeatsBeforeApprove atomic.Int32
		heartbeatCh             = make(chan struct{}, 16)
		completed               atomic.Bool
		failedCode              atomic.Value
		failedMsg               atomic.Value
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			select {
			case heartbeatCh <- struct{}{}:
			default:
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "/complete"):
			completed.Store(true)
			_, _ = w.Write([]byte(`{"ok":true,"drop_id":"drop_hb","short_id":"hb1","message_id":"msg_hb"}`))
		case strings.HasSuffix(r.URL.Path, "/fail"):
			var meta contract.FailReportMetadata
			_ = json.NewDecoder(r.Body).Decode(&meta)
			failedCode.Store(string(meta.ErrorCode))
			failedMsg.Store(meta.Error)
			_, _ = w.Write([]byte(`{"ok":true,"triage":{"code":"FALLBACK_DECLINED","action":"diagnostic_escalation"}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	cfg := contract.Config{From: "win-pc", ServiceURL: srv.URL, ServiceToken: "desk-tok", FallbackGate: "tray"}
	client := bus.NewDesktopClient(cfg)
	client.RetryBaseDelay = time.Millisecond

	drv := &fakeDriver{
		fbRes: driver.FallbackExecResult{
			StepResult: contract.StepResult{StepID: "s1", URL: "https://status.example.com", Fields: map[string]any{"status": "OK"}},
			ActionsRun: 1,
		},
	}
	rcp := &contract.Recipe{
		Name:  "status-board",
		Topic: "ops",
		Steps: []contract.Step{{
			ID:     "s1",
			URL:    "https://status.example.com",
			Expect: contract.Expectation{RequiredFields: []string{"status"}},
		}},
	}
	task := contract.Task{
		ID:     "task_slow_prompt",
		Kind:   contract.TaskKindFallback,
		Recipe: "status-board",
		Domain: "status.example.com",
		FallbackPlan: &contract.FallbackPlan{
			AllowedDomain: "status.example.com",
			TargetURL:     "https://status.example.com",
			StepID:        "s1",
			Reason:        "Click tab",
			Actions:       []contract.FallbackAction{{Action: "click", Selector: "button.tab"}},
		},
	}

	// 1. Slow human approval receives >=2 lease heartbeats BEFORE returning true, then completes cleanly.
	execFallbackTaskWithOptions(
		context.Background(),
		drv,
		cfg,
		client,
		"win-pc",
		task,
		rcp,
		func(_ contract.FallbackPlan) (bool, error) {
			for i := 0; i < 2; i++ {
				select {
				case <-heartbeatCh:
					heartbeatsBeforeApprove.Add(1)
				case <-time.After(2 * time.Second):
					return false, errors.New("timed out waiting for pre-approval heartbeat")
				}
			}
			return true, nil
		},
		fallbackExecOptions{
			HeartbeatInterval: 15 * time.Millisecond,
			PromptTimeout:     2 * time.Second,
			StepTimeout:       2 * time.Second,
		},
	)
	if heartbeatsBeforeApprove.Load() < 2 || !completed.Load() || drv.fallbackRuns != 1 {
		t.Fatalf("pre-approval heartbeats=%d completed=%v runs=%d, want >=2, true, 1",
			heartbeatsBeforeApprove.Load(), completed.Load(), drv.fallbackRuns)
	}

	// 2. Unattended prompt auto-declines on PromptTimeout without hanging or running driver actions.
	blockForever := make(chan struct{})
	defer close(blockForever)
	execFallbackTaskWithOptions(
		context.Background(),
		drv,
		cfg,
		client,
		"win-pc",
		task,
		rcp,
		func(_ contract.FallbackPlan) (bool, error) {
			<-blockForever
			return true, nil
		},
		fallbackExecOptions{
			HeartbeatInterval: 15 * time.Millisecond,
			PromptTimeout:     45 * time.Millisecond,
			StepTimeout:       time.Second,
		},
	)
	if drv.fallbackRuns != 1 {
		t.Fatalf("timed-out prompt must not run ExecuteFallback: runs=%d", drv.fallbackRuns)
	}
	gotCode, _ := failedCode.Load().(string)
	gotMsg, _ := failedMsg.Load().(string)
	if gotCode != string(contract.ErrorCodeFallbackDeclined) || !strings.Contains(gotMsg, "timed out") {
		t.Fatalf("timeout fail = (%q, %q), want FALLBACK_DECLINED with timed out reason", gotCode, gotMsg)
	}
}

func TestM63TrayCoordinatorNonBlockingFallbackKeepsTickersAndActionsResponsive(t *testing.T) {
	var (
		claimedOnce   atomic.Bool
		statusChecks  atomic.Int32
		taskHBs       atomic.Int32
		completedTask = make(chan struct{}, 1)
		promptEntered = make(chan struct{}, 1)
		releasePrompt = make(chan bool, 1)
	)

	rcp := contract.Recipe{
		Name:  "status-board",
		Topic: "ops",
		Steps: []contract.Step{{
			ID:     "s1",
			URL:    "https://status.example.com",
			Expect: contract.Expectation{RequiredFields: []string{"status"}},
		}},
	}
	fbTask := contract.Task{
		ID:          "task_tray_fb",
		Kind:        contract.TaskKindFallback,
		Recipe:      "status-board",
		Domain:      "status.example.com",
		Attempt:     1,
		MaxAttempts: 1,
		FallbackPlan: &contract.FallbackPlan{
			AllowedDomain: "status.example.com",
			TargetURL:     "https://status.example.com",
			StepID:        "s1",
			Reason:        "Open tab",
			Actions:       []contract.FallbackAction{{Action: "wait", WaitMs: 10}},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/status":
			statusChecks.Add(1)
			_, _ = w.Write([]byte(`{"ok":true,"server_version":"v1.4.3"}`))
		case r.URL.Path == "/v1/tasks/claim":
			if claimedOnce.CompareAndSwap(false, true) {
				_ = json.NewEncoder(w).Encode(contract.ClaimTaskResponse{
					Task:           &fbTask,
					ResolvedRecipe: &rcp,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(contract.ClaimTaskResponse{})
		case strings.HasSuffix(r.URL.Path, "/heartbeat") && strings.Contains(r.URL.Path, "/tasks/"):
			taskHBs.Add(1)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "/complete"):
			select {
			case completedTask <- struct{}{}:
			default:
			}
			_, _ = w.Write([]byte(`{"ok":true,"drop_id":"drop_tray","short_id":"tr1","message_id":"msg_tr"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	drv := &fakeDriver{
		name: "cdp",
		fbRes: driver.FallbackExecResult{
			StepResult: contract.StepResult{StepID: "s1", URL: "https://status.example.com", Fields: map[string]any{"status": "OK"}},
			ActionsRun: 1,
		},
	}

	coord := newTrayCoordinator("", "", 25*time.Millisecond, "tray", "", nil)
	coord.probeInterval = 20 * time.Millisecond
	coord.hbInterval = 15 * time.Millisecond
	coord.promptTimeout = 2 * time.Second
	coord.retryBaseDelay = time.Millisecond
	coord.loadCfgFn = func(_ string) (contract.Config, error) {
		return contract.Config{
			From:         "win-pc",
			ServiceURL:   srv.URL,
			ServiceToken: "desk-tok",
			FallbackGate: "tray",
		}, nil
	}
	coord.makeDriverFn = func(_ string, _ *cdp.SessionPool, _ contract.Config) driver.TargetDriver {
		return drv
	}
	coord.approverFactory = func(_ string) FallbackApprover {
		return func(_ contract.FallbackPlan) (bool, error) {
			select {
			case promptEntered <- struct{}{}:
			default:
			}
			ok := <-releasePrompt
			return ok, nil
		}
	}

	var probeTicks atomic.Int32
	coord.onRefreshStatus = func() {
		probeTicks.Add(1)
	}

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		coord.workerLoop()
	}()

	// Wait until the Tier-2 confirmation prompt is actively blocking on its background goroutine.
	select {
	case <-promptEntered:
	case <-time.After(2 * time.Second):
		close(coord.stopCh)
		<-loopDone
		t.Fatal("timed out waiting for async Tier-2 confirmation prompt to start")
	}

	probesAtPromptStart := probeTicks.Load()
	statusAtPromptStart := statusChecks.Load()

	// Dispatch menuIDToggleGate over actionCh while the prompt is open — must apply immediately without blocking!
	coord.actionCh <- trayAction{cmdID: menuIDToggleGate}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, _, _, gate, _, _ := coord.snapshotState()
		if gate == "auto" &&
			probeTicks.Load() >= probesAtPromptStart+2 &&
			statusChecks.Load() >= statusAtPromptStart+1 &&
			taskHBs.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, _, _, gateNow, busyNow, _ := coord.snapshotState()
	if gateNow != "auto" {
		t.Fatalf("actionCh blocked while Tier-2 prompt open: gate=%q, want auto", gateNow)
	}
	if !busyNow {
		t.Fatal("expected tray busy=true while Tier-2 fallback prompt is active")
	}
	if probeTicks.Load() < probesAtPromptStart+2 {
		t.Fatalf("probeTicker froze while Tier-2 prompt open: before=%d after=%d", probesAtPromptStart, probeTicks.Load())
	}
	if statusChecks.Load() < statusAtPromptStart+1 {
		t.Fatalf("syncTicker froze while Tier-2 prompt open: before=%d after=%d", statusAtPromptStart, statusChecks.Load())
	}
	if taskHBs.Load() < 1 {
		t.Fatalf("expected >=1 task lease heartbeat while Tier-2 prompt open, got %d", taskHBs.Load())
	}

	// Approve the open prompt and confirm completion + clean shutdown.
	releasePrompt <- true
	select {
	case <-completedTask:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for approved async Tier-2 fallback to complete")
	}

	close(coord.stopCh)
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("workerLoop hung on stopCh close")
	}
}

type failingStepDriver struct {
	fakeDriver
	stepErr error
}

func (d *failingStepDriver) ExecuteStep(_ context.Context, _ contract.Step) (contract.StepResult, error) {
	return contract.StepResult{}, d.stepErr
}

func TestM813CLIWarningSurfaceOnDroppedErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	captureStderr := func(fn func()) string {
		orig := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stderr = w
		done := make(chan string, 1)
		go func() {
			var b strings.Builder
			buf := make([]byte, 1024)
			for {
				n, rerr := r.Read(buf)
				if n > 0 {
					b.Write(buf[:n])
				}
				if rerr != nil {
					break
				}
			}
			done <- b.String()
		}()
		fn()
		_ = w.Close()
		os.Stderr = orig
		out := <-done
		_ = r.Close()
		return out
	}

	// 1. cmdStatus logs WARN when ReportWorkerStatus fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/workers/heartbeat" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"server_version":"v1.5.0"}`))
	}))
	defer srv.Close()

	cfgPath := filepath.Join(t.TempDir(), "cfg.json")
	_ = os.WriteFile(cfgPath, []byte(`{"service_url":"`+srv.URL+`","service_token":"tok","driver":"uia"}`), 0o600)
	stderr1 := captureStderr(func() {
		_ = cmdStatus([]string{"--config", cfgPath, "--driver", "uia"})
	})
	if !strings.Contains(stderr1, "WARN status heartbeat failed") {
		t.Fatalf("expected heartbeat warning on stderr, got:\n%s", stderr1)
	}

	// 2. executeRecipeAndDeliver logs WARN on BuildDiagnosticZip air-gap block and SaveDiagnosticZip ENOTDIR error.
	origMake := makeTargetDriver
	defer func() { makeTargetDriver = origMake }()

	rcp := contract.Recipe{
		Name:  "diag-warn",
		Topic: "ops",
		Steps: []contract.Step{{ID: "s1", URL: "https://example.com"}},
	}

	makeTargetDriver = func(_ string, _ *cdp.SessionPool, _ contract.Config) driver.TargetDriver {
		return &failingStepDriver{
			stepErr: &driver.StepExecError{
				StepID:  "s1",
				Err:     errors.New("drift"),
				DOMHTML: "<html><body>blocked-secret-term</body></html>",
			},
		}
	}
	stderr2 := captureStderr(func() {
		_ = executeRecipeAndDeliver(rcp, "cdp", contract.Config{
			ForbiddenTerms: []string{"blocked-secret-term"},
		}, "", true)
	})
	if !strings.Contains(stderr2, "WARN failed building diagnostic bundle") {
		t.Fatalf("expected diagnostic build warning on stderr, got:\n%s", stderr2)
	}

	// Use a regular file as DebugDir parent so SaveDiagnosticZip fails deterministically even as uid=0 in CI.
	notADir := filepath.Join(t.TempDir(), "regular-file")
	_ = os.WriteFile(notADir, []byte("x"), 0o600)
	makeTargetDriver = func(_ string, _ *cdp.SessionPool, _ contract.Config) driver.TargetDriver {
		return &failingStepDriver{
			stepErr: &driver.StepExecError{
				StepID:  "s1",
				Err:     errors.New("drift"),
				DOMHTML: "<html><body>clean</body></html>",
			},
		}
	}
	stderr3 := captureStderr(func() {
		_ = executeRecipeAndDeliver(rcp, "cdp", contract.Config{
			DebugDir: filepath.Join(notADir, "nested"),
		}, "", true)
	})
	if !strings.Contains(stderr3, "WARN failed saving diagnostic bundle") {
		t.Fatalf("expected diagnostic save warning on stderr, got:\n%s", stderr3)
	}

	// 3. SendErrorWithContext failure during non-dry-run escalation logs WARN to stderr.
	errRelay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "relay down", http.StatusBadRequest)
	}))
	defer errRelay.Close()
	stderr4 := captureStderr(func() {
		_ = executeRecipeAndDeliver(rcp, "cdp", contract.Config{
			From:         "win-pc",
			ServiceURL:   errRelay.URL,
			ServiceToken: "tok",
			DebugDir:     t.TempDir(),
		}, "ops-agent", false)
	})
	if !strings.Contains(stderr4, "WARN failed escalating error bundle") {
		t.Fatalf("expected error bundle escalation warning on stderr, got:\n%s", stderr4)
	}
}
