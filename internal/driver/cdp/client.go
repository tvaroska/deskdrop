package cdp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TargetInfo describes an open browser page/tab returned by Target.getTargets.
type TargetInfo struct {
	TargetID         string `json:"targetId"`
	Type             string `json:"type"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	Attached         bool   `json:"attached"`
	OpenerID         string `json:"openerId,omitempty"`
	BrowserContextID string `json:"browserContextId,omitempty"`
}

type rpcRequest struct {
	ID        int64  `json:"id"`
	Method    string `json:"method"`
	Params    any    `json:"params,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

type rpcMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Client multiplexes Chrome DevTools Protocol JSON-RPC calls over a browser WebSocket.
type Client struct {
	ws                *WSConn
	nextID            atomic.Int64
	mu                sync.Mutex
	pending           map[int64]chan rpcMessage
	closed            chan struct{}
	closeMu           sync.Once
	browser           string
	userDataDir       string
	preferredSession  string
	verifiedContextID string
	settleDelay       time.Duration
}

// SetPreferredSession configures which browser profile/session to prefer for tabs and scratch pages.
func (c *Client) SetPreferredSession(browser string, userDataDir string, preferredSession string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	newPref := strings.TrimSpace(preferredSession)
	if !strings.EqualFold(c.preferredSession, newPref) {
		c.verifiedContextID = ""
	}
	c.browser = strings.TrimSpace(browser)
	c.userDataDir = strings.TrimSpace(userDataDir)
	c.preferredSession = newPref
	c.mu.Unlock()
}

// SetVerifiedContextID records a BrowserContextID verified as logged in for the current session.
func (c *Client) SetVerifiedContextID(ctxID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.verifiedContextID = strings.TrimSpace(ctxID)
	c.mu.Unlock()
}

// Connect dials a browser-level CDP WebSocket and starts the response dispatcher.
func Connect(ctx context.Context, wsURL string) (*Client, error) {
	ws, err := DialWS(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	c := &Client{
		ws:      ws,
		pending: make(map[int64]chan rpcMessage),
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *Client) readLoop() {
	defer func() {
		c.mu.Lock()
		c.closeMu.Do(func() { close(c.closed) })
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
	}()

	for {
		raw, err := c.ws.ReadText()
		if err != nil {
			return
		}
		var msg rpcMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.ID != 0 {
			c.mu.Lock()
			ch, ok := c.pending[msg.ID]
			if ok {
				delete(c.pending, msg.ID)
			}
			c.mu.Unlock()
			if ok {
				ch <- msg
			}
		}
	}
}

// Call invokes a CDP method (optionally scoped to a flattened page sessionID) and decodes the result.
func (c *Client) Call(ctx context.Context, sessionID string, method string, params any, out any) error {
	id := c.nextID.Add(1)
	reqBytes, err := json.Marshal(rpcRequest{
		ID:        id,
		Method:    method,
		Params:    params,
		SessionID: sessionID,
	})
	if err != nil {
		return err
	}

	ch := make(chan rpcMessage, 1)
	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		return fmt.Errorf("cdp connection closed before %s", method)
	default:
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.ws.WriteText(reqBytes); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.closed:
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("cdp socket closed while waiting for %s", method)
	case msg, ok := <-ch:
		if !ok {
			return fmt.Errorf("cdp socket closed while waiting for %s", method)
		}
		if msg.Error != nil {
			return fmt.Errorf("cdp %s error (%d): %s", method, msg.Error.Code, msg.Error.Message)
		}
		if out != nil && len(msg.Result) > 0 {
			return json.Unmarshal(msg.Result, out)
		}
		return nil
	}
}

// ListAllTargets returns all targets reported by Target.getTargets.
func (c *Client) ListAllTargets(ctx context.Context) ([]TargetInfo, error) {
	var res struct {
		TargetInfos []TargetInfo `json:"targetInfos"`
	}
	if err := c.Call(ctx, "", "Target.getTargets", nil, &res); err != nil {
		return nil, err
	}
	return res.TargetInfos, nil
}

// ListPages returns all open page targets in the connected browser, prioritizing the configured Chrome session.
func (c *Client) ListPages(ctx context.Context) ([]TargetInfo, error) {
	targets, err := c.ListAllTargets(ctx)
	if err != nil {
		return nil, err
	}
	var pages []TargetInfo
	for _, t := range targets {
		if t.Type == "page" {
			pages = append(pages, t)
		}
	}
	bestCtx := c.resolveBrowserContextFromTargets(targets, "")
	if bestCtx != "" && len(pages) > 1 {
		sort.SliceStable(pages, func(i, j int) bool {
			iMatch := pages[i].BrowserContextID == bestCtx
			jMatch := pages[j].BrowserContextID == bestCtx
			if iMatch != jMatch {
				return iMatch
			}
			return false
		})
	}
	return pages, nil
}

// ResolveBrowserContextID selects the best Chrome BrowserContextID for the configured session and target URL.
func (c *Client) ResolveBrowserContextID(ctx context.Context, targetURL string) string {
	ranked := c.RankBrowserContextIDs(ctx, targetURL)
	if len(ranked) == 0 {
		return ""
	}
	return ranked[0]
}

// RankBrowserContextIDs returns all open Chrome BrowserContextIDs ordered from best match to worst match.
func (c *Client) RankBrowserContextIDs(ctx context.Context, targetURL string) []string {
	targets, err := c.ListAllTargets(ctx)
	if err != nil {
		return nil
	}
	return c.rankBrowserContextsFromTargets(targets, targetURL)
}

func (c *Client) resolveBrowserContextFromTargets(targets []TargetInfo, targetURL string) string {
	ranked := c.rankBrowserContextsFromTargets(targets, targetURL)
	if len(ranked) == 0 {
		return ""
	}
	return ranked[0]
}

func (c *Client) rankBrowserContextsFromTargets(targets []TargetInfo, targetURL string) []string {
	c.mu.Lock()
	browser := c.browser
	userDataDir := c.userDataDir
	prefSession := c.preferredSession
	verifiedCtx := c.verifiedContextID
	c.mu.Unlock()

	var ctxOrder []string
	byCtx := make(map[string][]TargetInfo)
	for _, t := range targets {
		cid := strings.TrimSpace(t.BrowserContextID)
		if cid == "" {
			continue
		}
		if _, seen := byCtx[cid]; !seen {
			ctxOrder = append(ctxOrder, cid)
		}
		byCtx[cid] = append(byCtx[cid], t)
	}
	if len(ctxOrder) == 0 {
		return nil
	}
	if len(ctxOrder) == 1 {
		return ctxOrder
	}

	allProfiles := ListBrowserProfiles(browser, userDataDir)
	var prof BrowserProfile
	var hasProf bool
	var extIDs map[string]bool
	var sessBlob string
	otherExtIDs := make(map[string]bool)
	var otherEmails []string

	if prefSession != "" {
		prof, hasProf = ResolveBrowserProfile(browser, userDataDir, prefSession)
		if hasProf {
			extIDs, sessBlob = readProfileSignals(browser, userDataDir, prof.DirName)
			for _, op := range allProfiles {
				if op.IsDedicated || op.DirName == prof.DirName {
					continue
				}
				if op.UserName != "" && !strings.EqualFold(op.UserName, prof.UserName) {
					otherEmails = append(otherEmails, strings.ToLower(op.UserName))
				}
				oExts, _ := readProfileSignals(browser, userDataDir, op.DirName)
				for k := range oExts {
					otherExtIDs[k] = true
				}
			}
		}
	}

	targetHost := extractHostRoot(targetURL)
	prefLower := strings.ToLower(prefSession)

	ambiguousPref := false
	if prefLower != "" {
		matchCount := 0
		for _, op := range allProfiles {
			if op.IsDedicated {
				continue
			}
			hay := strings.ToLower(op.DisplayName + " " + op.Name + " " + op.UserName)
			if strings.Contains(hay, prefLower) {
				matchCount++
			}
		}
		if matchCount > 1 {
			ambiguousPref = true
		}
	}

	type scoredCtx struct {
		cid   string
		score int
		idx   int
	}
	scored := make([]scoredCtx, 0, len(ctxOrder))

	for idx, cid := range ctxOrder {
		score := 0
		if verifiedCtx != "" && cid == verifiedCtx {
			score += 1000
		}
		if prefLower != "" && strings.EqualFold(cid, prefLower) {
			score += 500
		}

		matchedTargetHost := false
		matchedSelectedEmail := false
		matchedOtherEmail := false

		for _, t := range byCtx[cid] {
			uLower := strings.ToLower(t.URL)
			tLower := strings.ToLower(t.Title)
			if t.Type == "page" && (strings.HasPrefix(uLower, "http://") || strings.HasPrefix(uLower, "https://")) {
				score += 1
				if targetHost != "" && strings.Contains(uLower, targetHost) && !isLikelyLoginURL(uLower) {
					matchedTargetHost = true
				}
				if sessBlob != "" {
					cleanURL := strings.Split(uLower, "#")[0]
					if len(cleanURL) > 12 && strings.Contains(sessBlob, cleanURL) {
						score += 80
					}
				}
			}
			if len(extIDs) > 0 && strings.HasPrefix(uLower, "chrome-extension://") {
				rest := strings.TrimPrefix(uLower, "chrome-extension://")
				extID := strings.SplitN(rest, "/", 2)[0]
				if extIDs[extID] {
					if !otherExtIDs[extID] {
						score += 150
					} else {
						score += 10
					}
				}
			}
			if hasProf && prof.UserName != "" {
				em := strings.ToLower(prof.UserName)
				if strings.Contains(tLower, em) || strings.Contains(uLower, em) {
					matchedSelectedEmail = true
				}
			}
			for _, oem := range otherEmails {
				if oem != "" && (strings.Contains(tLower, oem) || strings.Contains(uLower, oem)) {
					matchedOtherEmail = true
				}
			}
			if !ambiguousPref && prefLower != "" {
				if strings.Contains(tLower, prefLower) || strings.Contains(uLower, prefLower) {
					score += 40
				}
			}
		}

		if matchedTargetHost {
			score += 500
		}
		if matchedSelectedEmail {
			score += 350
		}
		if matchedOtherEmail && !matchedSelectedEmail {
			score -= 500
		}

		scored = append(scored, scoredCtx{cid: cid, score: score, idx: idx})
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].idx < scored[j].idx
	})

	out := make([]string, len(scored))
	for i, s := range scored {
		out[i] = s.cid
	}
	return out
}

func isLikelyLoginURL(uLower string) bool {
	for _, pat := range []string{"/login", "/uas/login", "/authwall", "/signin", "/i/flow/login", "accounts.google.com/v3/signin"} {
		if strings.Contains(uLower, pat) {
			return true
		}
	}
	return false
}

func extractHostRoot(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	return strings.TrimPrefix(h, "www.")
}

// AttachPage attaches a flattened CDP session to an existing targetId without reloading it.
func (c *Client) AttachPage(ctx context.Context, targetID string) (string, error) {
	var res struct {
		SessionID string `json:"sessionId"`
	}
	params := map[string]any{
		"targetId": targetID,
		"flatten":  true,
	}
	if err := c.Call(ctx, "", "Target.attachToTarget", params, &res); err != nil {
		return "", err
	}
	return res.SessionID, nil
}

// DetachPage detaches a session cleanly while leaving the tab open.
func (c *Client) DetachPage(_ context.Context, sessionID string) {
	if sessionID == "" {
		return
	}
	cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Call(cctx, "", "Target.detachFromTarget", map[string]any{"sessionId": sessionID}, nil)
}

// CreateScratchPage opens a temporary tab in the preferred Chrome session and navigates to targetURL.
func (c *Client) CreateScratchPage(ctx context.Context, targetURL string) (targetID string, sessionID string, err error) {
	return c.CreateScratchPageWithContext(ctx, targetURL, c.ResolveBrowserContextID(ctx, targetURL))
}

// CreateScratchPageWithContext opens a temporary tab in the specified BrowserContextID (if non-empty).
// When Chrome rejects browserContextId for regular disk profiles, opens via an anchor tab inside that context
// so cookies/session match the chosen Chrome profile.
func (c *Client) CreateScratchPageWithContext(ctx context.Context, targetURL string, browserContextID string) (targetID string, sessionID string, err error) {
	cid := strings.TrimSpace(browserContextID)
	var created struct {
		TargetID string `json:"targetId"`
	}
	params := map[string]any{
		"url":        targetURL,
		"background": false,
	}
	if cid != "" {
		params["browserContextId"] = cid
	}
	if err := c.Call(ctx, "", "Target.createTarget", params, &created); err != nil {
		if cid != "" {
			if tid, sid, aerr := c.createScratchPageViaAnchorTab(ctx, targetURL, cid); aerr == nil {
				return tid, sid, nil
			}
			delete(params, "browserContextId")
			if err2 := c.Call(ctx, "", "Target.createTarget", params, &created); err2 != nil {
				return "", "", err2
			}
		} else {
			return "", "", err
		}
	}
	sid, err := c.AttachPage(ctx, created.TargetID)
	if err != nil {
		_ = c.CloseScratchPage(context.Background(), created.TargetID, "")
		return "", "", err
	}
	_ = c.Call(ctx, sid, "Page.enable", nil, nil)
	_ = c.Call(ctx, sid, "Runtime.enable", nil, nil)
	return created.TargetID, sid, nil
}

func (c *Client) createScratchPageViaAnchorTab(ctx context.Context, targetURL string, browserContextID string) (string, string, error) {
	targets, err := c.ListAllTargets(ctx)
	if err != nil {
		return "", "", err
	}
	beforeIDs := make(map[string]bool, len(targets))
	var anchors []TargetInfo
	targetHost := extractHostRoot(targetURL)

	for _, t := range targets {
		beforeIDs[t.TargetID] = true
		if t.Type != "page" || strings.TrimSpace(t.BrowserContextID) != browserContextID {
			continue
		}
		uLower := strings.ToLower(t.URL)
		if !strings.HasPrefix(uLower, "http://") && !strings.HasPrefix(uLower, "https://") {
			continue
		}
		if isLikelyLoginURL(uLower) {
			continue
		}
		anchors = append(anchors, t)
	}
	if len(anchors) == 0 {
		return "", "", fmt.Errorf("no open http/https anchor tab in browserContextId %s", browserContextID)
	}

	sort.SliceStable(anchors, func(i, j int) bool {
		iMatch := targetHost != "" && strings.Contains(strings.ToLower(anchors[i].URL), targetHost)
		jMatch := targetHost != "" && strings.Contains(strings.ToLower(anchors[j].URL), targetHost)
		if iMatch != jMatch {
			return iMatch
		}
		return false
	})

	urlJSON, _ := json.Marshal(targetURL)
	openExpr := fmt.Sprintf(`(() => {
		const u = %s;
		try {
			const w = window.open(u, '_blank');
			if (w) return true;
		} catch (e) {}
		try {
			const a = document.createElement('a');
			a.href = u;
			a.target = '_blank';
			a.rel = 'noopener';
			(document.body || document.documentElement).appendChild(a);
			a.click();
			a.remove();
			return true;
		} catch (e) {
			return false;
		}
	})()`, string(urlJSON))

	maxAnchors := len(anchors)
	if maxAnchors > 2 {
		maxAnchors = 2
	}
	for i := 0; i < maxAnchors; i++ {
		anchor := anchors[i]
		actx, acancel := context.WithTimeout(ctx, 4*time.Second)
		anchorSid, aerr := c.AttachPage(actx, anchor.TargetID)
		if aerr != nil {
			acancel()
			continue
		}
		var evalRes struct {
			Result struct {
				Value bool `json:"value"`
			} `json:"result"`
		}
		callErr := c.Call(actx, anchorSid, "Runtime.evaluate", map[string]any{
			"expression":    openExpr,
			"userGesture":   true,
			"returnByValue": true,
		}, &evalRes)
		c.DetachPage(context.Background(), anchorSid)
		acancel()
		if callErr != nil || !evalRes.Result.Value {
			continue
		}

		deadline := time.Now().Add(2500 * time.Millisecond)
		for time.Now().Before(deadline) {
			curTargets, lerr := c.ListAllTargets(ctx)
			if lerr == nil {
				for _, t := range curTargets {
					if beforeIDs[t.TargetID] || t.Type != "page" {
						continue
					}
					if strings.TrimSpace(t.BrowserContextID) == browserContextID || t.OpenerID == anchor.TargetID {
						sid, serr := c.AttachPage(ctx, t.TargetID)
						if serr != nil {
							_ = c.CloseScratchPage(context.Background(), t.TargetID, "")
							break
						}
						_ = c.Call(ctx, sid, "Page.enable", nil, nil)
						_ = c.Call(ctx, sid, "Runtime.enable", nil, nil)
						return t.TargetID, sid, nil
					}
				}
			}
			select {
			case <-ctx.Done():
				return "", "", ctx.Err()
			case <-time.After(80 * time.Millisecond):
			}
		}
	}
	return "", "", fmt.Errorf("failed opening scratch tab via anchor in browserContextId %s", browserContextID)
}

// CloseScratchPage closes a temporary tab reliably using independent timeouts so cleanup always runs.
func (c *Client) CloseScratchPage(_ context.Context, targetID string, sessionID string) error {
	if sessionID != "" {
		bctx, bcancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		_ = c.EvalJSON(bctx, sessionID, "try { window.onbeforeunload = null; window.stop(); } catch (e) {} true", nil)
		bcancel()
	}

	var closeErr error
	if targetID != "" {
		tctx, tcancel := context.WithTimeout(context.Background(), 3*time.Second)
		closeErr = c.Call(tctx, "", "Target.closeTarget", map[string]any{"targetId": targetID}, nil)
		tcancel()
	}

	if sessionID != "" {
		pctx, pcancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
		_ = c.Call(pctx, sessionID, "Page.close", map[string]any{}, nil)
		pcancel()
		c.DetachPage(context.Background(), sessionID)
	}

	if targetID != "" {
		for attempt := 0; attempt < 4; attempt++ {
			lctx, lcancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			targets, err := c.ListAllTargets(lctx)
			lcancel()
			if err != nil {
				break
			}
			stillOpen := false
			for _, t := range targets {
				if t.TargetID == targetID {
					stillOpen = true
					break
				}
			}
			if !stillOpen {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
			rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
			closeErr = c.Call(rctx, "", "Target.closeTarget", map[string]any{"targetId": targetID}, nil)
			rcancel()
		}
	}
	return closeErr
}

// EvalJSON evaluates a read-only internal JS expression returning a JSON-serializable object.
func (c *Client) EvalJSON(ctx context.Context, sessionID string, expression string, out any) error {
	var res struct {
		Result struct {
			Type        string          `json:"type"`
			Subtype     string          `json:"subtype"`
			Description string          `json:"description"`
			Value       json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	params := map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	}
	if err := c.Call(ctx, sessionID, "Runtime.evaluate", params, &res); err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		desc := res.ExceptionDetails.Exception.Description
		if desc == "" {
			desc = res.ExceptionDetails.Text
		}
		return fmt.Errorf("js runtime exception: %s", desc)
	}
	if out != nil && len(res.Result.Value) > 0 {
		return json.Unmarshal(res.Result.Value, out)
	}
	return nil
}

// CaptureScreenshotPNG captures a PNG screenshot (used strictly on error escalation and Tier-2 fallback).
func (c *Client) CaptureScreenshotPNG(ctx context.Context, sessionID string) ([]byte, error) {
	var res struct {
		Data string `json:"data"`
	}
	if err := c.Call(ctx, sessionID, "Page.captureScreenshot", map[string]any{"format": "png"}, &res); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(res.Data)
}

// IsClosed reports whether the underlying CDP WebSocket connection has closed.
func (c *Client) IsClosed() bool {
	if c == nil {
		return true
	}
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// Close detaches the CDP WebSocket without closing the user's running browser.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closeMu.Do(func() { close(c.closed) })
	c.mu.Unlock()
	return c.ws.Close()
}
