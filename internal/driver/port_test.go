package driver

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tvaroska/deskdrop/internal/contract"
)

func TestStepExecErrorAndDiagnosticZipBuildAndSave(t *testing.T) {
	inner := errors.New("selector .missing timed out")
	stepErr := &StepExecError{
		StepID: "overview",
		Partial: contract.StepResult{
			StepID: "overview",
			URL:    "https://status.example.com/overview",
		},
		Err:           inner,
		DOMHTML:       `<html><head><script>bad()</script></head><body><input type="password" value="pw123"><p>Visible</p></body></html>`,
		AXTreeText:    "[RootWebArea] \"Status\"\n  [heading] \"Visible\"",
		ScreenshotPNG: []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00},
	}

	if !errors.Is(stepErr, inner) {
		t.Fatal("StepExecError.Unwrap did not match inner error")
	}
	if !strings.Contains(stepErr.Error(), "overview") || !strings.Contains(stepErr.Error(), inner.Error()) {
		t.Fatalf("unexpected StepExecError.Error(): %q", stepErr.Error())
	}

	zipBytes, err := BuildDiagnosticZip("status-board", stepErr, []string{"forbidden-secret"})
	if err != nil {
		t.Fatalf("BuildDiagnosticZip failed: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("zip.NewReader failed: %v", err)
	}
	gotEntries := make(map[string]string)
	for _, f := range zr.File {
		rc, oerr := f.Open()
		if oerr != nil {
			t.Fatal(oerr)
		}
		raw, _ := io.ReadAll(rc)
		_ = rc.Close()
		gotEntries[f.Name] = string(raw)
	}
	for _, name := range []string{"error.json", "dom.html", "axtree.txt", "screenshot.png"} {
		if _, ok := gotEntries[name]; !ok {
			t.Fatalf("missing entry %q in diagnostic zip", name)
		}
	}
	if strings.Contains(gotEntries["dom.html"], "bad()") || strings.Contains(gotEntries["dom.html"], "pw123") {
		t.Fatalf("dom.html was not scrubbed: %s", gotEntries["dom.html"])
	}

	// Air-gap violations in error metadata, dom.html, and axtree.txt must all fail closed.
	for _, badErr := range []*StepExecError{
		{StepID: "s1", Err: errors.New("contains forbidden-secret")},
		{StepID: "s1", Err: errors.New("ok"), DOMHTML: "<html><body>forbidden-secret</body></html>"},
		{StepID: "s1", Err: errors.New("ok"), AXTreeText: "[heading] \"forbidden-secret\""},
	} {
		if _, err := BuildDiagnosticZip("status-board", badErr, []string{"forbidden-secret"}); err == nil {
			t.Fatalf("expected air-gap violation to block BuildDiagnosticZip for %+v", badErr)
		}
	}

	outDir := filepath.Join(t.TempDir(), "debug")
	saved, err := SaveDiagnosticZip(outDir, "ops/status:v1", zipBytes)
	if err != nil {
		t.Fatalf("SaveDiagnosticZip failed: %v", err)
	}
	if filepath.Dir(saved) != outDir || !strings.HasPrefix(filepath.Base(saved), "debug_ops_status_v1_") {
		t.Fatalf("unexpected saved diagnostic path: %q", saved)
	}
	diskBytes, err := os.ReadFile(saved)
	if err != nil || !bytes.Equal(diskBytes, zipBytes) {
		t.Fatalf("saved diagnostic zip mismatch: err=%v len=%d/%d", err, len(diskBytes), len(zipBytes))
	}

	if got := sanitizeFilename("///"); got != "___" {
		t.Fatalf("sanitizeFilename(///) = %q, want ___", got)
	}
	if got := sanitizeFilename(""); got != "drop" {
		t.Fatalf("sanitizeFilename(\"\") = %q, want drop", got)
	}
}
