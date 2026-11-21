package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/tvaroska/deskdrop/internal/bus"
	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/driver"
)

// FallbackApprover decides whether a bounded Tier-2 fallback_task is approved to run on the desktop.
type FallbackApprover func(plan contract.FallbackPlan) (bool, error)

func makeFallbackApprover(gate string) FallbackApprover {
	if strings.EqualFold(strings.TrimSpace(gate), "tray") {
		return confirmFallbackPlan
	}
	return func(_ contract.FallbackPlan) (bool, error) {
		return true, nil
	}
}

func collectWorkerStatus(ctx context.Context, drv driver.TargetDriver, cfg contract.Config, hostLabel string, busy bool, lastAction string, lastErr string) contract.WorkerStatus {
	hostname, _ := os.Hostname()
	if hostLabel == "" {
		hostLabel = resolveHostLabel(cfg)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	st := drv.ProbeStatus(probeCtx)
	errMsg := lastErr
	if errMsg == "" {
		errMsg = st.LastError
	}
	return contract.WorkerStatus{
		Host:              hostLabel,
		Hostname:          hostname,
		Version:           version,
		OS:                runtime.GOOS,
		Arch:              runtime.GOARCH,
		BrowserConnected:  st.Connected,
		BrowserStatus:     st.BrowserStatus,
		ReadyToAccept:     st.Connected && !busy,
		Busy:              busy,
		FallbackGate:      cfg.NormalizedFallbackGate(),
		ChromeSession:     cfg.PreferredChromeSession(),
		AvailableSessions: st.AvailableSessions,
		OpenTabsCount:     st.OpenTabsCount,
		LastAction:        lastAction,
		LastError:         errMsg,
	}
}

func drainQueue(
	ctx context.Context,
	drv driver.TargetDriver,
	cfg contract.Config,
	svcClient *bus.DesktopClient,
	hostLabel string,
	remaining int,
	hasCap bool,
	approver FallbackApprover,
) (int, error) {
	ws := collectWorkerStatus(ctx, drv, cfg, hostLabel, false, "sync", "")
	_ = svcClient.ReportWorkerStatus(ctx, ws)
	if !ws.BrowserConnected {
		return 0, errors.New(ws.BrowserStatus)
	}

	count := 0
	for {
		if hasCap && count >= remaining {
			return count, nil
		}
		claimResp, err := svcClient.ClaimNextTask(ctx, hostLabel, 90, &ws)
		if err != nil {
			return count, err
		}
		if claimResp.Task == nil {
			return count, nil
		}
		task := *claimResp.Task
		count++
		fmt.Printf("Claimed task %s (kind=%s, recipe=%s, attempt=%d/%d)\n", task.ID, task.Kind, task.Recipe, task.Attempt, task.MaxAttempts)

		if task.Kind == contract.TaskKindFallback {
			execFallbackTask(ctx, drv, cfg, svcClient, hostLabel, task, claimResp.ResolvedRecipe, approver)
			continue
		}

		if claimResp.ResolvedRecipe == nil {
			_, _, _ = svcClient.FailTask(ctx, task.ID, contract.FailReportMetadata{
				From:      hostLabel,
				Recipe:    task.Recipe,
				Domain:    task.Domain,
				ErrorCode: contract.ErrorCodeContractInvalid,
				Error:     "relay returned no resolved recipe for task",
			}, nil)
			continue
		}
		execExtractTask(ctx, drv, cfg, svcClient, hostLabel, task, *claimResp.ResolvedRecipe)
	}
}

func execExtractTask(
	parentCtx context.Context,
	drv driver.TargetDriver,
	cfg contract.Config,
	svcClient *bus.DesktopClient,
	hostLabel string,
	task contract.Task,
	recipe contract.Recipe,
) {
	stepCtx, cancel := context.WithTimeout(parentCtx, 120*time.Second)
	defer cancel()

	hbDone := startTaskHeartbeat(stepCtx, svcClient, task.ID, hostLabel)

	hostname, _ := os.Hostname()
	report := contract.DropReport{
		SchemaVersion: "2.0",
		Mode:          contract.DropModeDeterministic,
		Recipe:        recipe.Name,
		Topic:         recipe.Topic,
		Host:          hostname,
		CapturedAtUTC: time.Now().UTC().Format(time.RFC3339),
	}

	for _, step := range recipe.Steps {
		res, stepErr := drv.ExecuteStep(stepCtx, step)
		if stepErr != nil {
			cancel()
			<-hbDone
			var zipBytes []byte
			landedURL := ""
			var execErr *driver.StepExecError
			if errors.As(stepErr, &execErr) {
				landedURL = execErr.Partial.URL
				if cfg.ShouldSendErrorBundle() {
					zipBytes, _ = driver.BuildDiagnosticZip(recipe.Name, execErr, cfg.ForbiddenTerms)
					if len(zipBytes) > 0 && cfg.DebugDir != "" {
						_, _ = driver.SaveDiagnosticZip(cfg.DebugDir, recipe.Topic, zipBytes)
					}
				}
			}
			domain := task.Domain
			if domain == "" {
				domain = contract.InferRecipeDomain(recipe)
			}
			triage, diagID, ferr := svcClient.FailTask(parentCtx, task.ID, contract.FailReportMetadata{
				From:      hostLabel,
				Recipe:    recipe.Name,
				Topic:     recipe.Topic,
				StepID:    step.ID,
				LandedURL: landedURL,
				Domain:    domain,
				Error:     stepErr.Error(),
			}, zipBytes)
			if ferr != nil {
				fmt.Fprintf(os.Stderr, "Task %s fail report error: %v\n", task.ID, ferr)
			} else {
				fmt.Fprintf(os.Stderr, "Task %s triaged as %s (%s, diag=%s)\n", task.ID, triage.Code, triage.Action, diagID)
			}
			return
		}
		report.Steps = append(report.Steps, res)
	}

	cancel()
	<-hbDone

	sent, cerr := svcClient.CompleteTask(parentCtx, task.ID, task.NotifyTo, report)
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "Task %s complete upload failed: %v\n", task.ID, cerr)
		return
	}
	fmt.Printf("Completed task %s -> drop %s [bus:%s]\n", task.ID, sent.Attachment, sent.ShortID)
}

func execFallbackTask(
	parentCtx context.Context,
	drv driver.TargetDriver,
	cfg contract.Config,
	svcClient *bus.DesktopClient,
	hostLabel string,
	task contract.Task,
	resolvedRecipe *contract.Recipe,
	approver FallbackApprover,
) {
	if task.FallbackPlan == nil {
		_, _, _ = svcClient.FailTask(parentCtx, task.ID, contract.FailReportMetadata{
			From:      hostLabel,
			Recipe:    task.Recipe,
			Domain:    task.Domain,
			ErrorCode: contract.ErrorCodeContractInvalid,
			Error:     "fallback_task missing fallback_plan",
		}, nil)
		return
	}
	plan := *task.FallbackPlan
	var allowedDomains []string
	if resolvedRecipe != nil {
		allowedDomains = contract.InferRecipeDomains(*resolvedRecipe)
	}
	if err := contract.ValidateFallbackPlan(plan, allowedDomains); err != nil {
		_, _, _ = svcClient.FailTask(parentCtx, task.ID, contract.FailReportMetadata{
			From:      hostLabel,
			Recipe:    task.Recipe,
			Domain:    task.Domain,
			ErrorCode: contract.ErrorCodeContractInvalid,
			Error:     err.Error(),
		}, nil)
		return
	}

	if approver == nil {
		approver = makeFallbackApprover(cfg.NormalizedFallbackGate())
	}
	ok, aerr := approver(plan)
	if aerr != nil || !ok {
		reason := "fallback declined by desktop fallback-gate=tray"
		if aerr != nil {
			reason += ": " + aerr.Error()
		}
		_, _, _ = svcClient.FailTask(parentCtx, task.ID, contract.FailReportMetadata{
			From:      hostLabel,
			Recipe:    task.Recipe,
			Domain:    task.Domain,
			LandedURL: plan.TargetURL,
			ErrorCode: contract.ErrorCodeFallbackDeclined,
			Error:     reason,
		}, nil)
		return
	}

	stepCtx, cancel := context.WithTimeout(parentCtx, 60*time.Second)
	defer cancel()
	hbDone := startTaskHeartbeat(stepCtx, svcClient, task.ID, hostLabel)

	var step contract.Step
	if resolvedRecipe != nil && len(resolvedRecipe.Steps) > 0 {
		step = resolvedRecipe.Steps[0]
		for _, candidate := range resolvedRecipe.Steps {
			if candidate.ID == plan.StepID {
				step = candidate
				break
			}
		}
	} else {
		step = contract.Step{ID: plan.StepID, URL: plan.TargetURL}
	}

	fbRes, ferr := drv.ExecuteFallback(stepCtx, step, plan)
	cancel()
	<-hbDone

	if ferr != nil {
		_, _, _ = svcClient.FailTask(parentCtx, task.ID, contract.FailReportMetadata{
			From:      hostLabel,
			Recipe:    task.Recipe,
			Domain:    task.Domain,
			LandedURL: plan.TargetURL,
			Error:     ferr.Error(),
		}, nil)
		return
	}

	// If deterministic expectations now pass after unblocking, complete directly.
	if resolvedRecipe != nil && contract.ValidateStepResult(step, fbRes.StepResult) == nil {
		hostname, _ := os.Hostname()
		report := contract.DropReport{
			SchemaVersion: "2.0",
			Mode:          contract.DropModeComputerUseFallback,
			Recipe:        resolvedRecipe.Name,
			Topic:         resolvedRecipe.Topic,
			Host:          hostname,
			CapturedAtUTC: time.Now().UTC().Format(time.RFC3339),
			Steps:         []contract.StepResult{fbRes.StepResult},
		}
		_, _ = svcClient.CompleteTask(parentCtx, task.ID, task.NotifyTo, report)
		return
	}

	// Otherwise upload the fresh scrubbed snapshot for Tier-1 offline extraction + proposal drafting.
	zipBytes, zerr := driver.BuildDiagnosticZip(task.Recipe, &driver.StepExecError{
		StepID:        step.ID,
		Partial:       fbRes.StepResult,
		Err:           errors.New("tier-2 fallback snapshot"),
		ScreenshotPNG: fbRes.ScreenshotPNG,
		DOMHTML:       fbRes.DOMHTML,
	}, cfg.ForbiddenTerms)
	if zerr != nil {
		_, _, _ = svcClient.FailTask(parentCtx, task.ID, contract.FailReportMetadata{
			From:      hostLabel,
			Recipe:    task.Recipe,
			Domain:    task.Domain,
			LandedURL: fbRes.StepResult.URL,
			Error:     zerr.Error(),
		}, nil)
		return
	}
	dropID, propID, serr := svcClient.SubmitFallbackResult(parentCtx, task.ID, contract.FailReportMetadata{
		From:      hostLabel,
		Recipe:    task.Recipe,
		Topic:     task.Recipe,
		StepID:    step.ID,
		LandedURL: fbRes.StepResult.URL,
		Domain:    task.Domain,
		Error:     "tier-2 fallback snapshot ready",
	}, zipBytes)
	if serr != nil {
		fmt.Fprintf(os.Stderr, "Fallback task %s upload failed: %v\n", task.ID, serr)
		return
	}
	fmt.Printf("Submitted fallback snapshot for %s (drop=%s, proposal=%s)\n", task.ID, dropID, propID)
}

func startTaskHeartbeat(ctx context.Context, svcClient *bus.DesktopClient, taskID string, hostLabel string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = svcClient.HeartbeatTask(ctx, taskID, hostLabel, 90)
			}
		}
	}()
	return done
}

func performSelfUpdate(ctx context.Context, svcClient *bus.DesktopClient) (string, error) {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return "", fmt.Errorf("cannot resolve executable path: %w", err)
	}
	data, remoteVer, err := svcClient.DownloadReleaseBinary(ctx, runtime.GOOS)
	if err != nil {
		return "", err
	}
	if len(data) < 512*1024 {
		return "", fmt.Errorf("downloaded binary too small (%d bytes)", len(data))
	}
	if runtime.GOOS == "windows" && (data[0] != 'M' || data[1] != 'Z') {
		return "", errors.New("downloaded binary is not a valid Windows PE executable")
	}

	tmpPath := exe + ".new"
	oldPath := exe + ".old"
	_ = os.Remove(tmpPath)
	_ = os.Remove(oldPath)

	if err := os.WriteFile(tmpPath, data, 0o755); err != nil {
		return "", fmt.Errorf("write %s failed: %w", tmpPath, err)
	}
	_ = os.Remove(tmpPath + ":Zone.Identifier")

	if err := os.Rename(exe, oldPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("rename current executable failed: %w", err)
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		_ = os.Rename(oldPath, exe)
		return "", fmt.Errorf("activate updated executable failed: %w", err)
	}
	_ = os.Remove(exe + ":Zone.Identifier")
	_ = os.Remove(oldPath)
	if remoteVer == "" {
		remoteVer = "latest"
	}
	return remoteVer, nil
}

func isNewerVersion(remoteVer, localVer string) bool {
	r := parseSemverParts(remoteVer)
	l := parseSemverParts(localVer)
	if len(r) != 3 || len(l) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if r[i] > l[i] {
			return true
		}
		if r[i] < l[i] {
			return false
		}
	}
	return false
}

func parseSemverParts(v string) []int {
	s := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(v)), "v")
	s = strings.SplitN(s, "-", 2)[0]
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}

func spawnDetachedSelf(args []string) error {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return err
	}
	cmd := exec.Command(exe, args...)
	return cmd.Start()
}
