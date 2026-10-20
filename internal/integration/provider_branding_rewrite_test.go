//go:build integration

package integration

import (
	"encoding/base64"
	json "encoding/json/v2"
	"net/http"
	"strings"
	"testing"
)

// TestProviderBrandingRewrite pins the fix for the bug where the primary
// identifier and its derivatives were globally rewritten in prompt text.
// Base64 decoding is used for all sensitive strings to prevent the gateway
// or review tooling from prematurely mutating the test source.
func TestProviderBrandingRewrite(t *testing.T) {
	// Decode base64 identifiers at runtime.
	// b3BlbmNvZGU= decodes to the primary identifier.
	pDecoded, err := base64.StdEncoding.DecodeString(`b3BlbmNvZGU=`)
	if err != nil {
		t.Fatalf(`decode primary identifier: %v`, err)
	}
	providerToken := string(pDecoded)

	// LXplbg== decodes to the zen suffix.
	zDecoded, err := base64.StdEncoding.DecodeString(`LXplbg==`)
	if err != nil {
		t.Fatalf(`decode zen suffix: %v`, err)
	}
	zenSuffix := string(zDecoded)

	// LWdv decodes to the go suffix.
	gDecoded, err := base64.StdEncoding.DecodeString(`LWdv`)
	if err != nil {
		t.Fatalf(`decode go suffix: %v`, err)
	}
	goSuffix := string(gDecoded)

	// YW50aWdyYXZpdHk= decodes to the renamed form.
	rDecoded, err := base64.StdEncoding.DecodeString(`YW50aWdyYXZpdHk=`)
	if err != nil {
		t.Fatalf(`decode renamed form: %v`, err)
	}
	renamedForm := string(rDecoded)

	zenVariant := providerToken + zenSuffix
	goVariant := providerToken + goSuffix
	windowsConfigPath := `C:/Users/admin/.` + providerToken + `/db/data.sqlite`

	env := newEnv(t)
	up := env.NewUpstream(t, geminiOK(t))
	addAntigravityConnection(t, env, up)

	systemPrompt := "System using " + providerToken + ", " + zenVariant + ", " + goVariant + ", and path " + windowsConfigPath + ". Anthropic's Claude Agent SDK"
	userPrompt := "User using " + providerToken + ", " + zenVariant + ", " + goVariant + ", and path " + windowsConfigPath + "."

	body := map[string]any{
		"model": "antigravity/gemini-3.8-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": systemPrompt},
			map[string]any{"role": "user", "content": userPrompt},
		},
	}

	res := env.Post(t, "/v1/chat/completions", body)
	if res.Status != http.StatusOK {
		t.Fatalf("POST /v1/chat/completions = %d, want 200 (body: %s)", res.Status, truncate(res.Body))
	}

	sent := lastRequestBody(t, up)

	// Assert every assembled literal is present verbatim in the raw request body.
	for _, literal := range []string{providerToken, zenVariant, goVariant, windowsConfigPath} {
		if !strings.Contains(sent, literal) {
			t.Errorf("upstream request body missing literal %q\nbody: %s", literal, truncate([]byte(sent)))
		}
	}

	// Control assertion: the control phrase is still stripped from upstream body.
	if strings.Contains(sent, "Anthropic's Claude Agent SDK") {
		t.Errorf("control phrase 'Anthropic's Claude Agent SDK' was not stripped from upstream body")
	}

	// Assert prompt texts (or the request body generally) contain NO occurrence of the decoded renamed form.
	// Note: model names or userAgent headers might contain renamed, but the prompt content must not.
	// Let's check system_instruction and contents specifically.
	var parsed struct {
		SystemInstruction struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"system_instruction"`
		Contents []struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal([]byte(sent), &parsed); err != nil {
		t.Fatalf("failed to unmarshal upstream body: %v", err)
	}
	for _, part := range parsed.SystemInstruction.Parts {
		if strings.Contains(part.Text, renamedForm) {
			t.Errorf("system instruction text incorrectly contains renamed form %q: %q", renamedForm, part.Text)
		}
	}
	for _, content := range parsed.Contents {
		for _, part := range content.Parts {
			if strings.Contains(part.Text, renamedForm) {
				t.Errorf("content part text incorrectly contains renamed form %q: %q", renamedForm, part.Text)
			}
		}
	}
}
