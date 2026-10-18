package dashboard

import (
	json "encoding/json/v2"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

func rateKeys(rates map[string]any) []string {
	keys := make([]string, 0, len(rates))
	for key := range rates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestNormalizeDisplayCurrency(t *testing.T) {
	tooMany := make(map[string]any, 30)
	for _, code := range cappedRateKeys(30) {
		tooMany[code] = 1.5
	}

	tests := []struct {
		name       string
		input      any
		clear      bool
		code       string
		rateKeys   []string
		showBoth   bool
		hasUpdated bool
	}{
		{name: "no value clears the setting", input: nil, clear: true},
		{name: "a scalar clears the setting", input: "IDR", clear: true},
		{name: "an object with nothing usable clears the setting", input: map[string]any{}, clear: true},
		{
			name:  "a malformed code without rates clears the setting",
			input: map[string]any{"code": "RUPIAH"},
			clear: true,
		},
		{
			name:     "the code is upper-cased and defaults to a single amount",
			input:    map[string]any{"code": " idr "},
			code:     "IDR",
			rateKeys: []string{},
			showBoth: false,
		},
		{
			name: "rates are upper-cased and junk rates are dropped",
			input: map[string]any{
				"code":     "IDR",
				"showBoth": true,
				"rates": map[string]any{
					"idr": 16400.0, "eur": 0.92, "usd": "1", "gbp": -1.0,
					"jpy": math.NaN(), "not-a-code": 5.0, "sgd": 1.34,
				},
			},
			code:     "IDR",
			rateKeys: []string{"EUR", "IDR", "SGD"},
			showBoth: true,
		},
		{
			name:     "the rate table is capped",
			input:    map[string]any{"code": "USD", "rates": tooMany},
			code:     "USD",
			rateKeys: cappedRateKeys(maxCurrencyRates),
			showBoth: false,
		},
		{
			name:     "dual display can be switched off",
			input:    map[string]any{"code": "EUR", "showBoth": false},
			code:     "EUR",
			rateKeys: []string{},
			showBoth: false,
		},
		{
			name:       "an RFC3339 stamp survives and anything else is dropped",
			input:      map[string]any{"code": "IDR", "updatedAt": "2026-10-18T09:00:00Z"},
			code:       "IDR",
			rateKeys:   []string{},
			showBoth:   false,
			hasUpdated: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeDisplayCurrency(tt.input)
			if tt.clear {
				if got != nil {
					t.Fatalf("normalizeDisplayCurrency() = %v, want nil (clear)", got)
				}
				return
			}
			if got == nil {
				t.Fatal("normalizeDisplayCurrency() = nil, want a stored shape")
			}
			if got["code"] != tt.code {
				t.Errorf("code = %v, want %v", got["code"], tt.code)
			}
			if got["showBoth"] != tt.showBoth {
				t.Errorf("showBoth = %v, want %v", got["showBoth"], tt.showBoth)
			}
			rates, ok := got["rates"].(map[string]any)
			if !ok {
				t.Fatalf("rates = %#v, want a map", got["rates"])
			}
			if got := strings.Join(rateKeys(rates), ","); got != strings.Join(tt.rateKeys, ",") {
				t.Errorf("rate keys = %s, want %s", got, strings.Join(tt.rateKeys, ","))
			}
			_, hasUpdated := got["updatedAt"]
			if hasUpdated != tt.hasUpdated {
				t.Errorf("updatedAt present = %v, want %v", hasUpdated, tt.hasUpdated)
			}
		})
	}
}

// cappedRateKeys spells the codes a capped rate table keeps: three letters only,
// already in the order the normalizer stores them.
func cappedRateKeys(n int) []string {
	keys := make([]string, 0, n)
	for i := range n {
		keys = append(keys, fmt.Sprintf("A%c%c", 'A'+i/26, 'A'+i%26))
	}
	return keys
}

func TestHandleUpdateSettingsDisplayCurrency(t *testing.T) {
	repo, cleanup := setupSettingsTestDB(t)
	defer cleanup()
	router := setupTestRouter(repo)

	put := func(t *testing.T, payload string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT /api/settings = %d: %s", rec.Code, rec.Body.String())
		}
	}
	get := func(t *testing.T) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode settings: %v", err)
		}
		return body
	}

	// A payload the panel could not have produced must not reach the cost
	// surfaces: it clears the setting instead of storing half a table.
	put(t, `{"displayCurrency":{"code":"rupiah","rates":{"IDR":-5}}}`)
	if _, present := get(t)["displayCurrency"]; present {
		t.Fatalf("an unusable payload must clear the setting")
	}

	put(t, `{"displayCurrency":{"code":"idr","rates":{"IDR":16400,"eur":0.92,"jpy":"x"},"showBoth":true}}`)
	currency, ok := get(t)["displayCurrency"].(map[string]any)
	if !ok {
		t.Fatalf("displayCurrency missing from GET /api/settings")
	}
	if currency["code"] != "IDR" || currency["showBoth"] != true {
		t.Fatalf("stored currency = %#v", currency)
	}
	rates, _ := currency["rates"].(map[string]any)
	if got := strings.Join(rateKeys(rates), ","); got != "EUR,IDR" {
		t.Fatalf("stored rates = %s, want EUR,IDR", got)
	}
}

func TestHandleRefreshCurrencyRates(t *testing.T) {
	repo, cleanup := setupSettingsTestDB(t)
	defer cleanup()
	router := setupTestRouter(repo)

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":"success","rates":{"IDR":16400.5,"EUR":0.92,"JPY":151.2}}`))
	}))
	defer good.Close()

	restore := currencyRateSources
	t.Cleanup(func() { currencyRateSources = restore })

	post := func(t *testing.T, payload string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/settings/currency/refresh", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// The first provider is down, so the second one answers.
	currencyRateSources = []string{broken.URL, good.URL}
	rec := post(t, `{"code":"IDR","showBoth":true,"codes":["IDR","EUR","JPY"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode refresh: %v", err)
	}
	if body["source"] != good.URL {
		t.Errorf("source = %v, want the provider that answered (%s)", body["source"], good.URL)
	}
	if body["ratesStored"] != float64(3) {
		t.Errorf("ratesStored = %v, want 3", body["ratesStored"])
	}
	stored, _ := body["displayCurrency"].(map[string]any)
	if stored["code"] != "IDR" || stored["updatedAt"] == nil {
		t.Fatalf("stored currency = %#v", stored)
	}

	// Only the codes the caller asked for are kept.
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, req)
	var settings map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	persisted, _ := settings["displayCurrency"].(map[string]any)
	rates, _ := persisted["rates"].(map[string]any)
	if got := strings.Join(rateKeys(rates), ","); got != "EUR,IDR,JPY" {
		t.Fatalf("persisted rates = %s, want EUR,IDR,JPY", got)
	}

	// Every provider down: the stored table must survive untouched.
	currencyRateSources = []string{broken.URL}
	before := fmt.Sprint(persisted)
	if rec := post(t, `{"code":"IDR","codes":["IDR"]}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("refresh with every provider down = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	getRec = httptest.NewRecorder()
	router.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if err := json.Unmarshal(getRec.Body.Bytes(), &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	after, _ := settings["displayCurrency"].(map[string]any)
	if fmt.Sprint(after) != before {
		t.Fatalf("a failed refresh changed the stored table: %v -> %v", before, after)
	}

	// A request with no codes cannot be served.
	if rec := post(t, `{"code":"IDR","codes":[]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("refresh without codes = %d, want 400", rec.Code)
	}
}
