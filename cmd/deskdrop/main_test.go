package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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
