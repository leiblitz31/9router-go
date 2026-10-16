package oauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// nousTestUpstream serves both portal endpoints and records what it received.
type nousTestUpstream struct {
	server  *httptest.Server
	tokens  []url.Values
	headers []http.Header
	agent   []string
}

func newNousTestUpstream(t *testing.T, tokenStatus int, tokenBody string, agentStatus int, agentBody string) *nousTestUpstream {
	t.Helper()
	up := &nousTestUpstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		vals, _ := url.ParseQuery(string(body))
		up.tokens = append(up.tokens, vals)
		up.headers = append(up.headers, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(tokenStatus)
		io.WriteString(w, tokenBody)
	})
	mux.HandleFunc("/api/oauth/agent-key", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		up.agent = append(up.agent, string(body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(agentStatus)
		io.WriteString(w, agentBody)
	})
	up.server = httptest.NewServer(mux)
	t.Cleanup(up.server.Close)
	return up
}

// nousServerSwap points the package endpoints at the fake portal for one test.
func nousServerSwap(t *testing.T, up *nousTestUpstream) {
	t.Helper()
	origToken, origKey := nousTokenURL, nousAgentKeyURL
	nousTokenURL = up.server.URL + "/api/oauth/token"
	nousAgentKeyURL = up.server.URL + "/api/oauth/agent-key"
	t.Cleanup(func() {
		nousTokenURL, nousAgentKeyURL = origToken, origKey
	})
}

func TestRefreshNous_MintsAgentKey(t *testing.T) {
	up := newNousTestUpstream(t,
		http.StatusOK, `{"access_token":"acc-2","refresh_token":"ref-2","expires_in":1800}`,
		http.StatusOK, `{"api_key":"agent-key-1","key_id":"kid-1","expires_at":"2026-10-16T20:00:00Z"}`,
	)
	nousServerSwap(t, up)

	res, err := refreshNous(context.Background(), &Params{
		Client: up.server.Client(), Provider: "nous", RefreshToken: "ref-1",
	})
	if err != nil {
		t.Fatalf("refreshNous: %v", err)
	}
	if res.AccessToken != "acc-2" {
		t.Errorf("AccessToken = %q, want acc-2", res.AccessToken)
	}
	if res.RefreshToken != "ref-2" {
		t.Errorf("RefreshToken = %q, want the rotated ref-2", res.RefreshToken)
	}
	if res.APIKey != "agent-key-1" {
		t.Errorf("APIKey = %q, want the minted agent key", res.APIKey)
	}
	if res.Extra["agentKeyId"] != "kid-1" || res.Extra["agentKeyExpiresAt"] != "2026-10-16T20:00:00Z" {
		t.Errorf("Extra = %v, want the agent-key id and expiry", res.Extra)
	}

	if len(up.tokens) != 1 {
		t.Fatalf("token endpoint called %d times, want 1", len(up.tokens))
	}
	form := up.tokens[0]
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "ref-1" || form.Get("client_id") != nousClientID {
		t.Errorf("token form = %v", form)
	}
	if got := up.headers[0].Get("x-nous-refresh-token"); got != "ref-1" {
		t.Errorf("x-nous-refresh-token = %q, want ref-1", got)
	}
	if len(up.agent) != 1 {
		t.Fatalf("agent-key endpoint called %d times, want 1", len(up.agent))
	}
	if !strings.Contains(up.agent[0], `"min_ttl_seconds":7200`) {
		t.Errorf("agent-key body = %s, want the 7200s min ttl", up.agent[0])
	}
}

// A failed mint is soft: the access token still authenticates inference, so the
// refresh must succeed and hand the token back as the api key.
func TestRefreshNous_AgentKeyFailureFallsBackToAccessToken(t *testing.T) {
	up := newNousTestUpstream(t,
		http.StatusOK, `{"access_token":"acc-2","expires_in":1800}`,
		http.StatusForbidden, `{"error":"forbidden"}`,
	)
	nousServerSwap(t, up)

	res, err := refreshNous(context.Background(), &Params{
		Client: up.server.Client(), Provider: "nous", RefreshToken: "ref-1",
	})
	if err != nil {
		t.Fatalf("a failed mint must not fail the refresh: %v", err)
	}
	if res.APIKey != "acc-2" {
		t.Errorf("APIKey = %q, want the access token as fallback", res.APIKey)
	}
	if len(res.Extra) != 0 {
		t.Errorf("Extra = %v, want empty when nothing was minted", res.Extra)
	}
}

func TestRefreshNous_OmittedFieldsGetDefaults(t *testing.T) {
	up := newNousTestUpstream(t,
		http.StatusOK, `{"access_token":"acc-2"}`,
		http.StatusOK, `{"api_key":"k"}`,
	)
	nousServerSwap(t, up)

	res, err := refreshNous(context.Background(), &Params{
		Client: up.server.Client(), Provider: "nous", RefreshToken: "ref-1",
	})
	if err != nil {
		t.Fatalf("refreshNous: %v", err)
	}
	if res.RefreshToken != "ref-1" {
		t.Errorf("RefreshToken = %q, want the current one kept when the answer omits it", res.RefreshToken)
	}
	if res.ExpiresIn != nousDefaultExpiresIn {
		t.Errorf("ExpiresIn = %d, want %d (zero would refresh on every request)", res.ExpiresIn, nousDefaultExpiresIn)
	}
}

func TestRefreshNous_TokenFailures(t *testing.T) {
	t.Run("non-200", func(t *testing.T) {
		up := newNousTestUpstream(t, http.StatusBadRequest, `{"error":"invalid_grant"}`, 200, `{}`)
		nousServerSwap(t, up)
		if _, err := refreshNous(context.Background(), &Params{Client: up.server.Client(), RefreshToken: "r"}); err == nil {
			t.Fatal("want an error on a 400 token answer")
		}
	})
	t.Run("no access_token", func(t *testing.T) {
		up := newNousTestUpstream(t, http.StatusOK, `{"token_type":"Bearer"}`, 200, `{}`)
		nousServerSwap(t, up)
		if _, err := refreshNous(context.Background(), &Params{Client: up.server.Client(), RefreshToken: "r"}); err == nil {
			t.Fatal("want an error when access_token is missing")
		}
	})
	t.Run("missing refresh token", func(t *testing.T) {
		if _, err := refreshNous(context.Background(), &Params{}); err == nil {
			t.Fatal("want an error when refresh_token is empty")
		}
	})
}

// BuildConnectionUpdate must carry the minted key and merge, not replace, the
// connection's existing providerSpecificData.
func TestBuildConnectionUpdate_CarriesNousFields(t *testing.T) {
	update := BuildConnectionUpdate(&TokenResult{
		AccessToken: "acc-2", RefreshToken: "ref-2", ExpiresIn: 1800,
		APIKey: "agent-key-1",
		Extra:  map[string]string{"agentKeyId": "kid-1"},
	})
	if update["apiKey"] != "agent-key-1" {
		t.Errorf("apiKey = %v, want the minted key", update["apiKey"])
	}
	psd, ok := update["providerSpecificData"].(map[string]any)
	if !ok {
		t.Fatalf("providerSpecificData = %#v, want a map", update["providerSpecificData"])
	}
	if psd["agentKeyId"] != "kid-1" {
		t.Errorf("agentKeyId = %v", psd["agentKeyId"])
	}

	// A result with no extras must not invent the key.
	plain := BuildConnectionUpdate(&TokenResult{AccessToken: "a", ExpiresIn: 60})
	if _, present := plain["apiKey"]; present {
		t.Error("apiKey written for a provider that did not mint one")
	}
	if _, present := plain["providerSpecificData"]; present {
		t.Error("providerSpecificData written for a result with no extras")
	}
}

func TestNousRegistered(t *testing.T) {
	if Get("nous") == nil {
		t.Fatal("nous refresher is not registered")
	}
}

var _ = json.Marshal
