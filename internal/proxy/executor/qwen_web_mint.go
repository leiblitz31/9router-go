package executor

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Qwen Web token mint: the refresh call the site's own frontend makes
// (performHttpRefresh in the chat.qwen.ai SPA bundle), captured live
// from the operator's browser on 2026-11-03:
//
//	GET https://auth.qwen.ai/api/v2/auths/refresh   (cookie credentials)
//
// Three properties of that call decide whether the mint works, and all
// three differ from the chat API's shape:
//
//   - The auth host is separate (auth.qwen.ai, /api/v2/). The captured
//     browser request carries no Baxia headers, but replaying that set
//     verbatim from this gateway is refused at the WAF stage
//     ("Invalid request header"); the lane's Baxia header set passes
//     that stage. See qwenWebMintHeaders for the union that works.
//   - Alongside the standard Origin, the SPA sends x-request-origin.
//     The auth server is the judge of "origin": a GET to the correct
//     URL with the executor's chat headers (Origin present,
//     x-request-origin absent) is refused with
//     {"success":false,"data":{"code":"Bad_Request",
//     "details":"Missing origin"}} — measured live on 2026-11-02. That,
//     not the cookie, is what kept every earlier mint failing: the old
//     code called POST chat.qwen.ai/api/v1/auths/ and was refused 405
//     before the cookie was ever judged.
//   - The response rotates the session: Set-Cookie carries the next
//     refresh_token (the captured request's refresh_token had been
//     issued 19 seconds earlier, by the previous refresh). The rotated
//     cookie must be merged back and stored, or the next mint presents
//     a refresh_token the server already consumed.

const qwenWebRefreshURL = "https://auth.qwen.ai/api/v2/auths/refresh"

// qwenWebMintHeaders assembles the refresh call's header set. Neither
// the captured browser set alone nor the lane's chat header set alone
// passes the auth server's validation — measured live on 2026-11-03:
//
//   - The browser set verbatim (no Baxia headers) is refused with
//     {"code":"Bad_Request","details":"Invalid request header"}.
//   - The chat set verbatim (Baxia headers, Origin, no x-request-origin)
//     passes header validation and is refused one stage later with
//     "Missing origin".
//
// So the mint sends the union: the lane's Baxia header set (which the
// WAF stage demands), plus the capture's x-request-origin (which the
// origin stage demands), with the cross-subdomain fetch metadata of
// the captured call.
func qwenWebMintHeaders(ctx context.Context, client *http.Client) http.Header {
	h := qwenWebHeaders(qwenWebBXUA(), qwenWebUMIDToken(ctx, client, false))
	h.Set("Accept", "application/json, text/plain, */*")
	h.Del("Content-Type")
	h.Set("Sec-Fetch-Site", "same-site")
	h.Set("x-request-origin", qwenWebBaseURL)
	return h
}

// qwenWebMintToken exchanges the session cookie for a fresh JWT via the
// frontend's refresh call. It returns the new token and the session
// cookie with the response's Set-Cookie values merged in; the caller
// persists both (see qwenWebRefreshToken). The token's location in the
// response varies across the site's builds (data.token /
// data.access_token / data.data.token / token), so all documented
// shapes are tried.
func qwenWebMintToken(ctx context.Context, client *http.Client, cred qwenWebCredential) (string, string, error) {
	if cred.Cookie == "" {
		return "", "", fmt.Errorf("no session cookie stored on the connection")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qwenWebRefreshURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header = qwenWebMintHeaders(ctx, client)
	req.Header.Set("Cookie", cred.Cookie)
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("mint request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return "", "", fmt.Errorf("read mint response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("mint refused with HTTP %d: %s", resp.StatusCode, qwenWebMintErrorDetail(body))
	}
	token := qwenWebFindMintedToken(body)
	if token == "" {
		return "", "", fmt.Errorf("mint response carried no token: %s", qwenWebMintErrorDetail(body))
	}
	return token, qwenWebMergeCookieHeader(cred.Cookie, resp.Cookies()), nil
}

// qwenWebMintErrorDetail renders the failure envelope's code/details
// ({"data":{"code":…,"details":…}}) for error messages; anything else
// is truncated raw body. Failure envelopes carry no credential, so
// this is safe to surface in errors and logs.
func qwenWebMintErrorDetail(body []byte) string {
	var envelope struct {
		Data struct {
			Code    string `json:"code"`
			Details string `json:"details"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Data.Code != "" {
		return envelope.Data.Code + ": " + envelope.Data.Details
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 160 {
		s = s[:160]
	}
	return s
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

// qwenWebMergeCookieHeader folds a response's Set-Cookie values into a
// stored Cookie header: a name the response sets replaces the stored
// value (or is appended when new), and names the response does not
// mention are kept verbatim, in their original order. It returns the
// input unchanged when the response sets nothing new.
func qwenWebMergeCookieHeader(existing string, setCookies []*http.Cookie) string {
	if len(setCookies) == 0 {
		return existing
	}
	type pair struct{ name, value string }
	pairs := []pair{}
	index := map[string]int{}
	put := func(name, value string) {
		if i, ok := index[name]; ok {
			pairs[i].value = value
			return
		}
		index[name] = len(pairs)
		pairs = append(pairs, pair{name, value})
	}
	for _, part := range strings.Split(existing, ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
			put(k, strings.TrimSpace(v))
		}
	}
	changed := false
	for _, c := range setCookies {
		if c.Name == "" {
			continue
		}
		if i, ok := index[c.Name]; ok && pairs[i].value == c.Value {
			continue
		}
		put(c.Name, c.Value)
		changed = true
	}
	if !changed {
		return existing
	}
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.name + "=" + p.value
	}
	return strings.Join(out, "; ")
}
