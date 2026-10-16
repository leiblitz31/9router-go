package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func init() {
	Register("nous", refreshNous)
}

// Nous Research Portal (client_id "hermes-cli"). The flow is two steps:
// refresh_token → a new access_token, then mint a short-lived agent key with
// that token. Only successful inference proves the credential was accepted
// upstream, so the agent key — not the access token — is the api key that
// rides on requests.
//
// Port of upstream open-sse/services/tokenRefresh/providers.js refreshNousToken
// (decolua/9router 165f9b90).
const (
	nousPortalBaseURL = "https://portal.nousresearch.com"
	nousClientID      = "hermes-cli"

	// nousAgentKeyMinTTL asks the portal to keep the minted key valid for at
	// least two hours, so a gateway refresh cycle never runs the key dry
	// between two refreshes.
	nousAgentKeyMinTTL = 7200

	// nousDefaultExpiresIn covers an answer that omits expires_in. Storing
	// zero would mark the connection expired and refresh it on every request.
	nousDefaultExpiresIn = 3600

	nousTokenTimeout    = 30 * time.Second
	nousAgentKeyTimeout = 15 * time.Second
)

// Endpoints are vars so a test can point them at a fake portal.
var (
	nousTokenURL    = nousPortalBaseURL + "/api/oauth/token"
	nousAgentKeyURL = nousPortalBaseURL + "/api/oauth/agent-key"
)

// nousTokenPayload is the token endpoint's answer. refresh_token is optional:
// the portal rotates it on some grants and omits it on others, and keeping the
// current one is the only safe default.
type nousTokenPayload struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// nousAgentKeyPayload is the agent-key endpoint's answer.
type nousAgentKeyPayload struct {
	APIKey    string `json:"api_key"`
	KeyID     string `json:"key_id"`
	ExpiresAt string `json:"expires_at"`
}

// refreshNous refreshes a Nous Portal credential.
//
// The agent key is best-effort: when the mint fails the access token still
// authenticates inference, so the refresh succeeds and the connection is not
// parked on a portal hiccup (upstream parity — it logs a warning and falls
// back to the access_token).
func refreshNous(ctx context.Context, p *Params) (*TokenResult, error) {
	if p.RefreshToken == "" {
		return nil, fmt.Errorf("nous: missing refresh_token")
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {p.RefreshToken},
		"client_id":     {nousClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, nousTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("nous: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// The portal expects the refresh token in the header as well.
	req.Header.Set("x-nous-refresh-token", p.RefreshToken)

	client := clientOrDefault(p)
	tok, err := nousDo(client, req, nousTokenTimeout)
	if err != nil {
		return nil, fmt.Errorf("nous: token refresh: %w", err)
	}
	defer tok.Body.Close()
	if tok.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nous: token refresh returned %s", tok.Status)
	}
	var parsed nousTokenPayload
	if err := json.NewDecoder(tok.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("nous: decode token answer: %w", err)
	}
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("nous: token refresh returned no access_token")
	}

	result := &TokenResult{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		ExpiresIn:    parsed.ExpiresIn,
	}
	if result.RefreshToken == "" {
		result.RefreshToken = p.RefreshToken
	}
	if result.ExpiresIn <= 0 {
		result.ExpiresIn = nousDefaultExpiresIn
	}

	key, id, expiresAt := mintNousAgentKey(ctx, client, parsed.AccessToken)
	if key != "" {
		result.APIKey = key
	}
	if id != "" || expiresAt != "" {
		result.Extra = make(map[string]string, 2)
		if id != "" {
			result.Extra["agentKeyId"] = id
		}
		if expiresAt != "" {
			result.Extra["agentKeyExpiresAt"] = expiresAt
		}
	}
	// Without a minted key the access token is the credential inference accepts.
	if result.APIKey == "" {
		result.APIKey = parsed.AccessToken
	}
	return result, nil
}

// mintNousAgentKey exchanges an access token for a short-lived inference key.
// Every failure is soft: the caller falls back to the access token.
func mintNousAgentKey(ctx context.Context, client *http.Client, accessToken string) (key, keyID, expiresAt string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, nousAgentKeyURL,
		bytes.NewReader([]byte(`{"min_ttl_seconds":`+fmt.Sprint(nousAgentKeyMinTTL)+`}`)))
	if err != nil {
		return "", "", ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := nousDo(client, req, nousAgentKeyTimeout)
	if err != nil {
		return "", "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", ""
	}
	var payload nousAgentKeyPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", "", ""
	}
	return payload.APIKey, payload.KeyID, payload.ExpiresAt
}

// nousDo sends the request, bounding it with timeout when the caller's context
// carries no deadline of its own.
func nousDo(client *http.Client, req *http.Request, timeout time.Duration) (*http.Response, error) {
	if _, hasDeadline := req.Context().Deadline(); hasDeadline {
		return client.Do(req)
	}
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	// The deadline is released when the body closes, not before: the body is
	// still being read under it.
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// clientOrDefault never hands a nil client to http.Client.Do.
func clientOrDefault(p *Params) *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

// cancelOnClose releases a context cancel when the body closes.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
