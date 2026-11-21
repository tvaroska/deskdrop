package cdp

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// ErrApprovalLatchActive is returned when a background loop tries to re-dial Chrome 144+'s
// DevToolsActivePort after a prior attempt without an explicit user reset, preventing popup spam.
var ErrApprovalLatchActive = errors.New("CDP DevToolsActivePort approval latch active: reconnect explicitly to re-prompt Chrome")

// SessionPool maintains a persistent browser-level CDP WebSocket connection with a single-attempt
// approval latch for Chrome 144+ DevToolsActivePort endpoints.
type SessionPool struct {
	mu                  sync.Mutex
	client              *Client
	wsURL               string
	activePortAttempted bool
	discoverFn          func(ctx context.Context, cdpURL string, userDataDir string) (string, DiscoverySource, error)
	connectFn           func(ctx context.Context, wsURL string) (*Client, error)
}

// NewSessionPool creates an empty persistent CDP session pool.
func NewSessionPool() *SessionPool {
	return &SessionPool{
		discoverFn: DiscoverBrowserWSWithSource,
		connectFn:  Connect,
	}
}

// ResetApprovalLatch allows a subsequent Acquire call to dial DevToolsActivePort again
// (invoked strictly on explicit user action such as clicking "Connect to Open Chrome").
func (p *SessionPool) ResetApprovalLatch() {
	p.mu.Lock()
	p.activePortAttempted = false
	p.mu.Unlock()
}

// ActivePortAttempted reports whether the single-attempt latch has already fired.
func (p *SessionPool) ActivePortAttempted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.activePortAttempted
}

// CurrentClient returns the active pooled CDP client if still open, without dialing.
func (p *SessionPool) CurrentClient() *Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil && !p.client.IsClosed() {
		return p.client
	}
	return nil
}

// Acquire returns the active persistent CDP Client, dialing at most once for DevToolsActivePort
// until ResetApprovalLatch is called.
func (p *SessionPool) Acquire(ctx context.Context, cdpURL string, userDataDir string) (*Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.client != nil && !p.client.IsClosed() {
		return p.client, nil
	}
	p.client = nil
	p.wsURL = ""

	disc := p.discoverFn
	if disc == nil {
		disc = DiscoverBrowserWSWithSource
	}
	conn := p.connectFn
	if conn == nil {
		conn = Connect
	}

	wsURL, src, err := disc(ctx, cdpURL, userDataDir)
	if err != nil {
		return nil, err
	}
	if src == SourceActivePort {
		if p.activePortAttempted {
			return nil, ErrApprovalLatchActive
		}
		p.activePortAttempted = true
	}

	c, err := conn(ctx, wsURL)
	if err != nil {
		return nil, err
	}

	p.client = c
	p.wsURL = wsURL
	return c, nil
}

// ProbeQuiet checks whether a persistent CDP connection is active or whether dedicated port :9222 is up,
// without dialing DevToolsActivePort unprompted.
func (p *SessionPool) ProbeQuiet(ctx context.Context, cdpURL string) (connected bool, label string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.client != nil && !p.client.IsClosed() {
		return true, shortenWSLabel(p.wsURL)
	}
	p.client = nil
	p.wsURL = ""

	targetHTTP := "http://127.0.0.1:9222"
	u := strings.TrimSpace(cdpURL)
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		targetHTTP = u
	}

	wsURL, err := resolveHTTPVersionEndpoint(ctx, targetHTTP)
	if err != nil || wsURL == "" {
		if p.activePortAttempted {
			return false, "Disconnected (click Connect to retry)"
		}
		return false, "Browser Not Connected"
	}

	conn := p.connectFn
	if conn == nil {
		conn = Connect
	}
	c, err := conn(ctx, wsURL)
	if err != nil {
		return false, "Browser Not Connected"
	}
	p.client = c
	p.wsURL = wsURL
	return true, shortenWSLabel(wsURL)
}

// Close closes the pooled CDP WebSocket connection if open.
func (p *SessionPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		_ = p.client.Close()
		p.client = nil
		p.wsURL = ""
	}
}

func shortenWSLabel(wsURL string) string {
	if strings.Contains(wsURL, ":9222/") {
		return "Connected (Chrome :9222)"
	}
	return "Connected (CDP)"
}
