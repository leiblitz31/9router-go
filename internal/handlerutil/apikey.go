package handlerutil

import "strings"

// MaskAPIKey returns the form of an API key that is safe to persist and display.
//
// The input is trimmed first so padded variants mask identically to the
// stored key (upstream 813a0121 trims in maskApiKey and before the apiKeyMap
// lookup; without it a trailing newline splits one key into two buckets).
//
// The tail is kept because every key an instance mints shares the same
// `sk-{machineId}` prefix, so a prefix-only mask collapses a whole team key set
// into one row in the usage breakdown. Upstream made the same change
// (open-sse/../src/lib/db/repos/usageRepo.js maskApiKey, v0.5.91).
//
// Keys too short to have a distinguishing tail collapse to a fixed "***" — they
// are never real keys, and the previous "***" sentinel is kept so existing
// no-key usage rows stay in the same bucket instead of splitting.
func MaskAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 8 {
		return "***"
	}
	return key[:8] + "***" + key[len(key)-4:]
}
