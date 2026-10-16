package proxy

import (
	"slices"
	"testing"

	json "encoding/json/v2"
)

func tagsOf(t *testing.T, body []byte) []string {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, ok := parsed["tags"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, isStr := v.(string); isStr {
			out = append(out, s)
		}
	}
	return out
}

func TestApplyNousAttribution_AddsNeutralTags(t *testing.T) {
	body := ApplyNousAttribution(
		"openai-compatible-chat-nous",
		"sk-nous-secret",
		"https://inference-api.nousresearch.com/v1/chat/completions",
		[]byte(`{"model":"stepfun/step-5-preview:free","messages":[{"role":"user","content":"hi"}]}`),
	)
	got := tagsOf(t, body)
	want := []string{"product=9router", "client=9router", "user=9router"}
	if !slices.Equal(got, want) {
		t.Errorf("tags = %v, want %v", got, want)
	}
	// The credential must never travel in a tag.
	for _, tag := range got {
		if len(tag) > 40 || slices.Contains([]string{"sk-"}, tag) {
			t.Errorf("tag %q looks like it carries a credential", tag)
		}
	}
}

func TestApplyNousAttribution_PreservesCallerTags(t *testing.T) {
	body := ApplyNousAttribution(
		"openai-compatible-chat-nous",
		"sk-x",
		"https://inference-api.nousresearch.com/v1/chat/completions",
		[]byte(`{"model":"m","messages":[],"tags":["product=caller-app","team=research"]}`),
	)
	got := tagsOf(t, body)
	// A caller's real value wins over the fallback; only the missing keys are added.
	if !slices.Contains(got, "product=caller-app") {
		t.Errorf("caller tag dropped: %v", got)
	}
	if slices.Contains(got, "product=9router") {
		t.Errorf("fallback overwrote the caller's value: %v", got)
	}
	if !slices.Contains(got, "team=research") {
		t.Errorf("unrelated caller tag dropped: %v", got)
	}
	if !slices.Contains(got, "client=9router") || !slices.Contains(got, "user=9router") {
		t.Errorf("missing fallbacks: %v", got)
	}
}

func TestApplyNousAttribution_BlankCallerValueReplaced(t *testing.T) {
	// An empty value does not satisfy the upstream requirement.
	body := ApplyNousAttribution(
		"openai-compatible-chat-nous",
		"sk-x",
		"https://inference-api.nousresearch.com/v1/chat/completions",
		[]byte(`{"model":"m","messages":[],"tags":["product=   "]}`),
	)
	got := tagsOf(t, body)
	if slices.Contains(got, "product=") || slices.Contains(got, "product=   ") {
		t.Errorf("blank caller value kept: %v", got)
	}
	if !slices.Contains(got, "product=9router") {
		t.Errorf("blank value not replaced: %v", got)
	}
}

func TestApplyNousAttribution_NoOps(t *testing.T) {
	in := []byte(`{"model":"m","messages":[]}`)

	t.Run("no api key", func(t *testing.T) {
		out := ApplyNousAttribution("openai-compatible-chat-nous", "", "https://inference-api.nousresearch.com/v1/chat/completions", in)
		if string(out) != string(in) {
			t.Errorf("body changed without a key: %s", out)
		}
	})
	t.Run("not a custom node", func(t *testing.T) {
		out := ApplyNousAttribution("nous", "sk-x", "https://inference-api.nousresearch.com/v1/chat/completions", in)
		if string(out) != string(in) {
			t.Errorf("body changed for the registry provider: %s", out)
		}
	})
	t.Run("other host", func(t *testing.T) {
		out := ApplyNousAttribution("openai-compatible-chat-x", "sk-x", "https://api.example.com/v1/chat/completions", in)
		if string(out) != string(in) {
			t.Errorf("body changed for another host: %s", out)
		}
	})
	t.Run("other path on the same host", func(t *testing.T) {
		out := ApplyNousAttribution("openai-compatible-chat-x", "sk-x", "https://inference-api.nousresearch.com/v1/responses", in)
		if string(out) != string(in) {
			t.Errorf("body changed for another path: %s", out)
		}
	})
	t.Run("plain http", func(t *testing.T) {
		out := ApplyNousAttribution("openai-compatible-chat-nous", "sk-x", "http://inference-api.nousresearch.com/v1/chat/completions", in)
		if string(out) != string(in) {
			t.Errorf("body changed over http: %s", out)
		}
	})
	t.Run("tags is not an array", func(t *testing.T) {
		bad := []byte(`{"model":"m","messages":[],"tags":"product=x"}`)
		out := ApplyNousAttribution("openai-compatible-chat-nous", "sk-x", "https://inference-api.nousresearch.com/v1/chat/completions", bad)
		if string(out) != string(bad) {
			t.Errorf("non-array tags rewritten: %s", out)
		}
	})
}

func TestIsNousChatCompletions(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://inference-api.nousresearch.com/v1/chat/completions", true},
		{"https://inference-api.nousresearch.com/v1/chat/completions?x=1", true},
		{"https://inference-api.nousresearch.com/v1/chat/completions/", false},
		{"https://inference-api.nousresearch.com/v1/completions", false},
		{"https://evil.inference-api.nousresearch.com/v1/chat/completions", false},
		{"http://inference-api.nousresearch.com/v1/chat/completions", false},
		{"", false},
	} {
		if got := isNousChatCompletions(tc.url); got != tc.want {
			t.Errorf("isNousChatCompletions(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
