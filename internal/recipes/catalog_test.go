package recipes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
)

func TestReloadableCatalogLocalFileHotReloadAndWriteBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recipes.json")

	initial := Bundle{
		SchemaVersion: "2.0",
		Recipes: []contract.Recipe{
			{
				Name:  "status-board",
				Topic: "ops",
				Steps: []contract.Step{{ID: "main", URL: "https://status.example.com"}},
			},
		},
	}
	raw, _ := json.MarshalIndent(initial, "", "  ")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	cat := NewReloadableCatalog(path)
	cat.MinReloadInterval = 0

	ctx := context.Background()
	r, ok, err := cat.Get(ctx, "status-board")
	if err != nil || !ok || r.Topic != "ops" {
		t.Fatalf("Get(status-board) = (%+v, %v, %v)", r, ok, err)
	}

	// Reject unapproved ad-hoc URL / domain.
	_, err = cat.ValidateAndResolveTask(ctx, "evil", &contract.Recipe{
		Name:  "evil",
		Topic: "evil",
		Steps: []contract.Step{{ID: "s1", URL: "https://unapproved.invalid/secret"}},
	}, nil)
	if !errors.Is(err, ErrUnapprovedUseCase) {
		t.Fatalf("expected ErrUnapprovedUseCase, got %v", err)
	}

	// Atomic write-back on approval updates the file and hot-reloads cleanly.
	updated := r
	updated.Description = "healed v2"
	updated.Steps[0].WaitSelector = ".v2-status"
	if err := cat.SaveRecipe(ctx, updated); err != nil {
		t.Fatalf("SaveRecipe failed: %v", err)
	}

	// Fresh catalog instance reads persisted update.
	cat2 := NewReloadableCatalog(path)
	cat2.MinReloadInterval = 0
	r2, ok, err := cat2.Get(ctx, "status-board")
	if err != nil || !ok || r2.Steps[0].WaitSelector != ".v2-status" {
		t.Fatalf("reloaded recipe mismatch: %+v ok=%v err=%v", r2, ok, err)
	}

	// External modification triggers mtime reload.
	future := time.Now().Add(2 * time.Second)
	updated.Description = "externally edited"
	raw2, _ := json.Marshal(Bundle{Recipes: []contract.Recipe{updated}})
	if err := os.WriteFile(path, raw2, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(path, future, future)

	r3, ok, err := cat2.Get(ctx, "status-board")
	if err != nil || !ok || r3.Description != "externally edited" {
		t.Fatalf("expected mtime hot-reload, got %+v err=%v", r3, err)
	}
}

func TestReloadableCatalogGCSHotReloadETagAndWriteBack(t *testing.T) {
	var (
		mu      sync.Mutex
		payload []byte
		etag    = `"etag-v1"`
		getHits int
	)
	payload, _ = json.Marshal(Bundle{
		Recipes: []contract.Recipe{
			{
				Name:  "release-feed",
				Topic: "releases",
				Steps: []contract.Step{{ID: "list", URL: "https://releases.example.com"}},
			},
		},
	})

	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing header", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"test-wi-access-token","expires_in":3600}`))
	}))
	defer metaSrv.Close()

	gcsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-wi-access-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet {
			getHits++
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			_, _ = w.Write(payload)
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/upload/storage/v1/") {
			body, _ := io.ReadAll(r.Body)
			payload = body
			etag = `"etag-v2"`
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"name":"desktop/recipes.json"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer gcsSrv.Close()

	cat := NewReloadableCatalog("gs://test-bucket/desktop/recipes.json")
	cat.MinReloadInterval = 0
	cat.GCSBaseURL = gcsSrv.URL
	cat.GCSTokenURL = metaSrv.URL

	ctx := context.Background()
	r, ok, err := cat.Get(ctx, "release-feed")
	if err != nil || !ok {
		t.Fatalf("initial GCS Get failed: ok=%v err=%v", ok, err)
	}
	// Second call hits 304 Not Modified cleanly.
	if _, ok2, err2 := cat.Get(ctx, "release-feed"); err2 != nil || !ok2 {
		t.Fatalf("second GCS Get failed: ok=%v err=%v", ok2, err2)
	}

	// Write back updated recipe atomically to GCS.
	r.Description = "patched via approval"
	if err := cat.SaveRecipe(ctx, r); err != nil {
		t.Fatalf("SaveRecipe GCS failed: %v", err)
	}
	catFresh := NewReloadableCatalog("gs://test-bucket/desktop/recipes.json")
	catFresh.GCSBaseURL = gcsSrv.URL
	catFresh.GCSTokenURL = metaSrv.URL
	r2, ok, err := catFresh.Get(ctx, "release-feed")
	if err != nil || !ok || r2.Description != "patched via approval" {
		t.Fatalf("fresh GCS catalog read mismatch: %+v ok=%v err=%v", r2, ok, err)
	}
}
