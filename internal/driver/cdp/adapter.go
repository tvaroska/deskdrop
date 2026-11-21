package cdp

import (
	"context"
	"fmt"

	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/driver"
)

// Driver adapts SessionPool + CDP Client to the driver.TargetDriver Port.
type Driver struct {
	Pool        *SessionPool
	Browser     string
	CDPURL      string
	UserDataDir string
	Session     string
	ownsPool    bool
}

var _ driver.TargetDriver = (*Driver)(nil)

// NewDriver creates a CDP TargetDriver backed by pool (or a new SessionPool if nil).
func NewDriver(pool *SessionPool, cfg contract.Config) *Driver {
	owns := false
	if pool == nil {
		pool = NewSessionPool()
		owns = true
	}
	return &Driver{
		Pool:        pool,
		Browser:     cfg.Browser,
		CDPURL:      cfg.CDPURL,
		UserDataDir: cfg.UserDataDir,
		Session:     cfg.PreferredChromeSession(),
		ownsPool:    owns,
	}
}

func (d *Driver) Name() string { return "cdp" }

func (d *Driver) ExecuteStep(ctx context.Context, step contract.Step) (contract.StepResult, error) {
	client, err := d.Pool.Acquire(ctx, d.CDPURL, d.UserDataDir)
	if err != nil {
		return contract.StepResult{}, err
	}
	client.SetPreferredSession(d.Browser, d.UserDataDir, d.Session)
	return client.ExecuteStep(ctx, step)
}

func (d *Driver) ExecuteFallback(ctx context.Context, step contract.Step, plan contract.FallbackPlan) (driver.FallbackExecResult, error) {
	client, err := d.Pool.Acquire(ctx, d.CDPURL, d.UserDataDir)
	if err != nil {
		return driver.FallbackExecResult{}, err
	}
	client.SetPreferredSession(d.Browser, d.UserDataDir, d.Session)
	return client.ExecuteFallback(ctx, step, plan)
}

// ProbeStatus checks driver connectivity quietly (never triggering unsolicited Chrome 144+ popups).
func (d *Driver) ProbeStatus(ctx context.Context) driver.DriverStatus {
	profiles := ListBrowserProfiles(d.Browser, d.UserDataDir)
	avail := make([]string, 0, len(profiles))
	for _, pr := range profiles {
		lbl := pr.DisplayName
		if lbl == "" {
			lbl = pr.DirName
		}
		avail = append(avail, lbl)
	}

	conn, label := d.Pool.ProbeQuiet(ctx, d.CDPURL)
	if !conn {
		return driver.DriverStatus{
			Connected:         false,
			Label:             label,
			BrowserStatus:     label,
			AvailableSessions: avail,
		}
	}

	client := d.Pool.CurrentClient()
	if client == nil {
		return driver.DriverStatus{
			Connected:         false,
			Label:             "Browser Not Connected",
			BrowserStatus:     "Browser Not Connected",
			AvailableSessions: avail,
		}
	}
	client.SetPreferredSession(d.Browser, d.UserDataDir, d.Session)
	pages, err := client.ListPages(ctx)
	if err != nil {
		return driver.DriverStatus{
			Connected:         false,
			Label:             "CDP Error",
			BrowserStatus:     "CDP page list failed: " + err.Error(),
			AvailableSessions: avail,
			LastError:         err.Error(),
		}
	}

	bStat := fmt.Sprintf("Chrome connected (%d open tabs)", len(pages))
	if d.Session != "" {
		bStat = fmt.Sprintf("Chrome connected (%d open tabs, session=%s)", len(pages), d.Session)
	}
	return driver.DriverStatus{
		Connected:         true,
		Label:             label,
		BrowserStatus:     bStat,
		OpenTabsCount:     len(pages),
		AvailableSessions: avail,
	}
}

func (d *Driver) Close() error {
	if d.ownsPool && d.Pool != nil {
		d.Pool.Close()
	}
	return nil
}
