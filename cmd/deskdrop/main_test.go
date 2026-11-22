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
)

type fakeDriver struct {
	mu           sync.Mutex
	fallbackRuns int
	fbRes        driver.FallbackExecResult
}

func (f *fakeDriver) Name() string { return "fake" }
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
	return driver.DriverStatus{Connected: true, Label: "Connected", BrowserStatus: "Ready"}
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
