package cdp

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/driver"
)

func TestSessionPoolSingleAttemptApprovalLatch(t *testing.T) {
	pool := NewSessionPool()
	dialCount := 0
	pool.discoverFn = func(_ context.Context, _ string, _ string) (string, DiscoverySource, error) {
		return "ws://127.0.0.1:1/devtools/browser/fake", SourceActivePort, nil
	}
	pool.connectFn = func(_ context.Context, _ string) (*Client, error) {
		dialCount++
		return nil, errors.New("user dismissed prompt")
	}

	ctx := context.Background()
	if _, err := pool.Acquire(ctx, "", ""); err == nil {
		t.Fatal("expected first Acquire to fail")
	}
	if dialCount != 1 {
		t.Fatalf("dialCount = %d, want 1", dialCount)
	}
	if !pool.ActivePortAttempted() {
		t.Fatal("expected ActivePortAttempted to be true")
	}

	// Second call must hit the latch without re-dialing Chrome.
	if _, err := pool.Acquire(ctx, "", ""); !errors.Is(err, ErrApprovalLatchActive) {
		t.Fatalf("expected ErrApprovalLatchActive, got %v", err)
	}
	if dialCount != 1 {
		t.Fatalf("latch failed to suppress second dial: dialCount = %d", dialCount)
	}

	// Explicit user reset permits one fresh attempt.
	pool.ResetApprovalLatch()
	if _, err := pool.Acquire(ctx, "", ""); err == nil {
		t.Fatal("expected Acquire after reset to run connectFn")
	}
	if dialCount != 2 {
		t.Fatalf("dialCount after reset = %d, want 2", dialCount)
	}
}

func TestBuildDiagnosticZipScrubsHTMLAndEnforcesAirGap(t *testing.T) {
	stepErr := &driver.StepExecError{
		StepID: "metrics",
		Err:    errors.New("step \"metrics\" extracted 0 item(s), expected >= 1"),
		DOMHTML: `<html><head><script>secretScript()</script><style>body{}</style></head>` +
			`<body><svg><rect/></svg><input value="my-secret-pass"><div class="card">Hello</div></body></html>`,
		ScreenshotPNG: []byte{0x89, 0x50, 0x4E, 0x47},
	}

	zipBytes, err := driver.BuildDiagnosticZip("demo", stepErr, nil)
	if err != nil {
		t.Fatalf("BuildDiagnosticZip failed: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("invalid zip: %v", err)
	}
	entries := make(map[string][]byte)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(rc)
		_ = rc.Close()
		entries[f.Name] = raw
	}
	for _, name := range []string{"error.json", "dom.html", "screenshot.png"} {
		if _, ok := entries[name]; !ok {
			t.Fatalf("missing %s in diagnostic zip", name)
		}
	}
	domStr := string(entries["dom.html"])
	for _, banned := range []string{"secretScript", "<style", "<svg", "my-secret-pass"} {
		if strings.Contains(domStr, banned) {
			t.Errorf("dom.html leaked %q", banned)
		}
	}

	// Air-gap violation must block zip creation.
	leakedErr := &driver.StepExecError{
		StepID:  "metrics",
		Err:     errors.New("selector failed"),
		DOMHTML: `<html><body>forbidden-client-token</body></html>`,
	}
	if _, err := driver.BuildDiagnosticZip("demo", leakedErr, []string{"forbidden-client-token"}); err == nil {
		t.Fatal("expected BuildDiagnosticZip to block air-gap violation")
	}
}

func TestExecuteStepAndFallbackCloseScratchTabOnFailure(t *testing.T) {
	var (
		mu             sync.Mutex
		closedTargetID string
		methods        []string
	)

	srv := newMockCDPServer(t, func(method string, params map[string]any) (any, error) {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()

		switch method {
		case "Target.getTargets":
			return map[string]any{"targetInfos": []TargetInfo{}}, nil
		case "Target.createTarget":
			return map[string]any{"targetId": "scratch-1"}, nil
		case "Target.attachToTarget":
			return map[string]any{"sessionId": "sess-1"}, nil
		case "Target.closeTarget":
			mu.Lock()
			if tid, ok := params["targetId"].(string); ok {
				closedTargetID = tid
			}
			mu.Unlock()
			return map[string]any{"success": true}, nil
		case "Page.captureScreenshot":
			return map[string]any{"data": base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4E, 0x47})}, nil
		case "Runtime.evaluate":
			expr, _ := params["expression"].(string)
			switch {
			case expr == "document.readyState":
				return map[string]any{"result": map[string]any{"type": "string", "value": "complete"}}, nil
			case strings.Contains(expr, "location.href || ''"):
				return map[string]any{"result": map[string]any{"type": "string", "value": "https://example.com/dash"}}, nil
			case strings.Contains(expr, "cloneClean"):
				return map[string]any{"result": map[string]any{"type": "string", "value": "<html><body><div class='v2'>Data</div></body></html>"}}, nil
			case strings.Contains(expr, "window.scrollBy"):
				return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
			case strings.Contains(expr, "evalItemRule"):
				return map[string]any{
					"result": map[string]any{
						"type": "object",
						"value": map[string]any{
							"title":        "Dash",
							"url":          "https://example.com/dash",
							"headings":     []string{"Dash"},
							"text_excerpt": "Data",
						},
					},
				}, nil
			default:
				return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, nil
			}
		default:
			return map[string]any{}, nil
		}
	})
	defer srv.Close()

	wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	client, err := Connect(context.Background(), wsURL)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	// Step expects >=1 item so it fails with StepExecError AND must still close scratch-1 immediately.
	step := contract.Step{
		ID:  "dash",
		URL: "https://example.com/dash",
		Expect: contract.Expectation{
			MinItems: 1,
		},
	}
	_, err = client.ExecuteStep(context.Background(), step)
	var execErr *driver.StepExecError
	if !errors.As(err, &execErr) {
		t.Fatalf("expected *driver.StepExecError, got %v", err)
	}
	if !strings.Contains(execErr.DOMHTML, "Data") {
		t.Fatalf("expected scrubbed DOMHTML on error, got %q", execErr.DOMHTML)
	}
	mu.Lock()
	gotClosed := closedTargetID
	mu.Unlock()
	if gotClosed != "scratch-1" {
		t.Fatalf("expected scratch tab to be closed immediately on failure, got %q", gotClosed)
	}

	// Tier-2 fallback also opens a fresh scratch tab, executes <=3 actions, and closes it.
	mu.Lock()
	closedTargetID = ""
	mu.Unlock()
	fbRes, err := client.ExecuteFallback(context.Background(), step, contract.FallbackPlan{
		AllowedDomain: "example.com",
		TargetURL:     "https://example.com/dash",
		Actions: []contract.FallbackAction{
			{Action: "scroll", DeltaY: 400},
			{Action: "wait", WaitMs: 10},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteFallback failed: %v", err)
	}
	if fbRes.ActionsRun != 2 {
		t.Fatalf("ActionsRun = %d, want 2", fbRes.ActionsRun)
	}
	if len(fbRes.ScreenshotPNG) == 0 || !strings.Contains(fbRes.DOMHTML, "Data") {
		t.Fatalf("expected screenshot + scrubbed HTML from fallback")
	}
	mu.Lock()
	gotClosed = closedTargetID
	mu.Unlock()
	if gotClosed != "scratch-1" {
		t.Fatalf("expected fallback scratch tab closed immediately, got %q", gotClosed)
	}
}

func newMockCDPServer(t *testing.T, handler func(method string, params map[string]any) (any, error)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Sec-WebSocket-Key")
		h := sha1.Sum([]byte(key + wsGUID))
		accept := base64.StdEncoding.EncodeToString(h[:])

		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("hijack unsupported")
			return
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+accept+"\r\n\r\n")
		br := bufrw.Reader
		for {
			payload, err := readServerWSFrame(br)
			if err != nil {
				return
			}
			var req struct {
				ID     int64          `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal(payload, &req) != nil {
				continue
			}
			res, callErr := handler(req.Method, req.Params)
			resp := map[string]any{"id": req.ID}
			if callErr != nil {
				resp["error"] = map[string]any{"code": -32000, "message": callErr.Error()}
			} else {
				resp["result"] = res
			}
			raw, _ := json.Marshal(resp)
			if err := writeServerWSFrame(conn, raw); err != nil {
				return
			}
		}
	}))
}

func readServerWSFrame(br *bufio.Reader) ([]byte, error) {
	var b2 [2]byte
	if _, err := io.ReadFull(br, b2[:]); err != nil {
		return nil, err
	}
	opcode := b2[0] & 0x0f
	if opcode == 0x8 {
		return nil, io.EOF
	}
	masked := (b2[1] & 0x80) != 0
	length := uint64(b2[1] & 0x7f)
	if length == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	} else if length == 127 {
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(br, mask[:]); err != nil {
			return nil, err
		}
	}
	data := make([]byte, int(length))
	if _, err := io.ReadFull(br, data); err != nil {
		return nil, err
	}
	if masked {
		for i := range data {
			data[i] ^= mask[i%4]
		}
	}
	return data, nil
}

func writeServerWSFrame(conn net.Conn, payload []byte) error {
	var hdr [10]byte
	hdr[0] = 0x81
	n := len(payload)
	off := 2
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 65535:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		off = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		off = 10
	}
	if _, err := conn.Write(hdr[:off]); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}
