package chat

import (
	"net"
	"net/url"
	"strings"
)

// isLoopbackCustomNode reports whether provider names a custom node whose
// declared base URL is loopback (127.0.0.0/8, ::1, or localhost). Such a
// node is local by design: the operator registered an endpoint that only
// exists on this machine, so dispatch to it must pass the SSRF guard the
// same way a registry provider flagged LocalByDesign does. Anything else —
// a private LAN address, a Tailscale address, a public host, a missing row,
// or a malformed URL — stays blocked: this is loopback-only, fail-closed.
func (h *ChatHandler) isLoopbackCustomNode(provider string) bool {
	if h == nil || h.Repo == nil || provider == "" {
		return false
	}
	if _, data, err := h.Repo.GetProviderNodeByPrefix(provider); err == nil && data != nil && isLoopbackBaseURL(data.BaseURL) {
		return true
	}
	if _, data, err := h.Repo.GetProviderNodeByID(provider); err == nil && data != nil && isLoopbackBaseURL(data.BaseURL) {
		return true
	}
	return false
}

// isLoopbackBaseURL reports whether raw is a URL whose host is loopback.
// A URL without a scheme has no host to judge, so it is not loopback.
func isLoopbackBaseURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
