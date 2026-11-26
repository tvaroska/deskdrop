package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tvaroska/deskdrop/internal/bus"
	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/driver"
	"github.com/tvaroska/deskdrop/internal/driver/cdp"
)

const (
	menuIDConnectChrome = 1001
	menuIDSyncNow       = 1003
	menuIDToggleGate    = 1004
	menuIDUpdate        = 1005
	menuIDQuit          = 1009
	menuIDProfileBase   = 2000
	maxMenuProfiles     = 32
)

type trayAction struct {
	cmdID   int
	profile cdp.BrowserProfile
}

// trayCoordinator runs the non-blocking tray event loop across syncTicker, probeTicker, and actionCh,
// executing sync cycles and Tier-2 confirmation prompts asynchronously off workerLoop so modal
// confirmation dialogs never freeze background heartbeats or menu actions.
type trayCoordinator struct {
	cfgPath        string
	cdpURL         string
	interval       time.Duration
	probeInterval  time.Duration
	promptTimeout  time.Duration
	hbInterval     time.Duration
	stepTimeout    time.Duration
	retryBaseDelay time.Duration
	pool           *cdp.SessionPool

	mu              sync.Mutex
	browserStat     string
	lastAction      string
	selectedProfile string
	fallbackGate    string
	busy            bool
	connected       bool
	syncActive      bool
	activeFallbacks int

	promptMu sync.Mutex
	execMu   sync.Mutex
	actionCh chan trayAction
	stopCh   chan struct{}
	bgWG     sync.WaitGroup

	loadCfgFn       func(string) (contract.Config, error)
	makeDriverFn    func(string, *cdp.SessionPool, contract.Config) driver.TargetDriver
	approverFactory func(gate string) FallbackApprover
	onRefreshUI     func()
	onTryConnect    func(notifyOnSuccess bool)
	onRefreshStatus func()
	onSelectProfile func(prof cdp.BrowserProfile)
	onSelfUpdate    func(manual bool) bool
}

func newTrayCoordinator(cfgPath string, cdpURL string, interval time.Duration, gate string, selectedProfile string, pool *cdp.SessionPool) *trayCoordinator {
	if interval <= 0 {
		interval = 20 * time.Second
	}
	if gate == "" {
		gate = "auto"
	}
	return &trayCoordinator{
		cfgPath:         cfgPath,
		cdpURL:          cdpURL,
		interval:        interval,
		probeInterval:   6 * time.Second,
		promptTimeout:   defaultFallbackPromptTimeout,
		hbInterval:      defaultTaskHeartbeatInterval,
		stepTimeout:     defaultFallbackStepTimeout,
		pool:            pool,
		browserStat:     "Checking Chrome...",
		lastAction:      "Idle (ready)",
		selectedProfile: selectedProfile,
		fallbackGate:    gate,
		actionCh:        make(chan trayAction, 16),
		stopCh:          make(chan struct{}),
		loadCfgFn:       loadConfig,
		makeDriverFn:    makeTargetDriver,
	}
}

func (c *trayCoordinator) withPromptLock(fn func()) {
	c.promptMu.Lock()
	defer c.promptMu.Unlock()
	fn()
}

func (c *trayCoordinator) withDriverLock(fn func()) {
	c.execMu.Lock()
	defer c.execMu.Unlock()
	fn()
}

func (c *trayCoordinator) loadCfg(path string) (contract.Config, error) {
	if c.loadCfgFn != nil {
		return c.loadCfgFn(path)
	}
	return loadConfig(path)
}

func (c *trayCoordinator) makeDriver(name string, pool *cdp.SessionPool, cfg contract.Config) driver.TargetDriver {
	if c.makeDriverFn != nil {
		return c.makeDriverFn(name, pool, cfg)
	}
	return makeTargetDriver(name, pool, cfg)
}

func (c *trayCoordinator) setBusy(busy bool, action string) {
	c.mu.Lock()
	c.busy = busy || c.activeFallbacks > 0
	if action != "" {
		c.lastAction = action
	}
	c.mu.Unlock()
	if c.onRefreshUI != nil {
		c.onRefreshUI()
	}
}

func (c *trayCoordinator) snapshotState() (browserStat, lastAction, selectedProfile, fallbackGate string, busy, connected bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.browserStat, c.lastAction, c.selectedProfile, c.fallbackGate, c.busy, c.connected
}

func (c *trayCoordinator) hasActiveWork() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeFallbacks > 0 || c.syncActive
}

func (c *trayCoordinator) hasActiveFallback() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeFallbacks > 0
}

func (c *trayCoordinator) workerLoop() {
	stopCtx, stopCancel := context.WithCancel(context.Background())
	defer stopCancel()

	if c.onTryConnect != nil {
		c.onTryConnect(false)
	}
	c.startSyncCycle(stopCtx, false)

	syncInt := c.interval
	if syncInt <= 0 {
		syncInt = 20 * time.Second
	}
	probeInt := c.probeInterval
	if probeInt <= 0 {
		probeInt = 6 * time.Second
	}

	syncTicker := time.NewTicker(syncInt)
	defer syncTicker.Stop()
	probeTicker := time.NewTicker(probeInt)
	defer probeTicker.Stop()

	for {
		select {
		case <-c.stopCh:
			stopCancel()
			c.bgWG.Wait()
			return
		case act := <-c.actionCh:
			switch act.cmdID {
			case menuIDProfileBase:
				if c.onSelectProfile != nil {
					prof := act.profile
					c.bgWG.Add(1)
					go func() {
						defer c.bgWG.Done()
						c.onSelectProfile(prof)
					}()
				}
			case menuIDConnectChrome:
				c.bgWG.Add(1)
				go func() {
					defer c.bgWG.Done()
					if c.pool != nil {
						c.pool.ResetApprovalLatch()
					}
					if c.onTryConnect != nil {
						c.onTryConnect(true)
					}
				}()
			case menuIDSyncNow:
				c.startSyncCycle(stopCtx, true)
			case menuIDToggleGate:
				c.mu.Lock()
				if c.fallbackGate == "tray" {
					c.fallbackGate = "auto"
				} else {
					c.fallbackGate = "tray"
				}
				next := c.fallbackGate
				fbActive := c.activeFallbacks > 0
				c.mu.Unlock()
				c.setBusy(fbActive, "Fallback gate: "+next)
			case menuIDUpdate:
				if c.onSelfUpdate != nil {
					c.bgWG.Add(1)
					go func() {
						defer c.bgWG.Done()
						c.onSelfUpdate(true)
					}()
				}
			}
		case <-syncTicker.C:
			c.startSyncCycle(stopCtx, false)
		case <-probeTicker.C:
			if c.onRefreshStatus != nil {
				c.onRefreshStatus()
			}
		}
	}
}

func (c *trayCoordinator) startSyncCycle(stopCtx context.Context, manual bool) {
	c.mu.Lock()
	if c.syncActive {
		c.mu.Unlock()
		return
	}
	c.syncActive = true
	c.mu.Unlock()

	c.bgWG.Add(1)
	go func() {
		defer func() {
			c.mu.Lock()
			c.syncActive = false
			c.mu.Unlock()
			c.bgWG.Done()
		}()
		c.runSyncCycle(stopCtx, manual)
	}()
}

func (c *trayCoordinator) newServiceClient(cfg contract.Config) *bus.DesktopClient {
	svcClient := bus.NewDesktopClient(cfg)
	svcClient.ClientVersion = version
	if c.retryBaseDelay > 0 {
		svcClient.RetryBaseDelay = c.retryBaseDelay
	}
	return svcClient
}

func (c *trayCoordinator) runSyncCycle(stopCtx context.Context, manual bool) {
	cfg, err := c.loadCfg(c.cfgPath)
	if err != nil {
		c.setBusy(false, "Config error: "+err.Error())
		return
	}
	if cfg.ServiceURL == "" || cfg.ServiceToken == "" {
		return
	}
	if c.cdpURL != "" {
		cfg.CDPURL = c.cdpURL
	}
	c.mu.Lock()
	cfg.FallbackGate = c.fallbackGate
	c.mu.Unlock()

	svcClient := c.newServiceClient(cfg)
	if svcClient.ValidateAuth() != nil {
		return
	}
	if manual {
		c.setBusy(true, "Checking relay queue...")
	}

	drv := c.makeDriver(cfg.NormalizedDriver(), c.pool, cfg)
	defer drv.Close()

	dispatchFallback := func(task contract.Task, resolvedRecipe *contract.Recipe) bool {
		c.dispatchAsyncFallback(stopCtx, cfg, task, resolvedRecipe)
		return true
	}

	drained, updated, derr := runTraySyncStepWithDispatcher(
		stopCtx,
		drv,
		cfg,
		svcClient,
		resolveHostLabel(cfg),
		nil,
		func(_ string) bool {
			if c.hasActiveFallback() || c.onSelfUpdate == nil {
				return false
			}
			return c.onSelfUpdate(false)
		},
		dispatchFallback,
		c.withDriverLock,
	)
	if updated {
		return
	}
	if errors.Is(derr, errRelayRestarting) {
		c.setBusy(false, "Relay restarting")
		return
	}
	if c.onRefreshStatus != nil {
		c.onRefreshStatus()
	}
	stamp := time.Now().Format("15:04:05")
	if derr != nil {
		if errors.Is(derr, context.Canceled) {
			return
		}
		c.setBusy(false, fmt.Sprintf("Sync err (%s): %v", stamp, derr))
		return
	}
	if drained > 0 {
		if c.hasActiveFallback() {
			c.setBusy(true, fmt.Sprintf("Awaiting Tier-2 fallback approval (%s)", stamp))
		} else {
			c.setBusy(false, fmt.Sprintf("Synced %d task(s) at %s", drained, stamp))
		}
	} else if manual {
		c.setBusy(false, fmt.Sprintf("Queue empty (%s)", stamp))
	}
}

func (c *trayCoordinator) dispatchAsyncFallback(stopCtx context.Context, baseCfg contract.Config, task contract.Task, resolvedRecipe *contract.Recipe) {
	c.mu.Lock()
	c.activeFallbacks++
	gate := c.fallbackGate
	c.mu.Unlock()
	baseCfg.FallbackGate = gate
	c.setBusy(true, fmt.Sprintf("Tier-2 fallback prompt (%s)", task.Recipe))

	c.bgWG.Add(1)
	go func() {
		defer func() {
			c.mu.Lock()
			c.activeFallbacks--
			stillFallback := c.activeFallbacks > 0
			c.mu.Unlock()
			c.setBusy(stillFallback, fmt.Sprintf("Fallback %s finished at %s", task.Recipe, time.Now().Format("15:04:05")))
			if c.onRefreshStatus != nil {
				c.onRefreshStatus()
			}
			c.bgWG.Done()
		}()

		svcClient := c.newServiceClient(baseCfg)
		if svcClient.ValidateAuth() != nil {
			return
		}
		drv := c.makeDriver(baseCfg.NormalizedDriver(), c.pool, baseCfg)
		defer drv.Close()

		var approver FallbackApprover
		if c.approverFactory != nil {
			approver = c.approverFactory(strings.ToLower(strings.TrimSpace(gate)))
		}
		execFallbackTaskWithOptions(
			stopCtx,
			drv,
			baseCfg,
			svcClient,
			resolveHostLabel(baseCfg),
			task,
			resolvedRecipe,
			approver,
			fallbackExecOptions{
				HeartbeatInterval: c.hbInterval,
				PromptTimeout:     c.promptTimeout,
				StepTimeout:       c.stepTimeout,
				WrapPrompt:        c.withPromptLock,
				WrapDriverExec:    c.withDriverLock,
			},
		)
	}()
}
