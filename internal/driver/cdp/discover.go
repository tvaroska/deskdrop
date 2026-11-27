package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DiscoverySource distinguishes quiet HTTP port discovery from DevToolsActivePort discovery
// (which can prompt Chrome 144+ "Allow remote debugging?" dialogs).
type DiscoverySource string

const (
	SourceExplicit   DiscoverySource = "explicit"
	SourceHTTP9222   DiscoverySource = "http_9222"
	SourceActivePort DiscoverySource = "devtools_active_port"
)

// DiscoverBrowserWS resolves a browser-level CDP WebSocket URL from explicit config,
// DevToolsActivePort files, or a dedicated http://127.0.0.1:9222/json/version endpoint.
func DiscoverBrowserWS(ctx context.Context, cdpURL string, userDataDir string) (string, error) {
	wsURL, _, err := DiscoverBrowserWSWithSource(ctx, cdpURL, userDataDir)
	return wsURL, err
}

// DiscoverBrowserWSWithSource resolves the CDP WebSocket URL and reports how it was discovered.
func DiscoverBrowserWSWithSource(ctx context.Context, cdpURL string, userDataDir string) (string, DiscoverySource, error) {
	u := strings.TrimSpace(cdpURL)
	if strings.HasPrefix(u, "ws://") {
		return u, SourceExplicit, nil
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		ws, err := resolveHTTPVersionEndpoint(ctx, u)
		return ws, SourceExplicit, err
	}

	candidates := candidateUserDataDirs(userDataDir)
	for _, dir := range candidates {
		wsURL, err := readDevToolsActivePort(dir)
		if err == nil && wsURL != "" && isWSReachable(wsURL) {
			return wsURL, SourceActivePort, nil
		}
	}

	if wsURL, err := resolveHTTPVersionEndpoint(ctx, "http://127.0.0.1:9222"); err == nil && wsURL != "" {
		return wsURL, SourceHTTP9222, nil
	}

	return "", "", errors.New(
		"could not locate a running Chrome/Edge CDP endpoint: enable chrome://inspect/#remote-debugging or run 'deskdrop open-browser'",
	)
}

func isWSReachable(wsURL string) bool {
	parsed, err := url.Parse(wsURL)
	if err != nil || parsed.Host == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", parsed.Host, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func resolveHTTPVersionEndpoint(ctx context.Context, base string) (string, error) {
	endpoint := strings.TrimRight(base, "/") + "/json/version"
	cctx, cancel := context.WithTimeout(ctx, 900*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, endpoint)
	}

	var payload struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("empty webSocketDebuggerUrl from %s", endpoint)
	}
	return payload.WebSocketDebuggerURL, nil
}

func readDevToolsActivePort(userDataDir string) (string, error) {
	if strings.TrimSpace(userDataDir) == "" {
		return "", errors.New("empty dir")
	}
	p := filepath.Join(userDataDir, "DevToolsActivePort")
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	if len(lines) < 2 {
		return "", fmt.Errorf("malformed DevToolsActivePort in %s", p)
	}
	portStr := strings.TrimSpace(lines[0])
	wsPath := strings.TrimSpace(lines[1])
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", fmt.Errorf("invalid port %q in %s", portStr, p)
	}
	if !strings.HasPrefix(wsPath, "/") {
		wsPath = "/" + wsPath
	}
	return fmt.Sprintf("ws://127.0.0.1:%d%s", port, wsPath), nil
}

func candidateUserDataDirs(explicit string) []string {
	var dirs []string
	if strings.TrimSpace(explicit) != "" {
		dirs = append(dirs, explicit)
	}

	switch runtime.GOOS {
	case "windows":
		localApp := os.Getenv("LOCALAPPDATA")
		if localApp != "" {
			dirs = append(dirs,
				filepath.Join(localApp, "Google", "Chrome", "User Data"),
				filepath.Join(localApp, "Microsoft", "Edge", "User Data"),
				filepath.Join(localApp, "BraveSoftware", "Brave-Browser", "User Data"),
				filepath.Join(localApp, "DeskDrop", "chrome-profile"),
				filepath.Join(localApp, "BrowserDrop", "chrome-profile"),
			)
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			dirs = append(dirs,
				filepath.Join(home, "Library", "Application Support", "Google", "Chrome"),
				filepath.Join(home, "Library", "Application Support", "Microsoft Edge"),
				filepath.Join(home, "Library", "Application Support", "DeskDrop", "chrome-profile"),
			)
		}
	default:
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			dirs = append(dirs,
				filepath.Join(home, ".config", "google-chrome"),
				filepath.Join(home, ".config", "chromium"),
				filepath.Join(home, ".config", "microsoft-edge"),
				filepath.Join(home, ".local", "share", "deskdrop", "chrome-profile"),
			)
		}
	}
	return dirs
}
