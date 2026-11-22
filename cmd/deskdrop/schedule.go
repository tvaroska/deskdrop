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
	"strings"
	"time"
)

const (
	RunRegistryKey      = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	DefaultRunValueName = "DeskDrop"
)

var validRunValueNameRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,64}$`)

// LogonSpec defines a non-Admin Windows HKCU\Software\Microsoft\Windows\CurrentVersion\Run
// logon registration that launches the persistent DeskDrop system tray worker at user sign-in.
type LogonSpec struct {
	Name         string
	ExePath      string
	ConfigPath   string
	CDPURL       string
	FallbackGate string
}

type commandExecFn func(ctx context.Context, binary string, args []string) (string, error)

var defaultRegExec commandExecFn = func(ctx context.Context, binary string, args []string) (string, error) {
	if runtime.GOOS != "windows" {
		return "", errors.New("reg.exe requires Windows (pass --dry-run to preview the HKCU Run command)")
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func cmdSchedule(args []string) error {
	return runScheduleWithExec(args, defaultRegExec)
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
  deskdrop schedule install   [--name DeskDrop] [--fallback-gate tray|auto] [--config <path>] [--cdp <url>] [--exe <path>] [--dry-run]
  deskdrop schedule uninstall [--name DeskDrop] [--dry-run]
  deskdrop schedule status    [--name DeskDrop] [--dry-run]`)
}

func cmdScheduleInstall(args []string, execFn commandExecFn) error {
	fs := flag.NewFlagSet("schedule install", flag.ContinueOnError)
	name := fs.String("name", DefaultRunValueName, "HKCU Run value name")
	fallbackGate := fs.String("fallback-gate", "tray", "Tier-2 fallback gate (tray|auto)")
	cfgPath := fs.String("config", "", "Optional deskdrop.json config path")
	cdpURL := fs.String("cdp", "", "Optional explicit CDP endpoint (e.g. http://127.0.0.1:9222)")
	exePath := fs.String("exe", "", "Explicit path to deskdrop.exe (defaults to current binary)")
	dryRun := fs.Bool("dry-run", false, "Print reg.exe command without executing")
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

	spec := LogonSpec{
		Name:         *name,
		ExePath:      resolvedExe,
		ConfigPath:   resolvedCfg,
		CDPURL:       strings.TrimSpace(*cdpURL),
		FallbackGate: gate,
	}
	regArgs, err := BuildRegAddRunArgs(spec)
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Println(FormatRegCommand(regArgs))
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := execFn(ctx, "reg.exe", regArgs)
	if err != nil {
		if out != "" {
			return fmt.Errorf("reg.exe add failed: %w (%s)", err, out)
		}
		return fmt.Errorf("reg.exe add failed: %w", err)
	}
	valName := strings.TrimSpace(spec.Name)
	if valName == "" {
		valName = DefaultRunValueName
	}
	fmt.Printf("Registered HKCU Run tray logon %q (%s).\n", valName, RunRegistryKey)
	if out != "" {
		fmt.Println(out)
	}
	return nil
}

func cmdScheduleUninstall(args []string, execFn commandExecFn) error {
	fs := flag.NewFlagSet("schedule uninstall", flag.ContinueOnError)
	name := fs.String("name", DefaultRunValueName, "HKCU Run value name")
	dryRun := fs.Bool("dry-run", false, "Print reg.exe command without executing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	regArgs, err := BuildRegDeleteRunArgs(*name)
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Println(FormatRegCommand(regArgs))
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := execFn(ctx, "reg.exe", regArgs)
	if err != nil {
		if out != "" {
			return fmt.Errorf("reg.exe delete failed: %w (%s)", err, out)
		}
		return fmt.Errorf("reg.exe delete failed: %w", err)
	}
	valName := strings.TrimSpace(*name)
	if valName == "" {
		valName = DefaultRunValueName
	}
	fmt.Printf("Removed HKCU Run tray logon %q.\n", valName)
	if out != "" {
		fmt.Println(out)
	}
	return nil
}

func cmdScheduleStatus(args []string, execFn commandExecFn) error {
	fs := flag.NewFlagSet("schedule status", flag.ContinueOnError)
	name := fs.String("name", DefaultRunValueName, "HKCU Run value name")
	dryRun := fs.Bool("dry-run", false, "Print reg.exe command without executing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	regArgs, err := BuildRegQueryRunArgs(*name)
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Println(FormatRegCommand(regArgs))
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := execFn(ctx, "reg.exe", regArgs)
	if err != nil {
		if out != "" {
			return fmt.Errorf("reg.exe query failed: %w (%s)", err, out)
		}
		return fmt.Errorf("reg.exe query failed: %w", err)
	}
	fmt.Println(out)
	return nil
}

// BuildRegAddRunArgs validates spec and constructs direct reg.exe add arguments
// for HKCU\Software\Microsoft\Windows\CurrentVersion\Run (zero shell interpolation, no Admin elevation).
func BuildRegAddRunArgs(spec LogonSpec) ([]string, error) {
	name, err := normalizeRunValueName(spec.Name)
	if err != nil {
		return nil, err
	}
	exe := strings.TrimSpace(spec.ExePath)
	if exe == "" {
		return nil, errors.New("executable path is required")
	}
	if err := rejectUnsafeRunArg("--exe", exe); err != nil {
		return nil, err
	}
	if err := rejectUnsafeRunArg("--config", spec.ConfigPath); err != nil {
		return nil, err
	}
	if err := rejectUnsafeRunArg("--cdp", spec.CDPURL); err != nil {
		return nil, err
	}

	gate, err := validateFallbackGate(spec.FallbackGate)
	if err != nil {
		return nil, err
	}

	// Always quote executable path in the HKCU Run REG_SZ command line so paths with spaces
	// cannot trigger Windows CreateProcessW unquoted-path ambiguity at user logon.
	runTokens := []string{`"` + exe + `"`, "tray", "--fallback-gate", gate}
	if cfg := strings.TrimSpace(spec.ConfigPath); cfg != "" {
		runTokens = append(runTokens, "--config", quoteIfSpaces(cfg))
	}
	if cdp := strings.TrimSpace(spec.CDPURL); cdp != "" {
		runTokens = append(runTokens, "--cdp", quoteIfSpaces(cdp))
	}

	cmdValue := strings.Join(runTokens, " ")
	return []string{"add", RunRegistryKey, "/v", name, "/t", "REG_SZ", "/d", cmdValue, "/f"}, nil
}

// BuildRegDeleteRunArgs constructs direct reg.exe delete arguments for HKCU\...\Run.
func BuildRegDeleteRunArgs(rawName string) ([]string, error) {
	name, err := normalizeRunValueName(rawName)
	if err != nil {
		return nil, err
	}
	return []string{"delete", RunRegistryKey, "/v", name, "/f"}, nil
}

// BuildRegQueryRunArgs constructs direct reg.exe query arguments for HKCU\...\Run.
func BuildRegQueryRunArgs(rawName string) ([]string, error) {
	name, err := normalizeRunValueName(rawName)
	if err != nil {
		return nil, err
	}
	return []string{"query", RunRegistryKey, "/v", name}, nil
}

// FormatRegCommand formats a preview reg.exe command line for --dry-run output.
func FormatRegCommand(args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, "reg.exe")
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

func normalizeRunValueName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		name = DefaultRunValueName
	}
	if !validRunValueNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid --name %q (allowed: letters, digits, '.', '_', '-')", raw)
	}
	return name, nil
}

func rejectUnsafeRunArg(flagName string, val string) error {
	if strings.ContainsAny(val, "\"\r\n\x00") {
		return fmt.Errorf("%s must not contain quotes or control characters", flagName)
	}
	return nil
}

func quoteIfSpaces(val string) string {
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
