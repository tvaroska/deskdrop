package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
)

func (s *Service) persistDiagnosticIfPresent(ctx context.Context, taskID string, meta contract.FailReportMetadata, triage contract.ErrorTriage, zipBytes []byte) (string, error) {
	if len(zipBytes) == 0 {
		return "", nil
	}
	diagID := newID("diag")
	blobKey := fmt.Sprintf("diagnostics/%s.zip", diagID)
	if err := s.blobs.Put(ctx, blobKey, "application/zip", zipBytes); err != nil {
		return "", err
	}
	topic := strings.TrimSpace(meta.Topic)
	if topic == "" {
		topic = "error"
	}
	rec := contract.DiagnosticRecord{
		ID:           diagID,
		TaskID:       taskID,
		Topic:        topic,
		Recipe:       meta.Recipe,
		Host:         meta.From,
		Domain:       triage.Domain,
		ErrorCode:    triage.Code,
		Error:        meta.Error,
		BlobKey:      blobKey,
		SizeBytes:    len(zipBytes),
		CreatedAtUTC: nowISO(),
	}
	if err := s.store.SaveDiagnostic(ctx, rec); err != nil {
		return "", err
	}
	return diagID, nil
}

func (s *Service) emitTriageBusAlert(ctx context.Context, from, to, topic string, triage contract.ErrorTriage, errMsg, diagID, taskID string) {
	if from == "" {
		from = "deskdrop-svc"
	}
	alertTopic := strings.TrimSpace(topic)
	if alertTopic == "" {
		alertTopic = "desktop"
	}
	alertTopic += ":error"

	var body string
	switch triage.Code {
	case contract.ErrorCodeAuthExpired:
		body = fmt.Sprintf("[AUTH_EXPIRED] Domain %q paused (remaining tasks held until browser re-login): %s", triage.Domain, truncateStr(errMsg, 360))
	case contract.ErrorCodeSelectorDrift:
		body = fmt.Sprintf("[SELECTOR_DRIFT] Recipe selector/expectation failed on %q: %s", triage.Domain, truncateStr(errMsg, 360))
	default:
		body = fmt.Sprintf("[%s] Desktop extraction failed: %s", triage.Code, truncateStr(errMsg, 360))
	}
	if diagID != "" {
		dl := fmt.Sprintf("/v1/diagnostics/%s/bundle.zip", diagID)
		if s.cfg.PublicBaseURL != "" {
			dl = strings.TrimRight(s.cfg.PublicBaseURL, "/") + dl
		}
		body += fmt.Sprintf(" (diagnostic bundle: %s)", dl)
	}

	msg, created, err := s.store.PostMessage(ctx, contract.PostMessageRequest{
		From:  from,
		To:    strings.Split(to, ","),
		Topic: alertTopic,
		Body:  body,
		Meta: map[string]any{
			"kind":          "error_triage",
			"error_code":    triage.Code,
			"action":        triage.Action,
			"domain":        triage.Domain,
			"diagnostic_id": diagID,
			"task_id":       taskID,
		},
	}, "")
	if err == nil && created {
		s.mirrorWebhookAsync(formatDiscordBusLine(msg.ShortID, msg.From, to, alertTopic, body))
	}
}

func (s *Service) mirrorWebhookAsync(content string) {
	wh := strings.TrimSpace(s.cfg.DiscordWebhookURL)
	if wh == "" {
		return
	}
	if err := contract.EnforceAirGap(content, s.cfg.ForbiddenTerms); err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		payload, _ := json.Marshal(map[string]string{"content": truncateStr(content, 1950)})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh, bytes.NewReader(payload))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "DeskDrop/1.0")
		resp, err := s.httpClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
}

type validationErr struct {
	Code string
	Msg  string
}

func (e *validationErr) Error() string { return e.Msg }

func (s *Service) writeIngestError(w http.ResponseWriter, err error) {
	var ve *validationErr
	if errors.As(err, &ve) {
		writeAPIError(w, http.StatusUnprocessableEntity, ve.Code, ve.Msg, nil)
		return
	}
	writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
}

func parseFailRequest(r *http.Request) (contract.FailReportMetadata, []byte, error) {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "multipart/form-data") {
		r.Body = http.MaxBytesReader(nil, r.Body, 15*1024*1024)
		if err := r.ParseMultipartForm(15 * 1024 * 1024); err != nil {
			return contract.FailReportMetadata{}, nil, fmt.Errorf("invalid multipart form: %w", err)
		}
		var meta contract.FailReportMetadata
		rawJSON := r.FormValue("payload_json")
		if rawJSON == "" {
			return meta, nil, errors.New("multipart form missing payload_json")
		}
		if err := contract.ParseJSONStrict([]byte(rawJSON), &meta); err != nil {
			return meta, nil, fmt.Errorf("invalid payload_json: %w", err)
		}
		var zipBytes []byte
		file, _, ferr := r.FormFile("bundle")
		if ferr == nil {
			defer file.Close()
			zipBytes, _ = io.ReadAll(io.LimitReader(file, 14*1024*1024))
		}
		return meta, zipBytes, nil
	}

	var meta contract.FailReportMetadata
	if err := decodeJSONBody(r, 512*1024, &meta); err != nil {
		return meta, nil, err
	}
	return meta, nil, nil
}

func inspectDiagnosticZip(zipBytes []byte) (domHTML string, screenshotPNG []byte, err error) {
	if len(zipBytes) == 0 {
		return "", nil, nil
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return "", nil, err
	}
	for _, f := range zr.File {
		switch f.Name {
		case "dom.html":
			rc, oerr := f.Open()
			if oerr == nil {
				raw, _ := io.ReadAll(io.LimitReader(rc, 512*1024))
				_ = rc.Close()
				domHTML = contract.ScrubHTML(string(raw))
			}
		case "screenshot.png":
			rc, oerr := f.Open()
			if oerr == nil {
				screenshotPNG, _ = io.ReadAll(io.LimitReader(rc, 8*1024*1024))
				_ = rc.Close()
			}
		}
	}
	return domHTML, screenshotPNG, nil
}

func formatDropBusBody(rec contract.DropRecord, baseURL string) string {
	compact, _ := json.Marshal(rec.Report)
	summary := summarizeDropShort(rec, baseURL)
	if len(compact) <= 1200 {
		return fmt.Sprintf("%s\n```json\n%s\n```", summary, string(compact))
	}
	return summary
}

func summarizeDropShort(rec contract.DropRecord, baseURL string) string {
	metrics, tables, rows, items := 0, 0, 0, 0
	for _, st := range rec.Report.Steps {
		metrics += len(st.Metrics)
		tables += len(st.Tables)
		items += len(st.Items)
		for _, tb := range st.Tables {
			rows += len(tb.Rows)
		}
	}
	ref := fmt.Sprintf("/v1/drops/%s", rec.ID)
	if baseURL != "" {
		ref = strings.TrimRight(baseURL, "/") + ref
	}
	return fmt.Sprintf("Drop `%s` [%s] (%d step(s), %d item(s), %d metric(s), %d table(s)/%d row(s)) -> %s",
		rec.Report.Recipe, rec.Report.Mode, len(rec.Report.Steps), items, metrics, tables, rows, ref)
}

func formatDiscordBusLine(shortID, from, to, topic, body string) string {
	tp := ""
	if topic != "" {
		tp = fmt.Sprintf(" `[%s]`", topic)
	}
	return fmt.Sprintf("`[bus:%s]` **%s -> %s**%s: %s", shortID, from, to, tp, body)
}

func decodeJSONBody(r *http.Request, maxBytes int64, dst any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > maxBytes {
		return fmt.Errorf("request body exceeds %d bytes", maxBytes)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return contract.ParseJSONStrict(raw, dst)
}

func parseIntQuery(r *http.Request, key string, def int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func truncateStr(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "..."
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

func writeAPIError(w http.ResponseWriter, status int, code string, message string, details any) {
	errObj := map[string]any{
		"code":    code,
		"message": message,
	}
	if details != nil {
		errObj["details"] = details
	}
	writeJSON(w, status, map[string]any{
		"ok":    false,
		"error": errObj,
	})
}
