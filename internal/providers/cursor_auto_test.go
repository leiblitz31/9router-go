package providers

import "testing"

func TestResolveCursorAutoModel(t *testing.T) {
	concrete := CursorAutoModel()
	if concrete == "" || concrete == "default" || concrete == "auto" {
		t.Fatalf("CursorAutoModel must be a concrete id, got %q", concrete)
	}
	for _, model := range []string{"default", "auto"} {
		if got := ResolveCursorAutoModel("cursor", model); got != concrete {
			t.Errorf("cursor/%s = %q, want %q", model, got, concrete)
		}
	}
	if got := ResolveCursorAutoModel("cursor", "claude-4.5-sonnet"); got != "claude-4.5-sonnet" {
		t.Errorf("concrete id must pass through, got %q", got)
	}
	if got := ResolveCursorAutoModel("openai", "default"); got != "default" {
		t.Errorf("other providers must pass through, got %q", got)
	}
}
