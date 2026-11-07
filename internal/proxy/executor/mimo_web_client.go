package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	json "encoding/json/v2"
	"fmt"
	"io"
	mrand "math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"9router/proxy/internal/proxy"
)

// Xiaomi MiMo Studio (aistudio.xiaomimimo.com) spoken as a plain HTTP API.
//
// The Studio is a signed-in web chat. Its page talks to same-origin
// endpoints under /open-apis, the chat turn itself is one POST that answers
// a server-sent event stream, and the credential is the session the Xiaomi
// account login plants on the domain: three cookies (serviceToken, userId,
// xiaomichatbot_ph). There is no API key and no proof-of-work; a client
// that presents the cookies with a consistent browser fingerprint is
// answered like the page. Protocol shape verified against the live site
// and two independent open implementations, 2026-11-07.
//
// The one operational fact that shapes everything here: a copied
// serviceToken lives about a day. Nothing in this executor pretends
// otherwise — when the session dies the upstream says so (401), and the
// failure message tells the operator to capture a fresh one.

const (
	mimoWebBaseURL = "https://aistudio.xiaomimimo.com"

	// The v3 chat path. The older /open-apis/bot/chat answers every
	// request with an error event since the platform upgrade.
	mimoWebChatPath = "/open-apis/react/chat"
	// UltraSpeed models are refused by the v3 path with a model-name
	// error; they live on the fastchat gateway, whose save path moves
	// with them.
	mimoWebUltraChatPath = "/fastchat/open-apis/bot/chat"
	mimoWebSavePath      = "/open-apis/chat/conversation/save"
	mimoWebUltraSavePath = "/fastchat/open-apis/chat/conversation/save"
	mimoWebValidatePath  = "/open-apis/user/mi/get"

	mimoWebUltraModel = "mimo-v2.6-pro-ultraspeed-studio"

	// Browser fingerprint. The upstream cross-checks the UA against the
	// client-hint headers, so the version named in both must agree, and
	// the language and timezone must not contradict each other.
	mimoWebUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"
	mimoWebSecCHUA   = "\"Google Chrome\";v=\"153\", \"Chromium\";v=\"153\", \"Not)A;Brand\";v=\"24\""
)

// mimoWebCredentials is one captured Studio session.
type mimoWebCredentials struct {
	ServiceToken string
	UserID       string
	PH           string
}

// mimoWebParseCredentials reads the connection's API key, which the
// operator fills with the Studio session in whichever shape they have at
// hand: a JSON object naming the three cookies, or a Cookie header copied
// out of a browser's developer tools. Values are never logged or echoed;
// a parse failure names only what is missing.
func mimoWebParseCredentials(raw string) (mimoWebCredentials, error) {
	var creds mimoWebCredentials
	s := strings.TrimSpace(raw)
	s = strings.TrimSpace(strings.TrimPrefix(s, "Cookie:"))
	if s == "" {
		return creds, fmt.Errorf("mimo-web needs the MiMo Studio session — paste the serviceToken, userId and xiaomichatbot_ph cookies from a signed-in aistudio.xiaomimimo.com tab into the connection")
	}
	if strings.HasPrefix(s, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(s), &obj); err == nil {
			creds.ServiceToken = mimoWebCredentialValue(obj, "serviceToken", "service_token", "xiaomichatbot_serviceToken")
			creds.UserID = mimoWebCredentialValue(obj, "userId", "user_id")
			creds.PH = mimoWebCredentialValue(obj, "xiaomichatbot_ph", "ph")
		}
	}
	if creds.ServiceToken == "" || creds.UserID == "" || creds.PH == "" {
		pairs := mimoWebCookiePairs(s)
		if creds.ServiceToken == "" {
			creds.ServiceToken = pairs["serviceToken"]
		}
		if creds.ServiceToken == "" {
			creds.ServiceToken = pairs["xiaomichatbot_serviceToken"]
		}
		if creds.UserID == "" {
			creds.UserID = pairs["userId"]
		}
		if creds.PH == "" {
			creds.PH = pairs["xiaomichatbot_ph"]
		}
	}
	creds.ServiceToken = mimoWebTrimCredential(creds.ServiceToken)
	creds.UserID = mimoWebTrimCredential(creds.UserID)
	creds.PH = mimoWebTrimCredential(creds.PH)
	var missing []string
	if creds.ServiceToken == "" {
		missing = append(missing, "serviceToken")
	}
	if creds.UserID == "" {
		missing = append(missing, "userId")
	}
	if creds.PH == "" {
		missing = append(missing, "xiaomichatbot_ph")
	}
	if len(missing) > 0 {
		return creds, fmt.Errorf("mimo-web session is incomplete: missing %s — copy all three cookies from a signed-in aistudio.xiaomimimo.com tab", strings.Join(missing, ", "))
	}
	return creds, nil
}

func mimoWebCredentialValue(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// mimoWebCookiePairs splits a Cookie header value into name → value.
func mimoWebCookiePairs(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 {
			out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return out
}

// mimoWebTrimCredential removes the wrapping quotes and stray whitespace a
// value picks up on its way through developer tools. The ph value is
// checked upstream in a query string, where a stray quote reads as a
// different session and the chat path answers 401 while the cookie-only
// health check still passes — one shared cleanup keeps the two in step.
func mimoWebTrimCredential(s string) string {
	return strings.Trim(strings.TrimSpace(s), "\"")
}

// cookieHeader renders the session the way the site's own browser sends
// it: the token travels under both its plain and its prefixed name, the
// prefixed values quoted.
func (c mimoWebCredentials) cookieHeader() string {
	return fmt.Sprintf("userId=%s; serviceToken=%s; xiaomichatbot_serviceToken=%q; xiaomichatbot_ph=%q",
		c.UserID, c.ServiceToken, c.ServiceToken, c.PH)
}

func mimoWebSetHeaders(h http.Header) {
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("Origin", mimoWebBaseURL)
	h.Set("Referer", mimoWebBaseURL+"/")
	h.Set("x-timezone", "Asia/Shanghai")
	h.Set("User-Agent", mimoWebUserAgent)
	h.Set("sec-ch-ua", mimoWebSecCHUA)
	h.Set("sec-ch-ua-mobile", "?0")
	h.Set("sec-ch-ua-platform", "\"Windows\"")
	h.Set("sec-fetch-dest", "empty")
	h.Set("sec-fetch-mode", "cors")
	h.Set("sec-fetch-site", "same-origin")
}

func mimoWebRandomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// mimoWebChatBody is the v3 turn payload. multiMedias must serialize as
// an array — a null is refused as an invalid parameter.
type mimoWebChatBody struct {
	MsgID          string             `json:"msgId"`
	ConversationID string             `json:"conversationId"`
	Query          string             `json:"query"`
	IsEditedQuery  bool               `json:"isEditedQuery"`
	SceneType      any                `json:"sceneType"`
	Params         map[string]any     `json:"params"`
	ModelConfig    mimoWebModelConfig `json:"modelConfig"`
	MultiMedias    []any              `json:"multiMedias"`
	Attachments    []any              `json:"attachments"`
}

type mimoWebModelConfig struct {
	EnableThinking  bool     `json:"enableThinking"`
	WebSearchStatus string   `json:"webSearchStatus"`
	Model           string   `json:"model"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
}

// mimoWebClient is one Studio session spoken over the router's transport.
type mimoWebClient struct {
	http  *http.Client
	creds mimoWebCredentials
}

func newMimoWebClient(client *http.Client, creds mimoWebCredentials) *mimoWebClient {
	if client == nil {
		client = http.DefaultClient
	}
	return &mimoWebClient{http: client, creds: creds}
}

func mimoWebIsUltra(model string) bool { return model == mimoWebUltraModel }

func (c *mimoWebClient) chatURL(model string) string {
	path := mimoWebChatPath
	if mimoWebIsUltra(model) {
		path = mimoWebUltraChatPath
	}
	return mimoWebBaseURL + path + "?xiaomichatbot_ph=" + url.QueryEscape(c.creds.PH)
}

// chat opens one turn and returns the live event stream. A 429 or 5xx
// before the stream starts is retried inside a bounded budget: agent
// clients call at a rate the site throttles, and passing the first 429
// straight back makes the client hammer the same channel with its own
// retries. Every attempt mints a fresh msgId — the upstream deduplicates
// on it and answers a reused one with "already processed".
func (c *mimoWebClient) chat(ctx context.Context, body mimoWebChatBody) (io.ReadCloser, error) {
	const (
		maxRetries  = 5
		retryBudget = 60 * time.Second
	)
	deadline := time.Now().Add(retryBudget)
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			body.MsgID = mimoWebRandomID()
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("mimo-web: marshal turn: %w", err)
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatURL(body.ModelConfig.Model), bytes.NewReader(encoded))
		if err != nil {
			return nil, fmt.Errorf("mimo-web: build turn: %w", err)
		}
		mimoWebSetHeaders(httpReq.Header)
		httpReq.Header.Set("Cookie", c.creds.cookieHeader())

		resp, err := c.http.Do(httpReq)
		if err != nil {
			return nil, proxy.UpstreamFailure(http.StatusBadGateway, "mimo-web: connection failed: "+err.Error())
		}
		if resp.StatusCode == http.StatusOK {
			return resp.Body, nil
		}
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()

		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if retryable && attempt < maxRetries {
			secs := 1 << (attempt + 1)
			if secs > 16 {
				secs = 16
			}
			backoff := time.Duration(secs)*time.Second + time.Duration(mrand.Int63n(1000))*time.Millisecond
			if backoff <= time.Until(deadline) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(backoff):
				}
				continue
			}
		}
		return nil, mimoWebStatusFailure(resp.StatusCode, string(snippet))
	}
}

// mimoWebStatusFailure maps a refused turn onto the failure the router
// acts on, with the message the operator can do something about.
func mimoWebStatusFailure(status int, snippet string) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return proxy.UpstreamFailure(status,
			"mimo-web session refused (the Studio login has expired or was revoked) — capture a fresh serviceToken, userId and xiaomichatbot_ph from a signed-in aistudio.xiaomimimo.com tab")
	case 461:
		// The platform's own ban signal on the chat paths: the account
		// has been flagged, temporarily or permanently. Retrying or
		// rotating will not clear it.
		return proxy.UpstreamFailure(status,
			"mimo-web account restricted by the platform (HTTP 461) — this Xiaomi account is flagged; use a different account")
	}
	msg := strings.TrimSpace(snippet)
	if len(msg) > 300 {
		msg = msg[:300]
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return proxy.UpstreamFailure(status, "mimo-web: upstream answered "+msg)
}

// saveConversation registers the conversation on the account, the way the
// page does after a turn: it is what later turns (and the site's own
// history) hang off. Best effort — a failed save never fails the turn.
func (c *mimoWebClient) saveConversation(ctx context.Context, conversationID, query string, ultra bool) {
	path := mimoWebSavePath
	if ultra {
		path = mimoWebUltraSavePath
	}
	title := []rune(query)
	if len(title) > 30 {
		title = title[:30]
	}
	payload, err := json.Marshal(map[string]any{
		"conversationId": conversationID,
		"title":          string(title),
		"type":           "chat",
		"multiMedias":    []any{},
	})
	if err != nil {
		return
	}
	saveCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(saveCtx, http.MethodPost,
		mimoWebBaseURL+path+"?xiaomichatbot_ph="+url.QueryEscape(c.creds.PH), bytes.NewReader(payload))
	if err != nil {
		return
	}
	mimoWebSetHeaders(req.Header)
	req.Header.Set("Cookie", c.creds.cookieHeader())
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	_ = resp.Body.Close()
}

// validateSession reports whether the captured session is still alive:
// the same identity call the page makes on load, answered 200 with a zero
// business code only for a live session.
func (c *mimoWebClient) validateSession(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mimoWebBaseURL+mimoWebValidatePath, nil)
	if err != nil {
		return err
	}
	mimoWebSetHeaders(req.Header)
	req.Header.Set("Cookie", c.creds.cookieHeader())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mimo-web session check: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var result struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(body, &result); err == nil && result.Code != 0 {
		return fmt.Errorf("mimo-web session check: code %d", result.Code)
	}
	return nil
}
