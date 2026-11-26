package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mapGetenv(vals map[string]string) func(string) string {
	return func(k string) string {
		return vals[k]
	}
}

func TestParseWriteTimeoutBounds(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"", defaultWriteTimeout},
		{"   ", defaultWriteTimeout},
		{"abc", defaultWriteTimeout},
		{"10", defaultWriteTimeout},
		{"601", defaultWriteTimeout},
		{"30", 30 * time.Second},
		{"240", 240 * time.Second},
		{"600", 600 * time.Second},
	}
	for _, tc := range cases {
		if got := parseWriteTimeout(tc.raw); got != tc.want {
			t.Errorf("parseWriteTimeout(%q) = %s, want %s", tc.raw, got, tc.want)
		}
	}
}

func TestBuildServerRuntimeValidationAndWiring(t *testing.T) {
	dir := t.TempDir()
	recipesFile := filepath.Join(dir, "recipes.json")
	if err := os.WriteFile(recipesFile, []byte(`{"name":"status-board","topic":"ops","steps":[{"id":"s1","url":"https://status.example.com"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Missing all auth tokens fails closed.
	if _, err := buildServerRuntime(mapGetenv(map[string]string{
		"STORE_BACKEND":   "memory",
		"STORAGE_BACKEND": "memory",
		"HEALER_ENABLED":  "false",
	}), nil); err == nil || !strings.Contains(err.Error(), "ADMIN_TOKEN") {
		t.Fatalf("missing tokens err = %v, want token requirement error", err)
	}

	// 2. Unsupported STORE_BACKEND fails closed.
	if _, err := buildServerRuntime(mapGetenv(map[string]string{
		"STORE_BACKEND":   "postgres",
		"STORAGE_BACKEND": "memory",
		"ADMIN_TOKEN":     "tok",
	}), nil); err == nil || !strings.Contains(err.Error(), "STORE_BACKEND") {
		t.Fatalf("bad STORE_BACKEND err = %v, want STORE_BACKEND error", err)
	}

	// 3. Unsupported STORAGE_BACKEND fails closed.
	if _, err := buildServerRuntime(mapGetenv(map[string]string{
		"STORE_BACKEND":   "memory",
		"STORAGE_BACKEND": "s3",
		"ADMIN_TOKEN":     "tok",
	}), nil); err == nil || !strings.Contains(err.Error(), "STORAGE_BACKEND") {
		t.Fatalf("bad STORAGE_BACKEND err = %v, want STORAGE_BACKEND error", err)
	}

	// 4. STORAGE_BACKEND=gcs without GCS_BUCKET fails closed.
	if _, err := buildServerRuntime(mapGetenv(map[string]string{
		"STORE_BACKEND":   "memory",
		"STORAGE_BACKEND": "gcs",
		"ADMIN_TOKEN":     "tok",
	}), nil); err == nil || !strings.Contains(err.Error(), "GCS_BUCKET") {
		t.Fatalf("missing GCS_BUCKET err = %v, want GCS_BUCKET error", err)
	}

	// 5. GCS backend + default WriteTimeout (180s) succeeds.
	rtGCS, err := buildServerRuntime(mapGetenv(map[string]string{
		"STORE_BACKEND":   "memory",
		"STORAGE_BACKEND": "gcs",
		"GCS_BUCKET":      "example-bucket",
		"HEALER_ENABLED":  "false",
		"ADMIN_TOKEN":     "tok",
	}), nil)
	if err != nil {
		t.Fatalf("GCS runtime build failed: %v", err)
	}
	defer rtGCS.store.Close()
	if rtGCS.httpSrv.WriteTimeout != 180*time.Second || rtGCS.blobBackend != "gcs" || rtGCS.healerActive {
		t.Fatalf("unexpected GCS runtime: timeout=%s blob=%s healer=%v", rtGCS.httpSrv.WriteTimeout, rtGCS.blobBackend, rtGCS.healerActive)
	}

	// 6. Production-like FileStore + local blobs + RECIPES_URI + custom WRITE_TIMEOUT_SECONDS + graceful healer fallback when credentials missing.
	statePath := filepath.Join(dir, "state.json")
	rtFile, err := buildServerRuntime(mapGetenv(map[string]string{
		"LISTEN_ADDR":           "127.0.0.1:0",
		"STORE_BACKEND":         "file",
		"STATE_FILE":            statePath,
		"STORAGE_BACKEND":       "local",
		"LOCAL_BLOB_DIR":        filepath.Join(dir, "blobs"),
		"RECIPES_URI":           recipesFile,
		"EXTRA_FORBIDDEN_TERMS": "secret-a, secret-b",
		"WRITE_TIMEOUT_SECONDS": "240",
		"HEALER_ENABLED":        "true", // missing credentials degrades gracefully
		"DESKTOP_CLIENT_TOKEN":  "desk-tok",
		"AGENT_BUS_TOKEN":       "agent-tok",
		"ADMIN_TOKEN":           "admin-tok",
	}), func() []string { return []string{"local-term"} })
	if err != nil {
		t.Fatalf("file runtime build failed: %v", err)
	}
	defer rtFile.store.Close()
	if rtFile.httpSrv.WriteTimeout != 240*time.Second || rtFile.storeBackend != "file" || rtFile.healerActive {
		t.Fatalf("unexpected file runtime: timeout=%s store=%s healer=%v", rtFile.httpSrv.WriteTimeout, rtFile.storeBackend, rtFile.healerActive)
	}

	// Healthcheck and loaded recipe catalog work through rtFile.httpSrv.Handler.
	hz := httptest.NewRecorder()
	rtFile.httpSrv.Handler.ServeHTTP(hz, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if hz.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200 (%s)", hz.Code, hz.Body.String())
	}
	recReq := httptest.NewRequest(http.MethodGet, "/desktop/v1/recipes", nil)
	recReq.Header.Set("Authorization", "Bearer desk-tok")
	recW := httptest.NewRecorder()
	rtFile.httpSrv.Handler.ServeHTTP(recW, recReq)
	if recW.Code != http.StatusOK || !strings.Contains(recW.Body.String(), `"status-board"`) {
		t.Fatalf("GET /desktop/v1/recipes = %d (%s), want 200 with status-board", recW.Code, recW.Body.String())
	}
}
