package dashboard

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/proxy"
)

// displayCurrencySettingKey is the settings key the dashboard's Currency section
// writes and every cost surface reads. Costs themselves are always computed and
// stored in USD (internal/pricing); this only says how they are shown.
const displayCurrencySettingKey = "displayCurrency"

// maxCurrencyRates caps the stored rate table. The curated catalog lives in
// web/src/lib/currency.ts; this only keeps the stored shape honest.
const maxCurrencyRates = 24

// currencyRateTimeout bounds one rate provider call.
const currencyRateTimeout = 8 * time.Second

// currencyCodePattern matches the ISO-4217 codes the panel can offer.
var currencyCodePattern = regexp.MustCompile(`^[A-Z]{3}$`)

// currencyRateSources are the public, key-less rate providers, tried in order.
// A test replaces them with an httptest server.
var currencyRateSources = []string{
	"https://open.er-api.com/v6/latest/USD",
	"https://api.frankfurter.app/latest?from=USD",
}

// normalizeDisplayCurrency coerces a client payload into the stored shape
// {"code": "IDR", "rates": {"IDR": 16400}, "showBoth": true, "updatedAt": …}.
//
// It returns nil when the payload carries no usable code and no usable rate,
// which callers treat as "clear the setting": a write that cannot be understood
// must not leave a half-applied table behind.
func normalizeDisplayCurrency(value any) map[string]any {
	container, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	code, hasCode := normalizeCurrencyCode(container["code"])
	rates := normalizeCurrencyRates(container["rates"])
	if !hasCode && len(rates) == 0 {
		return nil
	}
	if !hasCode {
		code = "USD"
	}
	// A row that predates the feature carries no flag; a single amount in the
	// chosen currency is what it showed, so that is what it keeps.
	showBoth := false
	if raw, present := container["showBoth"]; present {
		showBoth, _ = raw.(bool)
	}
	out := map[string]any{
		"code":     code,
		"rates":    rates,
		"showBoth": showBoth,
	}
	if stamp, ok := normalizeCurrencyTimestamp(container["updatedAt"]); ok {
		out["updatedAt"] = stamp
	}
	return out
}

// normalizeCurrencyCode accepts a three-letter ISO code in any case.
func normalizeCurrencyCode(raw any) (string, bool) {
	text, ok := raw.(string)
	if !ok {
		return "", false
	}
	code := strings.ToUpper(strings.TrimSpace(text))
	if !currencyCodePattern.MatchString(code) {
		return "", false
	}
	return code, true
}

// normalizeCurrencyRates keeps the codes and rates a rate of exchange can use,
// so a hand-edited row cannot inject a NaN, a negative rate or an unbounded
// table into the settings blob. The result is sorted, which makes the stored
// JSON stable across writes.
func normalizeCurrencyRates(value any) map[string]any {
	raw, ok := value.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	valid := make(map[string]any, len(raw))
	for key, candidate := range raw {
		code, ok := normalizeCurrencyCode(key)
		if !ok {
			continue
		}
		rate, ok := currencyRate(candidate)
		if !ok {
			continue
		}
		if _, seen := valid[code]; seen {
			continue
		}
		valid[code] = rate
	}
	// The cap is applied after sorting, so which codes survive does not depend
	// on Go's randomized map order.
	codes := make([]string, 0, len(valid))
	for code := range valid {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	if len(codes) > maxCurrencyRates {
		codes = codes[:maxCurrencyRates]
	}
	out := make(map[string]any, len(codes))
	for _, code := range codes {
		out[code] = valid[code]
	}
	return out
}

// currencyRate accepts the JSON numbers a rate table carries: positive, finite
// and small enough that one USD is not suddenly worth a billion units.
func currencyRate(value any) (float64, bool) {
	var rate float64
	switch typed := value.(type) {
	case float64:
		rate = typed
	case int:
		rate = float64(typed)
	case int64:
		rate = float64(typed)
	default:
		return 0, false
	}
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 || rate > 1e9 {
		return 0, false
	}
	return rate, true
}

// normalizeCurrencyTimestamp keeps an RFC3339 stamp and drops anything else, so
// the panel never renders "updated NaN undefined ago".
func normalizeCurrencyTimestamp(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	if _, err := time.Parse(time.RFC3339, text); err != nil {
		return "", false
	}
	return text, true
}

// applyDisplayCurrencyUpdate normalizes the key in HandleUpdateSettings' payload.
//
// An unparsable payload is stored as an explicit null rather than deleted:
// UpdateSettingsRaw merges into the stored row, so dropping the map entry would
// leave the previous currency in place.
func applyDisplayCurrencyUpdate(updates map[string]any) {
	raw, present := updates[displayCurrencySettingKey]
	if !present {
		return
	}
	if normalized := normalizeDisplayCurrency(raw); normalized != nil {
		updates[displayCurrencySettingKey] = normalized
		return
	}
	updates[displayCurrencySettingKey] = nil
}

// sanitizeDisplayCurrency re-reads the stored value through the same normalizer,
// so a row written by an older build (or by hand) reaches the dashboard in the
// shape the cost surfaces expect.
func sanitizeDisplayCurrency(settings map[string]any) {
	raw, present := settings[displayCurrencySettingKey]
	if !present {
		return
	}
	if normalized := normalizeDisplayCurrency(raw); normalized != nil {
		settings[displayCurrencySettingKey] = normalized
		return
	}
	delete(settings, displayCurrencySettingKey)
}

// currencyRefreshRequest is the body of POST /api/settings/currency/refresh: the
// currency to show, whether to show USD alongside it, and the codes the caller
// wants rates for (web/src/lib/currency.ts owns that catalog).
type currencyRefreshRequest struct {
	Code     string   `json:"code"`
	ShowBoth *bool    `json:"showBoth"`
	Codes    []string `json:"codes"`
}

// HandleRefreshCurrencyRates handles POST /api/settings/currency/refresh: read
// the live USD rates from a public provider and store the requested subset.
// Manual rates keep working when every provider is unreachable — this only
// refreshes them, and a failure leaves the stored table untouched (502).
func (h *DashboardHandler) HandleRefreshCurrencyRates(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writePlainError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	defer func() { _ = r.Body.Close() }()

	var payload currencyRefreshRequest
	if err := json.Unmarshal(body, &payload); err != nil {
		writePlainError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	code, ok := normalizeCurrencyCode(payload.Code)
	if !ok {
		writePlainError(w, http.StatusBadRequest, "code must be a three-letter currency code")
		return
	}
	codes := normalizeRequestedCodes(payload.Codes)
	if len(codes) == 0 {
		writePlainError(w, http.StatusBadRequest, "codes must list at least one three-letter currency code")
		return
	}

	rates, source, err := fetchCurrencyRates(r.Context(), codes)
	if err != nil {
		writePlainError(w, http.StatusBadGateway, err.Error())
		return
	}

	showBoth := true
	if payload.ShowBoth != nil {
		showBoth = *payload.ShowBoth
	}
	stored := map[string]any{
		"code":      code,
		"rates":     rates,
		"showBoth":  showBoth,
		"updatedAt": time.Now().UTC().Format(time.RFC3339),
	}
	if err := h.Repo.UpdateSettingsRaw(map[string]any{displayCurrencySettingKey: stored}); err != nil {
		writePlainError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if raw, err := h.Repo.GetSettingsRaw(); err == nil {
		sanitizeDisplayCurrency(raw)
		handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
			"displayCurrency": raw[displayCurrencySettingKey],
			"source":          source,
			"ratesStored":     len(rates),
		})
		return
	}
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"displayCurrency": stored,
		"source":          source,
		"ratesStored":     len(rates),
	})
}

// normalizeRequestedCodes de-duplicates the codes a refresh asked for and keeps
// only well-formed ones, in request order.
func normalizeRequestedCodes(raw []string) []string {
	codes := make([]string, 0, len(raw))
	for _, candidate := range raw {
		code, ok := normalizeCurrencyCode(candidate)
		if !ok {
			continue
		}
		if len(codes) == maxCurrencyRates {
			break
		}
		duplicate := false
		for _, seen := range codes {
			if seen == code {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		codes = append(codes, code)
	}
	return codes
}

// fetchCurrencyRates walks the providers in order and returns the rates for the
// requested codes from the first one that answers. Only three-letter codes are
// kept, and the source URL is reported back so the panel can say where the
// numbers came from.
func fetchCurrencyRates(ctx context.Context, codes []string) (map[string]any, string, error) {
	client := &http.Client{Transport: proxy.NewSafeTransport(), Timeout: currencyRateTimeout}
	wanted := make(map[string]struct{}, len(codes))
	for _, code := range codes {
		wanted[code] = struct{}{}
	}

	var attempts []string
	for _, source := range currencyRateSources {
		rates, err := readCurrencyRates(ctx, client, source, wanted)
		if err != nil {
			attempts = append(attempts, fmt.Sprintf("%s: %v", source, err))
			continue
		}
		if len(rates) == 0 {
			attempts = append(attempts, source+": no requested currency in the response")
			continue
		}
		return rates, source, nil
	}
	return nil, "", fmt.Errorf("no rate provider answered (%s)", strings.Join(attempts, "; "))
}

// readCurrencyRates performs one provider call. The providers answer
// {"rates": {"IDR": 16400, …}}, which fields of a larger payload are otherwise
// ignored.
func readCurrencyRates(ctx context.Context, client *http.Client, source string, wanted map[string]struct{}) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "9Router")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	// A rate payload is small; the cap keeps a broken or hostile provider from
	// streaming an unbounded body into memory. encoding/json/v2 decodes bytes,
	// so the reader is drained rather than handed straight to Unmarshal.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var payload struct {
		Rates map[string]any `json:"rates"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}

	rates := make(map[string]any, len(wanted))
	for code, value := range payload.Rates {
		normalized, ok := normalizeCurrencyCode(code)
		if !ok {
			continue
		}
		if _, wantedCode := wanted[normalized]; !wantedCode {
			continue
		}
		if rate, ok := currencyRate(value); ok {
			rates[normalized] = rate
		}
	}
	return rates, nil
}
