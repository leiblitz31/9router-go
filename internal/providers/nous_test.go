package providers

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestNousCatalogNormalization(t *testing.T) {
	raw := []map[string]any{
		{
			"id":             "vendor/text-model",
			"name":           "Vendor Text Model",
			"context_length": 262144,
			"top_provider": map[string]any{
				"max_completion_tokens": 65536,
			},
			"architecture": map[string]any{
				"output_modalities": []any{"text"},
			},
		},
		{
			"id": "vendor/embedding-model",
			"architecture": map[string]any{
				"output_modalities": []any{"embeddings"},
			},
		},
		{
			"id": "vendor/image-model",
			"architecture": map[string]any{
				"output_modalities": []any{"image"},
			},
		},
		{
			"id":             "nousresearch/first-party-model",
			"name":           "Nous First Party",
			"description":    "First party reasoning model",
			"context_length": 262144,
			"architecture": map[string]any{
				"output_modalities": []any{"text"},
			},
		},
	}

	models := NormalizeNousModels(raw)
	if len(models) != 2 {
		t.Fatalf("expected 2 normalized models, got %d", len(models))
	}

	// First-party model must sort before third-party
	if models[0].ID != "nousresearch/first-party-model" {
		t.Errorf("expected first-party model first, got %q", models[0].ID)
	}
	if models[1].ID != "vendor/text-model" {
		t.Errorf("expected vendor model second, got %q", models[1].ID)
	}

	// Verify text model context and max output tokens
	textModel := models[1]
	if textModel.ContextLength == nil || *textModel.ContextLength != 262144 {
		t.Errorf("expected contextLength 262144, got %v", textModel.ContextLength)
	}
	if textModel.MaxOutputTokens == nil || *textModel.MaxOutputTokens != 65536 {
		t.Errorf("expected maxOutputTokens 65536, got %v", textModel.MaxOutputTokens)
	}
}

func TestNousCapabilities(t *testing.T) {
	nousModels := GetProviderModels("nous")
	if len(nousModels) == 0 {
		t.Fatal("no models registered for provider nous")
	}

	wantVision := []string{
		"meituan/longcat-2.5-preview:free",
		"stepfun/step-5-preview:free",
		"stepfun/step-3.7-flash:free",
	}
	wantVideo := []string{
		"stepfun/step-5-preview:free",
		"stepfun/step-3.7-flash:free",
	}

	for _, id := range nousModels {
		t.Run(id, func(t *testing.T) {
			caps := GetCapabilitiesForModel("nous", id)
			if !caps.Tools {
				t.Errorf("%s: expected tools=true", id)
			}
			if !caps.Reasoning {
				t.Errorf("%s: expected reasoning=true", id)
			}
			if caps.ThinkingFormat != "nous" {
				t.Errorf("%s: expected thinkingFormat=nous, got %q", id, caps.ThinkingFormat)
			}
			if caps.ContextWindow < 262144 {
				t.Errorf("%s: expected contextWindow >= 262144, got %d", id, caps.ContextWindow)
			}

			shouldHaveVision := slices.Contains(wantVision, id)
			if caps.Vision != shouldHaveVision {
				t.Errorf("%s: expected vision=%v, got %v", id, shouldHaveVision, caps.Vision)
			}

			shouldHaveVideo := slices.Contains(wantVideo, id)
			if caps.VideoInput != shouldHaveVideo {
				t.Errorf("%s: expected videoInput=%v, got %v", id, shouldHaveVideo, caps.VideoInput)
			}
		})
	}
}

func TestNousAttribution(t *testing.T) {
	t.Run("default attribution tags added", func(t *testing.T) {
		body := []byte(`{"model":"stepfun/step-5-preview:free","messages":[{"role":"user","content":"hi"}]}`)
		out := ApplyNousAttribution(body)

		var req map[string]any
		if err := json.Unmarshal(out, &req); err != nil {
			t.Fatalf("failed to unmarshal output: %v", err)
		}

		rawTags, ok := req["tags"].([]any)
		if !ok {
			t.Fatalf("expected tags array in output, got %T: %s", req["tags"], out)
		}

		var tags []string
		for _, tag := range rawTags {
			if s, ok := tag.(string); ok {
				tags = append(tags, s)
			}
		}

		want := []string{"product=9router", "client=9router", "user=9router"}
		for _, w := range want {
			if !slices.Contains(tags, w) {
				t.Errorf("missing tag %q in %v", w, tags)
			}
		}
	})

	t.Run("preserves caller product tag and removes empty tags", func(t *testing.T) {
		body := []byte(`{"model":"stepfun/step-5-preview:free","messages":[{"role":"user","content":"hi"}],"tags":["product=my-app","user=","client= ","custom=value"]}`)
		out := ApplyNousAttribution(body)

		var req map[string]any
		if err := json.Unmarshal(out, &req); err != nil {
			t.Fatalf("failed to unmarshal output: %v", err)
		}

		rawTags, ok := req["tags"].([]any)
		if !ok {
			t.Fatalf("expected tags array in output, got %T: %s", req["tags"], out)
		}

		var tags []string
		for _, tag := range rawTags {
			if s, ok := tag.(string); ok {
				tags = append(tags, s)
			}
		}

		// product=my-app must be preserved, not overwritten with product=9router
		if !slices.Contains(tags, "product=my-app") {
			t.Errorf("expected caller product=my-app preserved, got %v", tags)
		}
		if slices.Contains(tags, "product=9router") {
			t.Errorf("product=9router should not overwrite product=my-app: %v", tags)
		}
		if !slices.Contains(tags, "custom=value") {
			t.Errorf("expected custom=value preserved: %v", tags)
		}
		// Empty user= and client= should be cleaned up and filled with 9router defaults
		if !slices.Contains(tags, "client=9router") {
			t.Errorf("expected client=9router in %v", tags)
		}
		if !slices.Contains(tags, "user=9router") {
			t.Errorf("expected user=9router in %v", tags)
		}
		if slices.Contains(tags, "user=") || slices.Contains(tags, "client= ") {
			t.Errorf("empty tags not removed: %v", tags)
		}
	})
}
