package oauth

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Nous Research Portal login (client_id "hermes-cli", free account): a
// device-code grant against the portal, then the granted access token is
// exchanged for a short-lived inference agent key. That key — not the access
// token — is what the chat transport authenticates with.
//
// The device code is one-shot, so a failed mint must not lose the login: the
// poll loop remembers the granted token and retries only the mint.
//
// Port of open-sse/services/nous.js (decolua/9router 165f9b90).
const (
	nousDeviceClientID = "hermes-cli"
	nousDeviceScope    = "inference:invoke inference:mint_agent_key"

	// nousMintMaxTTL matches the value the refresh path asks for, so a key
	// minted at login lives as long as one minted on refresh.
	nousMintMaxTTL = 7200

	// nousPendingMintTTL bounds how long a consumed device code's access token
	// is remembered for mint retries. The entry only has to survive the modal's
	// poll loop.
	nousPendingMintTTL = 15 * time.Minute
	// nousPendingMintMaxTries caps how many polls retry the mint before the
	// login fails for real.
	nousPendingMintMaxTries = 3
)

const (
	nousDevicePortalURL   = "https://portal.nousresearch.com"
	nousDeviceCodeURL     = nousDevicePortalURL + "/api/oauth/device/code"
	nousDeviceTokenURL    = nousDevicePortalURL + "/api/oauth/token"
	nousDeviceAgentKeyURL = nousDevicePortalURL + "/api/oauth/agent-key"
)

// nousPendingMint remembers a granted access token whose device code has been
// consumed but whose inference key has not been minted yet.
type nousPendingMint struct {
	accessToken  string
	refreshToken string
	mintTries    int
	createdAt    time.Time
}

// nousPendingMints maps device_code -> nousPendingMint.
var nousPendingMints sync.Map

func nousPendingMintGet(code string) (nousPendingMint, bool) {
	v, ok := nousPendingMints.Load(code)
	if !ok {
		return nousPendingMint{}, false
	}
	pm, ok := v.(nousPendingMint)
	if !ok || time.Since(pm.createdAt) > nousPendingMintTTL {
		nousPendingMints.Delete(code)
		return nousPendingMint{}, false
	}
	return pm, true
}

// nousPendingMintPut remembers the token for a later mint retry.
func nousPendingMintPut(code, accessToken, refreshToken string) {
	pm := nousPendingMint{
		accessToken:  accessToken,
		refreshToken: refreshToken,
		createdAt:    time.Now(),
	}
	if existing, ok := nousPendingMintGet(code); ok {
		pm.mintTries = existing.mintTries
	}
	nousPendingMints.Store(code, pm)
}

func nousPendingMintTries(code string) int {
	if pm, ok := nousPendingMintGet(code); ok {
		return pm.mintTries
	}
	return 0
}

func nousPendingMintIncrement(code string) {
	pm, ok := nousPendingMintGet(code)
	if !ok {
		return
	}
	pm.mintTries++
	nousPendingMints.Store(code, pm)
}

func nousPendingMintDelete(code string) {
	nousPendingMints.Delete(code)
}

func nousStart() (map[string]any, error) {
	form := url.Values{
		"client_id": {nousDeviceClientID},
		"scope":     {nousDeviceScope},
	}
	data, status, err := postForm(nousDeviceCodeURL, form, map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/x-www-form-urlencoded",
	})
	if err != nil {
		return nil, fmt.Errorf("Nous device code request failed: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("Nous device code request failed: status %d", status)
	}
	if strVal(data, "device_code") == "" {
		return nil, fmt.Errorf("Nous device code response missing device_code")
	}
	if strVal(data, "user_code") == "" {
		return nil, fmt.Errorf("Nous device code response missing user_code")
	}
	if strVal(data, "verification_uri") == "" && strVal(data, "verification_url") == "" {
		return nil, fmt.Errorf("Nous device code response missing verification_uri")
	}
	data["session"] = map[string]any{}
	return data, nil
}

// nousPoll trades an approved device code for an inference key. The portal
// answers authorization_pending while the user is still in the browser, which
// the shared poll loop maps back to the pending status.
func nousPoll(code string) (deviceTokens, error) {
	var t deviceTokens

	// A previous poll may have consumed this one-shot code without finishing the
	// mint: reuse the remembered token so the user never approves twice.
	var access, refresh string
	if pm, ok := nousPendingMintGet(code); ok {
		access, refresh = pm.accessToken, pm.refreshToken
	}
	if access == "" {
		form := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {code},
			"client_id":   {nousDeviceClientID},
		}
		data, status, err := postForm(nousDeviceTokenURL, form, map[string]string{
			"Accept":       "application/json",
			"Content-Type": "application/x-www-form-urlencoded",
		})
		if err != nil {
			return t, fmt.Errorf("poll_failed: %w", err)
		}
		// A non-200 with no parseable OAuth error (WAF block, proxy 403, HTML
		// error page) must surface as an error; reporting authorization_pending
		// would make the modal poll forever.
		if status != http.StatusOK {
			if e := strVal(data, "error"); e != "" {
				if e == "authorization_pending" || e == "slow_down" {
					return t, errors.New(e)
				}
				return t, fmt.Errorf("%s: %s", e, strVal(data, "error_description"))
			}
			return t, fmt.Errorf("poll_failed: Nous token endpoint returned status %d", status)
		}
		if e := strVal(data, "error"); e == "authorization_pending" || e == "slow_down" {
			return t, errors.New(e)
		}
		access = strVal(data, "access_token")
		if access == "" {
			if e := strVal(data, "error"); e != "" {
				return t, fmt.Errorf("%s: %s", e, strVal(data, "error_description"))
			}
			return t, errors.New("authorization_pending")
		}
		refresh = strVal(data, "refresh_token")
		// The refresh token is what keeps the connection refreshable long after
		// this inference key expires; a grant that omits it would leave the
		// connection dead at first expiry.
		if refresh == "" {
			return t, fmt.Errorf("Nous token response missing refresh_token")
		}
	}

	key, err := nousMintDeviceKey(access)
	if err != nil {
		// A soft mint failure. Remember the granted token so the next poll
		// retries only the mint — the user already approved in the browser and
		// the device code is spent, so a second approval is impossible.
		nousPendingMintPut(code, access, refresh)
		nousPendingMintIncrement(code)
		tries := nousPendingMintTries(code)
		if tries < nousPendingMintMaxTries {
			return t, fmt.Errorf("mint_pending: Nous agent key mint failed (try %d/%d): %v",
				tries, nousPendingMintMaxTries, err)
		}
		nousPendingMintDelete(code)
		return t, fmt.Errorf("Nous agent key mint failed after %d tries: %w — please try logging in again", tries, err)
	}
	nousPendingMintDelete(code)

	apiKey := strVal(key, "api_key")
	if apiKey == "" {
		return t, fmt.Errorf("Nous agent key mint returned no api_key")
	}
	// The stored credential is the minted key; the access token stays so a
	// future re-mint needs no new login, and the refresh token keeps the
	// connection alive past this key's TTL.
	t.access = apiKey
	t.refresh = refresh
	t.email = strings.ToLower(strings.TrimSpace(strVal(key, "user_email")))
	t.extra = map[string]any{
		"authMethod":       "device_code",
		"oauthAccessToken": access,
	}
	if id := strVal(key, "key_id"); id != "" {
		t.extra["agentKeyId"] = id
	}
	if exp := strVal(key, "expires_at"); exp != "" {
		t.extra["agentKeyExpiresAt"] = exp
	}
	return t, nil
}

// nousMintDeviceKey exchanges the granted access token for an inference key.
func nousMintDeviceKey(accessToken string) (map[string]any, error) {
	data, status, err := postJSON(nousDeviceAgentKeyURL, map[string]any{
		"min_ttl_seconds": nousMintMaxTTL,
	}, map[string]string{
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + accessToken,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("agent key endpoint returned status %d", status)
	}
	return data, nil
}
