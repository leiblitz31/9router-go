package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"9router/proxy/internal/log"
)

// Nous Research Portal exposes an OpenRouter-shaped catalogue. The ids are
// namespaced (`vendor/model`), the JSON arrives as a bare array or under one of
// data/models/results, and the live list mixes chat, embedding and image-only
// entries — none of which are valid LLM choices.
//
// Port of upstream open-sse/services/nous.js normalizeNousModels /
// normalizeNousFreeModels (decolua/9router 165f9b90).
const (
	nousModelsURL        = "https://inference-api.nousresearch.com/v1/models"
	nousCatalogHTTP      = 15 * time.Second
	nousFirstPartyPrefix = "nousresearch/"
)

// nousFreeMinContext is the window floor the provider detail page applies to
// the zero-cost list: a 32K free model is noise next to a 1M one.
const nousFreeMinContext = 200000

// nousModelsPayload is the subset of each catalogue entry this code reads.
// `context_length` and `top_provider` sit at the top level in OpenRouter shape;
// pricing is per-model.
type nousModelsPayload struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	ContextLength *int   `json:"context_length"`
	Architecture  struct {
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
	TopProvider struct {
		ContextLength       *int `json:"context_length"`
		MaxCompletionTokens *int `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Pricing struct {
		Prompt     json.Number `json:"prompt"`
		Completion json.Number `json:"completion"`
	} `json:"pricing"`
}

// nousRawEnvelope covers the three container shapes the endpoint is seen in.
type nousRawEnvelope struct {
	Data    []nousModelsPayload `json:"data"`
	Models  []nousModelsPayload `json:"models"`
	Results []nousModelsPayload `json:"results"`
}

// nousRawModels returns the entries from whichever shape arrived.
func nousRawModels(payload []byte) []nousModelsPayload {
	var arr []nousModelsPayload
	if json.Unmarshal(payload, &arr) == nil && arr != nil {
		return arr
	}
	var env nousRawEnvelope
	if json.Unmarshal(payload, &env) != nil {
		return nil
	}
	switch {
	case env.Data != nil:
		return env.Data
	case env.Models != nil:
		return env.Models
	case env.Results != nil:
		return env.Results
	}
	return nil
}

// nousPositiveInt reads a value only when it is a positive integer.
func nousPositiveInt(v *int) int {
	if v == nil || *v <= 0 {
		return 0
	}
	return *v
}

func nousPositiveNumber(n json.Number) int {
	if n == "" {
		return 0
	}
	f, err := n.Float64()
	if err != nil {
		return 0
	}
	i := int(f)
	if float64(i) != f || i <= 0 {
		return 0
	}
	return i
}

// nousIsTextCapable drops entries that cannot answer with text.
func nousIsTextCapable(m nousModelsPayload) bool {
	out := m.Architecture.OutputModalities
	return len(out) == 0 || slicesContains(out, "text")
}

// nousIsZeroPrice reports an explicit 0.0 price. A missing or malformed price
// is NOT zero: the free list must never widen to a paid model by accident.
func nousIsZeroPrice(n json.Number) bool {
	if n == "" {
		return false
	}
	f, err := n.Float64()
	if err != nil {
		return false
	}
	return f == 0
}

func slicesContains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// normalizeNousModels converts the live payload into picker entries: text-only,
// id + limits resolved, first-party Nous models promoted while the upstream
// order is preserved inside each group.
func normalizeNousModels(payload []byte) []LiveModel {
	models := normalizeNousModelsFiltered(payload, nil)
	return models
}

// normalizeNousModelsFiltered applies the same transform under an extra
// predicate, so the free-catalogue path reuses one implementation.
func normalizeNousModelsFiltered(payload []byte, keep func(nousModelsPayload, int) bool) []LiveModel {
	raw := nousRawModels(payload)
	out := make([]LiveModel, 0, len(raw))
	for _, m := range raw {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		if !nousIsTextCapable(m) {
			continue
		}
		ctx := nousPositiveInt(m.ContextLength)
		if ctx == 0 {
			ctx = nousPositiveInt(m.TopProvider.ContextLength)
		}
		if keep != nil && !keep(m, ctx) {
			continue
		}
		entry := LiveModel{ID: id, Name: m.Name, ContextLength: ctx}
		if entry.Name == "" {
			entry.Name = id
		}
		entry.MaxOutput = nousPositiveInt(m.TopProvider.MaxCompletionTokens)
		out = append(out, entry)
	}

	// Stable: first-party first, upstream order kept inside each group.
	sort.SliceStable(out, func(i, j int) bool {
		return strings.HasPrefix(out[i].ID, nousFirstPartyPrefix) && !strings.HasPrefix(out[j].ID, nousFirstPartyPrefix)
	})
	return out
}

// normalizeNousFreeModels is the zero-cost list the provider detail page shows,
// largest window first.
func normalizeNousFreeModels(payload []byte) []LiveModel {
	models := normalizeNousModelsFiltered(payload, func(m nousModelsPayload, ctx int) bool {
		return nousIsZeroPrice(m.Pricing.Prompt) &&
			nousIsZeroPrice(m.Pricing.Completion) &&
			ctx >= nousFreeMinContext
	})
	sort.SliceStable(models, func(i, j int) bool {
		return models[i].ContextLength > models[j].ContextLength
	})
	return models
}

// fetchNousLiveCatalog reads the Portal catalogue. The endpoint is public, so
// the credential is only attached when one exists — an anonymous read still
// returns the same list, and a stale key must not blank the picker.
func (h *ChatHandler) fetchNousLiveCatalog(ctx context.Context, accessToken string) []LiveModel {
	reqCtx, cancel := context.WithTimeout(ctx, nousCatalogHTTP)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, nousModelsURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}

	resp, err := h.Client.Do(req)
	if err != nil {
		log.Warn("models", "nous catalog request failed", "error", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warn("models", "nous catalog returned", "status", resp.StatusCode)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil
	}
	models := normalizeNousModels(body)
	if len(models) == 0 {
		return nil
	}
	// Every Nous model is published under the nous/ prefix, so a caller can
	// request one without knowing the vendor namespace.
	for i := range models {
		models[i].ID = "nous/" + models[i].ID
	}
	return models
}
