package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/healer"
	"github.com/tvaroska/deskdrop/internal/recipes"
	"github.com/tvaroska/deskdrop/internal/server"
)

var version = "v1.4.3"

const defaultWriteTimeout = 180 * time.Second

type serverRuntime struct {
	httpSrv      *http.Server
	svc          *server.Service
	store        server.Store
	storeBackend string
	blobBackend  string
	healerActive bool
}

func (rt *serverRuntime) Close() error {
	if rt.svc != nil {
		_ = rt.svc.Close()
	}
	if rt.store != nil {
		return rt.store.Close()
	}
	return nil
}

func buildServerRuntime(getenv func(string) string, loadLocalTerms func() []string) (*serverRuntime, error) {
	getOrDefault := func(key, fallback string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return fallback
	}

	listenAddr := getOrDefault("LISTEN_ADDR", ":8080")
	storeBackend := strings.ToLower(getOrDefault("STORE_BACKEND", "file"))
	stateFile := getOrDefault("STATE_FILE", "/data/state.json")
	blobBackend := strings.ToLower(getOrDefault("STORAGE_BACKEND", "local"))
	recipesURI := strings.TrimSpace(getenv("RECIPES_URI"))

	var (
		store server.Store
		err   error
	)
	switch storeBackend {
	case "memory":
		store = server.NewMemoryStore()
	case "file":
		store, err = server.OpenFileStore(stateFile)
		if err != nil {
			return nil, fmt.Errorf("open file store (%s) failed: %w", stateFile, err)
		}
	default:
		return nil, fmt.Errorf("unsupported STORE_BACKEND=%q (expected file or memory)", storeBackend)
	}

	var blobs server.BlobStore
	switch blobBackend {
	case "memory":
		blobs = server.NewMemoryBlobStore()
	case "local":
		blobs = &server.DirBlobStore{Root: getOrDefault("LOCAL_BLOB_DIR", "/data/blobs")}
	case "gcs":
		bucket := strings.TrimSpace(getenv("GCS_BUCKET"))
		if bucket == "" {
			_ = store.Close()
			return nil, errors.New("GCS_BUCKET is required when STORAGE_BACKEND=gcs")
		}
		prefix := getOrDefault("GCS_PREFIX", "desktop")
		blobs = server.NewGCSBlobStore(bucket, prefix)
	default:
		_ = store.Close()
		return nil, fmt.Errorf("unsupported STORAGE_BACKEND=%q (expected gcs, local, or memory)", blobBackend)
	}

	catalog := recipes.NewReloadableCatalog(recipesURI)
	if recipesURI != "" {
		loadCtx, loadCancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := catalog.Reload(loadCtx); err != nil {
			log.Printf("WARN initial RECIPES_URI reload (%s): %v", recipesURI, err)
		}
		loadCancel()
	}

	var forbidden []string
	if loadLocalTerms != nil {
		forbidden = append(forbidden, loadLocalTerms()...)
	}
	if raw := strings.TrimSpace(getenv("EXTRA_FORBIDDEN_TERMS")); raw != "" {
		for _, p := range strings.Split(raw, ",") {
			if s := strings.TrimSpace(p); s != "" {
				forbidden = append(forbidden, s)
			}
		}
	}

	var driftHealer server.ExceptionHealer
	healerEnabled := strings.ToLower(strings.TrimSpace(getenv("HEALER_ENABLED")))
	gcpProject := strings.TrimSpace(getenv("GOOGLE_CLOUD_PROJECT"))
	geminiAPIKey := strings.TrimSpace(getenv("GEMINI_API_KEY"))
	if healerEnabled != "false" && (healerEnabled == "true" || gcpProject != "" || geminiAPIKey != "") {
		hCtx, hCancel := context.WithTimeout(context.Background(), 15*time.Second)
		h, herr := healer.New(hCtx, healer.Config{
			ModelName:      getOrDefault("HEALER_MODEL", healer.DefaultModelName),
			Project:        gcpProject,
			Location:       getOrDefault("GOOGLE_CLOUD_LOCATION", healer.DefaultLocation),
			APIKey:         geminiAPIKey,
			ForbiddenTerms: forbidden,
		})
		hCancel()
		if herr != nil {
			log.Printf("WARN exception healer disabled: %v", herr)
		} else {
			driftHealer = h
		}
	}

	cfg := server.ServerConfig{
		Version:            version,
		PublicBaseURL:      strings.TrimSpace(getenv("PUBLIC_BASE_URL")),
		DesktopClientToken: strings.TrimSpace(getenv("DESKTOP_CLIENT_TOKEN")),
		AgentBusToken:      strings.TrimSpace(getenv("AGENT_BUS_TOKEN")),
		AdminToken:         strings.TrimSpace(getenv("ADMIN_TOKEN")),
		DiscordWebhookURL:  strings.TrimSpace(getenv("DISCORD_WEBHOOK_URL")),
		ReleaseDir:         getOrDefault("RELEASE_DIR", "/release"),
		ForbiddenTerms:     forbidden,
	}
	if cfg.DesktopClientToken == "" && cfg.AgentBusToken == "" && cfg.AdminToken == "" {
		_ = store.Close()
		return nil, errors.New("at least one of DESKTOP_CLIENT_TOKEN, AGENT_BUS_TOKEN, or ADMIN_TOKEN must be configured")
	}

	writeTimeout := parseWriteTimeout(getenv("WRITE_TIMEOUT_SECONDS"))
	svc := server.NewService(cfg, store, blobs, catalog, driftHealer)
	httpSrv := &http.Server{
		Addr:              listenAddr,
		Handler:           svc,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
	}
	return &serverRuntime{
		httpSrv:      httpSrv,
		svc:          svc,
		store:        store,
		storeBackend: storeBackend,
		blobBackend:  blobBackend,
		healerActive: driftHealer != nil,
	}, nil
}

func parseWriteTimeout(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return defaultWriteTimeout
	}
	secs, err := strconv.Atoi(s)
	if err != nil || secs < 30 || secs > 600 {
		return defaultWriteTimeout
	}
	return time.Duration(secs) * time.Second
}

func main() {
	rt, err := buildServerRuntime(os.Getenv, contract.LoadLocalGuardTerms)
	if err != nil {
		log.Fatal(err)
	}
	defer rt.store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("deskdrop server (%s) listening on %s (store=%s, blobs=%s, healer=%v, write_timeout=%s)",
			version, rt.httpSrv.Addr, rt.storeBackend, rt.blobBackend, rt.healerActive, rt.httpSrv.WriteTimeout)
		if err := rt.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server error: %v", err)
		}
	}()

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = rt.httpSrv.Shutdown(shutCtx)
	if rt.svc != nil {
		_ = rt.svc.Shutdown(shutCtx)
	}
}
