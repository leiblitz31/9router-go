package executor

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Document upload for qw-web context externalization. chat.qwen.ai lets a
// signed-in session attach a parsed document to a chat message; the file
// rides in the message's `files` array and its full text counts as context
// without travelling in the request body. That is the lane's escape from
// the ±128 KiB risk-control body limit: oversized agent context (system
// prompt + tool definitions + history) is uploaded once and referenced,
// while only a budgeted live prompt crosses the wire inline.
//
// Wire shape measured against the two maintained wrappers and verified
// live from this gateway (2026-11-03): STS credentials from
// /api/v2/files/getstsToken (the /api/v1 path is the fallback), a signed
// PUT to the account's OSS bucket, then /api/v2/files/parse with status
// polling until the document is readable. The descriptor mirrors the
// site's own client shape (Rfym21/Qwen2API src/utils/upload.js,
// buildChatFileDescriptor).

const (
	qwenWebSTSPathV2 = "/api/v2/files/getstsToken"
	qwenWebSTSPathV1 = "/api/v1/files/getstsToken"
	qwenWebParsePath = "/api/v2/files/parse"
	qwenWebParsePoll = "/api/v2/files/parse/status"

	qwenWebParsePollEvery = 500 * time.Millisecond
	qwenWebParseTimeout   = 60 * time.Second
	qwenWebUploadBodyCap  = 64 << 10
)

// qwenWebSTS is one upload grant: short-lived OSS credentials plus the
// object identity the completion will reference.
type qwenWebSTS struct {
	AccessKeyID     string
	AccessKeySecret string
	SecurityToken   string
	FileID          string
	FilePath        string
	FileURL         string
	Bucket          string
	Region          string
	Endpoint        string
}

// qwenWebFileHeaders builds the header set for the file endpoints: the
// same Baxia lane and auth as a turn, freshly generated per call.
func qwenWebFileHeaders(ctx context.Context, client *http.Client, cred qwenWebCredential) http.Header {
	h := qwenWebHeaders(qwenWebBXUA(), qwenWebUMIDToken(ctx, client, true))
	qwenWebApplyAuth(h, cred)
	return h
}

func qwenWebFilePost(ctx context.Context, client *http.Client, cred qwenWebCredential, path string, payload any) (int, []byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qwenWebBaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header = qwenWebFileHeaders(ctx, client, cred)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, qwenWebUploadBodyCap))
	return resp.StatusCode, body, err
}

// qwenWebGetSTS asks for upload credentials, v2 first (the Go wrapper's
// measured path) and v1 as fallback (the JS wrapper's path). A refusal on
// both is returned whole; the caller decides what it means.
func qwenWebGetSTS(ctx context.Context, client *http.Client, cred qwenWebCredential, filename string, size int) (*qwenWebSTS, error) {
	var lastErr error
	for _, path := range []string{qwenWebSTSPathV2, qwenWebSTSPathV1} {
		sts, err := qwenWebGetSTSAt(ctx, client, cred, path, filename, size)
		if err == nil {
			return sts, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func qwenWebGetSTSAt(ctx context.Context, client *http.Client, cred qwenWebCredential, path, filename string, size int) (*qwenWebSTS, error) {
	status, body, err := qwenWebFilePost(ctx, client, cred, path, map[string]any{
		"filename": filename, "filesize": size, "filetype": "file",
	})
	if err != nil {
		return nil, fmt.Errorf("qw-web: getstsToken request: %w", err)
	}
	if qwenWebIsRiskControl(body) {
		return nil, &qwenWebRiskControlError{stage: "getstsToken"}
	}
	if status != http.StatusOK {
		return nil, qwenWebFailure(status, "qw-web: getstsToken refused: "+qwenWebSnippet(body))
	}
	var envelope struct {
		Success *bool          `json:"success"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("qw-web: getstsToken reply is not JSON: %w", err)
	}
	if envelope.Success != nil && !*envelope.Success {
		return nil, qwenWebFailure(http.StatusBadGateway, "qw-web: getstsToken answered success=false: "+qwenWebSnippet(body))
	}
	data := envelope.Data
	if data == nil {
		var flat map[string]any
		if err := json.Unmarshal(body, &flat); err == nil {
			data = flat
		}
	}
	get := func(key string) string {
		if v, ok := data[key].(string); ok {
			return v
		}
		return ""
	}
	sts := &qwenWebSTS{
		AccessKeyID: get("access_key_id"), AccessKeySecret: get("access_key_secret"),
		SecurityToken: get("security_token"), FileID: get("file_id"), FilePath: get("file_path"),
		FileURL: get("file_url"), Bucket: get("bucketname"), Region: get("region"), Endpoint: get("endpoint"),
	}
	if sts.AccessKeyID == "" || sts.AccessKeySecret == "" || sts.FileID == "" || sts.FilePath == "" || sts.Bucket == "" {
		return nil, qwenWebFailure(http.StatusBadGateway, "qw-web: getstsToken reply is missing fields: "+qwenWebSnippet(body))
	}
	return sts, nil
}

// qwenWebOSSURL is the PUT target: the canonical bucket URL when the STS
// reply names an endpoint or region, else the reply's own file URL.
func qwenWebOSSURL(sts *qwenWebSTS) string {
	path := strings.TrimPrefix(sts.FilePath, "/")
	endpoint := strings.TrimSpace(sts.Endpoint)
	switch {
	case strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://"):
		return strings.TrimSuffix(endpoint, "/") + "/" + path
	case endpoint != "":
		return "https://" + sts.Bucket + "." + endpoint + "/" + path
	case sts.Region != "":
		return "https://" + sts.Bucket + "." + sts.Region + ".aliyuncs.com/" + path
	default:
		return sts.FileURL
	}
}

// qwenWebOSSPut uploads the document bytes with an OSS V1 signature —
// the signing the ali-oss default performs in both reference wrappers.
// It is a package variable so unit tests can replace the network leg.
var qwenWebPutObject = func(ctx context.Context, client *http.Client, sts *qwenWebSTS, content []byte) error {
	target := qwenWebOSSURL(sts)
	if target == "" {
		return fmt.Errorf("qw-web: STS reply has no upload target")
	}
	date := time.Now().UTC().Format(http.TimeFormat)
	resource := "/" + sts.Bucket + "/" + strings.TrimPrefix(sts.FilePath, "/")
	stringToSign := "PUT\n\ntext/plain\n" + date + "\nx-oss-security-token:" + sts.SecurityToken + "\n" + resource
	mac := hmac.New(sha1.New, []byte(sts.AccessKeySecret))
	_, _ = mac.Write([]byte(stringToSign))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(content))
	if err != nil {
		return err
	}
	req.Header.Set("Date", date)
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("x-oss-security-token", sts.SecurityToken)
	req.Header.Set("Authorization", "OSS "+sts.AccessKeyID+":"+signature)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("qw-web: OSS PUT request: %w", err)
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, qwenWebUploadBodyCap))
	if resp.StatusCode < 200 || resp.StatusCode > 204 {
		return qwenWebFailure(resp.StatusCode, "qw-web: OSS PUT refused: "+qwenWebSnippet(snippet))
	}
	return nil
}

// qwenWebParseFile submits the uploaded file for parsing and polls until
// the document is readable, the upstream says it failed, or the bounded
// wait expires. A risk-control refusal is reported as its own type so the
// caller's breaker can count it.
func qwenWebParseFile(ctx context.Context, client *http.Client, cred qwenWebCredential, fileID string) error {
	status, body, err := qwenWebFilePost(ctx, client, cred, qwenWebParsePath, map[string]any{"file_id": fileID})
	if err != nil {
		return fmt.Errorf("qw-web: files/parse request: %w", err)
	}
	if qwenWebIsRiskControl(body) {
		return &qwenWebRiskControlError{stage: "files/parse"}
	}
	if status != http.StatusOK {
		return qwenWebFailure(status, "qw-web: files/parse refused: "+qwenWebSnippet(body))
	}
	deadline := time.Now().Add(qwenWebParseTimeout)
	for {
		st, err := qwenWebParseStatus(ctx, client, cred, fileID)
		if err != nil {
			return err
		}
		switch st {
		case "success", "completed", "done":
			return nil
		case "failed", "error":
			return qwenWebFailure(http.StatusBadGateway, "qw-web: file parse failed upstream")
		}
		if time.Now().After(deadline) {
			return qwenWebFailure(http.StatusGatewayTimeout, "qw-web: file parse did not finish in time")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(qwenWebParsePollEvery):
		}
	}
}

func qwenWebParseStatus(ctx context.Context, client *http.Client, cred qwenWebCredential, fileID string) (string, error) {
	status, body, err := qwenWebFilePost(ctx, client, cred, qwenWebParsePoll, map[string]any{"file_id_list": []string{fileID}})
	if err != nil {
		return "", fmt.Errorf("qw-web: files/parse/status request: %w", err)
	}
	if qwenWebIsRiskControl(body) {
		return "", &qwenWebRiskControlError{stage: "files/parse/status"}
	}
	if status != http.StatusOK {
		return "", qwenWebFailure(status, "qw-web: files/parse/status refused: "+qwenWebSnippet(body))
	}
	var envelope struct {
		Data jsontext.Value `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", fmt.Errorf("qw-web: parse/status reply is not JSON: %w", err)
	}
	var rows []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(envelope.Data, &rows); err == nil && len(rows) > 0 {
		return rows[0].Status, nil
	}
	var one struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(envelope.Data, &one); err == nil {
		return one.Status, nil
	}
	return "pending", nil
}

// Parse guard: the reference wrappers measured Qwen counting document
// parses per egress — past a small rate the WAF starts challenging, and
// after three consecutive challenges the sane move is to stop uploading
// for a while instead of feeding the counter. Process-wide state, reset
// by any successful parse.
var qwenWebParseGuard = struct {
	sync.Mutex
	attempts   []time.Time
	strikes    int
	openUntil  time.Time
	inCooldown bool
}{}

const (
	qwenWebParseMaxPerWindow = 6
	qwenWebParseWindow       = 120 * time.Second
	qwenWebParseBreakerFor   = 300 * time.Second
	qwenWebParseMaxStrikes   = 3
)

var errQwenWebParseGuard = fmt.Errorf("qw-web: document parsing is paused by the upload guard")

func qwenWebParseGuardAllow(now time.Time) bool {
	qwenWebParseGuard.Lock()
	defer qwenWebParseGuard.Unlock()
	if now.Before(qwenWebParseGuard.openUntil) {
		return false
	}
	cut := now.Add(-qwenWebParseWindow)
	kept := qwenWebParseGuard.attempts[:0]
	for _, at := range qwenWebParseGuard.attempts {
		if at.After(cut) {
			kept = append(kept, at)
		}
	}
	qwenWebParseGuard.attempts = kept
	if len(kept) >= qwenWebParseMaxPerWindow {
		return false
	}
	qwenWebParseGuard.attempts = append(qwenWebParseGuard.attempts, now)
	return true
}

func qwenWebParseGuardReport(err error) {
	qwenWebParseGuard.Lock()
	defer qwenWebParseGuard.Unlock()
	if err == nil {
		qwenWebParseGuard.strikes = 0
		return
	}
	var rc *qwenWebRiskControlError
	if !errors.As(err, &rc) {
		return
	}
	qwenWebParseGuard.strikes++
	if qwenWebParseGuard.strikes >= qwenWebParseMaxStrikes {
		qwenWebParseGuard.openUntil = time.Now().Add(qwenWebParseBreakerFor)
		qwenWebParseGuard.strikes = 0
	}
}

// qwenWebUploadContext uploads one text document and returns the wire
// descriptor for the message's files array. Guarded by the parse rate
// limiter above; the descriptor mirrors the site client's own shape.
func qwenWebUploadContext(ctx context.Context, client *http.Client, cred qwenWebCredential, filename, text string) (map[string]any, error) {
	if !qwenWebParseGuardAllow(time.Now()) {
		return nil, errQwenWebParseGuard
	}
	sts, err := qwenWebGetSTS(ctx, client, cred, filename, len(text))
	if err != nil {
		qwenWebParseGuardReport(err)
		return nil, err
	}
	if err := qwenWebPutObject(ctx, client, sts, []byte(text)); err != nil {
		return nil, err
	}
	if err := qwenWebParseFile(ctx, client, cred, sts.FileID); err != nil {
		qwenWebParseGuardReport(err)
		return nil, err
	}
	qwenWebParseGuardReport(nil)
	fileURL := sts.FileURL
	if fileURL == "" {
		fileURL = qwenWebOSSURL(sts)
	}
	nowMs := time.Now().UnixMilli()
	return map[string]any{
		"type": "file",
		"file": map[string]any{
			"id": sts.FileID, "filename": filename, "name": filename,
			"size": len(text), "type": "text/plain",
			"meta": map[string]any{
				"name": filename, "size": len(text), "content_type": "text/plain",
				"parse_meta": map[string]any{"parse_status": "success"},
			},
			"hash": nil, "data": map[string]any{},
			"created_at": nowMs, "update_at": nowMs,
		},
		"id": sts.FileID, "url": fileURL, "name": filename, "collection_name": "",
		"status": "uploaded", "progress": 100, "greenNet": "success",
		"file_type": "text/plain", "showType": "file", "file_class": "document",
		"context": "full",
	}, nil
}
