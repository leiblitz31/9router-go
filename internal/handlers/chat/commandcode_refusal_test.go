package chat

import (
	"net/http"
	"testing"
)

func TestIsCommandCodeModelRefusal(t *testing.T) {
	lagunaEnded := []byte(`{"success":false,"error":{"code":"FORBIDDEN","status":403,"message":"The free Laguna S 2.1 tier ended on November 4, 2026. Run /model and pick another model to keep going."}}`)
	modelUnknown := []byte(`{"success":false,"error":{"code":"FORBIDDEN","status":403,"message":"Model/provider not recognized: anthropic:mimo-v2.6-flash"}}`)
	spacedCode := []byte(`{"success": false, "error": {"code": "FORBIDDEN", "status": 403, "message": "Model/provider not recognized: anthropic:x"}}`)

	tests := []struct {
		name       string
		provider   string
		statusCode int
		body       []byte
		expected   bool
	}{
		{name: "retired free tier is a model refusal", provider: "commandcode", statusCode: http.StatusForbidden, body: lagunaEnded, expected: true},
		{name: "unrecognized model id is a model refusal", provider: "commandcode", statusCode: http.StatusForbidden, body: modelUnknown, expected: true},
		{name: "cmc alias also matches", provider: "cmc", statusCode: http.StatusForbidden, body: modelUnknown, expected: true},
		{name: "spaced code field still matches", provider: "commandcode", statusCode: http.StatusForbidden, body: spacedCode, expected: true},
		{name: "other provider with same envelope does not match", provider: "opencode", statusCode: http.StatusForbidden, body: modelUnknown, expected: false},
		{name: "non-403 does not match", provider: "commandcode", statusCode: http.StatusUnauthorized, body: modelUnknown, expected: false},
		{name: "403 without the structured code does not match", provider: "commandcode", statusCode: http.StatusForbidden, body: []byte(`{"error":{"message":"forbidden"}}`), expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCommandCodeModelRefusal(tt.provider, tt.statusCode, tt.body); got != tt.expected {
				t.Errorf("isCommandCodeModelRefusal() = %v, want %v", got, tt.expected)
			}
		})
	}
}
