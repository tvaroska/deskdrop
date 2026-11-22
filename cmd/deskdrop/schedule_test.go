package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildSchtasksArgsSyncLogonAndSafetyChecks(t *testing.T) {
	syncArgs, err := BuildSchtasksCreateArgs(ScheduleSpec{
		TaskName:     "DeskDropSync",
		Mode:         "sync",
		Every:        15 * time.Minute,
		ExePath:      `C:\Program Files\DeskDrop\deskdrop.exe`,
		ConfigPath:   `C:\Users\Alice Smith\AppData\Roaming\deskdrop\config.json`,
		CDPURL:       "http://127.0.0.1:9222",
		FallbackGate: "tray",
	})
	if err != nil {
		t.Fatalf("BuildSchtasksCreateArgs sync failed: %v", err)
	}
	preview := FormatSchtasksCommand(syncArgs)
	for _, want := range []string{
		"/Create",
		"/TN DeskDropSync",
		"--hide-console --quiet-cdp --fallback-gate tray",
		"/SC MINUTE /MO 15",
		"/F",
	} {
		if !strings.Contains(preview, want) {
			t.Fatalf("expected %q in preview:\n%s", want, preview)
		}
	}

	// 2h interval maps cleanly to /SC HOURLY /MO 2.
	hourlyArgs, err := BuildSchtasksCreateArgs(ScheduleSpec{
		TaskName: "DeskDropHourly",
		Mode:     "sync",
		Every:    2 * time.Hour,
		ExePath:  `C:\Tools\deskdrop.exe`,
	})
	if err != nil {
		t.Fatalf("hourly BuildSchtasksCreateArgs failed: %v", err)
	}
	if !strings.Contains(FormatSchtasksCommand(hourlyArgs), "/SC HOURLY /MO 2") {
		t.Fatalf("expected /SC HOURLY /MO 2, got %s", FormatSchtasksCommand(hourlyArgs))
	}

	// Logon mode launches persistent tray worker at ONLOGON.
	logonArgs, err := BuildSchtasksCreateArgs(ScheduleSpec{
		TaskName:     "DeskDropTray",
		Mode:         "logon",
		ExePath:      `C:\Tools\deskdrop.exe`,
		FallbackGate: "tray",
	})
	if err != nil {
		t.Fatalf("logon BuildSchtasksCreateArgs failed: %v", err)
	}
	if !strings.Contains(FormatSchtasksCommand(logonArgs), "/SC ONLOGON") || !strings.Contains(FormatSchtasksCommand(logonArgs), "tray --fallback-gate tray") {
		t.Fatalf("unexpected logon preview: %s", FormatSchtasksCommand(logonArgs))
	}

	// Reject shell/quote injection or invalid intervals.
	if _, err := BuildSchtasksCreateArgs(ScheduleSpec{
		TaskName: "Bad;Name",
		ExePath:  `C:\Tools\deskdrop.exe`,
		Every:    15 * time.Minute,
	}); err == nil {
		t.Fatal("expected invalid task name to fail")
	}
	if _, err := BuildSchtasksCreateArgs(ScheduleSpec{
		TaskName: "DeskDropSync",
		ExePath:  `C:\Tools\deskdrop.exe" & calc.exe`,
		Every:    15 * time.Minute,
	}); err == nil {
		t.Fatal("expected embedded quote in exe path to fail")
	}
	if _, err := BuildSchtasksCreateArgs(ScheduleSpec{
		TaskName: "DeskDropSync",
		Mode:     "sync",
		ExePath:  `C:\Tools\deskdrop.exe`,
		Every:    25 * time.Hour,
	}); err == nil {
		t.Fatal("expected >23h59m interval to fail")
	}
}

func TestScheduleCLIInstallStatusUninstallWithInjectedRunner(t *testing.T) {
	var calls [][]string
	mockExec := func(_ context.Context, binary string, args []string) (string, error) {
		row := append([]string{binary}, args...)
		calls = append(calls, row)
		return "SUCCESS", nil
	}

	if err := runScheduleWithExec([]string{
		"install",
		"--task-name", "DeskDropTest",
		"--every", "10m",
		"--exe", `C:\Tools\deskdrop.exe`,
		"--fallback-gate", "tray",
	}, mockExec); err != nil {
		t.Fatalf("schedule install failed: %v", err)
	}
	if err := runScheduleWithExec([]string{"status", "--task-name", "DeskDropTest"}, mockExec); err != nil {
		t.Fatalf("schedule status failed: %v", err)
	}
	if err := runScheduleWithExec([]string{"uninstall", "--task-name", "DeskDropTest"}, mockExec); err != nil {
		t.Fatalf("schedule uninstall failed: %v", err)
	}

	if len(calls) != 3 {
		t.Fatalf("expected 3 schtasks.exe invocations, got %d", len(calls))
	}
	if calls[0][0] != "schtasks.exe" || calls[0][1] != "/Create" {
		t.Fatalf("unexpected install call: %v", calls[0])
	}
	if calls[1][1] != "/Query" {
		t.Fatalf("unexpected status call: %v", calls[1])
	}
	if calls[2][1] != "/Delete" {
		t.Fatalf("unexpected uninstall call: %v", calls[2])
	}
}

func TestQuietCDPSyncExitsZeroAndReportsHeartbeatWhenBrowserOffline(t *testing.T) {
	var heartbeats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/workers/heartbeat") {
			heartbeats.Add(1)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cfgFile := filepath.Join(t.TempDir(), "deskdrop.json")
	t.Setenv("DESKDROP_SERVICE_URL", srv.URL)
	t.Setenv("DESKDROP_TOKEN", "desk-tok")

	// Point --cdp to a closed local HTTP port so ProbeQuiet returns connected=false without touching DevToolsActivePort.
	err := cmdSync([]string{"--config", cfgFile, "--cdp", "http://127.0.0.1:1", "--quiet-cdp"})
	if err != nil && !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("unexpected error: %v", err)
	}
	// Run without explicit missing --config so loadConfig uses env vars cleanly.
	if err := cmdSync([]string{"--cdp", "http://127.0.0.1:1", "--quiet-cdp"}); err != nil {
		t.Fatalf("expected --quiet-cdp drain to exit 0 cleanly when browser offline, got %v", err)
	}
	if heartbeats.Load() < 1 {
		t.Fatalf("expected worker heartbeat report when browser offline, got %d", heartbeats.Load())
	}
}
