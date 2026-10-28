package executor

import (
	"unicode/utf8"
)

// Token accounting for DeepSeek Web.
//
// The site reports no token counts: the answer arrives as text and nothing in
// the stream says how many tokens it was. The counts are therefore estimates,
// computed with the same method this repository already uses for a web
// provider that reports no counts (four characters per token, with the CJK
// adjustment): see geminiWebEstimateTokens. They are estimates, not upstream
// measurements, and are reported through the normal usage object so the
// router's accounting reads them like any other count.
const deepSeekCharsPerToken = 4

// deepSeekEstimateTokens estimates the tokens a text is worth.
//
// An empty text is zero tokens: a turn that said nothing costs nothing on
// the answer side. Everything else is at least one token, because a
// non-empty answer is never free.
func deepSeekEstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	cjkRunes := 0
	otherBytes := 0
	for _, r := range text {
		if unicodeRangesCJK(r) {
			cjkRunes++
			continue
		}
		otherBytes += utf8.RuneLen(r)
	}
	tokens := 0
	if cjkRunes > 0 {
		// A CJK character is about one and a half bytes of token, so three
		// characters make two tokens.
		tokens += (cjkRunes*3 + 1) / 2
	}
	if otherBytes > 0 {
		tokens += (otherBytes + deepSeekCharsPerToken - 1) / deepSeekCharsPerToken
	}
	if tokens < 1 {
		return 1
	}
	return tokens
}

// deepSeekBuildUsage renders the token accounting as the OpenAI usage object.
func deepSeekBuildUsage(prompt, answer, reasoning string) map[string]any {
	promptTokens := deepSeekEstimateTokens(prompt)
	reasoningTokens := deepSeekEstimateTokens(reasoning)
	completionTokens := deepSeekEstimateTokens(answer) + reasoningTokens
	usage := map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
		// The site reports no cache, so the count is zero. The field is
		// published anyway: a client that reads it finds the shape it
		// expects rather than a missing key.
		"prompt_tokens_details": map[string]any{"cached_tokens": 0},
	}
	if reasoningTokens > 0 {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoningTokens}
	}
	return usage
}
