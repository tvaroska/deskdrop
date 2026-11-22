//go:build !windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
)

func defaultShowsTrayOnNoArgs() bool {
	return false
}

func runPlatformTray(cfgPath string, cdpURL string, interval time.Duration, fallbackGate string, _ bool) error {
	if interval <= 0 {
		interval = 20 * time.Second
	}
	args := []string{"--interval", interval.String()}
	if cfgPath != "" {
		args = append(args, "--config", cfgPath)
	}
	if cdpURL != "" {
		args = append(args, "--cdp", cdpURL)
	}
	if fallbackGate != "" {
		args = append(args, "--fallback-gate", fallbackGate)
	}
	fmt.Printf("Running persistent deskdrop worker (interval=%s)...\n", interval)
	return cmdSync(args)
}

// confirmFallbackPlan asks for local confirmation when --fallback-gate=tray on non-Windows.
// Fails closed if stdin is not an interactive terminal.
func confirmFallbackPlan(plan contract.FallbackPlan) (bool, error) {
	fi, err := os.Stdin.Stat()
	if err != nil || (fi.Mode()&os.ModeCharDevice) == 0 {
		return false, nil
	}
	fmt.Fprintf(os.Stderr, "\n[DeskDrop Tier-2 Fallback Approval]\nDomain: %s\nURL: %s\nReason: %s\nActions (%d):\n",
		plan.AllowedDomain, plan.TargetURL, plan.Reason, len(plan.Actions))
	for i, a := range plan.Actions {
		fmt.Fprintf(os.Stderr, "  %d. %s selector=%q delta_y=%d wait_ms=%d\n", i+1, a.Action, a.Selector, a.DeltaY, a.WaitMs)
	}
	fmt.Fprint(os.Stderr, "Allow bounded stateless fallback? [y/N]: ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}
