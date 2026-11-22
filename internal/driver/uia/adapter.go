package uia

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/driver"
)

const DefaultMaxNodes = 2500

// WindowSummary describes a top-level native desktop window discovered via UI Automation.
type WindowSummary struct {
	Title        string
	ClassName    string
	AutomationID string
}

// TreeProvider is the platform Port supplying serialized AXNode trees from Windows UI Automation
// (or deterministic fixtures in offline cross-platform conformance tests).
type TreeProvider interface {
	Available() (bool, string)
	ListWindows(ctx context.Context) ([]WindowSummary, error)
	CaptureWindowTree(ctx context.Context, windowQuery string, maxNodes int) (nodes []contract.AXNode, matchedTitle string, err error)
	Close() error
}

// Driver adapts native Windows UI Automation (UIA) accessibility trees to driver.TargetDriver.
type Driver struct {
	Provider TreeProvider
	MaxNodes int
}

var _ driver.TargetDriver = (*Driver)(nil)

// NewDriver constructs the platform Windows UIA TargetDriver (or a clean stub on non-Windows hosts).
func NewDriver(_ contract.Config) *Driver {
	return NewDriverWithProvider(newPlatformProvider())
}

// NewDriverWithProvider constructs a UIA Driver with an explicit TreeProvider (used in offline tests).
func NewDriverWithProvider(p TreeProvider) *Driver {
	if p == nil {
		p = newPlatformProvider()
	}
	return &Driver{
		Provider: p,
		MaxNodes: DefaultMaxNodes,
	}
}

func (d *Driver) Name() string { return "uia" }

func (d *Driver) ExecuteStep(ctx context.Context, step contract.Step) (contract.StepResult, error) {
	timeout := 20 * time.Second
	if step.TimeoutMs > 0 {
		timeout = time.Duration(step.TimeoutMs) * time.Millisecond
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if step.WaitMs > 0 {
		select {
		case <-sctx.Done():
			return contract.StepResult{}, sctx.Err()
		case <-time.After(time.Duration(step.WaitMs) * time.Millisecond):
		}
	}

	query := resolveWindowQuery(step)
	maxNodes := d.MaxNodes
	if maxNodes <= 0 {
		maxNodes = DefaultMaxNodes
	}

	rawNodes, matchedTitle, err := d.Provider.CaptureWindowTree(sctx, query, maxNodes)
	if err != nil {
		return contract.StepResult{}, err
	}
	scrubbedNodes := contract.ScrubAXNodes(rawNodes)

	landedURI := strings.TrimSpace(step.URL)
	if landedURI == "" {
		if matchedTitle != "" {
			landedURI = "uia://window/" + matchedTitle
		} else if query != "" {
			landedURI = "uia://window/" + query
		} else {
			landedURI = "uia://desktop"
		}
	}

	res, evalErr := contract.ExtractStepFromAXNodes(step, scrubbedNodes, landedURI)
	if evalErr != nil {
		compactAX, _ := contract.FormatCompactAXTree(scrubbedNodes, 0)
		return contract.StepResult{}, &driver.StepExecError{
			StepID:     step.ID,
			Partial:    res,
			Err:        evalErr,
			AXTreeText: compactAX,
		}
	}
	if res.Title == "" && matchedTitle != "" {
		res.Title = matchedTitle
	}
	res.ReusedTab = true

	if valErr := contract.ValidateStepResult(step, res); valErr != nil {
		compactAX, _ := contract.FormatCompactAXTree(scrubbedNodes, 0)
		return contract.StepResult{}, &driver.StepExecError{
			StepID:     step.ID,
			Partial:    res,
			Err:        valErr,
			AXTreeText: compactAX,
		}
	}
	return res, nil
}

func (d *Driver) ExecuteFallback(_ context.Context, _ contract.Step, _ contract.FallbackPlan) (driver.FallbackExecResult, error) {
	return driver.FallbackExecResult{}, errors.New("uia driver is strictly read-only and does not execute interactive fallback actions")
}

func (d *Driver) ProbeStatus(ctx context.Context) driver.DriverStatus {
	ok, label := d.Provider.Available()
	if !ok {
		return driver.DriverStatus{
			Connected:     false,
			Label:         label,
			BrowserStatus: label,
		}
	}
	wins, err := d.Provider.ListWindows(ctx)
	if err != nil {
		return driver.DriverStatus{
			Connected:     false,
			Label:         "UIA Error",
			BrowserStatus: "Windows UIA enumeration failed: " + err.Error(),
			LastError:     err.Error(),
		}
	}
	titles := make([]string, 0, len(wins))
	for _, w := range wins {
		if w.Title != "" {
			titles = append(titles, w.Title)
			if len(titles) >= 25 {
				break
			}
		}
	}
	return driver.DriverStatus{
		Connected:         true,
		Label:             label,
		BrowserStatus:     fmt.Sprintf("Windows UIA ready (%d top-level windows)", len(wins)),
		OpenTabsCount:     len(wins),
		AvailableSessions: titles,
	}
}

func (d *Driver) Close() error {
	if d.Provider != nil {
		return d.Provider.Close()
	}
	return nil
}

func resolveWindowQuery(step contract.Step) string {
	if s := strings.TrimSpace(step.WindowTitle); s != "" {
		return s
	}
	if s := strings.TrimSpace(step.MatchTabURL); s != "" {
		return strings.TrimPrefix(strings.TrimPrefix(s, "uia://window/"), "uia://")
	}
	u := strings.TrimSpace(step.URL)
	if strings.HasPrefix(strings.ToLower(u), "uia://") {
		rest := u[len("uia://"):]
		rest = strings.TrimPrefix(rest, "window/")
		rest = strings.TrimPrefix(rest, "desktop/")
		return strings.Trim(rest, "/")
	}
	return ""
}

// StaticWindowFixture holds an in-memory desktop window accessibility tree for offline tests.
type StaticWindowFixture struct {
	Title        string
	ClassName    string
	AutomationID string
	Nodes        []contract.AXNode
}

// StaticTreeProvider implements TreeProvider over deterministic in-memory fixtures so Linux CI
// exercises the full UIA TargetDriver contract without Win32 COM syscalls.
type StaticTreeProvider struct {
	Windows []StaticWindowFixture
}

func (s *StaticTreeProvider) Available() (bool, string) {
	return true, "Connected (Static UIA Fixture)"
}

func (s *StaticTreeProvider) ListWindows(_ context.Context) ([]WindowSummary, error) {
	out := make([]WindowSummary, 0, len(s.Windows))
	for _, w := range s.Windows {
		out = append(out, WindowSummary{
			Title:        w.Title,
			ClassName:    w.ClassName,
			AutomationID: w.AutomationID,
		})
	}
	return out, nil
}

func (s *StaticTreeProvider) CaptureWindowTree(_ context.Context, windowQuery string, maxNodes int) ([]contract.AXNode, string, error) {
	q := strings.ToLower(strings.TrimSpace(windowQuery))
	for _, w := range s.Windows {
		if q == "" || q == "*" || q == "desktop" ||
			strings.Contains(strings.ToLower(w.Title), q) ||
			strings.Contains(strings.ToLower(w.AutomationID), q) ||
			strings.Contains(strings.ToLower(w.ClassName), q) {
			nodes := w.Nodes
			if maxNodes > 0 && len(nodes) > maxNodes {
				nodes = nodes[:maxNodes]
			}
			return nodes, w.Title, nil
		}
	}
	return nil, "", fmt.Errorf("no open desktop window matched %q", windowQuery)
}

func (s *StaticTreeProvider) Close() error { return nil }
