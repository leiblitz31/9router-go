package dashboard

import (
	"context"
	"fmt"
	"strings"

	"9router/proxy/internal/proxy/executor"
)

// gemini-web quota.
//
// The site publishes the account's allowance in two windows — a rolling
// five-hour one and a weekly one — and the dashboard draws them as two rows.
// The numbers come from the executor, which speaks the site's own RPCs; this
// file only renders what it returns.
//
// The provider is a cookie provider, not an API-key one, so the credential
// read here is the same cookie the chat path sends.
func fetchGeminiWebUsage(ctx context.Context, apiKey string) usageResult {
	cookie := executor.GeminiWebCookieHeader(apiKey)
	if strings.TrimSpace(cookie) == "" {
		return usageResult{
			plan:    "Gemini Web",
			message: "No cookie configured — an anonymous gemini.google.com session has no quota to report.",
		}
	}
	buckets, err := executor.GeminiWebQuota(ctx, cookie)
	if err != nil {
		return usageResult{plan: "Gemini Web", message: fmt.Sprintf("Failed to fetch Gemini Web quota: %v", err)}
	}
	if len(buckets) == 0 {
		return usageResult{
			plan:    "Gemini Web",
			message: "Gemini Web connected, but the site reported no quota windows.",
		}
	}
	quotas := make(map[string]any, len(buckets))
	for _, bucket := range buckets {
		row := map[string]any{
			"used":        bucket.Used,
			"total":       bucket.Total,
			"remaining":   bucket.Remaining,
			"displayName": bucket.Label,
		}
		if bucket.ResetAt != "" {
			row["resetAt"] = bucket.ResetAt
		}
		if bucket.Total > 0 {
			row["remainingPercentage"] = bucket.Remaining * 100 / bucket.Total
		}
		if bucket.Unlimited {
			row["unlimited"] = true
		}
		quotas[bucket.ID] = row
	}
	return usageResult{plan: "Gemini Web", quotas: quotas}
}
