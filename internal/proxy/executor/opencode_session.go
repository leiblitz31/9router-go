package executor

import "9router/proxy/internal/proxy"

// opencodeSessionHint resolves the upstream session a free-lane request
// travels with.
//
// On the free lane the router always uses its own stable per-connection
// session and ignores any client-supplied session: upstream answers
// client-supplied sessions on this tier with 429 FreeUsageLimitError while
// the stable session passes (measured Oct 2026: agent-originated requests
// carrying their own canonical session failed three times in a row, probes
// without one passed in the same minutes). Without a stable fallback, every
// request would mint a fresh upstream session, and the free tier accounts
// its quota per session: a fresh id per request burns through that budget
// and surfaces as 429 FreeUsageLimitError with a growing retry-after, while
// the official CLI reuses one long-lived session per conversation
// (decolua/9router stableSessionId; dsh-our-free-model
// sessionForConversation). A hint-less request therefore falls back to one
// stable session per connection, recycled after an idle TTL.
//
// Keyed lanes keep their previous per-request behavior: their quota is
// accounted per key, not per session, so the client's own session wins
// there and is never rotated.
func opencodeSessionHint(req *Request, free bool) string {
	if !free {
		return req.SessionID
	}
	key := "opencode:default"
	if req.ConnectionID != "" {
		key = "opencode:conn:" + req.ConnectionID
	}
	return proxy.StableOpenCodeSessionID(key)
}

// opencodeStableSessionKey reports the stable-session key a free-lane request
// resolves to, or "" — the marker that the upstream session is the router's
// to rotate when its budget is spent. The free flag must be the same one
// passed to opencodeSessionHint: a keyed lane's quota is accounted per key,
// so its session is never rotated. On the free lane the key is returned even
// when the client sent its own session, because the hint ignores it.
func opencodeStableSessionKey(req *Request, free bool) string {
	if !free {
		return ""
	}
	if req.ConnectionID != "" {
		return "opencode:conn:" + req.ConnectionID
	}
	return "opencode:default"
}

// rotateOpencodeStableSession builds the rotation hook forwardOpenCodeRotating
// calls when the gate reports the stable session's budget spent: expire the
// session that just failed and install a freshly minted one via install. A
// nil key (a client-supplied session) yields a nil hook, so no rotation is
// attempted. The expiry is conditional on failed so a rotation that loses a
// race against an already-rotated request keeps the newer session instead of
// discarding its fresh budget.
func rotateOpencodeStableSession(stableKey, failed string, install func(session string)) func() {
	if stableKey == "" {
		return nil
	}
	return func() {
		proxy.ExpireStableOpenCodeSession(stableKey, failed)
		if fresh := proxy.StableOpenCodeSessionID(stableKey); fresh != failed {
			install(fresh)
		}
	}
}
