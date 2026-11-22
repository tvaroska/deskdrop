package bus

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
)

func TestDesktopClientRetries502503504AcrossGETAndMultipart(t *testing.T) {
	var (
		statusHits  atomic.Int32
		recipesHits atomic.Int32
		releaseHits atomic.Int32
		failHits    atomic.Int32
		reportHits  atomic.Int32

		mu          sync.Mutex
		idempKeys   []string
		uploadedZip []byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/status":
			n := statusHits.Add(1)
			if n == 1 {
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			if n == 2 {
				http.Error(w, "pod recreating", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"server_version":"v1.1.1"}`))

		case "/v1/recipes":
			n := recipesHits.Add(1)
			if n <= 2 {
				http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"recipes":[{"name":"status-dashboard","topic":"ops","steps":[{"id":"s1","url":"https://status.example.com"}]}]}`))

		case "/v1/release/deskdrop.exe":
			n := releaseHits.Add(1)
			if n == 1 {
				http.Error(w, "service unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("X-DeskDrop-Version", "v1.1.1")
			_, _ = w.Write([]byte("MZ-mock-pe-binary"))

		case "/v1/tasks/task_1/fail":
			n := failHits.Add(1)
			if err := r.ParseMultipartForm(4 * 1024 * 1024); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f, _, ferr := r.FormFile("bundle")
			if ferr != nil {
				http.Error(w, "missing bundle", http.StatusBadRequest)
				return
			}
			raw, _ := io.ReadAll(f)
			_ = f.Close()
			if n <= 2 {
				http.Error(w, "rwo pvc reattaching", http.StatusServiceUnavailable)
				return
			}
			mu.Lock()
			uploadedZip = raw
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"diagnostic_id":"diag_9","triage":{"code":"SELECTOR_DRIFT","action":"diagnostic_escalation"}}`))

		case "/v1/drops":
			n := reportHits.Add(1)
			mu.Lock()
			idempKeys = append(idempKeys, r.Header.Get("Idempotency-Key"))
			mu.Unlock()
			if n == 1 {
				http.Error(w, "rollout 503", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"drop_id":"drop_1","short_id":"abc123","message_id":"msg_1"}`))

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := NewDesktopClient(contract.Config{
		From:         "win-pc",
		ServiceURL:   srv.URL,
		ServiceToken: "desk-tok",
	})
	client.RetryBaseDelay = 2 * time.Millisecond

	ctx := context.Background()

	st, err := client.GetDesktopStatus(ctx)
	if err != nil {
		t.Fatalf("GetDesktopStatus failed across 502/503 retry: %v", err)
	}
	if st["server_version"] != "v1.1.1" || statusHits.Load() != 3 {
		t.Fatalf("unexpected status result=%v hits=%d", st, statusHits.Load())
	}

	rcps, err := client.ListApprovedRecipes(ctx)
	if err != nil || len(rcps) != 1 || recipesHits.Load() != 3 {
		t.Fatalf("ListApprovedRecipes err=%v len=%d hits=%d", err, len(rcps), recipesHits.Load())
	}

	bin, ver, err := client.DownloadReleaseBinary(ctx, "windows")
	if err != nil || ver != "v1.1.1" || string(bin) != "MZ-mock-pe-binary" || releaseHits.Load() != 2 {
		t.Fatalf("DownloadReleaseBinary err=%v ver=%q bin=%q hits=%d", err, ver, string(bin), releaseHits.Load())
	}

	wantZip := []byte("PK\x03\x04mock-zip-payload-intact-across-retries")
	triage, diagID, err := client.FailTask(ctx, "task_1", contract.FailReportMetadata{
		Topic:     "ops",
		Recipe:    "status-dashboard",
		StepID:    "s1",
		LandedURL: "https://status.example.com",
		Error:     "step \"s1\" extracted 0 item(s)",
	}, wantZip)
	if err != nil {
		t.Fatalf("FailTask multipart retry failed: %v", err)
	}
	if diagID != "diag_9" || triage.Code != contract.ErrorCodeSelectorDrift || failHits.Load() != 3 {
		t.Fatalf("unexpected FailTask result triage=%+v diag=%q hits=%d", triage, diagID, failHits.Load())
	}
	mu.Lock()
	gotZip := append([]byte(nil), uploadedZip...)
	mu.Unlock()
	if !bytes.Equal(gotZip, wantZip) {
		t.Fatalf("multipart zip corrupted on retry: got %q, want %q", gotZip, wantZip)
	}

	sent, err := client.SendReport(ctx, "all", contract.DropReport{
		SchemaVersion: "2.0",
		Recipe:        "status-dashboard",
		Topic:         "ops",
		Host:          "win-pc",
		CapturedAtUTC: "2026-11-22T00:00:00Z",
		Steps:         []contract.StepResult{{StepID: "s1", URL: "https://status.example.com"}},
	})
	if err != nil || sent.ShortID != "abc123" || reportHits.Load() != 2 {
		t.Fatalf("SendReport err=%v sent=%+v hits=%d", err, sent, reportHits.Load())
	}
	mu.Lock()
	keys := append([]string(nil), idempKeys...)
	mu.Unlock()
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("expected identical non-empty Idempotency-Key across retries, got %v", keys)
	}
}

func TestDesktopClientDoesNotRetry4xxClientErrors(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, `{"ok":false,"error":{"code":"NOT_FOUND","message":"release asset not found"}}`, http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewDesktopClient(contract.Config{
		ServiceURL:   srv.URL,
		ServiceToken: "desk-tok",
	})
	client.RetryBaseDelay = 2 * time.Millisecond

	if _, _, err := client.DownloadReleaseBinary(context.Background(), "linux"); err == nil {
		t.Fatal("expected 404 error")
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 404 to fail immediately without retry, got %d hits", hits.Load())
	}
}
