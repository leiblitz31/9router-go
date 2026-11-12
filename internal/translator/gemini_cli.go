package translator

import (
	"strings"
)

// Cloud Code Assist ("gemini-cli") client identity.
//
// The lane shares a host family with antigravity, but the envelope is different
// in kind: antigravity wraps for an agent surface and carries a userId, a
// requestType and a synthetic requestId. The CLI envelope has none of those —
// it is the project, the model, and the request, which is all the backend
// accepts. Upstream builds it in transformRequest
// (open-sse/executors/gemini-cli.js:28-34).
//
// Only the identity helpers live here: the fork builds the CLI envelope on its
// own path, but the header injection in internal/proxy (upstream #1089) keys
// off the same UA and api-client values the CLI itself sends.

const (
	// GeminiCLIApiClient is the X-Goog-Api-Client value the CLI sends.
	GeminiCLIApiClient = "google-genai-sdk/1.41.0 gl-node/v22.19.0"
)

// GeminiCLIUserAgent mirrors the CLI's own UA. Google answers 403 when the
// model in it does not match the model in the envelope.
func GeminiCLIUserAgent(model string) string {
	return "GeminiCLI/0.34.0/" + GeminiCLIModel(model) + " (win32; x64; terminal)"
}

// GeminiCLIModel strips the provider prefix and 9router's own effort suffixes:
// the Cloud Code backend keys on the bare upstream id.
func GeminiCLIModel(model string) string {
	if _, after, ok := strings.Cut(model, "/"); ok {
		model = after
	}
	for _, suffix := range []string{"-thinking", "-agentic"} {
		model = strings.TrimSuffix(model, suffix)
	}
	if idx := strings.IndexByte(model, '('); idx > 0 {
		model = model[:idx]
	}
	return model
}
