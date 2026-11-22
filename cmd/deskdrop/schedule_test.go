package main

import (
	"context"
	"strings"
	"testing"
)

func TestBuildRegRunArgsAndSafetyChecks(t *testing.T) {
	addArgs, err := BuildRegAddRunArgs(LogonSpec{
		Name:         "DeskDrop",
		ExePath:      `C:\Program Files\DeskDrop\deskdrop.exe`,
		ConfigPath:   `C:\Users\Alice Smith\AppData\Roaming\deskdrop\config.json`,
		CDPURL:       "http://127.0.0.1:9222",
		FallbackGate: "tray",
	})
	if err != nil {
		t.Fatalf("BuildRegAddRunArgs failed: %v", err)
	}
	if addArgs[0] != "add" || addArgs[1] != RunRegistryKey || addArgs[2] != "/v" || addArgs[3] != "DeskDrop" || addArgs[5] != "REG_SZ" {
		t.Fatalf("unexpected reg add args: %v", addArgs)
	}
	cmdData := addArgs[7]
	for _, want := range []string{
		`"C:\Program Files\DeskDrop\deskdrop.exe" tray --fallback-gate tray`,
		`--config "C:\Users\Alice Smith\AppData\Roaming\deskdrop\config.json"`,
		`--cdp http://127.0.0.1:9222`,
	} {
		if !strings.Contains(cmdData, want) {
			t.Fatalf("expected %q in REG_SZ data:\n%s", want, cmdData)
		}
	}

	preview := FormatRegCommand(addArgs)
	if !strings.HasPrefix(preview, `reg.exe add HKCU\Software\Microsoft\Windows\CurrentVersion\Run /v DeskDrop /t REG_SZ /d `) {
		t.Fatalf("unexpected preview command:\n%s", preview)
	}

	// Reject invalid registry value names and quote/control-char injection.
	if _, err := BuildRegAddRunArgs(LogonSpec{
		Name:    `Bad\Name`,
		ExePath: `C:\Tools\deskdrop.exe`,
	}); err == nil {
		t.Fatal("expected backslash in --name to fail")
	}
	if _, err := BuildRegAddRunArgs(LogonSpec{
		Name:    "DeskDrop",
		ExePath: `C:\Tools\deskdrop.exe" & calc.exe`,
	}); err == nil {
		t.Fatal("expected embedded quote in --exe to fail")
	}
	if _, err := BuildRegAddRunArgs(LogonSpec{
		Name:         "DeskDrop",
		ExePath:      `C:\Tools\deskdrop.exe`,
		FallbackGate: "invalid",
	}); err == nil {
		t.Fatal("expected invalid --fallback-gate to fail")
	}
}

func TestScheduleCLIRegInstallStatusUninstallAndRejectsRetiredFlags(t *testing.T) {
	var calls [][]string
	mockExec := func(_ context.Context, binary string, args []string) (string, error) {
		row := append([]string{binary}, args...)
		calls = append(calls, row)
		return "The operation completed successfully.", nil
	}

	if err := runScheduleWithExec([]string{
		"install",
		"--name", "DeskDropWorker",
		"--exe", `C:\Users\Tester\deskdrop\deskdrop.exe`,
		"--fallback-gate", "tray",
	}, mockExec); err != nil {
		t.Fatalf("schedule install failed: %v", err)
	}
	if err := runScheduleWithExec([]string{"status", "--name", "DeskDropWorker"}, mockExec); err != nil {
		t.Fatalf("schedule status failed: %v", err)
	}
	if err := runScheduleWithExec([]string{"uninstall", "--name", "DeskDropWorker"}, mockExec); err != nil {
		t.Fatalf("schedule uninstall failed: %v", err)
	}

	if len(calls) != 3 {
		t.Fatalf("expected 3 reg.exe invocations, got %d", len(calls))
	}
	if calls[0][0] != "reg.exe" || calls[0][1] != "add" || calls[0][2] != RunRegistryKey {
		t.Fatalf("unexpected install call: %v", calls[0])
	}
	if calls[1][0] != "reg.exe" || calls[1][1] != "query" {
		t.Fatalf("unexpected status call: %v", calls[1])
	}
	if calls[2][0] != "reg.exe" || calls[2][1] != "delete" {
		t.Fatalf("unexpected uninstall call: %v", calls[2])
	}

	// Retired schtasks flags (--every, --mode) and sync scheduled flags (--quiet-cdp, --hide-console) must be rejected.
	if err := runScheduleWithExec([]string{"install", "--every", "15m"}, mockExec); err == nil {
		t.Fatal("expected retired --every flag on schedule install to fail")
	}
	if err := runScheduleWithExec([]string{"install", "--mode", "sync"}, mockExec); err == nil {
		t.Fatal("expected retired --mode flag on schedule install to fail")
	}
	if err := cmdSync([]string{"--quiet-cdp"}); err == nil {
		t.Fatal("expected retired --quiet-cdp flag on sync to fail")
	}
	if err := cmdSync([]string{"--hide-console"}); err == nil {
		t.Fatal("expected retired --hide-console flag on sync to fail")
	}
}
