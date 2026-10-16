package oauth

import (
	"time"

	"9router/proxy/internal/providers"
)

// RegisterAll registers all built-in OAuth refreshers.
// Standard OAuth2 providers (with client_id + token_url) are registered from KnownOAuthConfigs.
func RegisterAll() {
	for name, cfg := range providers.KnownOAuthConfigs {
		if cfg.ClientID == "" || cfg.TokenURL == "" {
			continue
		}
		// Skip providers that have their own custom refresher
		if Get(name) != nil {
			continue
		}
		Register(name, NewStandardRefresher(cfg.TokenURL, cfg.ClientID, cfg.ClientSecret))
	}
}

// accessTokenOnlyProviders authenticate with the OAuth access token alone.
// Their login handlers also copy that token into apiKey, so a stored apiKey is
// never an independent credential there — only a copy that can go stale.
var accessTokenOnlyProviders = map[string]bool{
	"antigravity": true,
	"gemini-cli":  true,
}

// IsAccessTokenOnlyProvider reports whether a provider's apiKey field is only
// a mirror of its OAuth access token.
func IsAccessTokenOnlyProvider(provider string) bool {
	return accessTokenOnlyProviders[provider]
}

// SyncMirroredAPIKey keeps a connection's apiKey in step with a refreshed
// access token when apiKey is just a copy of it. Login handlers write the
// first access token into both fields; a refresh that rotates only
// accessToken left every reader that prefers apiKey (extractAPIKey) sending
// the stale token, so each request answered 401 and forced another refresh.
//
// An apiKey that differs from the previous access token is an independent
// credential (iflow's HMAC platform key, a Kiro api_key connection) and is
// left alone — except on access-token-only providers, where it can only be a
// stale copy and is overwritten so rows that already drifted heal themselves.
//
// Call it before the new accessToken is written: prevAccess is the value the
// row held until now.
func SyncMirroredAPIKey(existing map[string]any, provider, prevAccess, newAccess string) {
	if existing == nil || newAccess == "" {
		return
	}
	apiKey, _ := existing["apiKey"].(string)
	if apiKey == "" || apiKey == newAccess {
		return
	}
	if apiKey == prevAccess || accessTokenOnlyProviders[provider] {
		existing["apiKey"] = newAccess
	}
}

// BuildConnectionUpdate builds a DB update map from a refresh result.
func BuildConnectionUpdate(result *TokenResult) map[string]any {
	m := map[string]any{
		"accessToken": result.AccessToken,
		"expiresAt":   time.Now().Add(time.Duration(result.ExpiresIn) * time.Second).Format(time.RFC3339),
	}
	if result.RefreshToken != "" {
		m["refreshToken"] = result.RefreshToken
	}
	// A provider that authenticates with a minted key (Nous) reports it here;
	// writing it keeps the connection's apiKey in step with the refresh.
	if result.APIKey != "" {
		m["apiKey"] = result.APIKey
	}
	if len(result.Extra) > 0 {
		psd, _ := m["providerSpecificData"].(map[string]any)
		if psd == nil {
			psd = map[string]any{}
		}
		for k, v := range result.Extra {
			psd[k] = v
		}
		m["providerSpecificData"] = psd
	}
	return m
}
