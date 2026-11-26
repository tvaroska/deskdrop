package recipes

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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
)

// ErrUnapprovedUseCase indicates a requested recipe or URL is not in the approved RECIPES_URI catalog (HTTP 403).
var ErrUnapprovedUseCase = errors.New("unapproved recipe or target URL")

// Catalog is the Driven Port for hot-reloadable approved extraction recipes (local file/dir or GCS).
type Catalog interface {
	List(ctx context.Context) ([]contract.Recipe, error)
	Get(ctx context.Context, name string) (contract.Recipe, bool, error)
	ValidateAndResolveTask(ctx context.Context, recipeName string, custom *contract.Recipe, vars map[string]string) (contract.Recipe, error)
	AllowedDomains(ctx context.Context) ([]string, error)
	SaveRecipe(ctx context.Context, recipe contract.Recipe) error
}

// Bundle is the canonical JSON envelope stored at RECIPES_URI (/data/recipes.json or gs://bucket/recipes.json).
type Bundle struct {
	SchemaVersion string            `json:"schema_version,omitempty"`
	UpdatedAtUTC  string            `json:"updated_at_utc,omitempty"`
	Recipes       []contract.Recipe `json:"recipes"`
}

// ReloadableCatalog loads approved declarative recipes from a local JSON file, local directory,
// or Cloud Storage URI (gs://<bucket>/<object>) with mtime/ETag hot-reload and atomic write-back.
type ReloadableCatalog struct {
	URI               string
	MinReloadInterval time.Duration
	GCSBaseURL        string
	GCSTokenURL       string
	HTTPClient        *http.Client

	mu          sync.RWMutex
	recipes     map[string]contract.Recipe
	lastChecked time.Time
	lastModTime time.Time
	lastETag    string
	cachedToken string
	tokenExpiry time.Time
	hasLoaded   bool
}

// NewReloadableCatalog creates a hot-reloadable recipe catalog for local path or gs:// URI.
func NewReloadableCatalog(uri string) *ReloadableCatalog {
	return &ReloadableCatalog{
		URI:               strings.TrimSpace(uri),
		MinReloadInterval: 3 * time.Second,
		HTTPClient:        &http.Client{Timeout: 20 * time.Second},
		recipes:           make(map[string]contract.Recipe),
	}
}

// NewStaticCatalog creates an in-memory Catalog preloaded with approved recipes (useful in tests).
func NewStaticCatalog(items ...contract.Recipe) *ReloadableCatalog {
	c := &ReloadableCatalog{
		recipes:   make(map[string]contract.Recipe, len(items)),
		hasLoaded: true,
	}
	for _, r := range items {
		c.recipes[strings.ToLower(strings.TrimSpace(r.Name))] = r
	}
	return c
}

// Reload forces an immediate mtime/ETag refresh from RECIPES_URI.
func (c *ReloadableCatalog) Reload(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reloadLocked(ctx, true)
}

func (c *ReloadableCatalog) ensureLoaded(ctx context.Context) error {
	c.mu.RLock()
	uri := c.URI
	fresh := c.hasLoaded && (uri == "" || time.Since(c.lastChecked) < c.MinReloadInterval)
	c.mu.RUnlock()
	if fresh {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasLoaded && (c.URI == "" || time.Since(c.lastChecked) < c.MinReloadInterval) {
		return nil
	}
	return c.reloadLocked(ctx, false)
}

func (c *ReloadableCatalog) reloadLocked(ctx context.Context, force bool) error {
	uri := strings.TrimSpace(c.URI)
	c.lastChecked = time.Now()
	if uri == "" {
		c.hasLoaded = true
		return nil
	}
	if strings.HasPrefix(uri, "gs://") {
		return c.reloadGCSLocked(ctx, uri, force)
	}
	return c.reloadLocalLocked(uri, force)
}

func (c *ReloadableCatalog) reloadLocalLocked(path string, force bool) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.recipes = make(map[string]contract.Recipe)
			c.hasLoaded = true
			return nil
		}
		return fmt.Errorf("stat recipes path %s: %w", path, err)
	}

	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		newest := info.ModTime()
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
				continue
			}
			if fi, ferr := e.Info(); ferr == nil && fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
		if !force && c.hasLoaded && !newest.After(c.lastModTime) {
			return nil
		}
		next := make(map[string]contract.Recipe)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
				continue
			}
			raw, rerr := os.ReadFile(filepath.Join(path, e.Name()))
			if rerr != nil {
				return rerr
			}
			parsed, perr := parseRecipesPayload(raw)
			if perr != nil {
				return fmt.Errorf("parse recipe %s: %w", e.Name(), perr)
			}
			for _, r := range parsed {
				next[strings.ToLower(strings.TrimSpace(r.Name))] = r
			}
		}
		c.recipes = next
		c.lastModTime = newest
		c.hasLoaded = true
		return nil
	}

	if !force && c.hasLoaded && !info.ModTime().After(c.lastModTime) {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	parsed, err := parseRecipesPayload(raw)
	if err != nil {
		return fmt.Errorf("parse recipes file %s: %w", path, err)
	}
	next := make(map[string]contract.Recipe, len(parsed))
	for _, r := range parsed {
		next[strings.ToLower(strings.TrimSpace(r.Name))] = r
	}
	c.recipes = next
	c.lastModTime = info.ModTime()
	c.hasLoaded = true
	return nil
}

func (c *ReloadableCatalog) reloadGCSLocked(ctx context.Context, uri string, force bool) error {
	bucket, obj, err := parseGCSURI(uri)
	if err != nil {
		return err
	}
	tok, err := c.accessTokenLocked(ctx)
	if err != nil {
		return err
	}
	u := fmt.Sprintf(
		"%s/storage/v1/b/%s/o/%s?alt=media",
		c.gcsBaseURL(),
		url.PathEscape(bucket),
		url.PathEscape(obj),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if !force && c.lastETag != "" {
		req.Header.Set("If-None-Match", c.lastETag)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		c.hasLoaded = true
		return nil
	}
	if resp.StatusCode == http.StatusNotFound {
		c.recipes = make(map[string]contract.Recipe)
		c.hasLoaded = true
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("gcs recipes download %s failed (HTTP %d): %s", uri, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return err
	}
	parsed, err := parseRecipesPayload(raw)
	if err != nil {
		return fmt.Errorf("parse gcs recipes %s: %w", uri, err)
	}
	next := make(map[string]contract.Recipe, len(parsed))
	for _, r := range parsed {
		next[strings.ToLower(strings.TrimSpace(r.Name))] = r
	}
	c.recipes = next
	if etag := strings.TrimSpace(resp.Header.Get("ETag")); etag != "" {
		c.lastETag = etag
	}
	c.hasLoaded = true
	return nil
}

// List returns all approved recipes sorted by name.
func (c *ReloadableCatalog) List(ctx context.Context) ([]contract.Recipe, error) {
	if err := c.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]contract.Recipe, 0, len(c.recipes))
	for _, r := range c.recipes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns an approved recipe by name (case-insensitive).
func (c *ReloadableCatalog) Get(ctx context.Context, name string) (contract.Recipe, bool, error) {
	if err := c.ensureLoaded(ctx); err != nil {
		return contract.Recipe{}, false, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.recipes[strings.ToLower(strings.TrimSpace(name))]
	return r, ok, nil
}

// AllowedDomains returns the sorted union of approved target domains across the loaded catalog.
func (c *ReloadableCatalog) AllowedDomains(ctx context.Context) ([]string, error) {
	list, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var out []string
	for _, r := range list {
		for _, d := range contract.InferRecipeDomains(r) {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// ValidateAndResolveTask enforces the ADR-004 approved-use-case boundary (returning ErrUnapprovedUseCase
// on unknown recipes or unapproved ad-hoc target URLs so HTTP handlers reject with 403).
func (c *ReloadableCatalog) ValidateAndResolveTask(ctx context.Context, recipeName string, custom *contract.Recipe, vars map[string]string) (contract.Recipe, error) {
	if err := c.ensureLoaded(ctx); err != nil {
		return contract.Recipe{}, err
	}

	approvedDomains, err := c.AllowedDomains(ctx)
	if err != nil {
		return contract.Recipe{}, err
	}

	var base contract.Recipe
	if custom != nil {
		base = *custom
		if strings.TrimSpace(base.Name) == "" {
			base.Name = strings.TrimSpace(recipeName)
		}
	} else {
		loaded, ok, gerr := c.Get(ctx, recipeName)
		if gerr != nil {
			return contract.Recipe{}, gerr
		}
		if !ok {
			return contract.Recipe{}, fmt.Errorf("%w: recipe %q is not registered in RECIPES_URI", ErrUnapprovedUseCase, recipeName)
		}
		base = loaded
	}

	resolved, err := contract.ResolveRecipe(base, vars)
	if err != nil {
		return contract.Recipe{}, err
	}

	if custom != nil {
		// Every resolved step URL/domain on a custom recipe must belong to an approved catalog domain.
		if err := contract.ValidateRecipeDomains(resolved, approvedDomains, nil); err != nil {
			return contract.Recipe{}, fmt.Errorf("%w: %v", ErrUnapprovedUseCase, err)
		}
	} else {
		// Even for registered catalog recipes, post-resolution step domains must remain inside
		// the base recipe's own approved domains so variable injection cannot pivot hosts.
		baseDomains := contract.InferRecipeDomains(base)
		if len(baseDomains) == 0 {
			baseDomains = approvedDomains
		}
		if err := contract.ValidateRecipeDomains(resolved, baseDomains, nil); err != nil {
			return contract.Recipe{}, fmt.Errorf("%w: %v", ErrUnapprovedUseCase, err)
		}
	}
	return resolved, nil
}

// SaveRecipe atomically updates or inserts a human-approved Recipe back into RECIPES_URI.
func (c *ReloadableCatalog) SaveRecipe(ctx context.Context, recipe contract.Recipe) error {
	if err := contract.ValidateRecipeName(recipe.Name); err != nil {
		return err
	}
	if len(recipe.Steps) == 0 {
		return errors.New("recipe must define at least 1 step")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.URI != "" && !c.hasLoaded {
		if err := c.reloadLocked(ctx, true); err != nil {
			return fmt.Errorf("reload recipes before save: %w", err)
		}
	}
	key := strings.ToLower(strings.TrimSpace(recipe.Name))
	next := make(map[string]contract.Recipe, len(c.recipes)+1)
	for k, v := range c.recipes {
		next[k] = v
	}
	next[key] = recipe

	uri := strings.TrimSpace(c.URI)
	if uri == "" {
		c.recipes = next
		c.hasLoaded = true
		return nil
	}
	if strings.HasPrefix(uri, "gs://") {
		if err := c.saveGCSLocked(ctx, uri, next); err != nil {
			return err
		}
		c.recipes = next
		c.hasLoaded = true
		return nil
	}
	if err := c.saveLocalLocked(uri, recipe, next); err != nil {
		return err
	}
	c.recipes = next
	c.hasLoaded = true
	return nil
}

func (c *ReloadableCatalog) saveLocalLocked(path string, updated contract.Recipe, next map[string]contract.Recipe) error {
	info, statErr := os.Stat(path)
	if statErr == nil && info.IsDir() {
		absDir, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		target := filepath.Join(absDir, updated.Name+".json")
		absTarget, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(absDir, absTarget)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Dir(rel) != "." {
			return fmt.Errorf("recipe path %q escapes recipes directory", updated.Name)
		}
		raw, err := json.MarshalIndent(updated, "", "  ")
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		if err := atomicWriteFile(absTarget, raw, 0o600); err != nil {
			return err
		}
		if fi, err := os.Stat(absTarget); err == nil {
			c.lastModTime = fi.ModTime()
		}
		return nil
	}

	bundleBytes, err := marshalBundleMap(next)
	if err != nil {
		return err
	}
	if err := atomicWriteFile(path, bundleBytes, 0o600); err != nil {
		return err
	}
	if fi, err := os.Stat(path); err == nil {
		c.lastModTime = fi.ModTime()
	}
	return nil
}

func (c *ReloadableCatalog) saveGCSLocked(ctx context.Context, uri string, next map[string]contract.Recipe) error {
	bucket, obj, err := parseGCSURI(uri)
	if err != nil {
		return err
	}
	tok, err := c.accessTokenLocked(ctx)
	if err != nil {
		return err
	}
	bundleBytes, err := marshalBundleMap(next)
	if err != nil {
		return err
	}
	u := fmt.Sprintf(
		"%s/upload/storage/v1/b/%s/o?uploadType=media&name=%s",
		c.gcsBaseURL(),
		url.PathEscape(bucket),
		url.QueryEscape(obj),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(bundleBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("gcs recipes write-back %s failed (HTTP %d): %s", uri, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if etag := strings.TrimSpace(resp.Header.Get("ETag")); etag != "" {
		c.lastETag = etag
	} else {
		c.lastETag = ""
	}
	return nil
}

func marshalBundleMap(items map[string]contract.Recipe) ([]byte, error) {
	list := make([]contract.Recipe, 0, len(items))
	for _, r := range items {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b := Bundle{
		SchemaVersion: "2.0",
		UpdatedAtUTC:  time.Now().UTC().Format(time.RFC3339),
		Recipes:       list,
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func parseRecipesPayload(raw []byte) ([]contract.Recipe, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var list []contract.Recipe
		if err := contract.ParseJSONStrict(trimmed, &list); err != nil {
			return nil, err
		}
		return list, nil
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return nil, err
	}
	if _, hasRecipes := probe["recipes"]; hasRecipes {
		var bundle Bundle
		if err := contract.ParseJSONStrict(trimmed, &bundle); err != nil {
			return nil, err
		}
		return bundle.Recipes, nil
	}
	var single contract.Recipe
	if err := contract.ParseJSONStrict(trimmed, &single); err != nil {
		return nil, err
	}
	return []contract.Recipe{single}, nil
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".recipes-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func parseGCSURI(uri string) (bucket string, obj string, err error) {
	if !strings.HasPrefix(uri, "gs://") {
		return "", "", fmt.Errorf("invalid gs:// URI %q", uri)
	}
	rest := strings.TrimPrefix(uri, "gs://")
	bucket, obj, ok := strings.Cut(rest, "/")
	if !ok || bucket == "" || obj == "" {
		return "", "", fmt.Errorf("gs:// URI %q must specify bucket and object path", uri)
	}
	return bucket, obj, nil
}

func (c *ReloadableCatalog) gcsBaseURL() string {
	if s := strings.TrimRight(strings.TrimSpace(c.GCSBaseURL), "/"); s != "" {
		return s
	}
	return "https://storage.googleapis.com"
}

func (c *ReloadableCatalog) gcsTokenURL() string {
	if s := strings.TrimSpace(c.GCSTokenURL); s != "" {
		return s
	}
	return "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
}

func (c *ReloadableCatalog) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *ReloadableCatalog) accessTokenLocked(ctx context.Context) (string, error) {
	if envTok := strings.TrimSpace(os.Getenv("GCS_ACCESS_TOKEN")); envTok != "" {
		return envTok, nil
	}
	if c.cachedToken != "" && time.Now().Add(60*time.Second).Before(c.tokenExpiry) {
		return c.cachedToken, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.gcsTokenURL(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := c.httpClient().Do(req)
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
		return "", fmt.Errorf("invalid metadata token payload")
	}
	c.cachedToken = parsed.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	return c.cachedToken, nil
}
