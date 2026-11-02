package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"9router/proxy/internal/db"
	"9router/proxy/internal/log"
)

// Qwen Web's second mode: a logged-in account. The connection mirrors the
// deepseek-web storage pattern exactly — the credential's API half lives in
// the row's apiKey (here a JWT from localStorage["token"] on chat.qwen.ai),
// and its cookie half in providerSpecificData.qwenCookie (the session
// cookie header, whose `token` cookie is the session's root). No connection
// — or a connection whose apiKey is empty/"guest" — means guest mode, and
// guest mode never pretends to be an account.
//
// Qwen's advantage over deepseek-web, and the reason this file exists: the
// JWT can be re-minted from the session cookie via POST /api/v1/auths/, so
// an expired token is not automatically a dead connection. The refresh is
// opportunistic and bounded: validate once, mint once, retry once — and if
// the mint fails, the error says the token is dead rather than quietly
// falling back to guest mode, which would hide a dead account behind an
// anonymous answer.

type qwenWebCredential struct {
	Token    string
	Cookie   string
	LoggedIn bool
}

// qwenWebCredentialFor reads the request's credential halves. The apiKey
// accepts the forms an operator actually pastes: a bare JWT, the
// {"value": …} wrapper shape other web providers' rows carry, a JSON
// {"token":…,"cookie":…} object, or a raw Cookie header string (from which
// the `token` cookie is extracted). The cookie may also arrive via
// providerSpecificData.qwenCookie, which wins when both are present.
func qwenWebCredentialFor(req *Request) qwenWebCredential {
	cred := qwenWebCredential{}
	if req.ConnData != nil {
		if v, ok := req.ConnData["qwenCookie"].(string); ok {
			cred.Cookie = strings.TrimSpace(v)
		}
	}
	raw := strings.TrimSpace(req.APIKey)
	switch {
	case raw == "" || raw == "guest" || raw == "anonymous":
		// guest mode; a cookie alone does not make an account
	case strings.HasPrefix(raw, "{"):
		var obj struct {
			Value  *string `json:"value"`
			Token  string  `json:"token"`
			Cookie string  `json:"cookie"`
		}
		if err := json.Unmarshal([]byte(raw), &obj); err == nil {
			if obj.Value != nil {
				cred.Token = strings.TrimSpace(*obj.Value)
			}
			if obj.Token != "" {
				cred.Token = strings.TrimSpace(obj.Token)
			}
			if obj.Cookie != "" && cred.Cookie == "" {
				cred.Cookie = strings.TrimSpace(obj.Cookie)
			}
		}
	case strings.Contains(raw, "token="):
		cred.Cookie = raw
	default:
		cred.Token = raw
	}
	if cred.Token == "" && cred.Cookie != "" {
		cred.Token = qwenWebCookieValue(cred.Cookie, "token")
	}
	cred.LoggedIn = cred.Token != ""
	return cred
}

// qwenWebCookieValue extracts one cookie from a Cookie header string.
func qwenWebCookieValue(header, name string) string {
	for _, part := range strings.Split(header, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && k == name {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// qwenWebJWTExpiry returns the exp claim of a JWT without verifying the
// signature — the upstream is the signature's judge; this only decides
// whether the token is *worth presenting* or should go straight to the
// mint path. ok is false for anything that is not a three-segment JWT
// with a parseable payload.
func qwenWebJWTExpiry(token string) (exp time.Time, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// qwenWebAuthError marks an upstream 401 so the execute flow can run its
// single mint-and-retry.
type qwenWebAuthError struct{ stage string }

func (e *qwenWebAuthError) Error() string {
	return "qw-web: account token rejected (401) at " + e.stage
}

// qwenWebSessionProbe performs the session probe and returns the
// upstream status and body: GET the chat history list — an endpoint only
// a session can answer, so a refusal here is a real auth verdict, unlike
// the public /api/models. It is the one implementation behind both the
// executor's pre-turn validation and the dashboard's "Check" probe, so
// the two judge a credential under the same header set.
func qwenWebSessionProbe(ctx context.Context, client *http.Client, cred qwenWebCredential) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qwenWebBaseURL+"/api/v2/chats/?page=1", nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header = qwenWebHeaders(qwenWebBXUA(), qwenWebUMIDToken(ctx, client, false))
	qwenWebApplyAuth(req.Header, cred)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

// QwenWebSessionRejected reports whether a session-probe response is an
// auth refusal. The site answers a dead token two ways: a bare HTTP 401,
// and — the shape measured live on 2026-11-02 — HTTP 200 with a
// {"success":false,"data":{"code":"unauthorized"}} envelope ("Your
// session has expired, or the token is no longer valid"). A probe that
// trusted the status alone would call every dead token valid, and the
// user would only find out on their first real request. Other
// success:false codes are not auth verdicts and do not count.
func QwenWebSessionRejected(status int, body []byte) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	if status != http.StatusOK {
		return false
	}
	var envelope struct {
		Success *bool `json:"success"`
		Data    struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false
	}
	return envelope.Success != nil && !*envelope.Success && envelope.Data.Code == "unauthorized"
}

// qwenWebValidateToken probes an endpoint only a session can answer — the
// chat history list — so a refusal here is a real auth verdict, unlike
// the public /api/models. Non-auth failures are not auth verdicts and do
// not block the turn.
func qwenWebValidateToken(ctx context.Context, client *http.Client, cred qwenWebCredential) error {
	status, body, err := qwenWebSessionProbe(ctx, client, cred)
	if err != nil {
		return nil
	}
	if QwenWebSessionRejected(status, body) {
		return &qwenWebAuthError{stage: "validation"}
	}
	return nil
}

// QwenWebToken parses a pasted qw-web credential down to its JWT: the
// apiKey in any form the add form accepts (bare JWT, {"value": …}
// wrapper, JSON object, raw Cookie header) plus the cookie half in
// providerSpecificData.qwenCookie. "" means guest mode — no account
// credential was pasted. It is the dashboard validator's half of
// qwenWebCredentialFor, exported so the parsing lives in exactly one
// place.
func QwenWebToken(apiKey string, providerData map[string]any) string {
	return qwenWebCredentialFor(&Request{APIKey: apiKey, ConnData: providerData}).Token
}

// QwenWebDoer is the transport the dashboard's probe seam speaks: one
// request in, status and body out. The dashboard adapts its stubbed,
// proxy-aware probe function to it (the qoderProbeDoer pattern), so the
// session probe below runs on the dashboard's transport while keeping
// the executor's own headers and credential handling.
type QwenWebDoer func(ctx context.Context, method, rawURL string, headers map[string]string, body []byte) (int, []byte, error)

// qwenWebDoerTransport carries an http.Client's requests over a
// QwenWebDoer, so the probe implementation (Baxia headers, umid fetch,
// auth) is shared verbatim instead of re-derived per caller. Response
// headers do not survive the seam; the one consumer that read one (the
// umid ETag fallback) simply falls back to the body's token pattern,
// then to the synthetic token — the same fallbacks, one rung shorter.
type qwenWebDoerTransport struct{ do QwenWebDoer }

func (t qwenWebDoerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		sent, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = sent
	}
	headers := make(map[string]string, len(req.Header))
	for name := range req.Header {
		headers[name] = req.Header.Get(name)
	}
	status, respBody, err := t.do(req.Context(), req.Method, req.URL.String(), headers, body)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Header:        make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(respBody)),
		ContentLength: int64(len(respBody)),
		Request:       req,
	}, nil
}

// QwenWebProbeSession is the dashboard-validator form of the executor's
// pre-turn session validation: the same probe (qwenWebSessionProbe) with
// the credential parsed the same way, executed over the caller's
// transport. It returns the upstream HTTP status and body — the caller
// reads the verdict with QwenWebSessionRejected. (0, nil, nil) means the
// credential parses to guest mode and there is nothing to probe.
func QwenWebProbeSession(ctx context.Context, do QwenWebDoer, apiKey string, providerData map[string]any) (int, []byte, error) {
	cred := qwenWebCredentialFor(&Request{APIKey: apiKey, ConnData: providerData})
	if !cred.LoggedIn {
		return 0, nil, nil
	}
	client := &http.Client{Transport: qwenWebDoerTransport{do: do}}
	return qwenWebSessionProbe(ctx, client, cred)
}

// qwenWebApplyAuth sets the account headers on an upstream request.
func qwenWebApplyAuth(h http.Header, cred qwenWebCredential) {
	if !cred.LoggedIn {
		return
	}
	h.Set("Authorization", "Bearer "+cred.Token)
	if cred.Cookie != "" {
		h.Set("Cookie", cred.Cookie)
	}
}

// qwenWebMintToken exchanges the session cookie for a fresh JWT via
// POST /api/v1/auths/. The token's location in the response varies across
// the site's builds (data.token / data.access_token / data.data.token /
// token), so all documented shapes are tried.
func qwenWebMintToken(ctx context.Context, client *http.Client, cred qwenWebCredential) (string, error) {
	if cred.Cookie == "" {
		return "", fmt.Errorf("no session cookie stored on the connection")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qwenWebBaseURL+"/api/v1/auths/", nil)
	if err != nil {
		return "", err
	}
	req.Header = qwenWebHeaders(qwenWebBXUA(), qwenWebUMIDToken(ctx, client, false))
	req.Header.Set("Cookie", cred.Cookie)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("mint request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return "", fmt.Errorf("read mint response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mint refused with HTTP %d", resp.StatusCode)
	}
	if token := qwenWebFindMintedToken(body); token != "" {
		return token, nil
	}
	return "", fmt.Errorf("mint response carried no token")
}

func qwenWebFindMintedToken(body []byte) string {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return ""
	}
	get := func(m map[string]any, keys ...string) string {
		for _, k := range keys {
			if s, ok := m[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	if s := get(root, "token", "access_token"); s != "" {
		return s
	}
	if data, ok := root["data"].(map[string]any); ok {
		if s := get(data, "token", "access_token"); s != "" {
			return s
		}
		if nested, ok := data["data"].(map[string]any); ok {
			return get(nested, "token", "access_token")
		}
	}
	return ""
}

// qwenWebRefreshToken runs the one mint the lane allows itself and, on
// success, stores the new JWT back on the connection row (the ds-web
// renewal pattern: only the data column is written, and only its apiKey
// field changes) so the next request starts from the fresh token. A store
// failure does not fail the refresh — the fresh token still serves this
// request — it is logged and left for the next mint to retry.
func qwenWebRefreshToken(ctx context.Context, client *http.Client, req *Request, cred *qwenWebCredential) error {
	fresh, err := qwenWebMintToken(ctx, client, *cred)
	if err != nil {
		return err
	}
	cred.Token = fresh
	if req.ConnectionID == "" {
		return nil
	}
	conn, err := db.GetConnection()
	if err != nil {
		log.Debug("qw-web", "minted a fresh token but no database handle to store it", "error", err)
		return nil
	}
	repo := db.NewRepo(conn)
	row, err := repo.GetProviderConnectionByID(req.ConnectionID)
	if err != nil {
		log.Debug("qw-web", "minted a fresh token but the connection row could not be read", "error", err)
		return nil
	}
	data := map[string]any{}
	if row.Data != "" {
		_ = json.Unmarshal([]byte(row.Data), &data)
	}
	data["apiKey"] = fresh
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	if err := repo.UpdateConnectionData(req.ConnectionID, string(encoded)); err != nil {
		log.Debug("qw-web", "minted a fresh token but storing it failed", "error", err)
	}
	return nil
}

// qwenWebEnsureAuth validates the account token before the turn, taking
// the mint path when the token is already expired on its face or the
// upstream rejects it. It returns a clean 401 failure when the account
// cannot be authenticated — never a silent guest fallback.
func qwenWebEnsureAuth(ctx context.Context, client *http.Client, req *Request, cred *qwenWebCredential) error {
	dead := func(reason string) error {
		return qwenWebFailure(http.StatusUnauthorized,
			"qw-web: account token is dead ("+reason+") and re-minting from the stored cookie failed or no cookie is stored — paste a fresh token from chat.qwen.ai (DevTools console: copy(localStorage.getItem('token')))")
	}
	if exp, ok := qwenWebJWTExpiry(cred.Token); ok && time.Now().After(exp) {
		if err := qwenWebRefreshToken(ctx, client, req, cred); err != nil {
			return dead("expired " + exp.Format(time.RFC3339))
		}
		return nil
	}
	if err := qwenWebValidateToken(ctx, client, *cred); err != nil {
		if err := qwenWebRefreshToken(ctx, client, req, cred); err != nil {
			return dead("rejected by upstream")
		}
		if err := qwenWebValidateToken(ctx, client, *cred); err != nil {
			return dead("rejected again after re-mint")
		}
	}
	return nil
}
