package main_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
	"github.com/tvaroska/deskdrop/internal/verify"
)

const e2ePNG1x1Base64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

// mockCDPServer speaks /json/version and RFC 6455 CDP JSON-RPC over loopback so the compiled
// deskdrop binary can run full end-to-end extraction and drift diagnostics without Chrome.
type mockCDPServer struct {
	mu     sync.Mutex
	html   string
	url    string
	step   contract.Step
	server *httptest.Server
}

func startMockCDPServer(t *testing.T, step contract.Step, targetURL string, initialHTML string) *mockCDPServer {
	t.Helper()
	m := &mockCDPServer{
		html: initialHTML,
		url:  targetURL,
		step: step,
	}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json/version":
			wsURL := "ws://" + r.Host + "/devtools/browser"
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(`{"Browser":"MockChrome/144","webSocketDebuggerUrl":%q}`, wsURL)))
		case "/devtools/browser":
			m.serveWS(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockCDPServer) setHTML(html string) {
	m.mu.Lock()
	m.html = html
	m.mu.Unlock()
}

func (m *mockCDPServer) serveWS(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing websocket key", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(h[:])
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n")
	if err := rw.Flush(); err != nil {
		return
	}

	br := rw.Reader
	for {
		frame, rerr := readE2EWSFrame(br)
		if rerr != nil {
			return
		}
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(frame, &req); err != nil {
			continue
		}
		result := m.handleCDPMethod(req.Method, req.Params)
		respBytes, _ := json.Marshal(map[string]any{
			"id":     req.ID,
			"result": result,
		})
		if err := writeE2EWSFrame(conn, respBytes); err != nil {
			return
		}
	}
}

func (m *mockCDPServer) handleCDPMethod(method string, rawParams json.RawMessage) any {
	m.mu.Lock()
	html := m.html
	targetURL := m.url
	step := m.step
	m.mu.Unlock()

	switch method {
	case "Target.getTargets":
		return map[string]any{
			"targetInfos": []map[string]any{
				{"targetId": "tab-1", "type": "page", "url": targetURL, "title": "Status Dashboard"},
			},
		}
	case "Target.createTarget":
		return map[string]any{"targetId": "scratch-1"}
	case "Target.attachToTarget":
		return map[string]any{"sessionId": "sess-1"}
	case "Page.captureScreenshot":
		return map[string]any{"data": e2ePNG1x1Base64}
	case "Accessibility.getFullAXTree":
		return map[string]any{
			"nodes": []map[string]any{
				{"nodeId": "1", "role": map[string]any{"value": "RootWebArea"}, "name": map[string]any{"value": "Status Dashboard"}},
			},
		}
	case "Runtime.evaluate":
		var params struct {
			Expression string `json:"expression"`
		}
		_ = json.Unmarshal(rawParams, &params)
		expr := params.Expression
		switch {
		case strings.Contains(expr, "document.readyState"):
			return map[string]any{"result": map[string]any{"value": "complete"}}
		case strings.Contains(expr, "cloneNode(true)"):
			return map[string]any{"result": map[string]any{"value": contract.ScrubHTML(html)}}
		case strings.Contains(expr, "evalFieldRule"):
			extracted, _ := verify.ExtractStepFromHTML(step, html, targetURL)
			return map[string]any{"result": map[string]any{"value": extracted}}
		case strings.Contains(expr, "location.href"):
			return map[string]any{"result": map[string]any{"value": targetURL}}
		default:
			return map[string]any{"result": map[string]any{"value": true}}
		}
	default:
		return map[string]any{}
	}
}

func readE2EWSFrame(br *bufio.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return nil, err
	}
	masked := (hdr[1] & 0x80) != 0
	length := uint64(hdr[1] & 0x7f)
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

func writeE2EWSFrame(conn net.Conn, payload []byte) error {
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

func reserveLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func waitHealthy(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/desktop/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("deskdrop-server at %s did not become healthy within 10s", baseURL)
}

func e2eAPIRequest(t *testing.T, method, urlStr, token string, body []byte) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, urlStr, r)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, urlStr, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func TestE2ECompiledCLIToRelayLoopback(t *testing.T) {
	repoRoot := findRepoRoot(t)
	tmp := t.TempDir()
	isolatedHome := filepath.Join(tmp, "home")
	if err := os.MkdirAll(isolatedHome, 0o700); err != nil {
		t.Fatal(err)
	}

	// 1. Build real deskdrop CLI and deskdrop-server binaries.
	serverBin := filepath.Join(tmp, "deskdrop-server")
	cliBin := filepath.Join(tmp, "deskdrop")
	for _, target := range []struct {
		out string
		pkg string
	}{
		{serverBin, "./cmd/server"},
		{cliBin, "./cmd/deskdrop"},
	} {
		buildCmd := exec.Command("go", "build", "-o", target.out, target.pkg)
		buildCmd.Dir = repoRoot
		if out, err := buildCmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s failed: %v\n%s", target.pkg, err, string(out))
		}
	}

	// 2. Load drift benchmark recipe & HTML fixtures (v1 healthy + v2 drifted).
	v1RecipeRaw, err := os.ReadFile(filepath.Join(repoRoot, "testdata", "drift", "status_v1_recipe.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rcp contract.Recipe
	if err := contract.ParseJSONStrict(v1RecipeRaw, &rcp); err != nil {
		t.Fatal(err)
	}
	// Clear wait_selector so the drift fixture immediately runs extraction and produces SELECTOR_DRIFT.
	rcp.Steps[0].WaitSelector = ""
	rcp.Steps[0].TimeoutMs = 5000

	recipesPath := filepath.Join(tmp, "recipes.json")
	rcpBytes, _ := json.MarshalIndent(rcp, "", "  ")
	if err := os.WriteFile(recipesPath, rcpBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	v1HTML, err := os.ReadFile(filepath.Join(repoRoot, "testdata", "drift", "status_v1.html"))
	if err != nil {
		t.Fatal(err)
	}
	v2HTML, err := os.ReadFile(filepath.Join(repoRoot, "testdata", "drift", "status_v2_drifted.html"))
	if err != nil {
		t.Fatal(err)
	}

	mockCDP := startMockCDPServer(t, rcp.Steps[0], "https://status.example.com/overview", string(v1HTML))

	statePath := filepath.Join(tmp, "state.json")
	blobDir := filepath.Join(tmp, "blobs")

	startRelay := func(addr string) (*exec.Cmd, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(ctx, serverBin)
		cmd.Env = append(os.Environ(),
			"HOME="+isolatedHome,
			"LISTEN_ADDR="+addr,
			"STORE_BACKEND=file",
			"STATE_FILE="+statePath,
			"STORAGE_BACKEND=local",
			"LOCAL_BLOB_DIR="+blobDir,
			"RECIPES_URI="+recipesPath,
			"HEALER_ENABLED=false",
			"DESKTOP_CLIENT_TOKEN=e2e-desk-tok",
			"AGENT_BUS_TOKEN=e2e-agent-tok",
			"ADMIN_TOKEN=e2e-admin-tok",
		)
		var logs bytes.Buffer
		cmd.Stdout = &logs
		cmd.Stderr = &logs
		if err := cmd.Start(); err != nil {
			cancel()
			t.Fatalf("start deskdrop-server: %v", err)
		}
		t.Cleanup(func() {
			cancel()
			_ = cmd.Wait()
		})
		waitHealthy(t, "http://"+addr)
		return cmd, cancel
	}

	addr := reserveLoopbackAddr(t)
	srvCmd, stopRelay := startRelay(addr)
	relayBase := "http://" + addr + "/desktop"

	cfgPath := filepath.Join(tmp, "deskdrop.json")
	cfgJSON, _ := json.Marshal(map[string]any{
		"from":          "e2e-worker",
		"service_url":   relayBase,
		"service_token": "e2e-desk-tok",
		"cdp_url":       mockCDP.server.URL,
		"debug_dir":     filepath.Join(tmp, "debug"),
	})
	if err := os.WriteFile(cfgPath, cfgJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	// 3. Happy-path E2E: enqueue -> compiled `deskdrop sync --once` -> claim -> drop -> bus message.
	code, enqRaw := e2eAPIRequest(t, http.MethodPost, relayBase+"/v1/tasks", "e2e-agent-tok",
		[]byte(`{"recipe":"service-status-board","requested_by":"e2e-agent","notify_to":"e2e-agent"}`))
	if code != http.StatusCreated {
		t.Fatalf("enqueue task 1 = %d (%s)", code, string(enqRaw))
	}
	var enq1 struct {
		Task contract.Task `json:"task"`
	}
	_ = json.Unmarshal(enqRaw, &enq1)

	syncCmd1 := exec.Command(cliBin, "sync", "--config", cfgPath)
	syncCmd1.Env = append(os.Environ(), "HOME="+isolatedHome)
	if out, err := syncCmd1.CombinedOutput(); err != nil {
		t.Fatalf("deskdrop sync (v1) failed: %v\n%s", err, string(out))
	}

	code, taskRaw := e2eAPIRequest(t, http.MethodGet, relayBase+"/v1/tasks/"+enq1.Task.ID, "e2e-agent-tok", nil)
	if code != http.StatusOK {
		t.Fatalf("GET task 1 = %d (%s)", code, string(taskRaw))
	}
	var task1 contract.Task
	_ = json.Unmarshal(taskRaw, &task1)
	if task1.Status != contract.TaskStatusSucceeded || task1.DropID == "" {
		t.Fatalf("expected task 1 succeeded with drop_id, got %+v", task1)
	}

	code, dropRaw := e2eAPIRequest(t, http.MethodGet, relayBase+"/v1/drops/"+task1.DropID, "e2e-agent-tok", nil)
	if code != http.StatusOK {
		t.Fatalf("GET drop 1 = %d (%s)", code, string(dropRaw))
	}
	var drop1 contract.DropRecord
	_ = json.Unmarshal(dropRaw, &drop1)
	if len(drop1.Report.Steps) != 1 || len(drop1.Report.Steps[0].Items) != 3 {
		t.Fatalf("expected 3 extracted items in drop 1, got %+v", drop1.Report.Steps)
	}

	code, msgRaw := e2eAPIRequest(t, http.MethodGet, relayBase+"/v1/messages?for=e2e-agent", "e2e-agent-tok", nil)
	if code != http.StatusOK || !strings.Contains(string(msgRaw), "service-status-board") {
		t.Fatalf("expected bus drop message for e2e-agent, got %d %s", code, string(msgRaw))
	}

	// 4. Drift E2E: switch mock CDP to drifted v2 HTML -> compiled CLI uploads diagnostic bundle -> failed_diagnostic + bus alert.
	mockCDP.setHTML(string(v2HTML))
	code, enq2Raw := e2eAPIRequest(t, http.MethodPost, relayBase+"/v1/tasks", "e2e-agent-tok",
		[]byte(`{"recipe":"service-status-board","requested_by":"e2e-agent","notify_to":"e2e-agent"}`))
	if code != http.StatusCreated {
		t.Fatalf("enqueue task 2 = %d (%s)", code, string(enq2Raw))
	}
	var enq2 struct {
		Task contract.Task `json:"task"`
	}
	_ = json.Unmarshal(enq2Raw, &enq2)

	syncCmd2 := exec.Command(cliBin, "sync", "--config", cfgPath)
	syncCmd2.Env = append(os.Environ(), "HOME="+isolatedHome)
	if out, err := syncCmd2.CombinedOutput(); err != nil {
		t.Fatalf("deskdrop sync (v2 drift) failed: %v\n%s", err, string(out))
	}

	code, task2Raw := e2eAPIRequest(t, http.MethodGet, relayBase+"/v1/tasks/"+enq2.Task.ID, "e2e-agent-tok", nil)
	if code != http.StatusOK {
		t.Fatalf("GET task 2 = %d (%s)", code, string(task2Raw))
	}
	var task2 contract.Task
	_ = json.Unmarshal(task2Raw, &task2)
	if task2.Status != contract.TaskStatusFailedDiagnostic || task2.DiagnosticID == "" {
		t.Fatalf("expected task 2 failed_diagnostic with diagnostic_id, got %+v", task2)
	}

	code, zipBytes := e2eAPIRequest(t, http.MethodGet, relayBase+"/v1/diagnostics/"+task2.DiagnosticID+"/bundle.zip", "e2e-agent-tok", nil)
	if code != http.StatusOK || len(zipBytes) < 4 || string(zipBytes[:2]) != "PK" {
		t.Fatalf("GET diagnostic zip = %d len=%d, want 200 PK zip", code, len(zipBytes))
	}

	// 5. FileStore restart persistence: stop relay, start fresh deskdrop-server on same state.json, verify drop + diagnostic + tasks survive.
	stopRelay()
	_ = srvCmd.Wait()

	addr2 := reserveLoopbackAddr(t)
	_, _ = startRelay(addr2)
	relayBase2 := "http://" + addr2 + "/desktop"

	code, persistedDrop := e2eAPIRequest(t, http.MethodGet, relayBase2+"/v1/drops/"+task1.DropID, "e2e-agent-tok", nil)
	if code != http.StatusOK || !strings.Contains(string(persistedDrop), "service-status-board") {
		t.Fatalf("persisted drop missing after restart: %d %s", code, string(persistedDrop))
	}
	code, persistedZip := e2eAPIRequest(t, http.MethodGet, relayBase2+"/v1/diagnostics/"+task2.DiagnosticID+"/bundle.zip", "e2e-agent-tok", nil)
	if code != http.StatusOK || len(persistedZip) != len(zipBytes) {
		t.Fatalf("persisted diagnostic zip missing after restart: %d len=%d (want %d)", code, len(persistedZip), len(zipBytes))
	}
}
