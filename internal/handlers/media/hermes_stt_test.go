package media

import (
	"strings"
	"testing"
)

// Port of upstream tests/unit/hermes-yaml.test.js STT section (d6431f8b #4763).
func TestUpsertStt9Router_AppendsBlockWhenAbsent(t *testing.T) {
	got := upsertStt9Router("telemetry: false\n", "groq/whisper-large-v3", "http://127.0.0.1:20130/v1")
	for _, want := range []string{
		"stt:", `provider: "openai"`, `model: "groq/whisper-large-v3"`,
		`base_url: "http://127.0.0.1:20130/v1"`, "api_key: ${OPENAI_API_KEY}", "telemetry: false",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %q:\n%s", want, got)
		}
	}
	if got2 := parseHermesSttBlock(got); !is9RouterStt(got2) {
		t.Errorf("round-trip is not recognized as 9router STT:\n%s", got)
	}
}

func TestUpsertStt9Router_PreservesSiblings(t *testing.T) {
	in := "stt:\n  enabled: true\n  provider: \"openai\"\n  language: \"id\"\n  local:\n    model: \"tiny\"\n"
	got := upsertStt9Router(in, "groq/whisper-large-v3", "http://127.0.0.1:20130/v1")
	for _, want := range []string{`language: "id"`, "local:", "tiny"} {
		if !strings.Contains(got, want) {
			t.Errorf("sibling %q dropped:\n%s", want, got)
		}
	}
}

func TestRemoveStt9Router_KeepsUserSettings(t *testing.T) {
	in := buildSttBlock("groq/whisper-large-v3", "http://127.0.0.1:20130/v1")
	in += "other: 1\n"
	// Simulate a user-owned sibling inside the block.
	in = strings.Replace(in, "  openai:\n", "  language: \"id\"\n  openai:\n", 1)
	got := removeStt9Router(in)
	if strings.Contains(got, "openai:") {
		t.Errorf("openai wiring survived:\n%s", got)
	}
	if !strings.Contains(got, `language: "id"`) {
		t.Errorf("user sibling dropped:\n%s", got)
	}
}

func TestRemoveStt9Router_DropsEmptyBlock(t *testing.T) {
	got := removeStt9Router(buildSttBlock("groq/whisper-large-v3", "http://x/v1"))
	if strings.Contains(got, "stt:") {
		t.Errorf("empty stt block survived:\n%s", got)
	}
}

func TestIs9RouterStt_RejectsForeignSetup(t *testing.T) {
	foreign := &hermesSttBlock{
		Provider: new("openai"), BaseURL: new("https://api.openai.com/v1"),
		APIKey: new("sk-secret"),
	}
	if is9RouterStt(foreign) {
		t.Error("foreign OpenAI STT setup misdetected as 9router wiring")
	}
	if is9RouterStt(nil) {
		t.Error("nil block misdetected")
	}
}

func TestParseHermesSettings_ReadsStt(t *testing.T) {
	s := parseHermesSettings(buildSttBlock("groq/whisper-large-v3", "http://127.0.0.1:20130/v1"))
	if s.Stt == nil || s.Stt.Model == nil || *s.Stt.Model != "groq/whisper-large-v3" {
		t.Fatalf("stt model not parsed: %+v", s.Stt)
	}
}

func TestResetHermesConfig_RemovesOwnSttKeepsForeign(t *testing.T) {
	own := buildSttBlock("groq/whisper-large-v3", "http://x/v1") + "telemetry: false\n"
	remaining, removed, _ := resetHermesConfig(own)
	if removed["stt"] != true || strings.Contains(remaining, "openai:") {
		t.Errorf("own stt not removed: removed=%v\n%s", removed, remaining)
	}

	foreign := "stt:\n  enabled: true\n  provider: \"openai\"\n  openai:\n    model: \"whisper-1\"\n    base_url: \"https://api.openai.com/v1\"\n    api_key: sk-x\n"
	remaining2, removed2, kept := resetHermesConfig(foreign)
	if removed2["stt"] == true || !strings.Contains(remaining2, "sk-x") {
		t.Errorf("foreign stt touched: removed=%v kept=%v\n%s", removed2, kept, remaining2)
	}
	found := false
	for _, k := range kept {
		if k == "stt" {
			found = true
		}
	}
	if !found {
		t.Errorf("foreign stt not reported as kept: %v", kept)
	}
}
