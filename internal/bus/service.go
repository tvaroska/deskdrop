package bus

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
)

// HTTPStatusError represents a non-2xx HTTP response returned by the relay.
type HTTPStatusError struct {
	Prefix     string
	StatusCode int
	Body       string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("%s HTTP %d: %s", e.Prefix, e.StatusCode, e.Body)
}

// IsRelayTransientError reports whether err is a transport error or rollout 502/503/504 status.
func IsRelayTransientError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == http.StatusBadGateway ||
			statusErr.StatusCode == http.StatusServiceUnavailable ||
			statusErr.StatusCode == http.StatusGatewayTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

const (
	defaultRetryBaseDelay   = 400 * time.Millisecond
	defaultMaxRetryAttempts = 4
)

// SendResult describes a delivered drop or diagnostic escalation.
type SendResult struct {
	ShortID    string `json:"short_id"`
	MessageID  string `json:"message_id,omitempty"`
	Inline     bool   `json:"inline"`
	Attachment string `json:"attachment,omitempty"`
}

// Transport abstracts outbound delivery of drops and failure bundles.
type Transport interface {
	ValidateAuth() error
	SendReport(ctx context.Context, to string, report contract.DropReport) (SendResult, error)
	SendErrorWithContext(ctx context.Context, to string, meta contract.FailReportMetadata, zipBytes []byte) (SendResult, error)
}

// NewTransport constructs the relay HTTP client (pure Go stdlib).
func NewTransport(cfg contract.Config) Transport {
	return NewDesktopClient(cfg)
}

// DesktopClient communicates with the deskdrop relay (/v1/* or /desktop/v1/*).
type DesktopClient struct {
	From             string
	DefaultTo        string
	BaseURL          string
	Token            string
	ClientVersion    string
	ForbiddenTerms   []string
	HTTPClient       *http.Client
	RetryBaseDelay   time.Duration
	MaxRetryAttempts int
}

// NewDesktopClient builds a relay client from Config.
// BaseURL is preserved cleanly (e.g., https://relay.example.com or https://relay.example.com/desktop).
func NewDesktopClient(cfg contract.Config) *DesktopClient {
	from := strings.TrimSpace(cfg.From)
	if from == "" {
		from = "desktop-worker"
	}
	to := strings.TrimSpace(cfg.DefaultTo)
	if to == "" {
		to = "all"
	}
	return &DesktopClient{
		From:             from,
		DefaultTo:        to,
		BaseURL:          strings.TrimRight(strings.TrimSpace(cfg.ServiceURL), "/"),
		Token:            strings.TrimSpace(cfg.ServiceToken),
		ForbiddenTerms:   cfg.ForbiddenTerms,
		HTTPClient:       &http.Client{Timeout: 30 * time.Second},
		RetryBaseDelay:   defaultRetryBaseDelay,
		MaxRetryAttempts: defaultMaxRetryAttempts,
	}
}

func (c *DesktopClient) ValidateAuth() error {
	if c.BaseURL == "" {
		return errors.New("missing service_url in deskdrop config or DESKDROP_SERVICE_URL")
	}
	if c.Token == "" {
		return errors.New("missing service_token in deskdrop config or DESKDROP_TOKEN")
	}
	return nil
}

func (c *DesktopClient) SendReport(ctx context.Context, to string, report contract.DropReport) (SendResult, error) {
	if err := c.ValidateAuth(); err != nil {
		return SendResult{}, err
	}
	pretty, err := json.Marshal(report)
	if err != nil {
		return SendResult{}, err
	}
	if err := contract.EnforceAirGap(string(pretty), c.ForbiddenTerms); err != nil {
		return SendResult{}, err
	}
	target := strings.TrimSpace(to)
	if target == "" {
		target = c.DefaultTo
	}

	reqBody := contract.SubmitDropRequest{
		From:   c.From,
		To:     target,
		Report: report,
	}
	idemp := "drop-" + randomHex(8)
	var resp struct {
		DropID    string `json:"drop_id"`
		ShortID   string `json:"short_id"`
		MessageID string `json:"message_id"`
	}
	if err := c.postJSONWithRetry(ctx, "/v1/drops", idemp, reqBody, &resp); err != nil {
		return SendResult{}, err
	}
	return SendResult{
		ShortID:    resp.ShortID,
		MessageID:  resp.MessageID,
		Inline:     true,
		Attachment: resp.DropID,
	}, nil
}

func (c *DesktopClient) SendErrorWithContext(ctx context.Context, to string, meta contract.FailReportMetadata, zipBytes []byte) (SendResult, error) {
	if err := c.ValidateAuth(); err != nil {
		return SendResult{}, err
	}
	if meta.From == "" {
		meta.From = c.From
	}
	if to != "" {
		meta.To = to
	} else if meta.To == "" {
		meta.To = c.DefaultTo
	}
	if err := contract.EnforceAirGap(meta.Topic+" "+meta.Error+" "+meta.LandedURL, c.ForbiddenTerms); err != nil {
		return SendResult{}, err
	}

	var resp struct {
		DiagnosticID string               `json:"diagnostic_id"`
		Triage       contract.ErrorTriage `json:"triage"`
	}
	if err := c.postErrorMultipart(ctx, "/v1/errors", "err-"+randomHex(8), meta, zipBytes, &resp); err != nil {
		return SendResult{}, err
	}
	return SendResult{
		ShortID:    string(resp.Triage.Code),
		Inline:     len(zipBytes) == 0,
		Attachment: resp.DiagnosticID,
	}, nil
}

func (c *DesktopClient) ClaimNextTask(ctx context.Context, host string, leaseSeconds int, worker *contract.WorkerStatus, allowedDrivers ...string) (contract.ClaimTaskResponse, error) {
	var resp contract.ClaimTaskResponse
	err := c.postJSONWithRetry(ctx, "/v1/tasks/claim", "", contract.ClaimTaskRequest{
		Host:           host,
		LeaseSeconds:   leaseSeconds,
		AllowedDrivers: allowedDrivers,
		Worker:         worker,
	}, &resp)
	return resp, err
}

func (c *DesktopClient) ReportWorkerStatus(ctx context.Context, ws contract.WorkerStatus) error {
	if err := c.ValidateAuth(); err != nil {
		return err
	}
	if ws.Host == "" {
		ws.Host = c.From
	}
	if ws.Version == "" {
		ws.Version = c.ClientVersion
	}
	return c.postJSONWithRetry(ctx, "/v1/workers/heartbeat", "", ws, nil)
}

func (c *DesktopClient) GetDesktopStatus(ctx context.Context) (map[string]any, error) {
	if err := c.ValidateAuth(); err != nil {
		return nil, err
	}
	raw, _, err := c.getWithRetry(ctx, "/v1/status", 256*1024, "relay")
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *DesktopClient) ListApprovedRecipes(ctx context.Context) ([]contract.Recipe, error) {
	if err := c.ValidateAuth(); err != nil {
		return nil, err
	}
	raw, _, err := c.getWithRetry(ctx, "/v1/recipes", 2*1024*1024, "relay recipes")
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Recipes []contract.Recipe `json:"recipes"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	return parsed.Recipes, nil
}

// DownloadReleaseBinary downloads the latest deskdrop binary from /v1/release/{asset}.
func (c *DesktopClient) DownloadReleaseBinary(ctx context.Context, goos string) ([]byte, string, error) {
	if err := c.ValidateAuth(); err != nil {
		return nil, "", err
	}
	asset := "deskdrop"
	if goos == "windows" {
		asset = "deskdrop.exe"
	}
	data, hdr, err := c.getWithRetry(ctx, "/v1/release/"+asset, 25*1024*1024, "download release")
	if err != nil {
		return nil, "", err
	}
	return data, strings.TrimSpace(hdr.Get("X-DeskDrop-Version")), nil
}

// DownloadReleaseChecksums fetches /v1/release/SHA256SUMS.txt to verify self-update integrity.
func (c *DesktopClient) DownloadReleaseChecksums(ctx context.Context) (string, error) {
	if err := c.ValidateAuth(); err != nil {
		return "", err
	}
	data, _, err := c.getWithRetry(ctx, "/v1/release/SHA256SUMS.txt", 64*1024, "download release checksums")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (c *DesktopClient) HeartbeatTask(ctx context.Context, taskID string, host string, leaseSeconds int) error {
	return c.postJSONWithRetry(ctx, "/v1/tasks/"+taskID+"/heartbeat", "", contract.ClaimTaskRequest{
		Host:         host,
		LeaseSeconds: leaseSeconds,
	}, nil)
}

func (c *DesktopClient) CompleteTask(ctx context.Context, taskID string, to string, report contract.DropReport) (SendResult, error) {
	pretty, err := json.Marshal(report)
	if err != nil {
		return SendResult{}, err
	}
	if err := contract.EnforceAirGap(string(pretty), c.ForbiddenTerms); err != nil {
		return SendResult{}, err
	}
	var resp struct {
		DropID    string `json:"drop_id"`
		ShortID   string `json:"short_id"`
		MessageID string `json:"message_id"`
	}
	err = c.postJSONWithRetry(ctx, "/v1/tasks/"+taskID+"/complete", "complete-"+taskID, contract.SubmitDropRequest{
		From:   c.From,
		To:     to,
		TaskID: taskID,
		Report: report,
	}, &resp)
	if err != nil {
		return SendResult{}, err
	}
	return SendResult{
		ShortID:    resp.ShortID,
		MessageID:  resp.MessageID,
		Inline:     true,
		Attachment: resp.DropID,
	}, nil
}

func (c *DesktopClient) FailTask(ctx context.Context, taskID string, meta contract.FailReportMetadata, zipBytes []byte) (contract.ErrorTriage, string, error) {
	if meta.From == "" {
		meta.From = c.From
	}
	if err := contract.EnforceAirGap(meta.Error+" "+meta.LandedURL, c.ForbiddenTerms); err != nil {
		return contract.ErrorTriage{}, "", err
	}
	var resp struct {
		Triage       contract.ErrorTriage `json:"triage"`
		DiagnosticID string               `json:"diagnostic_id"`
	}
	if err := c.postErrorMultipart(ctx, "/v1/tasks/"+taskID+"/fail", "fail-"+taskID, meta, zipBytes, &resp); err != nil {
		return contract.ErrorTriage{}, "", err
	}
	return resp.Triage, resp.DiagnosticID, nil
}

// SubmitFallbackResult uploads a fresh scrubbed Tier-2 snapshot bundle for offline Tier-1 healing.
func (c *DesktopClient) SubmitFallbackResult(ctx context.Context, taskID string, meta contract.FailReportMetadata, zipBytes []byte) (string, string, error) {
	if meta.From == "" {
		meta.From = c.From
	}
	if err := contract.EnforceAirGap(meta.Error+" "+meta.LandedURL, c.ForbiddenTerms); err != nil {
		return "", "", err
	}
	var resp struct {
		FallbackDropID string `json:"fallback_drop_id"`
		ProposalID     string `json:"proposal_id"`
	}
	if err := c.postErrorMultipart(ctx, "/v1/tasks/"+taskID+"/fallback-result", "fbres-"+taskID, meta, zipBytes, &resp); err != nil {
		return "", "", err
	}
	return resp.FallbackDropID, resp.ProposalID, nil
}

func (c *DesktopClient) setCommonHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", "DeskDrop/1.0")
	if c.ClientVersion != "" {
		req.Header.Set("X-DeskDrop-Version", c.ClientVersion)
	}
}

func (c *DesktopClient) maxAttempts() int {
	if c.MaxRetryAttempts > 0 {
		return c.MaxRetryAttempts
	}
	return defaultMaxRetryAttempts
}

func (c *DesktopClient) backoffDelay(attempt int) time.Duration {
	base := c.RetryBaseDelay
	if base <= 0 {
		base = defaultRetryBaseDelay
	}
	if attempt <= 1 {
		return base
	}
	shift := attempt - 1
	if shift > 4 {
		shift = 4
	}
	return base * time.Duration(1<<shift)
}

func isRolloutRetryStatus(status int, retryAll5xx bool) bool {
	if status == http.StatusTooManyRequests ||
		status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout {
		return true
	}
	return retryAll5xx && status >= 500
}

func (c *DesktopClient) getWithRetry(ctx context.Context, subPath string, maxBytes int64, errPrefix string) ([]byte, http.Header, error) {
	urlStr := c.BaseURL + subPath
	attempts := c.maxAttempts()
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
		if rerr != nil {
			return nil, nil, rerr
		}
		c.setCommonHeaders(req)

		status, raw, hdr, doErr := c.doReqWithLimit(req, maxBytes)
		if doErr == nil && status >= 200 && status < 300 {
			return raw, hdr, nil
		}
		if doErr != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			lastErr = doErr
		} else {
			errPreview := raw
			if len(errPreview) > 4096 {
				errPreview = errPreview[:4096]
			}
			lastErr = &HTTPStatusError{
				Prefix:     errPrefix,
				StatusCode: status,
				Body:       strings.TrimSpace(string(errPreview)),
			}
			if !isRolloutRetryStatus(status, false) {
				return nil, nil, lastErr
			}
		}
		if attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(c.backoffDelay(attempt)):
		}
	}
	return nil, nil, lastErr
}

func (c *DesktopClient) postJSONWithRetry(ctx context.Context, subPath string, idempKey string, payload any, dst any) error {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	urlStr := c.BaseURL + subPath
	attempts := c.maxAttempts()
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(bodyBytes))
		if rerr != nil {
			return rerr
		}
		req.Header.Set("Content-Type", "application/json")
		c.setCommonHeaders(req)
		if idempKey != "" {
			req.Header.Set("Idempotency-Key", idempKey)
		}

		status, raw, _, doErr := c.doReqWithLimit(req, 256*1024)
		if doErr == nil && status >= 200 && status < 300 {
			if dst != nil {
				if err := json.Unmarshal(raw, dst); err != nil {
					return fmt.Errorf("decode relay JSON response: %w", err)
				}
			}
			return nil
		}
		if doErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = doErr
		} else {
			lastErr = &HTTPStatusError{
				Prefix:     "relay",
				StatusCode: status,
				Body:       strings.TrimSpace(string(raw)),
			}
			if !isRolloutRetryStatus(status, true) {
				return lastErr
			}
		}
		if attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.backoffDelay(attempt)):
		}
	}
	return lastErr
}

func (c *DesktopClient) postErrorMultipart(ctx context.Context, subPath string, idempKey string, meta contract.FailReportMetadata, zipBytes []byte, dst any) error {
	if len(zipBytes) == 0 {
		return c.postJSONWithRetry(ctx, subPath, idempKey, meta, dst)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	pHdr := make(textproto.MIMEHeader)
	pHdr.Set("Content-Disposition", `form-data; name="payload_json"`)
	pHdr.Set("Content-Type", "application/json")
	pw, err := mw.CreatePart(pHdr)
	if err != nil {
		return err
	}
	if _, err := pw.Write(metaBytes); err != nil {
		return err
	}

	fHdr := make(textproto.MIMEHeader)
	fHdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="bundle"; filename="debug_%s.zip"`, sanitizeSlug(meta.Topic)))
	fHdr.Set("Content-Type", "application/zip")
	fw, err := mw.CreatePart(fHdr)
	if err != nil {
		return err
	}
	if _, err := fw.Write(zipBytes); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	mpBytes := buf.Bytes()
	contentType := mw.FormDataContentType()
	urlStr := c.BaseURL + subPath
	attempts := c.maxAttempts()
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(mpBytes))
		if rerr != nil {
			return rerr
		}
		req.Header.Set("Content-Type", contentType)
		c.setCommonHeaders(req)
		if idempKey != "" {
			req.Header.Set("Idempotency-Key", idempKey)
		}

		status, raw, _, doErr := c.doReqWithLimit(req, 256*1024)
		if doErr == nil && status >= 200 && status < 300 {
			if dst != nil {
				if err := json.Unmarshal(raw, dst); err != nil {
					return fmt.Errorf("decode relay JSON response: %w", err)
				}
			}
			return nil
		}
		if doErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = doErr
		} else {
			lastErr = &HTTPStatusError{
				Prefix:     "relay",
				StatusCode: status,
				Body:       strings.TrimSpace(string(raw)),
			}
			if !isRolloutRetryStatus(status, false) {
				return lastErr
			}
		}
		if attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.backoffDelay(attempt)):
		}
	}
	return lastErr
}

func (c *DesktopClient) doReqWithLimit(req *http.Request, maxBytes int64) (int, []byte, http.Header, error) {
	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if readErr != nil {
		return resp.StatusCode, nil, resp.Header, fmt.Errorf("read response body: %w", readErr)
	}
	if int64(len(raw)) > maxBytes {
		return resp.StatusCode, nil, resp.Header, fmt.Errorf("response body exceeded limit of %d bytes", maxBytes)
	}
	if resp.ContentLength >= 0 && int64(len(raw)) != resp.ContentLength {
		return resp.StatusCode, nil, resp.Header, fmt.Errorf("truncated response body: got %d bytes, expected %d", len(raw), resp.ContentLength)
	}
	return resp.StatusCode, raw, resp.Header, nil
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "000000"
	}
	return hex.EncodeToString(b)
}

func sanitizeSlug(s string) string {
	var out []rune
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			out = append(out, r)
		} else {
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "drop"
	}
	return string(out)
}
