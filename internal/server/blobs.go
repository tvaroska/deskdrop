package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrNotFound = errors.New("not found")

// BlobStore is the Driven Port for binary diagnostic bundles (.zip).
type BlobStore interface {
	Put(ctx context.Context, key string, contentType string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
}

// MemoryBlobStore stores diagnostic bundles in memory for offline tests and local dev.
type MemoryBlobStore struct {
	mu    sync.RWMutex
	blobs map[string][]byte
}

func NewMemoryBlobStore() *MemoryBlobStore {
	return &MemoryBlobStore{blobs: make(map[string][]byte)}
}

func (m *MemoryBlobStore) Put(_ context.Context, key string, _ string, data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	m.mu.Lock()
	m.blobs[key] = cp
	m.mu.Unlock()
	return nil
}

func (m *MemoryBlobStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.RLock()
	b, ok := m.blobs[key]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp, nil
}

// DirBlobStore stores blobs in a local filesystem directory.
type DirBlobStore struct {
	Root string
}

func (d *DirBlobStore) Put(_ context.Context, key string, _ string, data []byte) error {
	clean := filepath.Clean(strings.TrimLeft(key, "/"))
	if strings.HasPrefix(clean, "..") {
		return errors.New("invalid blob key")
	}
	target := filepath.Join(d.Root, clean)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, data, 0o600)
}

func (d *DirBlobStore) Get(_ context.Context, key string) ([]byte, error) {
	clean := filepath.Clean(strings.TrimLeft(key, "/"))
	if strings.HasPrefix(clean, "..") {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(d.Root, clean))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

// GCSBlobStore uploads and downloads objects via GCS JSON API using GKE Workload Identity
// metadata tokens (zero external GCS SDK dependencies).
type GCSBlobStore struct {
	Bucket     string
	Prefix     string
	BaseURL    string
	TokenURL   string
	HTTPClient *http.Client

	mu          sync.Mutex
	cachedToken string
	tokenExpiry time.Time
}

func NewGCSBlobStore(bucket, prefix string) *GCSBlobStore {
	return &GCSBlobStore{
		Bucket:     strings.TrimSpace(bucket),
		Prefix:     strings.Trim(strings.TrimSpace(prefix), "/"),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (g *GCSBlobStore) baseURL() string {
	if s := strings.TrimRight(strings.TrimSpace(g.BaseURL), "/"); s != "" {
		return s
	}
	return "https://storage.googleapis.com"
}

func (g *GCSBlobStore) tokenURL() string {
	if s := strings.TrimSpace(g.TokenURL); s != "" {
		return s
	}
	return "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
}

func (g *GCSBlobStore) httpClient() *http.Client {
	if g.HTTPClient != nil {
		return g.HTTPClient
	}
	return http.DefaultClient
}

func (g *GCSBlobStore) objectName(key string) string {
	k := strings.TrimLeft(key, "/")
	if g.Prefix == "" || strings.HasPrefix(k, "tools/") {
		return k
	}
	return g.Prefix + "/" + k
}

func (g *GCSBlobStore) Put(ctx context.Context, key string, contentType string, data []byte) error {
	tok, err := g.accessToken(ctx)
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	obj := g.objectName(key)
	u := fmt.Sprintf(
		"%s/upload/storage/v1/b/%s/o?uploadType=media&name=%s",
		g.baseURL(),
		url.PathEscape(g.Bucket),
		url.QueryEscape(obj),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", contentType)

	resp, err := g.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("gcs upload %s failed (HTTP %d): %s", obj, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (g *GCSBlobStore) Get(ctx context.Context, key string) ([]byte, error) {
	tok, err := g.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	obj := g.objectName(key)
	u := fmt.Sprintf(
		"%s/storage/v1/b/%s/o/%s?alt=media",
		g.baseURL(),
		url.PathEscape(g.Bucket),
		url.PathEscape(obj),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := g.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("gcs download %s failed (HTTP %d): %s", obj, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 20*1024*1024))
}

func (g *GCSBlobStore) accessToken(ctx context.Context) (string, error) {
	if envTok := os.Getenv("GCS_ACCESS_TOKEN"); envTok != "" {
		return envTok, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cachedToken != "" && time.Now().Add(60*time.Second).Before(g.tokenExpiry) {
		return g.cachedToken, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.tokenURL(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("metadata token fetch failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16384))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata server HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.AccessToken == "" {
		return "", fmt.Errorf("invalid metadata token payload: %w", err)
	}
	g.cachedToken = parsed.AccessToken
	g.tokenExpiry = time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	return g.cachedToken, nil
}
