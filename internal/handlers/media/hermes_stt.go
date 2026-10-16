package media

import (
	"fmt"
	"regexp"
	"strings"
)

// Hermes `stt:` block editor — audio transcription via 9router (#4763).
// Port of upstream `src/app/api/cli-tools/hermes-settings/hermesYaml.js`
// (d6431f8b): Hermes runs voice/audio through `stt.provider`; the "openai"
// backend honours stt.openai.base_url + stt.openai.api_key as endpoint
// overrides, and load_config() expands ${OPENAI_API_KEY} from the profile's
// .env. Pointing it at 9router's /v1/audio/transcriptions routes every
// transcription through the proxy.

var sttBlockRE = regexp.MustCompile(`(?m)^stt:[ \t]*\r?\n((?:[ \t]+.*\r?\n?|[ \t]*\r?\n)*)`)

// hermesSttBlock is the parsed top-level "stt:" block. Model/BaseURL/APIKey
// read the "openai" sub-block — the backend our override targets; sibling
// settings (language, local, ...) are ignored.
type hermesSttBlock struct {
	Enabled  *string `json:"enabled"`
	Provider *string `json:"provider"`
	Model    *string `json:"model"`
	BaseURL  *string `json:"base_url"`
	APIKey   *string `json:"api_key"`
}

func sttSubRE(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(name) + `:[ \t]*\r?\n((?:(?:[ \t]{4,}.*\r?\n?)|(?:[ \t]*\r?\n))*)`)
}

func stripSttQuotes(value string) string {
	return strings.Trim(strings.Trim(strings.TrimSpace(value), `"`), "'")
}

// parseHermesSttBlock reads the stt wiring back to fields (best-effort).
// Returns nil when there is no top-level stt block.
func parseHermesSttBlock(yaml string) *hermesSttBlock {
	m := sttBlockRE.FindStringSubmatch(yaml)
	if m == nil {
		return nil
	}
	direct := map[string]string{}
	openai := map[string]string{}
	current := ""
	directRE := regexp.MustCompile(`^  ([A-Za-z0-9_]+):[ \t]*(.*)$`)
	openaiRE := regexp.MustCompile(`^ {4,}([A-Za-z0-9_]+):[ \t]*(.*)$`)
	headRE := regexp.MustCompile(`^  \S`)
	for _, line := range strings.Split(m[1], "\n") {
		if headRE.MatchString(line) {
			kv := directRE.FindStringSubmatch(line)
			current = ""
			if kv == nil {
				continue
			}
			if kv[2] != "" {
				direct[kv[1]] = stripSttQuotes(kv[2])
			} else {
				current = kv[1]
			}
		} else if current == "openai" && strings.HasPrefix(line, "    ") {
			if kv := openaiRE.FindStringSubmatch(line); kv != nil && kv[2] != "" {
				openai[kv[1]] = stripSttQuotes(kv[2])
			}
		}
	}
	get := func(mm map[string]string, k string) *string {
		if v, ok := mm[k]; ok {
			return new(v)
		}
		return nil
	}
	return &hermesSttBlock{
		Enabled:  get(direct, "enabled"),
		Provider: get(direct, "provider"),
		Model:    get(openai, "model"),
		BaseURL:  get(openai, "base_url"),
		APIKey:   get(openai, "api_key"),
	}
}

// is9RouterStt reports the stt wiring this card wrote: openai backend + the
// profile key reference. The api_key reference is the fingerprint — a user's
// own OpenAI audio setup keeps their own key (or none).
func is9RouterStt(cfg *hermesSttBlock) bool {
	if cfg == nil || cfg.Provider == nil || cfg.BaseURL == nil || cfg.APIKey == nil {
		return false
	}
	return *cfg.Provider == "openai" && *cfg.BaseURL != "" && *cfg.APIKey == apiKeyEnvPlaceholder
}

func buildSttBlock(model, baseURL string) string {
	return fmt.Sprintf("stt:\n  enabled: true\n  provider: \"openai\"\n  openai:\n    model: %q\n    base_url: %q\n    api_key: %s\n",
		model, baseURL, apiKeyEnvPlaceholder)
}

func upsertSttDirect(body, key, value string) string {
	re := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(key) + `:[^\r\n]*`)
	if re.MatchString(body) {
		return re.ReplaceAllString(body, "  "+key+": "+value)
	}
	head := "  " + key + ": " + value + "\n"
	if body != "" && !strings.HasSuffix(body, "\n") {
		return body + "\n" + head
	}
	return body + head
}

func upsertSttOpenai(body, key, value string) string {
	sub := sttSubRE("openai").FindStringSubmatch(body)
	if sub == nil {
		block := "  openai:\n    " + key + ": " + value + "\n"
		if body != "" && !strings.HasSuffix(body, "\n") {
			return body + "\n" + block
		}
		return body + block
	}
	re := regexp.MustCompile(`(?m)^ {4}` + regexp.QuoteMeta(key) + `:[^\r\n]*`)
	subBody := sub[1]
	if re.MatchString(subBody) {
		subBody = re.ReplaceAllString(subBody, "    "+key+": "+value)
	} else if subBody == "" || strings.HasSuffix(subBody, "\n") {
		subBody += "    " + key + ": " + value + "\n"
	} else {
		subBody += "\n    " + key + ": " + value + "\n"
	}
	return sttSubRE("openai").ReplaceAllString(body, "  openai:\n"+subBody)
}

// upsertStt9Router writes the 9router STT wiring, preserving every sibling
// setting the user already has (language, local model, echo_transcripts...).
func upsertStt9Router(yaml, model, baseURL string) string {
	m := sttBlockRE.FindStringSubmatch(yaml)
	if m == nil {
		prefix := yaml
		if prefix != "" && !strings.HasSuffix(prefix, "\n") {
			prefix += "\n"
		}
		return prefix + buildSttBlock(model, baseURL)
	}
	body := m[1]
	body = upsertSttDirect(body, "enabled", "true")
	body = upsertSttDirect(body, "provider", `"openai"`)
	body = upsertSttOpenai(body, "model", fmt.Sprintf("%q", model))
	body = upsertSttOpenai(body, "base_url", fmt.Sprintf("%q", baseURL))
	body = upsertSttOpenai(body, "api_key", apiKeyEnvPlaceholder)
	return sttBlockRE.ReplaceAllString(yaml, "stt:\n"+body)
}

// removeStt9Router drops only the keys the wiring above writes — enabled
// (only when true), provider (only when "openai") and the openai
// model/base_url/api_key trio. Anything the user owns survives.
func removeStt9Router(yaml string) string {
	m := sttBlockRE.FindStringSubmatch(yaml)
	if m == nil {
		return yaml
	}
	body := m[1]
	body = regexp.MustCompile(`(?m)^  enabled:[ \t]*["']?true["']?[ \t]*\r?\n?`).ReplaceAllString(body, "")
	body = regexp.MustCompile(`(?m)^  provider:[ \t]*["']?openai["']?[ \t]*\r?\n?`).ReplaceAllString(body, "")
	if sub := sttSubRE("openai").FindStringSubmatch(body); sub != nil {
		subBody := sub[1]
		for _, key := range []string{"model", "base_url", "api_key"} {
			subBody = regexp.MustCompile(`(?m)^ {4}`+key+`:[^\r\n]*\r?\n?`).ReplaceAllString(subBody, "")
		}
		if strings.TrimSpace(subBody) == "" {
			body = sttSubRE("openai").ReplaceAllString(body, "")
		} else {
			body = sttSubRE("openai").ReplaceAllString(body, "  openai:\n"+subBody)
		}
	}
	if strings.TrimSpace(body) == "" {
		return strings.TrimLeft(sttBlockRE.ReplaceAllString(yaml, ""), "\n")
	}
	return sttBlockRE.ReplaceAllString(yaml, "stt:\n"+body)
}
