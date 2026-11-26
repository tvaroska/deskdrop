package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tvaroska/deskdrop/internal/bus"
	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/driver"
	"github.com/tvaroska/deskdrop/internal/driver/uia"
)

const (
	defaultTaskHeartbeatInterval = 30 * time.Second
	defaultFallbackPromptTimeout = 3 * time.Minute
	defaultFallbackStepTimeout   = 60 * time.Second
)

// FallbackDispatcher schedules a claimed Tier-2 fallback_task off the main drain loop (used by tray mode).
type FallbackDispatcher func(task contract.Task, resolvedRecipe *contract.Recipe) bool

type fallbackExecOptions struct {
	HeartbeatInterval time.Duration
	PromptTimeout     time.Duration
	StepTimeout       time.Duration
	WrapPrompt        func(fn func())
	WrapDriverExec    func(fn func())
}

var makeTargetDriver = newTargetDriver

func uiaDriverAvailable(cfg contract.Config) bool {
	alt := makeTargetDriver("uia", nil, cfg)
	defer alt.Close()
	if uiaDrv, ok := alt.(*uia.Driver); ok && uiaDrv.Provider != nil {
		avail, _ := uiaDrv.Provider.Available()
		return avail
	}
	return alt.ProbeStatus(context.Background()).Connected
}

func resolveRunnableDrivers(primaryDriverName string, primaryConnected bool, uiaAvailable bool) []string {
	switch strings.ToLower(strings.TrimSpace(primaryDriverName)) {
	case "uia":
		if primaryConnected {
			return []string{"uia"}
		}
		return nil
	case "cdp":
		if primaryConnected {
			if uiaAvailable {
				return []string{"cdp", "uia"}
			}
			return []string{"cdp"}
		}
		if uiaAvailable {
			return []string{"uia"}
		}
		return nil
	default:
		if primaryConnected {
			return nil
		}
		if uiaAvailable {
			return []string{"uia"}
		}
		return nil
	}
}

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
	return drainQueueWithDispatcher(ctx, drv, cfg, svcClient, hostLabel, remaining, hasCap, approver, nil, nil)
}

func drainQueueWithDispatcher(
	ctx context.Context,
	drv driver.TargetDriver,
	cfg contract.Config,
	svcClient *bus.DesktopClient,
	hostLabel string,
	remaining int,
	hasCap bool,
	approver FallbackApprover,
	dispatchFallback FallbackDispatcher,
	wrapDriverExec func(fn func()),
) (int, error) {
	ws := collectWorkerStatus(ctx, drv, cfg, hostLabel, false, "sync", "")
	_ = svcClient.ReportWorkerStatus(ctx, ws)

	uiaAvail := strings.EqualFold(drv.Name(), "uia") && ws.BrowserConnected
	if !strings.EqualFold(drv.Name(), "uia") {
		uiaAvail = uiaDriverAvailable(cfg)
	}
	allowedDrivers := resolveRunnableDrivers(drv.Name(), ws.BrowserConnected, uiaAvail)
	if !ws.BrowserConnected && len(allowedDrivers) == 0 {
		return 0, errors.New(ws.BrowserStatus)
	}

	count := 0
	for {
		if hasCap && count >= remaining {
			break
		}
		claimResp, err := svcClient.ClaimNextTask(ctx, hostLabel, 90, &ws, allowedDrivers...)
		if err != nil {
			return count, err
		}
		if claimResp.Task == nil {
			break
		}
		task := *claimResp.Task
		count++
		fmt.Printf("Claimed task %s (kind=%s, recipe=%s, attempt=%d/%d)\n", task.ID, task.Kind, task.Recipe, task.Attempt, task.MaxAttempts)

		if task.Kind == contract.TaskKindFallback {
			if dispatchFallback != nil && dispatchFallback(task, claimResp.ResolvedRecipe) {
				continue
			}
			execFallbackTaskWithOptions(ctx, drv, cfg, svcClient, hostLabel, task, claimResp.ResolvedRecipe, approver, fallbackExecOptions{
				WrapDriverExec: wrapDriverExec,
			})
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
		execExtractTaskWithWrap(ctx, drv, cfg, svcClient, hostLabel, task, *claimResp.ResolvedRecipe, wrapDriverExec)
	}
	if count == 0 && !ws.BrowserConnected {
		return 0, errors.New(ws.BrowserStatus)
	}
	return count, nil
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
	execExtractTaskWithWrap(parentCtx, drv, cfg, svcClient, hostLabel, task, recipe, nil)
}

func execExtractTaskWithWrap(
	parentCtx context.Context,
	drv driver.TargetDriver,
	cfg contract.Config,
	svcClient *bus.DesktopClient,
	hostLabel string,
	task contract.Task,
	recipe contract.Recipe,
	wrapDriverExec func(fn func()),
) {
	stepCtx, cancel := context.WithTimeout(parentCtx, 120*time.Second)
	defer cancel()

	execDrv := drv
	if recipe.Driver != "" && !strings.EqualFold(recipe.NormalizedDriver(), drv.Name()) {
		altDrv := makeTargetDriver(recipe.NormalizedDriver(), nil, cfg)
		defer altDrv.Close()
		execDrv = altDrv
	}

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
		var (
			res     contract.StepResult
			stepErr error
		)
		runStep := func() {
			res, stepErr = execDrv.ExecuteStep(stepCtx, step)
		}
		if wrapDriverExec != nil {
			wrapDriverExec(runStep)
		} else {
			runStep()
		}
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
	execFallbackTaskWithOptions(parentCtx, drv, cfg, svcClient, hostLabel, task, resolvedRecipe, approver, fallbackExecOptions{})
}

type approvalOutcome struct {
	ok  bool
	err error
}

func awaitFallbackApproval(parentCtx context.Context, promptTimeout time.Duration, approver FallbackApprover, plan contract.FallbackPlan) (bool, error) {
	if promptTimeout <= 0 {
		promptTimeout = defaultFallbackPromptTimeout
	}
	pctx, cancel := context.WithTimeout(parentCtx, promptTimeout)
	defer cancel()

	resCh := make(chan approvalOutcome, 1)
	go func() {
		ok, err := approver(plan)
		resCh <- approvalOutcome{ok: ok, err: err}
	}()

	select {
	case out := <-resCh:
		return out.ok, out.err
	case <-pctx.Done():
		if parentCtx.Err() != nil {
			return false, parentCtx.Err()
		}
		return false, fmt.Errorf("approval prompt timed out after %s", promptTimeout)
	}
}

func execFallbackTaskWithOptions(
	parentCtx context.Context,
	drv driver.TargetDriver,
	cfg contract.Config,
	svcClient *bus.DesktopClient,
	hostLabel string,
	task contract.Task,
	resolvedRecipe *contract.Recipe,
	approver FallbackApprover,
	opts fallbackExecOptions,
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

	// Start task lease heartbeat BEFORE blocking on human prompt approval so DefaultLease (90s)
	// never expires a MaxAttempts=1 fallback_task into dead_letter while awaiting confirmation.
	hbInterval := opts.HeartbeatInterval
	if hbInterval <= 0 {
		hbInterval = defaultTaskHeartbeatInterval
	}
	hbCtx, cancelHB := context.WithCancel(parentCtx)
	hbDone := startTaskHeartbeatInterval(hbCtx, svcClient, task.ID, hostLabel, hbInterval)
	var stopOnce sync.Once
	stopHB := func() {
		stopOnce.Do(func() {
			cancelHB()
			<-hbDone
		})
	}
	defer stopHB()

	promptTimeout := opts.PromptTimeout
	if promptTimeout <= 0 {
		promptTimeout = defaultFallbackPromptTimeout
	}

	var (
		ok   bool
		aerr error
	)
	runPrompt := func() {
		if approver != nil {
			ok, aerr = awaitFallbackApproval(parentCtx, promptTimeout, approver, plan)
		} else if strings.EqualFold(cfg.NormalizedFallbackGate(), "tray") {
			ok, aerr = confirmFallbackPlanWithTimeout(parentCtx, promptTimeout, plan)
		} else {
			ok = true
		}
	}
	if opts.WrapPrompt != nil {
		opts.WrapPrompt(runPrompt)
	} else {
		runPrompt()
	}

	if aerr != nil || !ok {
		if parentCtx.Err() != nil {
			return
		}
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

	stepTimeout := opts.StepTimeout
	if stepTimeout <= 0 {
		stepTimeout = defaultFallbackStepTimeout
	}

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

	var (
		fbRes driver.FallbackExecResult
		ferr  error
	)
	runFallback := func() {
		stepCtx, stepCancel := context.WithTimeout(parentCtx, stepTimeout)
		defer stepCancel()
		fbRes, ferr = drv.ExecuteFallback(stepCtx, step, plan)
	}
	if opts.WrapDriverExec != nil {
		opts.WrapDriverExec(runFallback)
	} else {
		runFallback()
	}

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
		AXTreeText:    fbRes.AXTreeText,
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
	if dropID == "" && propID == "" {
		fmt.Printf("Submitted fallback snapshot for %s (async healing queued)\n", task.ID)
		return
	}
	fmt.Printf("Submitted fallback snapshot for %s (drop=%s, proposal=%s)\n", task.ID, dropID, propID)
}

func startTaskHeartbeat(ctx context.Context, svcClient *bus.DesktopClient, taskID string, hostLabel string) <-chan struct{} {
	return startTaskHeartbeatInterval(ctx, svcClient, taskID, hostLabel, defaultTaskHeartbeatInterval)
}

func startTaskHeartbeatInterval(ctx context.Context, svcClient *bus.DesktopClient, taskID string, hostLabel string, interval time.Duration) <-chan struct{} {
	if interval <= 0 {
		interval = defaultTaskHeartbeatInterval
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
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

var errRelayRestarting = errors.New("relay restarting")

// runTraySyncStep executes one tray sync tick: checks /v1/status first and skips drainQueue
// (0 heartbeat/claim calls) whenever the relay is mid-rollout (transport error or 502/503/504).
func runTraySyncStep(
	ctx context.Context,
	drv driver.TargetDriver,
	cfg contract.Config,
	svcClient *bus.DesktopClient,
	hostLabel string,
	approver FallbackApprover,
	onNewerVersion func(remoteVer string) bool,
) (drained int, updated bool, err error) {
	return runTraySyncStepWithDispatcher(ctx, drv, cfg, svcClient, hostLabel, approver, onNewerVersion, nil, nil)
}

func runTraySyncStepWithDispatcher(
	ctx context.Context,
	drv driver.TargetDriver,
	cfg contract.Config,
	svcClient *bus.DesktopClient,
	hostLabel string,
	approver FallbackApprover,
	onNewerVersion func(remoteVer string) bool,
	dispatchFallback FallbackDispatcher,
	wrapDriverExec func(fn func()),
) (drained int, updated bool, err error) {
	remoteStatus, serr := svcClient.GetDesktopStatus(ctx)
	if serr != nil {
		if bus.IsRelayTransientError(serr) {
			return 0, false, errRelayRestarting
		}
	} else if srvVer, _ := remoteStatus["server_version"].(string); isNewerVersion(srvVer, version) && onNewerVersion != nil {
		if onNewerVersion(srvVer) {
			return 0, true, nil
		}
	}

	drained, derr := drainQueueWithDispatcher(ctx, drv, cfg, svcClient, hostLabel, 0, false, approver, dispatchFallback, wrapDriverExec)
	return drained, false, derr
}

func performSelfUpdate(ctx context.Context, svcClient *bus.DesktopClient) (string, error) {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return "", fmt.Errorf("cannot resolve executable path: %w", err)
	}
	return performSelfUpdateTo(ctx, svcClient, exe, runtime.GOOS, 512*1024)
}

func performSelfUpdateTo(ctx context.Context, svcClient *bus.DesktopClient, exe string, goos string, minBytes int) (string, error) {
	asset := "deskdrop"
	if goos == "windows" {
		asset = "deskdrop.exe"
	}
	sumsRaw, err := svcClient.DownloadReleaseChecksums(ctx)
	if err != nil {
		return "", fmt.Errorf("fetch release SHA256SUMS.txt failed: %w", err)
	}
	expectedHex, err := parseSHA256Sum(sumsRaw, asset)
	if err != nil {
		return "", fmt.Errorf("parse SHA256SUMS.txt for %s failed: %w", asset, err)
	}

	data, remoteVer, err := svcClient.DownloadReleaseBinary(ctx, goos)
	if err != nil {
		return "", err
	}
	if len(data) < minBytes {
		return "", fmt.Errorf("downloaded binary too small (%d bytes)", len(data))
	}
	if goos == "windows" && (len(data) < 2 || data[0] != 'M' || data[1] != 'Z') {
		return "", errors.New("downloaded binary is not a valid Windows PE executable")
	}

	sum := sha256.Sum256(data)
	actualHex := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(actualHex), []byte(expectedHex)) != 1 {
		return "", fmt.Errorf("SHA-256 mismatch for %s: got %s, want %s", asset, actualHex, expectedHex)
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

func parseSHA256Sum(raw string, asset string) (string, error) {
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		digest := strings.ToLower(strings.TrimSpace(fields[0]))
		name := strings.TrimPrefix(strings.TrimSpace(fields[1]), "*")
		if filepath.Base(name) != asset {
			continue
		}
		if len(digest) != 64 {
			return "", fmt.Errorf("invalid sha256 digest length %d for %s", len(digest), asset)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return "", fmt.Errorf("invalid sha256 hex digest for %s: %w", asset, err)
		}
		return digest, nil
	}
	return "", fmt.Errorf("no SHA-256 entry found for %q", asset)
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
