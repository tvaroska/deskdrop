package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const DefaultScheduledTaskName = "DeskDropSync"

var validTaskNameRe = regexp.MustCompile(`^[A-Za-z0-9_.\-\\]{1,120}$`)

// ScheduleSpec defines a Windows Task Scheduler (schtasks.exe) job for background DeskDrop execution.
type ScheduleSpec struct {
	TaskName     string
	Mode         string        // "sync" (periodic drain) | "logon" (persistent tray worker)
	Every        time.Duration // used when Mode == "sync" (1m..1439m)
	ExePath      string
	ConfigPath   string
	CDPURL       string
	FallbackGate string
}

type commandExecFn func(ctx context.Context, binary string, args []string) (string, error)

var defaultSchtasksExec commandExecFn = func(ctx context.Context, binary string, args []string) (string, error) {
	if runtime.GOOS != "windows" {
		return "", errors.New("schtasks.exe requires Windows (pass --dry-run to preview the scheduled command)")
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func cmdSchedule(args []string) error {
	return runScheduleWithExec(args, defaultSchtasksExec)
}

func runScheduleWithExec(args []string, execFn commandExecFn) error {
	if len(args) == 0 {
		printScheduleUsage()
		return nil
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	switch sub {
	case "help", "-h", "--help":
		printScheduleUsage()
		return nil
	case "install", "create":
		return cmdScheduleInstall(args[1:], execFn)
	case "uninstall", "remove", "delete":
		return cmdScheduleUninstall(args[1:], execFn)
	case "status", "query":
		return cmdScheduleStatus(args[1:], execFn)
	default:
		return fmt.Errorf("unknown schedule subcommand %q (expected install, uninstall, or status)", args[0])
	}
}

func printScheduleUsage() {
	fmt.Println(`Usage:
  deskdrop schedule install   [--every 15m] [--mode sync|logon] [--task-name DeskDropSync] [--fallback-gate tray|auto] [--config <path>] [--cdp <url>] [--exe <path>] [--dry-run]
  deskdrop schedule uninstall [--task-name DeskDropSync] [--dry-run]
  deskdrop schedule status    [--task-name DeskDropSync] [--dry-run]`)
}

func cmdScheduleInstall(args []string, execFn commandExecFn) error {
	fs := flag.NewFlagSet("schedule install", flag.ContinueOnError)
	taskName := fs.String("task-name", DefaultScheduledTaskName, "Windows Scheduled Task name")
	mode := fs.String("mode", "sync", "Schedule mode: 'sync' (periodic quiet drain) or 'logon' (persistent tray)")
	every := fs.Duration("every", 15*time.Minute, "Poll interval for --mode=sync (1m..23h59m)")
	fallbackGate := fs.String("fallback-gate", "tray", "Tier-2 fallback gate (tray|auto)")
	cfgPath := fs.String("config", "", "Optional deskdrop.json config path")
	cdpURL := fs.String("cdp", "", "Optional explicit CDP endpoint (e.g. http://127.0.0.1:9222)")
	exePath := fs.String("exe", "", "Explicit path to deskdrop.exe (defaults to current binary)")
	dryRun := fs.Bool("dry-run", false, "Print schtasks.exe command without executing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	gate, err := validateFallbackGate(*fallbackGate)
	if err != nil {
		return err
	}
	resolvedExe := strings.TrimSpace(*exePath)
	if resolvedExe == "" {
		exe, eerr := os.Executable()
		if eerr != nil || exe == "" {
			return fmt.Errorf("cannot resolve executable path: %w", eerr)
		}
		resolvedExe = exe
	}
	resolvedCfg := strings.TrimSpace(*cfgPath)
	if resolvedCfg != "" && !isWindowsAbsPath(resolvedCfg) && !filepath.IsAbs(resolvedCfg) {
		if abs, aerr := filepath.Abs(resolvedCfg); aerr == nil {
			resolvedCfg = abs
		}
	}

	spec := ScheduleSpec{
		TaskName:     *taskName,
		Mode:         *mode,
		Every:        *every,
		ExePath:      resolvedExe,
		ConfigPath:   resolvedCfg,
		CDPURL:       strings.TrimSpace(*cdpURL),
		FallbackGate: gate,
	}
	schtasksArgs, err := BuildSchtasksCreateArgs(spec)
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Println(FormatSchtasksCommand(schtasksArgs))
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := execFn(ctx, "schtasks.exe", schtasksArgs)
	if err != nil {
		if out != "" {
			return fmt.Errorf("schtasks.exe /Create failed: %w (%s)", err, out)
		}
		return fmt.Errorf("schtasks.exe /Create failed: %w", err)
	}
	fmt.Printf("Installed scheduled task %q (%s).\n", spec.TaskName, spec.Mode)
	if out != "" {
		fmt.Println(out)
	}
	return nil
}

func cmdScheduleUninstall(args []string, execFn commandExecFn) error {
	fs := flag.NewFlagSet("schedule uninstall", flag.ContinueOnError)
	taskName := fs.String("task-name", DefaultScheduledTaskName, "Windows Scheduled Task name")
	dryRun := fs.Bool("dry-run", false, "Print schtasks.exe command without executing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	schtasksArgs, err := BuildSchtasksDeleteArgs(*taskName)
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Println(FormatSchtasksCommand(schtasksArgs))
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := execFn(ctx, "schtasks.exe", schtasksArgs)
	if err != nil {
		if out != "" {
			return fmt.Errorf("schtasks.exe /Delete failed: %w (%s)", err, out)
		}
		return fmt.Errorf("schtasks.exe /Delete failed: %w", err)
	}
	fmt.Printf("Removed scheduled task %q.\n", strings.TrimSpace(*taskName))
	if out != "" {
		fmt.Println(out)
	}
	return nil
}

func cmdScheduleStatus(args []string, execFn commandExecFn) error {
	fs := flag.NewFlagSet("schedule status", flag.ContinueOnError)
	taskName := fs.String("task-name", DefaultScheduledTaskName, "Windows Scheduled Task name")
	dryRun := fs.Bool("dry-run", false, "Print schtasks.exe command without executing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	schtasksArgs, err := BuildSchtasksQueryArgs(*taskName)
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Println(FormatSchtasksCommand(schtasksArgs))
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := execFn(ctx, "schtasks.exe", schtasksArgs)
	if err != nil {
		if out != "" {
			return fmt.Errorf("schtasks.exe /Query failed: %w (%s)", err, out)
		}
		return fmt.Errorf("schtasks.exe /Query failed: %w", err)
	}
	fmt.Println(out)
	return nil
}

// BuildSchtasksCreateArgs validates spec and constructs direct schtasks.exe /Create arguments
// (zero cmd.exe shell interpolation).
func BuildSchtasksCreateArgs(spec ScheduleSpec) ([]string, error) {
	name := strings.TrimSpace(spec.TaskName)
	if name == "" {
		name = DefaultScheduledTaskName
	}
	if !validTaskNameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid --task-name %q (allowed: letters, digits, '.', '_', '-', '\\')", spec.TaskName)
	}
	exe := strings.TrimSpace(spec.ExePath)
	if exe == "" {
		return nil, errors.New("executable path is required")
	}
	if err := rejectUnsafeTaskArg("--exe", exe); err != nil {
		return nil, err
	}
	if err := rejectUnsafeTaskArg("--config", spec.ConfigPath); err != nil {
		return nil, err
	}
	if err := rejectUnsafeTaskArg("--cdp", spec.CDPURL); err != nil {
		return nil, err
	}

	gate, err := validateFallbackGate(spec.FallbackGate)
	if err != nil {
		return nil, err
	}

	mode := strings.ToLower(strings.TrimSpace(spec.Mode))
	if mode == "" {
		mode = "sync"
	}

	var runTokens []string
	var scheduleFlags []string

	switch mode {
	case "sync":
		mins := int(spec.Every / time.Minute)
		if spec.Every <= 0 || spec.Every%time.Minute != 0 || mins < 1 || mins > 1439 {
			return nil, fmt.Errorf("--every must be whole minutes between 1m and 23h59m, got %s", spec.Every)
		}
		if mins >= 60 && mins%60 == 0 {
			scheduleFlags = []string{"/SC", "HOURLY", "/MO", strconv.Itoa(mins / 60)}
		} else {
			scheduleFlags = []string{"/SC", "MINUTE", "/MO", strconv.Itoa(mins)}
		}
		runTokens = []string{quoteTaskToken(exe), "sync", "--hide-console", "--quiet-cdp", "--fallback-gate", gate}
	case "logon":
		scheduleFlags = []string{"/SC", "ONLOGON"}
		runTokens = []string{quoteTaskToken(exe), "tray", "--fallback-gate", gate}
	default:
		return nil, fmt.Errorf("invalid --mode %q (expected 'sync' or 'logon')", spec.Mode)
	}

	if strings.TrimSpace(spec.ConfigPath) != "" {
		runTokens = append(runTokens, "--config", quoteTaskToken(strings.TrimSpace(spec.ConfigPath)))
	}
	if strings.TrimSpace(spec.CDPURL) != "" {
		runTokens = append(runTokens, "--cdp", quoteTaskToken(strings.TrimSpace(spec.CDPURL)))
	}

	taskRun := strings.Join(runTokens, " ")
	if len(taskRun) > 260 {
		return nil, fmt.Errorf("scheduled task command length (%d) exceeds schtasks.exe 261-char /TR limit; shorten paths", len(taskRun))
	}

	out := []string{"/Create", "/TN", name, "/TR", taskRun}
	out = append(out, scheduleFlags...)
	out = append(out, "/F")
	return out, nil
}

// BuildSchtasksDeleteArgs constructs direct schtasks.exe /Delete arguments.
func BuildSchtasksDeleteArgs(taskName string) ([]string, error) {
	name := strings.TrimSpace(taskName)
	if name == "" {
		name = DefaultScheduledTaskName
	}
	if !validTaskNameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid --task-name %q", taskName)
	}
	return []string{"/Delete", "/TN", name, "/F"}, nil
}

// BuildSchtasksQueryArgs constructs direct schtasks.exe /Query arguments.
func BuildSchtasksQueryArgs(taskName string) ([]string, error) {
	name := strings.TrimSpace(taskName)
	if name == "" {
		name = DefaultScheduledTaskName
	}
	if !validTaskNameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid --task-name %q", taskName)
	}
	return []string{"/Query", "/TN", name, "/FO", "LIST", "/V"}, nil
}

// FormatSchtasksCommand formats a preview command line for --dry-run output.
func FormatSchtasksCommand(args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, "schtasks.exe")
	for _, a := range args {
		if strings.ContainsAny(a, " \t\"") {
			escaped := strings.ReplaceAll(a, `"`, `\"`)
			parts = append(parts, `"`+escaped+`"`)
		} else {
			parts = append(parts, a)
		}
	}
	return strings.Join(parts, " ")
}

func rejectUnsafeTaskArg(flagName string, val string) error {
	if strings.ContainsAny(val, "\"\r\n\x00") {
		return fmt.Errorf("%s must not contain quotes or control characters", flagName)
	}
	return nil
}

func quoteTaskToken(val string) string {
	if strings.ContainsAny(val, " \t") {
		return `"` + val + `"`
	}
	return val
}

func isWindowsAbsPath(p string) bool {
	if len(p) >= 3 && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return true
	}
	return strings.HasPrefix(p, `\\`)
}
