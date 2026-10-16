package chat

import (
	"slices"
	"testing"

	"9router/proxy/internal/providers"
)

// Upstream 165f9b90: the live Portal catalogue is OpenRouter-shaped, mixes chat
// with embedding/image-only entries, and arrives in one of three envelopes.

func TestNormalizeNousModels_EnvelopeShapes(t *testing.T) {
	entry := `{"id":"nousresearch/hermes-4:free","name":"Hermes 4","context_length":262144,"top_provider":{"max_completion_tokens":32768}}`
	for _, payload := range []string{
		"[" + entry + "]",
		`{"data":[` + entry + `]}`,
		`{"models":[` + entry + `]}`,
		`{"results":[` + entry + `]}`,
	} {
		got := normalizeNousModels([]byte(payload))
		if len(got) != 1 {
			t.Fatalf("payload %s → %d models, want 1", payload, len(got))
		}
		if got[0].ID != "nousresearch/hermes-4:free" || got[0].Name != "Hermes 4" {
			t.Errorf("model = %+v", got[0])
		}
		if got[0].ContextLength != 262144 || got[0].MaxOutput != 32768 {
			t.Errorf("limits = %d/%d, want 262144/32768", got[0].ContextLength, got[0].MaxOutput)
		}
	}
}

func TestNormalizeNousModels_DropsNonChat(t *testing.T) {
	payload := `{"data":[
		{"id":"vendor/text:free","architecture":{"output_modalities":["text"]}},
		{"id":"vendor/embed:free","architecture":{"output_modalities":["embedding"]}},
		{"id":"vendor/image:free","architecture":{"output_modalities":["image"]}},
		{"id":"vendor/blank:free","architecture":{"output_modalities":[]}},
		{"id":"","name":"nameless"},
		{"name":"no id"}
	]}`
	got := normalizeNousModels([]byte(payload))
	if len(got) != 2 {
		t.Fatalf("kept %d models (%+v), want the 2 text-capable ones", len(got), got)
	}
	for _, m := range got {
		if m.ID == "" {
			t.Error("a blank id survived")
		}
	}
}

func TestNormalizeNousModels_FirstPartyFirst(t *testing.T) {
	payload := `{"data":[
		{"id":"stepfun/step-5:free"},
		{"id":"nousresearch/a:free"},
		{"id":"poolside/laguna:free"},
		{"id":"nousresearch/b:free"}
	]}`
	got := normalizeNousModels([]byte(payload))
	want := []string{"nousresearch/a:free", "nousresearch/b:free", "stepfun/step-5:free", "poolside/laguna:free"}
	if len(got) != len(want) {
		t.Fatalf("got %d models, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("position %d = %q, want %q (full: %+v)", i, got[i].ID, want[i], got)
		}
	}
}

// A missing price is NOT free: the zero-cost list must never widen to a paid
// model because the endpoint omitted pricing.
func TestNormalizeNousFreeModels_RequiresExplicitZeroPrice(t *testing.T) {
	payload := `{"data":[
		{"id":"free/big:free","context_length":1000000,"pricing":{"prompt":"0","completion":"0"}},
		{"id":"priced/big","context_length":1000000,"pricing":{"prompt":"0.000003","completion":"0"}},
		{"id":"unpriced/big","context_length":1000000},
		{"id":"nullprice/big","context_length":1000000,"pricing":{"prompt":null,"completion":null}},
		{"id":"free/small:free","context_length":8000,"pricing":{"prompt":"0","completion":"0"}}
	]}`
	got := normalizeNousFreeModels([]byte(payload))
	if len(got) != 1 {
		t.Fatalf("free list = %+v, want only free/big:free", got)
	}
	if got[0].ID != "free/big:free" {
		t.Errorf("id = %q", got[0].ID)
	}
}

func TestNormalizeNousFreeModels_LargestWindowFirst(t *testing.T) {
	payload := `{"data":[
		{"id":"a:free","context_length":262144,"pricing":{"prompt":"0","completion":"0"}},
		{"id":"b:free","context_length":1048576,"pricing":{"prompt":"0","completion":"0"}},
		{"id":"c:free","top_provider":{"context_length":524288},"pricing":{"prompt":"0","completion":"0"}}
	]}`
	got := normalizeNousFreeModels([]byte(payload))
	want := []string{"b:free", "c:free", "a:free"}
	if len(got) != len(want) {
		t.Fatalf("got %d models, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("position %d = %q, want %q", i, got[i].ID, want[i])
		}
	}
}

func TestNormalizeNousModels_NameFallsBackToID(t *testing.T) {
	got := normalizeNousModels([]byte(`[{"id":"vendor/x:free"}]`))
	if len(got) != 1 {
		t.Fatalf("got %d models", len(got))
	}
	if got[0].Name != "vendor/x:free" {
		t.Errorf("Name = %q, want the id", got[0].Name)
	}
}

func TestNormalizeNousModels_GarbageIsEmpty(t *testing.T) {
	for _, payload := range []string{"", "not json", "{}", `{"data":null}`} {
		if got := normalizeNousModels([]byte(payload)); len(got) != 0 {
			t.Errorf("payload %q → %d models, want 0", payload, len(got))
		}
	}
}

func TestNousStaticCatalog(t *testing.T) {
	want := []string{
		"meituan/longcat-2.0:free", "meituan/longcat-2.5-preview:free",
		"stepfun/step-5-preview:free", "upstage/solar-mini4:free",
		"stepfun/step-3.7-flash:free", "poolside/laguna-s-2.1:free",
		"poolside/laguna-xs-2.1:free", "stealth/missingno",
	}
	got := providers.ProviderModels["nous"]
	if len(got) != len(want) {
		t.Fatalf("nous catalog = %v, want %d entries", got, len(want))
	}
	for _, m := range want {
		if !slices.Contains(got, m) {
			t.Errorf("catalog missing %q", m)
		}
	}
}
