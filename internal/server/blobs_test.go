package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestMemoryAndDirBlobStoreAtomicAndTraversalGuard(t *testing.T) {
	ctx := context.Background()

	// MemoryBlobStore round-trip, missing key, and defensive copy isolation.
	mem := NewMemoryBlobStore()
	if _, err := mem.Get(ctx, "missing.zip"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MemoryBlobStore missing = %v, want ErrNotFound", err)
	}
	payload := []byte("zip-bytes-v1")
	if err := mem.Put(ctx, "diagnostics/d1.zip", "application/zip", payload); err != nil {
		t.Fatalf("MemoryBlobStore Put failed: %v", err)
	}
	payload[0] = 'X' // mutate caller slice after Put
	got, err := mem.Get(ctx, "diagnostics/d1.zip")
	if err != nil || string(got) != "zip-bytes-v1" {
		t.Fatalf("MemoryBlobStore Get = (%q, %v), want (zip-bytes-v1, nil)", string(got), err)
	}
	got[0] = 'Y' // mutate returned slice
	got2, _ := mem.Get(ctx, "diagnostics/d1.zip")
	if string(got2) != "zip-bytes-v1" {
		t.Fatalf("MemoryBlobStore leaked backing slice mutation: %q", string(got2))
	}

	// DirBlobStore atomic writes + traversal rejection.
	parent := t.TempDir()
	root := filepath.Join(parent, "blobs")
	dirStore := &DirBlobStore{Root: root}

	if _, err := dirStore.Get(ctx, "diagnostics/missing.zip"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DirBlobStore missing = %v, want ErrNotFound", err)
	}
	if err := dirStore.Put(ctx, "diagnostics/diag_1.zip", "application/zip", []byte("PK-atomic-1")); err != nil {
		t.Fatalf("DirBlobStore Put failed: %v", err)
	}
	b, err := dirStore.Get(ctx, "diagnostics/diag_1.zip")
	if err != nil || string(b) != "PK-atomic-1" {
		t.Fatalf("DirBlobStore Get = (%q, %v), want (PK-atomic-1, nil)", string(b), err)
	}
	// Verify no leftover .blob-*.tmp files remain after atomic rename.
	entries, err := os.ReadDir(filepath.Join(root, "diagnostics"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".blob-") {
			t.Fatalf("leftover temp file in DirBlobStore: %s", e.Name())
		}
	}

	// Overwrite atomically.
	if err := dirStore.Put(ctx, "diagnostics/diag_1.zip", "application/zip", []byte("PK-atomic-2")); err != nil {
		t.Fatalf("DirBlobStore overwrite failed: %v", err)
	}
	b2, err := dirStore.Get(ctx, "diagnostics/diag_1.zip")
	if err != nil || string(b2) != "PK-atomic-2" {
		t.Fatalf("DirBlobStore Get overwrite = (%q, %v), want (PK-atomic-2, nil)", string(b2), err)
	}

	// Seed a file outside root to verify traversal keys cannot read or overwrite it.
	secretOutside := filepath.Join(parent, "escape.txt")
	if err := os.WriteFile(secretOutside, []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, badKey := range []string{
		"",
		"   ",
		".",
		"..",
		"../escape.txt",
		"/../escape.txt",
		"diagnostics/../../escape.txt",
		`diagnostics\..\escape.txt`,
	} {
		if err := dirStore.Put(ctx, badKey, "text/plain", []byte("pwned")); err == nil {
			t.Fatalf("DirBlobStore.Put(%q) succeeded, want traversal rejection", badKey)
		}
		if _, err := dirStore.Get(ctx, badKey); !errors.Is(err, ErrNotFound) {
			t.Fatalf("DirBlobStore.Get(%q) = %v, want ErrNotFound", badKey, err)
		}
	}
	afterOutside, _ := os.ReadFile(secretOutside)
	if string(afterOutside) != "outside-secret" {
		t.Fatalf("outside file was modified: %q", string(afterOutside))
	}
}

func TestGCSBlobStoreHTTPParityTokenCacheAndErrorFormatting(t *testing.T) {
	ctx := context.Background()
	t.Setenv("GCS_ACCESS_TOKEN", "")

	var (
		mu         sync.Mutex
		objects    = map[string][]byte{}
		tokenHits  atomic.Int32
		tokenMode  atomic.Value // "ok", "missing_tok", "http_500", "slow_ok"
		slowGate   = make(chan struct{})
		slowJoined atomic.Int32
		force500   atomic.Bool
	)
	tokenMode.Store("ok")

	metaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing Metadata-Flavor header", http.StatusForbidden)
			return
		}
		tokenHits.Add(1)
		switch tokenMode.Load().(string) {
		case "missing_tok":
			_, _ = w.Write([]byte(`{"expires_in":3600}`))
		case "http_500":
			http.Error(w, "metadata down", http.StatusInternalServerError)
		case "slow_ok":
			if slowJoined.Add(1) == 2 {
				close(slowGate)
			}
			<-slowGate
			_, _ = w.Write([]byte(`{"access_token":"meta-tok-concurrent","expires_in":3600}`))
		default:
			_, _ = w.Write([]byte(`{"access_token":"meta-tok-1","expires_in":3600}`))
		}
	}))
	defer metaSrv.Close()

	gcsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if force500.Load() {
			http.Error(w, "upstream failure", http.StatusInternalServerError)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer meta-tok-1" && auth != "Bearer env-override-tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodPost:
			if r.URL.Path != "/upload/storage/v1/b/test-bucket/o" {
				http.Error(w, "bad upload path "+r.URL.Path, http.StatusBadRequest)
				return
			}
			name := r.URL.Query().Get("name")
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			objects[name] = raw
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"name":"` + name + `"}`))
		case http.MethodGet:
			prefix := "/storage/v1/b/test-bucket/o/"
			if !strings.HasPrefix(r.URL.Path, prefix) {
				http.Error(w, "bad get path "+r.URL.Path, http.StatusBadRequest)
				return
			}
			name := strings.TrimPrefix(r.URL.Path, prefix)
			mu.Lock()
			data, ok := objects[name]
			mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer gcsSrv.Close()

	store := NewGCSBlobStore("test-bucket", "desktop")
	store.BaseURL = gcsSrv.URL
	store.TokenURL = metaSrv.URL
	store.HTTPClient = gcsSrv.Client()

	// 1. Put + Get round-trip uses metadata token and caches it across calls.
	if err := store.Put(ctx, "diagnostics/d1.zip", "application/zip", []byte("gcs-zip-payload")); err != nil {
		t.Fatalf("GCSBlobStore.Put failed: %v", err)
	}
	got, err := store.Get(ctx, "diagnostics/d1.zip")
	if err != nil || !bytes.Equal(got, []byte("gcs-zip-payload")) {
		t.Fatalf("GCSBlobStore.Get = (%q, %v), want (gcs-zip-payload, nil)", string(got), err)
	}
	if hits := tokenHits.Load(); hits != 1 {
		t.Fatalf("expected metadata token to be cached (1 fetch), got %d", hits)
	}

	// 2. 404 maps to ErrNotFound; 500 returns descriptive error on Put & Get.
	if _, err := store.Get(ctx, "diagnostics/missing.zip"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GCSBlobStore missing = %v, want ErrNotFound", err)
	}
	force500.Store(true)
	if err := store.Put(ctx, "diagnostics/d2.zip", "", []byte("x")); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("GCSBlobStore.Put 500 = %v, want HTTP 500 error", err)
	}
	if _, err := store.Get(ctx, "diagnostics/d1.zip"); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("GCSBlobStore.Get 500 = %v, want HTTP 500 error", err)
	}
	force500.Store(false)

	// 3. Missing access_token formats cleanly without %!w(<nil>), and HTTP 500 metadata fails cleanly.
	fresh := NewGCSBlobStore("test-bucket", "")
	fresh.BaseURL = gcsSrv.URL
	fresh.TokenURL = metaSrv.URL
	fresh.HTTPClient = gcsSrv.Client()
	tokenMode.Store("missing_tok")
	_, err = fresh.accessToken(ctx)
	if err == nil || strings.Contains(err.Error(), "%!w") || !strings.Contains(err.Error(), "missing access_token") {
		t.Fatalf("missing access_token err = %v, want clean error without %%!w", err)
	}
	tokenMode.Store("http_500")
	if _, err := fresh.accessToken(ctx); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("metadata 500 err = %v, want HTTP 500", err)
	}

	// 4. S-12: accessToken drops g.mu across network I/O (two concurrent fetches synchronize via slowGate without deadlock).
	tokenMode.Store("slow_ok")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, terr := fresh.accessToken(ctx)
			if terr != nil {
				errs <- terr
				return
			}
			if tok != "meta-tok-concurrent" {
				errs <- errors.New("unexpected token: " + tok)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent accessToken failed: %v", e)
	}

	// 5. GCS_ACCESS_TOKEN environment override bypasses metadata endpoint.
	t.Setenv("GCS_ACCESS_TOKEN", "env-override-tok")
	beforeHits := tokenHits.Load()
	if err := fresh.Put(ctx, "env-check.zip", "application/zip", []byte("override")); err != nil {
		t.Fatalf("GCSBlobStore Put with env token failed: %v", err)
	}
	if tokenHits.Load() != beforeHits {
		t.Fatal("GCS_ACCESS_TOKEN override should not hit metadata server")
	}
}
