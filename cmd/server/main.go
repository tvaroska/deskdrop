package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tvaroska/deskdrop/internal/healer"
	"github.com/tvaroska/deskdrop/internal/recipes"
	"github.com/tvaroska/deskdrop/internal/server"
)

var version = "v1.0.0"

func main() {
	listenAddr := envOrDefault("LISTEN_ADDR", ":8080")
	storeBackend := strings.ToLower(envOrDefault("STORE_BACKEND", "file"))
	stateFile := envOrDefault("STATE_FILE", "/data/state.json")
	blobBackend := strings.ToLower(envOrDefault("STORAGE_BACKEND", "local"))
	recipesURI := strings.TrimSpace(os.Getenv("RECIPES_URI"))

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
			log.Fatalf("open file store (%s) failed: %v", stateFile, err)
		}
	default:
		log.Fatalf("unsupported STORE_BACKEND=%q (expected file or memory)", storeBackend)
	}
	defer store.Close()

	var blobs server.BlobStore
	switch blobBackend {
	case "memory":
		blobs = server.NewMemoryBlobStore()
	case "local":
		blobs = &server.DirBlobStore{Root: envOrDefault("LOCAL_BLOB_DIR", "/data/blobs")}
	case "gcs":
		bucket := strings.TrimSpace(os.Getenv("GCS_BUCKET"))
		if bucket == "" {
			log.Fatal("GCS_BUCKET is required when STORAGE_BACKEND=gcs")
		}
		prefix := envOrDefault("GCS_PREFIX", "desktop")
		blobs = server.NewGCSBlobStore(bucket, prefix)
	default:
		log.Fatalf("unsupported STORAGE_BACKEND=%q (expected gcs, local, or memory)", blobBackend)
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
	if raw := strings.TrimSpace(os.Getenv("EXTRA_FORBIDDEN_TERMS")); raw != "" {
		for _, p := range strings.Split(raw, ",") {
			if s := strings.TrimSpace(p); s != "" {
				forbidden = append(forbidden, s)
			}
		}
	}

	var driftHealer server.ExceptionHealer
	healerEnabled := strings.ToLower(strings.TrimSpace(os.Getenv("HEALER_ENABLED")))
	gcpProject := strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT"))
	geminiAPIKey := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	if healerEnabled != "false" && (healerEnabled == "true" || gcpProject != "" || geminiAPIKey != "") {
		hCtx, hCancel := context.WithTimeout(context.Background(), 15*time.Second)
		h, herr := healer.New(hCtx, healer.Config{
			ModelName:      envOrDefault("HEALER_MODEL", healer.DefaultModelName),
			Project:        gcpProject,
			Location:       envOrDefault("GOOGLE_CLOUD_LOCATION", healer.DefaultLocation),
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
		PublicBaseURL:      strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL")),
		DesktopClientToken: strings.TrimSpace(os.Getenv("DESKTOP_CLIENT_TOKEN")),
		AgentBusToken:      strings.TrimSpace(os.Getenv("AGENT_BUS_TOKEN")),
		AdminToken:         strings.TrimSpace(os.Getenv("ADMIN_TOKEN")),
		DiscordWebhookURL:  strings.TrimSpace(os.Getenv("DISCORD_WEBHOOK_URL")),
		ReleaseDir:         envOrDefault("RELEASE_DIR", "/release"),
		ForbiddenTerms:     forbidden,
	}
	if cfg.DesktopClientToken == "" && cfg.AgentBusToken == "" && cfg.AdminToken == "" {
		log.Fatal("at least one of DESKTOP_CLIENT_TOKEN, AGENT_BUS_TOKEN, or ADMIN_TOKEN must be configured")
	}

	svc := server.NewService(cfg, store, blobs, catalog, driftHealer)
	httpSrv := &http.Server{
		Addr:              listenAddr,
		Handler:           svc,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("deskdrop server (%s) listening on %s (store=%s, blobs=%s, healer=%v)",
			version, listenAddr, storeBackend, blobBackend, driftHealer != nil)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server error: %v", err)
		}
	}()

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
