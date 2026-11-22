package driver

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
)

// DriverStatus summarizes live target-driver readiness without triggering intrusive UI prompts.
type DriverStatus struct {
	Connected         bool
	Label             string
	BrowserStatus     string
	OpenTabsCount     int
	AvailableSessions []string
	LastError         string
}

// FallbackExecResult returns fresh scrubbed snapshot artifacts from a bounded Tier-2 fallback run.
type FallbackExecResult struct {
	StepResult    contract.StepResult
	ScreenshotPNG []byte
	DOMHTML       string
	AXTreeText    string
	ActionsRun    int
}

// TargetDriver is the Driven Port isolating declarative extraction from platform adapters
// (Browser CDP today; native desktop accessibility trees such as Windows UIA next).
type TargetDriver interface {
	Name() string
	ExecuteStep(ctx context.Context, step contract.Step) (contract.StepResult, error)
	ExecuteFallback(ctx context.Context, step contract.Step, plan contract.FallbackPlan) (FallbackExecResult, error)
	ProbeStatus(ctx context.Context) DriverStatus
	Close() error
}

// StepExecError carries error-only diagnostic artifacts captured strictly when extraction
// or expectation checks fail.
type StepExecError struct {
	StepID        string
	Partial       contract.StepResult
	Err           error
	ScreenshotPNG []byte
	DOMHTML       string
	AXTreeText    string
}

func (e *StepExecError) Error() string {
	return fmt.Sprintf("step %q failed: %v", e.StepID, e.Err)
}

func (e *StepExecError) Unwrap() error {
	return e.Err
}

// BuildDiagnosticZip scrubs captured HTML, enforces air-gap rules, and packages
// error.json, dom.html, and screenshot.png into an in-memory .zip archive.
func BuildDiagnosticZip(recipeName string, stepErr *StepExecError, extraForbidden []string) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	meta := map[string]any{
		"recipe":          recipeName,
		"step_id":         stepErr.StepID,
		"error":           stepErr.Err.Error(),
		"partial_result":  stepErr.Partial,
		"captured_at_utc": time.Now().UTC().Format(time.RFC3339),
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := contract.EnforceAirGap(string(metaBytes), extraForbidden); err != nil {
		return nil, err
	}
	if err := writeZipEntry(zw, "error.json", metaBytes); err != nil {
		return nil, err
	}

	if stepErr.DOMHTML != "" {
		scrubbed := contract.ScrubHTML(stepErr.DOMHTML)
		if err := contract.EnforceAirGap(scrubbed, extraForbidden); err != nil {
			return nil, err
		}
		if err := writeZipEntry(zw, "dom.html", []byte(scrubbed)); err != nil {
			return nil, err
		}
	}

	if stepErr.AXTreeText != "" {
		if err := contract.EnforceAirGap(stepErr.AXTreeText, extraForbidden); err != nil {
			return nil, err
		}
		if err := writeZipEntry(zw, "axtree.txt", []byte(stepErr.AXTreeText)); err != nil {
			return nil, err
		}
	}

	if len(stepErr.ScreenshotPNG) > 0 {
		if err := writeZipEntry(zw, "screenshot.png", stepErr.ScreenshotPNG); err != nil {
			return nil, err
		}
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SaveDiagnosticZip writes an error diagnostic bundle to dir and returns the saved path.
func SaveDiagnosticZip(dir string, topic string, zipBytes []byte) (string, error) {
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ts := time.Now().UTC().Format("20060102-150405Z")
	safeTopic := sanitizeFilename(topic)
	outPath := filepath.Join(dir, fmt.Sprintf("debug_%s_%s.zip", safeTopic, ts))
	if err := os.WriteFile(outPath, zipBytes, 0o600); err != nil {
		return "", err
	}
	return outPath, nil
}

func writeZipEntry(zw *zip.Writer, name string, data []byte) error {
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func sanitizeFilename(s string) string {
	var out []rune
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			out = append(out, r)
		} else {
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "drop"
	}
	return string(out)
}
